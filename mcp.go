package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

type mcpSession struct {
	userID     string
	seen       time.Time   // guarded by Server.mu
	guideShown atomic.Bool // concurrent tool calls share a session
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErr         `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

const serverInstructions = `vitrine hosts artifacts at stable links: single-file HTML pages (explainers, diagrams, prototypes, interactive apps) and markdown documents (specs, PRDs, notes, reports), which vitrine renders itself.
Create one with create (pass html for a page, markdown for a document) and give the user the link. To iterate, use edit (exact find-and-replace) or rewrite (whole page), always passing base_version = the version you last saw; open views reload by themselves. When the user pastes a link, call read first.
Pages can keep shared data (db: a last-writer-wins document store; for counters or co-editing, one document per writer, merged on read), see who else is here (room), call a model (sample) and know the viewer (user) through await claude.use(name), the same API as Claude artifacts. Call guide before writing a page that uses them.`

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	p, ok := s.bearer(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	body := http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxVersionBytes)*2+(1<<20))
	var raw json.RawMessage
	if err := json.NewDecoder(body).Decode(&raw); err != nil {
		writeJSON(w, 400, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32700, "parse error"}})
		return
	}
	sid := r.Header.Get("Mcp-Session-Id")
	batch := len(raw) > 0 && raw[0] == '['
	var reqs []rpcReq
	if batch {
		json.Unmarshal(raw, &reqs)
	} else {
		var one rpcReq
		if err := json.Unmarshal(raw, &one); err != nil {
			writeJSON(w, 400, rpcResp{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcErr{-32600, "invalid request"}})
			return
		}
		reqs = []rpcReq{one}
	}
	hasInit := slices.ContainsFunc(reqs, func(rq rpcReq) bool { return rq.Method == "initialize" })
	if sid != "" && !hasInit && !s.knownMCPSession(p, sid) {
		// expired, evicted, lost in a restart, or someone else's: the spec's 404 makes the client initialize again
		http.Error(w, "unknown Mcp-Session-Id, send initialize again", http.StatusNotFound)
		return
	}
	var out []rpcResp
	for _, rq := range reqs {
		if rq.Method == "initialize" {
			sid = randToken("")[:32]
			w.Header().Set("Mcp-Session-Id", sid)
		}
		res, rerr := s.mcpDispatch(p, sid, rq)
		if len(rq.ID) == 0 || string(rq.ID) == "null" {
			continue // notification
		}
		out = append(out, rpcResp{JSONRPC: "2.0", ID: rq.ID, Result: res, Error: rerr})
	}
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if batch {
		writeJSON(w, 200, out)
	} else {
		writeJSON(w, 200, out[0])
	}
}

const mcpSessionsPerUser = 50

// newMCPSession stores sid for p. Past the per-user cap it drops p's least
// recently used session; that client gets a 404 and initializes again.
// ponytail: scans all sessions per initialize; index by user if that ever shows in a profile.
func (s *Server) newMCPSession(p *Principal, sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, oldest := 0, ""
	for k, ms := range s.mcpSessions {
		if ms.userID == p.User.ID {
			if n++; oldest == "" || ms.seen.Before(s.mcpSessions[oldest].seen) {
				oldest = k
			}
		}
	}
	if n >= mcpSessionsPerUser {
		delete(s.mcpSessions, oldest)
	}
	s.mcpSessions[sid] = &mcpSession{userID: p.User.ID, seen: time.Now()}
}

func (s *Server) knownMCPSession(p *Principal, sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.mcpSessions[sid]
	return ms != nil && ms.userID == p.User.ID
}

// mcpSession returns p's session for sid. Only initialize stores sessions, so
// an unknown id, or one that belongs to another user, gets a throwaway: made-up
// ids can't grow the map or touch someone else's session.
func (s *Server) mcpSession(p *Principal, sid string) *mcpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.mcpSessions[sid]
	if !ok || ms.userID != p.User.ID {
		return &mcpSession{userID: p.User.ID}
	}
	ms.seen = time.Now()
	return ms
}

func (s *Server) mcpDispatch(p *Principal, sid string, rq rpcReq) (any, *rpcErr) {
	switch rq.Method {
	case "initialize":
		var ip struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(rq.Params, &ip)
		pv := ip.ProtocolVersion
		if pv == "" {
			pv = "2025-06-18"
		}
		s.newMCPSession(p, sid)
		return map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "vitrine", "title": "Artifacts", "version": version},
			"instructions":    serverInstructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, nil
	case "tools/call":
		var cp struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(rq.Params, &cp); err != nil {
			return nil, &rpcErr{-32602, "bad params"}
		}
		if !s.allow("mcp:"+p.User.ID, 10, 60) {
			return toolError("rate_limited", "too many calls, slow down", nil), nil
		}
		return s.callTool(p, s.mcpSession(p, sid), cp.Name, cp.Arguments), nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	case "resources/templates/list":
		return map[string]any{"resourceTemplates": []any{}}, nil
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, nil
	}
	if strings.HasPrefix(rq.Method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcErr{-32601, "method not found: " + rq.Method}
}

type toolResult struct {
	Content []map[string]any `json:"content"`
	IsError bool             `json:"isError,omitempty"`
}

func textResult(v any, extra ...string) *toolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	tr := &toolResult{Content: []map[string]any{{"type": "text", "text": string(b)}}}
	for _, e := range extra {
		tr.Content = append(tr.Content, map[string]any{"type": "text", "text": e})
	}
	return tr
}

func toolError(code, msg string, data map[string]any) *toolResult {
	v := map[string]any{"error": code, "message": msg}
	for k, x := range data {
		v[k] = x
	}
	b, _ := json.Marshal(v)
	return &toolResult{Content: []map[string]any{{"type": "text", "text": string(b)}}, IsError: true}
}

func errResult(err error) *toolResult {
	var oe *opError
	if errors.As(err, &oe) {
		return toolError(oe.Code, oe.Msg, oe.Data)
	}
	var de *DBError
	if errors.As(err, &de) {
		return toolError(de.Code, de.Message, nil)
	}
	log.Printf("tool error: %v", err)
	return toolError("internal", err.Error(), nil)
}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

var readOnly = map[string]any{"readOnlyHint": true}

func toolDefs() []map[string]any {
	idArg := str("Artifact id, or any link to it (a pasted URL works).")
	return []map[string]any{
		{"name": "guide", "description": "Runtime docs for artifact pages: page rules, the CDN allowlist, and the db / room / sample / user capabilities behind await claude.use(name), with patterns. Call before writing a page that keeps data, shows other viewers, or calls a model.",
			"inputSchema": schema(map[string]any{"topic": map[string]any{"type": "string", "enum": []string{"overview", "markdown", "page", "db", "room", "sample", "user", "patterns", "all"}, "description": "Default overview."}}),
			"annotations": readOnly},
		{"name": "create", "description": "Create a new artifact. Pass exactly one of: html (one self-contained HTML page) or markdown (a document: GitHub-flavored markdown with tables, task lists, footnotes and ```mermaid diagrams, rendered by vitrine). Returns its stable link (give it to the user) and version 1. HTML pages can use shared storage, presence and AI through claude.use(): call guide first for those; their scripts and styles load only from the CDN allowlist. Prefer markdown for writing that people read and keep (specs, plans, notes, reports).",
			"inputSchema": schema(map[string]any{
				"title":        str("Short title. For markdown it defaults to the first # heading."),
				"html":         str("The complete HTML document (for a page)."),
				"markdown":     str("The complete markdown text (for a document)."),
				"description":  str("Optional one-line description, used in listings and search."),
				"capabilities": map[string]any{"type": "object", "description": `HTML pages only. Optional declaration, e.g. {"db":{},"user":{"scopes":["profile"]},"room":{},"sample":{}}. See guide.`},
			})},
		{"name": "read", "description": "Read an artifact: its kind (html or markdown), the content of its head (or a given version), the head version, title and capabilities. Call this when the user pastes a link, and before editing an artifact you did not just write.",
			"inputSchema": schema(map[string]any{"id": idArg, "version": num("Optional version; default the head.")}, "id"),
			"annotations": readOnly},
		{"name": "edit", "description": "Change an artifact's content (HTML or markdown text) with exact find-and-replace edits, applied in order. Each old_text must occur exactly once; all edits apply or none do. Creates a new version; open views reload. base_version must be the head you last saw, or the call is refused with the current head.",
			"inputSchema": schema(map[string]any{
				"id": idArg, "base_version": num("The version these edits apply to (the head)."),
				"edits": map[string]any{"type": "array", "items": schema(map[string]any{"old_text": str("Exact text to replace."), "new_text": str("Replacement (may be empty).")}, "old_text", "new_text")},
				"title": str("Optional new title."),
			}, "id", "base_version", "edits")},
		{"name": "rewrite", "description": "Replace an artifact's whole content, creating a new version; open views reload. Pass html for an html artifact or markdown for a markdown one (the kind cannot change). base_version must be the head you last saw.",
			"inputSchema": schema(map[string]any{"id": idArg, "base_version": num("The head you last saw."), "html": str("The complete new HTML (html artifacts)."), "markdown": str("The complete new markdown (markdown artifacts)."), "title": str("Optional new title."), "description": str("Optional new description.")}, "id", "base_version")},
		{"name": "list", "description": "List artifacts you own or that are shared with you, newest first.",
			"inputSchema": schema(map[string]any{"limit": num("Default 30, max 200."), "before": num("Pagination: updated_at of the last item seen.")}),
			"annotations": readOnly},
		{"name": "search", "description": "Full-text search over titles, descriptions and page text of artifacts you can see.",
			"inputSchema": schema(map[string]any{"query": str("Words to find (prefix match)."), "limit": num("Default 30.")}, "query"),
			"annotations": readOnly},
		{"name": "history", "description": "List an artifact's versions (number, size, author, time).",
			"inputSchema": schema(map[string]any{"id": idArg}, "id"), "annotations": readOnly},
		{"name": "restore", "description": "Make an old version the new head (writes it as a new version).",
			"inputSchema": schema(map[string]any{"id": idArg, "version": num("Version to restore."), "base_version": num("The head you last saw.")}, "id", "version", "base_version")},
		{"name": "share", "description": `Grant or revoke access. who: an email address, "users" (everyone who can sign in) or "public" (anyone with the link). level: view, interact (can also write the page's shared storage), edit (people only), or none to revoke. For view/interact, version pins viewers to that version (omit for the live head). Only the owner can share. Returns all grants.`,
			"inputSchema": schema(map[string]any{"id": idArg, "who": str("email | users | public"), "level": map[string]any{"type": "string", "enum": []string{"view", "interact", "edit", "none"}}, "version": num("Optional pinned version.")}, "id", "who", "level")},
		{"name": "declare", "description": "Set an artifact's capability declaration (db rules, room topics, sample, user scopes). Replaces the whole declaration. When adding a capability, declare first, then publish code that uses it.",
			"inputSchema": schema(map[string]any{"id": idArg, "capabilities": map[string]any{"type": "object", "description": "Whole declaration; see guide."}}, "id", "capabilities")},
		{"name": "db_get", "description": "Read an artifact's storage: one document (even number of path segments) or every document in a collection (odd).",
			"inputSchema": schema(map[string]any{"id": idArg, "path": str("e.g. tasks/t1 or tasks")}, "id", "path"), "annotations": readOnly},
		{"name": "db_query", "description": "Query a collection of an artifact's storage with where / orderBy / limit, like the page API.",
			"inputSchema": schema(map[string]any{"id": idArg, "collection": str("Collection path."),
				"where":   map[string]any{"type": "array", "items": schema(map[string]any{"field": str("Top-level field."), "op": str("== != < <= > >= in not-in array-contains"), "value": map[string]any{"description": "Value to compare."}}, "field", "op", "value")},
				"orderBy": str("Top-level field."), "dir": map[string]any{"type": "string", "enum": []string{"asc", "desc"}}, "limit": num("1-1000")}, "id", "collection"),
			"annotations": readOnly},
		{"name": "db_write", "description": "Seed or fix an artifact's storage: set (replace), update (merge into an existing document) or delete one document. Open pages see it live.",
			"inputSchema": schema(map[string]any{"id": idArg, "path": str("Document path."), "op": map[string]any{"type": "string", "enum": []string{"set", "update", "delete"}}, "data": map[string]any{"type": "object"}}, "id", "path", "op")},
		{"name": "delete", "description": "Permanently delete an artifact with all versions and storage. Ask the user first, then pass confirm equal to the artifact id.",
			"inputSchema": schema(map[string]any{"id": idArg, "confirm": str("Must equal the artifact id.")}, "id", "confirm"),
			"annotations": map[string]any{"destructiveHint": true}},
	}
}

