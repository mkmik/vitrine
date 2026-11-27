package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

var doctypeRE = regexp.MustCompile(`(?i)^\s*<!doctype[^>]*>`)

// injectRuntime puts the runtime first in the page, so the page's own markup
// can't hide it: a <head> found by search could sit inside a comment, a
// <script> string or a <textarea>, and the runtime would never run. Only a
// leading doctype goes before it (keeping standards mode); a doctype ends at
// its first '>', so nothing in it can open a comment.
func injectRuntime(html, script string) string {
	tag := "<script>" + script + "</script>"
	n := 0
	if loc := doctypeRE.FindStringIndex(html); loc != nil {
		n = loc[1]
	}
	return html[:n] + tag + html[n:]
}

func readScripts(names ...string) string {
	var b strings.Builder
	for _, n := range names {
		s, _ := webFS.ReadFile("web/" + n)
		b.Write(s)
		b.WriteByte('\n')
	}
	return strings.ReplaceAll(b.String(), "</script", "<\\/script")
}

var (
	runtimeSrc = sync.OnceValue(func() string { return readScripts("harden.js", "runtime.js") })
	// outside the viewer (thumbnails, the bare page) claude.use has no shell to
	// talk to and always gives null: the full runtime would be 12 KB of dead
	// weight on every thumbnail in the list
	rawRuntimeSrc = sync.OnceValue(func() string { return readScripts("harden.js", "runtime-raw.js") })
)

// runtimeJS configures the page runtime; y > 0 opens the page scrolled down that far.
func (s *Server) runtimeJS(mode string, y int) string {
	cfg, _ := json.Marshal(map[string]any{"mode": mode, "y": y})
	src := runtimeSrc
	if mode == "raw" {
		src = rawRuntimeSrc
	}
	return "window.__vitrine_cfg=" + string(cfg) + ";\n" + src()
}

// permissionsPolicy turns off device and sensor access for artifact pages.
const permissionsPolicy = "camera=(), microphone=(), geolocation=(), display-capture=(), usb=(), serial=(), hid=(), bluetooth=(), " +
	"midi=(), payment=(), publickey-credentials-get=(), accelerometer=(), gyroscope=(), magnetometer=(), screen-wake-lock=(), " +
	"idle-detection=(), local-fonts=(), window-management=(), browsing-topics=()"

func contentHeaders(h http.Header, csp string) {
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", csp)
	h.Set("Permissions-Policy", permissionsPolicy)
	h.Set("X-DNS-Prefetch-Control", "off")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-cache") // revalidated by ETag, see handleFrame
}

func (s *Server) contentCSP(frameAncestors string) string {
	cdn := strings.Join(s.cfg.CDNs, " ")
	return strings.Join([]string{
		// no popups (links go through the shell), no alert/confirm/prompt
		"sandbox allow-scripts allow-forms allow-downloads",
		"default-src 'none'",
		"script-src 'unsafe-inline' 'unsafe-eval' blob: data: " + cdn,
		"style-src 'unsafe-inline' blob: data: " + cdn,
		"font-src data: blob: " + cdn,
		"img-src data: blob: " + cdn,
		"media-src data: blob:",
		"connect-src " + cdn,
		"worker-src blob:",
		"child-src 'none'",
		"frame-src 'none'",
		"manifest-src 'none'",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors " + frameAncestors,
	}, "; ")
}

func (s *Server) shellCSP() string {
	ws := "wss://" + s.base.Host
	if s.base.Scheme == "http" {
		ws = "ws://" + s.base.Host
	}
	// frame-src limits the artifact frame to /c/: browsers apply the parent's
	// frame-src to the frame's own navigations too, so a page cannot take its
	// frame to another site (leaking data in the URL) or to a vitrine page.
	return "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob: https://gravatar.com; connect-src 'self' " + ws +
		"; frame-src " + s.cfg.BaseURL + "/c/; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"
}

func (s *Server) securityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Content-Security-Policy", s.shellCSP())
	h.Set("X-Frame-Options", "DENY")
}

// page serves one of the app's HTML pages with boot data.
func (s *Server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// after sign-out the index shows "Sign in" instead: going straight to the
		// IdP would sign the user back in from its own session
		if s.cfg.RequireSignIn && s.userOf(r) == nil && !(name == "index.html" && r.URL.Query().Has("signed_out")) {
			http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusFound)
			return
		}
		if name == "index.html" && s.base.Scheme == "https" { // browsers only prefetch over https, WebKit logs errors otherwise
			w.Header().Set("Speculation-Rules", `"/speculationrules"`)
		}
		s.renderPage(w, name, s.bootFor(r, nil))
	}
}

