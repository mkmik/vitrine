package main

import (
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

var refRE = regexp.MustCompile(`(?:/(?:a|c)/)?([a-z2-7]{10})(?:/(?:raw/|source/)?v/(\d+)|/raw|/source|/\d+)?/?(?:[?#].*)?$`)

// parseRef accepts an artifact id or any of its URLs and returns the id and an optional version.
func parseRef(ref string) (string, int) {
	ref = strings.TrimSpace(ref)
	m := refRE.FindStringSubmatch(ref)
	if m == nil {
		return ref, 0
	}
	n := 0
	if m[2] != "" {
		fmt.Sscan(m[2], &n)
	}
	return m[1], n
}

type opError struct {
	Code string
	Msg  string
	Data map[string]any
}

func (e *opError) Error() string { return e.Msg }

func oerr(code, f string, a ...any) *opError { return &opError{Code: code, Msg: fmt.Sprintf(f, a...)} }

// checkContent validates one version's content; kind names the field for errors.
func (s *Server) checkContent(kind, text string) error {
	if strings.TrimSpace(text) == "" {
		return oerr("invalid_argument", "%s is empty", kind)
	}
	if len(text) > s.cfg.MaxVersionBytes {
		return oerr("too_large", "%s is %d bytes; the limit is %d", kind, len(text), s.cfg.MaxVersionBytes)
	}
	return nil
}

// mdTitle returns the text of a document's first "# " heading, if any.
func mdTitle(md string) string {
	for _, ln := range strings.SplitN(md, "\n", 40) {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "# ") {
			return strings.TrimSpace(strings.TrimLeft(ln, "# "))
		}
	}
	return ""
}

func (s *Server) opCreate(p *Principal, kind, title, desc, html string, caps json.RawMessage) (*Artifact, []string, error) {
	if !validKind(kind) {
		return nil, nil, oerr("invalid_argument", "kind must be html or markdown")
	}
	if kind == kindMarkdown && strings.TrimSpace(title) == "" {
		title = mdTitle(html)
	}
	if !s.mayCreate(p.User) {
		return nil, nil, oerr("not_permitted", "%s may not create artifacts on this server", p.User.Email)
	}
	if strings.TrimSpace(title) == "" {
		return nil, nil, oerr("invalid_argument", "title is required")
	}
	if err := s.checkContent(kind, html); err != nil {
		return nil, nil, err
	}
	var warnings []string
	if kind == kindMarkdown && len(caps) > 0 && string(caps) != "null" && string(caps) != "{}" {
		return nil, nil, oerr("invalid_argument", "markdown artifacts have no runtime; capabilities apply to html artifacts only")
	}
	if len(caps) > 0 && string(caps) != "null" {
		_, w, err := validateCaps(caps)
		if err != nil {
			return nil, nil, oerr("invalid_argument", "capabilities: %v", err)
		}
		warnings = w
	} else {
		caps = nil
	}
	a, err := s.st.createArtifact(p.User.ID, kind, strings.TrimSpace(title), desc, html, caps, p.Label)
	return a, warnings, err
}

func (s *Server) loadForAgent(p *Principal, ref string, need int) (*Artifact, Access, error) {
	id, _ := parseRef(ref)
	a, err := s.st.artifact(id)
	if err != nil {
		return nil, Access{}, oerr("not_found", "no artifact %q", id)
	}
	acc := s.st.accessFor(a, p.User)
	if acc.Level == lvNone {
		return nil, acc, oerr("not_found", "no artifact %q", id)
	}
	if acc.Level < need {
		return nil, acc, oerr("not_permitted", "you have %s access to %s; this needs %s", levelName(acc.Level), id, levelName(need))
	}
	return a, acc, nil
}

func conflictErr(a *Artifact, base int) error {
	return &opError{Code: "conflict", Msg: fmt.Sprintf("base_version %d is not the head: the head is %d. Call read to get the current content, re-apply your change, and retry with base_version %d.", base, a.Head, a.Head),
		Data: map[string]any{"head": a.Head}}
}

// opRewrite replaces the whole content. kind is the field the caller used
// (html or markdown); it must match the artifact's kind.
func (s *Server) opRewrite(p *Principal, ref string, base int, kind, html string, title, desc *string) (*Artifact, error) {
	a, _, err := s.loadForAgent(p, ref, lvEdit)
	if err != nil {
		return nil, err
	}
	if kind != a.Kind {
		return nil, oerr("invalid_argument", "%s is a %s artifact: pass its new content as %s, not %s", a.ID, a.Kind, a.Kind, kind)
	}
	if err := s.checkContent(kind, html); err != nil {
		return nil, err
	}
	na, err := s.st.writeVersion(a.ID, base, html, title, desc, p.Label, "")
	if err == errConflict {
		return nil, conflictErr(na, base)
	}
	if err != nil {
		return nil, err
	}
	s.hub.artifactChanged(na)
	return na, nil
}