func (s *Server) artifactSummary(a *Artifact) map[string]any {
	return map[string]any{"id": a.ID, "url": s.artifactURL(a.ID), "kind": a.Kind, "title": a.Title, "description": a.Description, "version": a.Head,
		"updated_at": time.Unix(a.UpdatedAt, 0).UTC().Format(time.RFC3339), "capabilities": a.Capabilities}
}

type toolArgs struct {
	ID           string          `json:"id"`
	Title        *string         `json:"title"`
	Description  *string         `json:"description"`
	HTML         string          `json:"html"`
	Markdown     string          `json:"markdown"`
	Capabilities json.RawMessage `json:"capabilities"`
	Version      int             `json:"version"`
	BaseVersion  *int            `json:"base_version"`
	Edits        []Edit          `json:"edits"`
	Query        string          `json:"query"`
	Limit        int             `json:"limit"`
	Before       int64           `json:"before"`
	Who          string          `json:"who"`
	Level        string          `json:"level"`
	Topic        string          `json:"topic"`
	Path         string          `json:"path"`
	Collection   string          `json:"collection"`
	Where        []Filter        `json:"where"`
	OrderBy      string          `json:"orderBy"`
	Dir          string          `json:"dir"`
	Op           string          `json:"op"`
	Data         map[string]any  `json:"data"`
	Confirm      string          `json:"confirm"`
}