var (
	tmplMu   sync.Mutex
	pageTmpl = map[string]*template.Template{}
	bootID   = strconv.FormatInt(time.Now().UnixNano(), 36)
	// assetVer changes whenever any file under web/static does, so pages never
	// pair a new template with a browser-cached copy of the old script or styles.
	assetVer = func() string {
		h := sha256.New()
		fs.WalkDir(webFS, "web/static", func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				b, _ := webFS.ReadFile(p)
				h.Write([]byte(p))
				h.Write(b)
			}
			return nil
		})
		return hex.EncodeToString(h.Sum(nil))[:10]
	}()
)

func (s *Server) renderPage(w http.ResponseWriter, name string, boot map[string]any) {
	tmplMu.Lock()
	t, ok := pageTmpl[name]
	if !ok {
		b, _ := webFS.ReadFile("web/" + name)
		t = template.Must(template.New(name).Parse(string(b)))
		pageTmpl[name] = t
	}
	tmplMu.Unlock()
	bj, _ := json.Marshal(boot)
	s.securityHeaders(w)
	if boot["frameable"] == true { // the raw wrapper may be embedded anywhere
		w.Header().Del("X-Frame-Options")
		w.Header().Set("Content-Security-Policy", strings.Replace(s.shellCSP(), "frame-ancestors 'none'", "frame-ancestors *", 1))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	t.Execute(w, map[string]any{"Boot": string(bj), "Title": boot["title"], "V": assetVer})
}

func (s *Server) bootFor(r *http.Request, extra map[string]any) map[string]any {
	b := map[string]any{"base": s.cfg.BaseURL, "auth": s.cfg.Auth, "mcp_url": s.cfg.BaseURL + "/mcp", "title": "Artifacts", "version": version,
		"sample": s.cfg.LLMURL != "", "org": s.cfg.OrgName, "require_signin": s.cfg.RequireSignIn, "public_sharing": !s.cfg.NoPublicSharing}
	if ss := s.sessionOf(r); ss != nil {
		b["user"] = map[string]string{"id": ss.User.ID, "email": ss.User.Email, "name": ss.User.DisplayName(), "avatar": gravatarURL(ss.User.Email, ss.User.DisplayName())}
		b["csrf"] = ss.CSRF
		b["can_create"] = s.mayCreate(ss.User)
	}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

// gravatarURL is the user's Gravatar image; without one, Gravatar draws their initials.
func gravatarURL(email, name string) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	var ini []rune
	for _, w := range strings.Fields(name) {
		if ini = append(ini, unicode.ToUpper([]rune(w)[0])); len(ini) == 2 {
			break
		}
	}
	return "https://gravatar.com/avatar/" + hex.EncodeToString(h[:]) + "?s=64&d=initials&initials=" + url.QueryEscape(string(ini))
}

// resolveView decides which version a viewer may see. n = 0 means "default".
func (s *Server) resolveView(a *Artifact, acc Access, n int) (shown int, follow bool, ok bool) {
	if acc.Level == lvNone {
		return 0, false, false
	}
	if n == 0 {
		if acc.Pinned > 0 {
			return acc.Pinned, false, true
		}
		return a.Head, true, true
	}
	if n < 1 || n > a.Head {
		return 0, false, false
	}
	if acc.Level >= lvEdit || n == acc.Pinned || (acc.Pinned == 0 && n == a.Head) {
		return n, false, true
	}
	return 0, false, false
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.securityHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(404)
	signin := ""
	if s.userOf(r) == nil {
		signin = `<p><a href="` + htmlEsc(s.loginURL(r.URL.RequestURI())) + `">Sign in</a> if this was shared with you.</p>`
	}
	w.Write([]byte(`<!doctype html><meta name=viewport content="width=device-width"><link rel=stylesheet href="/static/app.css"><main class="empty"><h1>Not found</h1><p>This artifact does not exist, or you don't have access to it.</p>` + signin + `<p><a href="/">All artifacts</a></p></main>`))
}

func pathInt(r *http.Request, k string) int {
	n, _ := strconv.Atoi(r.PathValue(k))
	return n
}

func (s *Server) handleShell(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	u := s.userOf(r)
	acc := s.st.accessFor(a, u)
	if acc.Level == lvNone && u == nil {
		http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusFound)
		return
	}
	n := pathInt(r, "n")
	shown, follow, ok := s.resolveView(a, acc, n)
	if !ok {
		if acc.Level != lvNone && n != 0 {
			http.Redirect(w, r, "/a/"+a.ID, http.StatusFound)
			return
		}
		s.notFound(w, r)
		return
	}
	s.renderPage(w, "shell.html", s.bootFor(r, map[string]any{
		"title": a.Title, "artifact": map[string]any{
			"id": a.ID, "title": a.Title, "description": a.Description, "head": a.Head, "shown": shown, "follow": follow,
			"pinned": acc.Pinned, "level": levelName(acc.Level), "caps": a.Caps().Names, "url": s.artifactURL(a.ID), "kind": a.Kind,
			"updated_at": a.UpdatedAt,
		},
		"thumb": thumbJSON(s.st.thumbMeta(a.ID)),
	}))
}