type Edit struct {
	OldText string `json:"old_text"`
	NewText string `json:"new_text"`
}

func applyEdits(html string, edits []Edit) (string, error) {
	if len(edits) == 0 {
		return "", oerr("invalid_argument", "edits is empty")
	}
	for i, e := range edits {
		if e.OldText == "" {
			return "", oerr("invalid_argument", "edit %d: old_text is empty", i)
		}
		c := strings.Count(html, e.OldText)
		if c == 0 {
			return "", oerr("edit_failed", "edit %d: old_text not found (nothing was applied). Read the current version and copy the text exactly, whitespace included.", i)
		}
		if c > 1 {
			return "", oerr("edit_failed", "edit %d: old_text occurs %d times (nothing was applied). Include more surrounding text so it matches once.", i, c)
		}
		html = strings.Replace(html, e.OldText, e.NewText, 1)
	}
	return html, nil
}

func (s *Server) opEdit(p *Principal, ref string, base int, edits []Edit, title *string) (*Artifact, error) {
	a, _, err := s.loadForAgent(p, ref, lvEdit)
	if err != nil {
		return nil, err
	}
	if base != a.Head {
		return nil, conflictErr(a, base)
	}
	cur, err := s.st.versionHTML(a.ID, a.Head)
	if err != nil {
		return nil, err
	}
	html, err := applyEdits(cur, edits)
	if err != nil {
		return nil, err
	}
	if err := s.checkContent(a.Kind, html); err != nil {
		return nil, err
	}
	na, err := s.st.writeVersion(a.ID, base, html, title, nil, p.Label, "")
	if err == errConflict {
		return nil, conflictErr(na, base)
	}
	if err != nil {
		return nil, err
	}
	s.hub.artifactChanged(na)
	return na, nil
}

func (s *Server) opRestore(id string, version, base int, author string) (*Artifact, error) {
	html, err := s.st.versionHTML(id, version)
	if err != nil {
		return nil, oerr("not_found", "no version %d", version)
	}
	na, err := s.st.writeVersion(id, base, html, nil, nil, author, fmt.Sprintf("restored from v%d", version))
	if err == errConflict {
		return nil, conflictErr(na, base)
	}
	if err != nil {
		return nil, err
	}
	s.hub.artifactChanged(na)
	return na, nil
}

// opShare grants or revokes. who: public | users | link | an email address.
func (s *Server) opShare(a *Artifact, who, level string, version int) ([]Grant, error) {
	who = strings.ToLower(strings.TrimSpace(who))
	var aud string
	switch who {
	case "public", "anyone":
		aud = "public"
	case "users", "signed-in", "all":
		aud = "users"
	case "link":
		aud = "link"
	default:
		addr, err := mail.ParseAddress(who)
		if err != nil {
			return nil, oerr("invalid_argument", "who must be public, users, link, or an email address")
		}
		aud = "email:" + strings.ToLower(addr.Address)
	}
	switch level {
	case "view", "interact", "edit", "none":
	default:
		return nil, oerr("invalid_argument", "level must be view, interact, edit or none")
	}
	if aud == "public" && level != "none" && s.cfg.NoPublicSharing { // revoking stays possible
		return nil, oerr("invalid_argument", "public sharing is turned off on this server: share with \"users\" (everyone who can sign in), \"link\" (anyone who can sign in and has the link) or with people by email")
	}
	if aud == "public" && level == "edit" {
		return nil, oerr("invalid_argument", "the public can get view or interact, never edit")
	}
	if level == "edit" {
		version = 0
	}
	if version < 0 || version > a.Head {
		return nil, oerr("invalid_argument", "version %d does not exist (head is %d)", version, a.Head)
	}
	if err := s.st.setGrant(a.ID, Grant{Audience: aud, Level: level, Pinned: version}); err != nil {
		return nil, err
	}
	s.hub.accessChanged(a.ID)
	gs, err := s.st.grants(a.ID)
	if gs == nil {
		gs = []Grant{}
	}
	return gs, err
}

func (s *Server) opDeclare(p *Principal, ref string, caps json.RawMessage) (*Artifact, []string, error) {
	a, _, err := s.loadForAgent(p, ref, lvEdit)
	if err != nil {
		return nil, nil, err
	}
	if a.Kind == kindMarkdown {
		return nil, nil, oerr("invalid_argument", "%s is a markdown artifact; it has no runtime to declare capabilities for", a.ID)
	}
	if len(caps) == 0 || string(caps) == "null" {
		caps = json.RawMessage(`{}`)
	}
	_, warnings, err := validateCaps(caps)
	if err != nil {
		return nil, nil, oerr("invalid_argument", "%v", err)
	}
	if err := s.st.setCapabilities(a.ID, caps); err != nil {
		return nil, nil, err
	}
	a, _ = s.st.artifact(a.ID)
	s.hub.artifactChanged(a)
	return a, warnings, nil
}