func (s *Server) callTool(p *Principal, ms *mcpSession, name string, args json.RawMessage) *toolResult {
	var a toolArgs
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &a); err != nil {
			return toolError("invalid_argument", "arguments: "+err.Error(), nil)
		}
	}
	needBase := func() (int, *toolResult) {
		if a.BaseVersion == nil {
			return 0, toolError("invalid_argument", "base_version is required: pass the head version you last saw (read gives it)", nil)
		}
		return *a.BaseVersion, nil
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	// content picks the html or markdown argument; exactly one must be given.
	content := func() (string, string, *toolResult) {
		switch {
		case a.HTML != "" && a.Markdown != "":
			return "", "", toolError("invalid_argument", "pass html or markdown, not both", nil)
		case a.Markdown != "":
			return kindMarkdown, a.Markdown, nil
		case a.HTML != "":
			return kindHTML, a.HTML, nil
		}
		return "", "", toolError("invalid_argument", "pass the content as html (a page) or markdown (a document)", nil)
	}
	switch name {
	case "guide":
		return &toolResult{Content: []map[string]any{{"type": "text", "text": guideText(a.Topic, s.cfg)}}}
	case "create":
		kind, text, e := content()
		if e != nil {
			return e
		}
		art, warnings, err := s.opCreate(p, kind, deref(a.Title), deref(a.Description), text, a.Capabilities)
		if err != nil {
			return errResult(err)
		}
		out := s.artifactSummary(art)
		out["note"] = fmt.Sprintf("Give the user the url. To change it later, call edit or rewrite with base_version %d.", art.Head)
		if len(warnings) > 0 {
			out["warnings"] = warnings
		}
		var extra []string
		if ms != nil && ms.guideShown.CompareAndSwap(false, true) {
			extra = append(extra, guideShort(s.cfg))
		}
		return textResult(out, extra...)
	case "read":
		art, acc, err := s.loadForAgent(p, a.ID, lvView)
		if err != nil {
			return errResult(err)
		}
		_, refN := parseRef(a.ID)
		n := a.Version
		if n == 0 {
			n = refN
		}
		shown, _, ok := s.resolveView(art, acc, n)
		if !ok {
			return toolError("not_permitted", fmt.Sprintf("version %d is not visible to you", n), nil)
		}
		html, err := s.st.versionHTML(art.ID, shown)
		if err != nil {
			return errResult(err)
		}
		out := s.artifactSummary(art)
		out["head"] = art.Head
		out["version"] = shown
		out["access"] = levelName(acc.Level)
		if shown != art.Head {
			out["note"] = fmt.Sprintf("This is version %d; the head is %d. Edits must use base_version %d.", shown, art.Head, art.Head)
		}
		return textResult(out, html)
	case "edit":
		base, e := needBase()
		if e != nil {
			return e
		}
		art, err := s.opEdit(p, a.ID, base, a.Edits, a.Title)
		if err != nil {
			return errResult(err)
		}
		return textResult(map[string]any{"id": art.ID, "url": s.artifactURL(art.ID), "version": art.Head, "applied": len(a.Edits)})
	case "rewrite":
		base, e := needBase()
		if e != nil {
			return e
		}
		kind, text, ce := content()
		if ce != nil {
			return ce
		}
		art, err := s.opRewrite(p, a.ID, base, kind, text, a.Title, a.Description)
		if err != nil {
			return errResult(err)
		}
		return textResult(map[string]any{"id": art.ID, "url": s.artifactURL(art.ID), "version": art.Head})
	case "list", "search":
		lim := a.Limit
		if lim == 0 {
			lim = 30
		}
		q := ""
		if name == "search" {
			if q = strings.TrimSpace(a.Query); q == "" {
				return toolError("invalid_argument", "query is required", nil)
			}
		}
		items, err := s.st.listFor(p.User, q, lim, a.Before, ListFilter{})
		if err != nil {
			return errResult(err)
		}
		out := []map[string]any{}
		for _, it := range items {
			m := map[string]any{"id": it.ID, "url": s.artifactURL(it.ID), "kind": it.Kind, "title": it.Title, "version": it.Head,
				"updated_at": it.UpdatedAt, "owner": it.OwnerEmail}
			if it.Description != "" {
				m["description"] = it.Description
			}
			if it.Snippet != "" {
				m["snippet"] = it.Snippet
			}
			out = append(out, m)
		}
		return textResult(map[string]any{"items": out})
	case "history":
		art, _, err := s.loadForAgent(p, a.ID, lvEdit)
		if err != nil {
			return errResult(err)
		}
		vs, _ := s.st.versions(art.ID)
		return textResult(map[string]any{"id": art.ID, "head": art.Head, "versions": vs})
	case "restore":
		base, e := needBase()
		if e != nil {
			return e
		}
		art, _, err := s.loadForAgent(p, a.ID, lvEdit)
		if err != nil {
			return errResult(err)
		}
		na, err := s.opRestore(art.ID, a.Version, base, p.Label)
		if err != nil {
			return errResult(err)
		}
		return textResult(map[string]any{"id": na.ID, "version": na.Head, "restored_from": a.Version})
	case "share":
		art, _, err := s.loadForAgent(p, a.ID, lvOwner)
		if err != nil {
			return errResult(err)
		}
		gs, err := s.opShare(art, a.Who, a.Level, a.Version)
		if err != nil {
			return errResult(err)
		}
		return textResult(map[string]any{"id": art.ID, "url": s.artifactURL(art.ID), "grants": gs})
	case "declare":
		art, warnings, err := s.opDeclare(p, a.ID, a.Capabilities)
		if err != nil {
			return errResult(err)
		}
		out := map[string]any{"id": art.ID, "capabilities": art.Capabilities}
		if len(warnings) > 0 {
			out["warnings"] = warnings
		}
		return textResult(out)
	case "db_get", "db_query", "db_write":
		art, acc, err := s.loadForAgent(p, a.ID, lvView)
		if err != nil {
			return errResult(err)
		}
		v := Viewer{UID: p.User.ID, Level: acc.Level}
		c := art.Caps()
		switch name {
		case "db_get":
			path := strings.Trim(a.Path, "/")
			if len(splitPath(path))%2 == 1 {
				docs, derr := s.st.docQuery(art.ID, c, v, Query{Coll: path})
				if derr != nil {
					return errResult(derr)
				}
				return textResult(map[string]any{"collection": path, "docs": docsOut(docs)})
			}
			d, derr := s.st.docGet(art.ID, c, v, path)
			if derr != nil {
				return errResult(derr)
			}
			return textResult(d)
		case "db_query":
			docs, derr := s.st.docQuery(art.ID, c, v, Query{Coll: strings.Trim(a.Collection, "/"), Where: a.Where, OrderBy: a.OrderBy, Dir: a.Dir, Limit: a.Limit})
			if derr != nil {
				return errResult(derr)
			}
			return textResult(map[string]any{"docs": docsOut(docs)})
		default:
			if acc.Level < lvEdit {
				return toolError("not_permitted", "db_write needs edit access", nil)
			}
			op := a.Op
			if op == "" {
				op = "set"
			}
			path := strings.Trim(a.Path, "/")
			res, derr := s.st.docWrite(art.ID, c, v, WriteOp{Op: op, Path: path, Data: a.Data}, s.cfg.MaxDocBytes, s.cfg.MaxDocs)
			if derr != nil {
				return errResult(derr)
			}
			s.hub.docChanged(art.ID, path)
			return textResult(map[string]any{"path": path, "op": op, "version": res.Version})
		}
	case "delete":
		art, _, err := s.loadForAgent(p, a.ID, lvOwner)
		if err != nil {
			return errResult(err)
		}
		if a.Confirm != art.ID {
			return toolError("confirm_required", `ask the user to confirm, then pass confirm = "`+art.ID+`"`, nil)
		}
		if err := s.st.deleteArtifact(art.ID); err != nil {
			return errResult(err)
		}
		s.hub.artifactDeleted(art.ID)
		return textResult(map[string]any{"deleted": art.ID})
	}
	return toolError("unknown_tool", "no tool "+name, nil)
}