func (s *Server) serveContent(w http.ResponseWriter, a *Artifact, html, mode, ancestors string, y int) {
	h := w.Header()
	if a.Kind == kindMarkdown {
		nonce := randToken("")[:22]
		contentHeaders(h, s.markdownCSP(nonce, ancestors))
		w.Write([]byte(markdownPage(a.Title, html, nonce)))
		return
	}
	contentHeaders(h, s.contentCSP(ancestors))
	w.Write([]byte(injectRuntime(html, s.runtimeJS(mode, y))))
}

// handleFrame serves a version for the shell's sandboxed iframe.
func (s *Server) handleFrame(w http.ResponseWriter, r *http.Request) {
	// Only ever inside a frame: the parent's CSP is what keeps a page from
	// navigating itself to another site. Opened directly, go to the viewer.
	if r.Header.Get("Sec-Fetch-Dest") == "document" {
		http.Redirect(w, r, "/a/"+r.PathValue("id")+"/v/"+r.PathValue("n"), http.StatusFound)
		return
	}
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	acc := s.st.accessFor(a, s.userOf(r))
	shown, _, ok := s.resolveView(a, acc, pathInt(r, "n"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	mode := "frame"
	if r.URL.Query().Get("raw") == "1" {
		mode = "raw"
	}
	y, _ := strconv.Atoi(r.URL.Query().Get("vy")) // documents read it themselves
	y = max(y, 0)
	// A version never changes, so the browser keeps the frame and asks again
	// each time (access is checked above): the list's thumbnails and a reload
	// get a 304 and skip the body. bootID covers a new runtime or CSP.
	sum := sha256.Sum256([]byte(fmt.Sprint(a.ID, shown, mode, y, a.Kind, a.Title)))
	etag := `W/"` + bootID + "-" + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm == "*" || strings.Contains(inm, etag[2:]) {
		w.Header().Set("Cache-Control", "private, no-cache")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	html, err := s.st.versionHTML(a.ID, shown)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveContent(w, a, html, mode, "'self'", y)
}

func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	u := s.userOf(r)
	acc := s.st.accessFor(a, u)
	if acc.Level == lvNone && u == nil {
		http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusFound)
		return
	}
	shown, _, ok := s.resolveView(a, acc, pathInt(r, "n"))
	if !ok {
		s.notFound(w, r)
		return
	}
	// The bare page is still framed: a top-level page could navigate itself
	// anywhere, a framed one is held to /c/ by this wrapper's CSP.
	s.renderPage(w, "raw.html", map[string]any{"title": a.Title, "frameable": true,
		"src": "/c/" + a.ID + "/" + strconv.Itoa(shown) + "?raw=1"})
}

// handleSource returns a version's stored text as-is: the markdown of a
// document or the HTML source of a page. ?download=1 saves it as a file.
func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r)
		return
	}
	u := s.userOf(r)
	acc := s.st.accessFor(a, u)
	if acc.Level == lvNone && u == nil {
		http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusFound)
		return
	}
	shown, _, ok := s.resolveView(a, acc, pathInt(r, "n"))
	if !ok {
		s.notFound(w, r)
		return
	}
	text, err := s.st.versionHTML(a.ID, shown)
	if err != nil {
		s.notFound(w, r)
		return
	}
	ext := ".html"
	if a.Kind == kindMarkdown {
		ext = ".md"
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	h.Set("Cache-Control", "private, no-store")
	if r.URL.Query().Get("download") != "" {
		h.Set("Content-Disposition", `attachment; filename="`+fileSlug(a.Title)+ext+`"`)
	}
	w.Write([]byte(text))
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

func fileSlug(t string) string {
	s := strings.Trim(slugRE.ReplaceAllString(strings.ToLower(t), "-"), "-")
	if len(s) > 60 {
		s = strings.TrimRight(s[:60], "-")
	}
	if s == "" {
		s = "artifact"
	}
	return s
}

// ---- UI API ----

func (s *Server) apiMe(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.bootFor(r, nil)) }

func (s *Server) apiList(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	qs := r.URL.Query()
	before, _ := strconv.ParseInt(qs.Get("before"), 10, 64)
	items, err := s.st.listFor(ss.User, qs.Get("q"), 60, before, ListFilter{Owner: qs.Get("owner"), Kind: qs.Get("kind"), Folder: qs.Get("folder")})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	for i := range items {
		acc := s.st.accessFor(items[i].Artifact, ss.User)
		items[i].Level = levelName(acc.Level)
		if acc.Level == lvOwner {
			items[i].Grants, _ = s.st.grants(items[i].ID)
		}
	}
	if items == nil {
		items = []ListItem{}
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (s *Server) loadForAPI(w http.ResponseWriter, r *http.Request, need int) (*Session, *Artifact, Access) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return nil, nil, Access{}
	}
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return nil, nil, Access{}
	}
	acc := s.st.accessFor(a, ss.User)
	if acc.Level < need {
		code := 403
		if acc.Level == lvNone {
			code = 404
		}
		writeJSON(w, code, map[string]string{"error": "not_permitted"})
		return nil, nil, Access{}
	}
	return ss, a, acc
}

func (s *Server) apiArtifact(w http.ResponseWriter, r *http.Request) {
	_, a, acc := s.loadForAPI(w, r, lvView)
	if a == nil {
		return
	}
	out := map[string]any{"artifact": a, "level": levelName(acc.Level), "pinned": acc.Pinned, "url": s.artifactURL(a.ID)}
	if acc.Level == lvOwner {
		out["grants"], _ = s.st.grants(a.ID)
	}
	writeJSON(w, 200, out)
}

func (s *Server) apiHistory(w http.ResponseWriter, r *http.Request) {
	_, a, _ := s.loadForAPI(w, r, lvEdit)
	if a == nil {
		return
	}
	vs, _ := s.st.versions(a.ID)
	writeJSON(w, 200, map[string]any{"head": a.Head, "versions": vs})
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (s *Server) apiRestore(w http.ResponseWriter, r *http.Request) {
	ss, a, _ := s.loadForAPI(w, r, lvEdit)
	if a == nil {
		return
	}
	var req struct {
		Version int `json:"version"`
	}
	readBody(r, &req)
	na, err := s.opRestore(a.ID, req.Version, a.Head, "user:"+ss.User.Email)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"head": na.Head})
}

func (s *Server) apiShare(w http.ResponseWriter, r *http.Request) {
	_, a, _ := s.loadForAPI(w, r, lvOwner)
	if a == nil {
		return
	}
	var req struct {
		Who     string `json:"who"`
		Level   string `json:"level"`
		Version int    `json:"version"`
	}
	if err := readBody(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	gs, err := s.opShare(a, req.Who, req.Level, req.Version)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"grants": gs})
}

func (s *Server) apiMeta(w http.ResponseWriter, r *http.Request) {
	_, a, _ := s.loadForAPI(w, r, lvEdit)
	if a == nil {
		return
	}
	var req struct {
		Title       *string `json:"title"`
		Description *string `json:"description"`
	}
	readBody(r, &req)
	na, err := s.st.setMeta(a.ID, req.Title, req.Description)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	s.hub.artifactChanged(na)
	writeJSON(w, 200, map[string]any{"artifact": na})
}