func docsOut(docs []*Doc) []map[string]any {
	out := []map[string]any{}
	for _, d := range docs {
		out = append(out, map[string]any{"path": d.Path, "id": d.ID, "data": d.Data, "version": d.Version})
	}
	return out
}

// ---- the same tools over plain HTTP ----

func (s *Server) apiToolList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.bearer(r); !ok {
		s.unauthorized(w)
		return
	}
	writeJSON(w, 200, map[string]any{"tools": toolDefs()})
}

func (s *Server) apiTool(w http.ResponseWriter, r *http.Request) {
	p, ok := s.bearer(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	var args json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxVersionBytes)*2)).Decode(&args); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	if !s.allow("mcp:"+p.User.ID, 10, 60) {
		writeJSON(w, 429, map[string]string{"error": "rate_limited"})
		return
	}
	tool := r.PathValue("tool")
	res := s.callTool(p, nil, tool, args)
	code := 200
	if res.IsError {
		code = 400
	}
	out := map[string]any{}
	if len(res.Content) > 0 {
		if t, ok := res.Content[0]["text"].(string); ok {
			var v any
			if json.Unmarshal([]byte(t), &v) == nil {
				out["result"] = v
			} else {
				out["text"] = t
			}
		}
		if len(res.Content) > 1 && tool == "read" {
			key := "html"
			if r, ok := out["result"].(map[string]any); ok && r["kind"] == kindMarkdown {
				key = "markdown"
			}
			out[key] = res.Content[1]["text"]
		}
	}
	writeJSON(w, code, out)
}