// apiDuplicate copies one version the caller can see into a new artifact they own.
func (s *Server) apiDuplicate(w http.ResponseWriter, r *http.Request) {
	ss, a, acc := s.loadForAPI(w, r, lvView)
	if a == nil {
		return
	}
	var req struct {
		Version int `json:"version"`
	}
	readBody(r, &req)
	n, _, ok := s.resolveView(a, acc, req.Version)
	if !ok {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	html, err := s.st.versionHTML(a.ID, n)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	na, _, err := s.opCreate(&Principal{User: ss.User, Label: "user:" + ss.User.Email}, a.Kind, "Copy of "+a.Title, a.Description, html, a.Capabilities)
	if err != nil {
		code := 400
		if oe, ok := err.(*opError); ok && oe.Code == "not_permitted" {
			code = 403
		}
		writeJSON(w, code, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"id": na.ID, "url": s.artifactURL(na.ID)})
}

// handleThumb serves an artifact's uploaded thumbnail image.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	a, err := s.st.artifact(r.PathValue("id"))
	if err != nil || s.st.accessFor(a, s.userOf(r)).Level == lvNone {
		http.NotFound(w, r)
		return
	}
	mime, data, err := s.st.thumbImage(a.ID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Revalidated on every use, so access is checked each time (a revoked share
	// or a sign-out takes effect at once); an unchanged image costs a 304.
	sum := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if inm := r.Header.Get("If-None-Match"); inm == "*" || strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", mime)
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(data)
}

const maxThumb = 2 << 20

var thumbTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/webp": true, "image/gif": true}

func thumbJSON(y int, img int64, auto int) map[string]any {
	return map[string]any{"y": y, "img": img, "auto": auto}
}

// apiThumb sets the thumbnail: a JSON {"y": n} shows the page scrolled to n
// (0 = the top, the default); an image body replaces it with that image.
// ?auto=n marks an image the viewer took by itself of version n: it never
// replaces one a person chose.
func (s *Server) apiThumb(w http.ResponseWriter, r *http.Request) {
	_, a, _ := s.loadForAPI(w, r, lvEdit)
	if a == nil {
		return
	}
	auto, _ := strconv.Atoi(r.URL.Query().Get("auto"))
	if auto < 0 || auto > a.Head || auto > 0 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, 400, map[string]string{"error": "bad auto version"})
		return
	}
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			Y int `json:"y"`
		}
		if err := readBody(r, &req); err != nil {
			writeJSON(w, 400, map[string]string{"error": "bad json"})
			return
		}
		err = s.st.setThumb(a.ID, max(req.Y, 0), "", nil)
	} else {
		b, rerr := io.ReadAll(http.MaxBytesReader(w, r.Body, maxThumb))
		if rerr != nil {
			writeJSON(w, 413, map[string]string{"error": "image too large (2 MB at most)"})
			return
		}
		// trust the bytes, not the header: never store something a browser would run (SVG, HTML)
		mime := http.DetectContentType(b)
		if !thumbTypes[mime] {
			writeJSON(w, 400, map[string]string{"error": "use a PNG, JPEG, WebP or GIF image"})
			return
		}
		if auto > 0 {
			err = s.st.setAutoThumb(a.ID, auto, mime, b)
		} else {
			err = s.st.setThumb(a.ID, 0, mime, b)
		}
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, thumbJSON(s.st.thumbMeta(a.ID)))
}

func (s *Server) apiDelete(w http.ResponseWriter, r *http.Request) {
	_, a, _ := s.loadForAPI(w, r, lvOwner)
	if a == nil {
		return
	}
	if err := s.st.deleteArtifact(a.ID); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	s.hub.artifactDeleted(a.ID)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// ---- folders: personal, see Store.folders ----

func (s *Server) apiFolders(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	fs, err := s.st.folders(ss.User)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"folders": fs})
}

// apiFolderSave creates a folder (no {fid}) or renames one.
func (s *Server) apiFolderSave(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := readBody(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	var out any
	var err error
	if id := r.PathValue("fid"); id == "" {
		out, err = s.st.createFolder(ss.User.ID, req.Name)
	} else {
		err = s.st.renameFolder(ss.User.ID, id, req.Name)
		out = map[string]bool{"ok": true}
	}
	folderReply(w, out, err)
}

func (s *Server) apiFolderDelete(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	folderReply(w, map[string]bool{"ok": true}, s.st.deleteFolder(ss.User.ID, r.PathValue("fid")))
}

// apiFile puts an artifact the caller can see into one of their folders, or back at the top ("").
func (s *Server) apiFile(w http.ResponseWriter, r *http.Request) {
	ss, a, _ := s.loadForAPI(w, r, lvView)
	if a == nil {
		return
	}
	var req struct {
		Folder string `json:"folder"`
	}
	if err := readBody(r, &req); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad json"})
		return
	}
	folderReply(w, map[string]bool{"ok": true}, s.st.fileArtifact(ss.User.ID, a.ID, req.Folder))
}

func folderReply(w http.ResponseWriter, out any, err error) {
	switch {
	case err == errNotFound:
		writeJSON(w, 404, map[string]string{"error": "not_found"})
	case err != nil:
		writeJSON(w, 400, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, 200, out)
	}
}

func (s *Server) apiTokens(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	ts, _ := s.st.listTokens(ss.User.ID)
	if ts == nil {
		ts = []TokenInfo{}
	}
	writeJSON(w, 200, map[string]any{"tokens": ts})
}

func (s *Server) apiCreateToken(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	readBody(r, &req)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "token"
	}
	t, err := s.st.createPAT(ss.User.ID, name)
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"token": t})
}

func (s *Server) apiRevokeToken(w http.ResponseWriter, r *http.Request) {
	ss := s.apiSession(w, r)
	if ss == nil {
		return
	}
	if err := s.st.revokeToken(ss.User.ID, r.PathValue("tid")); err != nil {
		writeJSON(w, 404, map[string]string{"error": "not_found"})
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
