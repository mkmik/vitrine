package main

import (
	"compress/gzip"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg  *Config
	st   *Store
	hub  *Hub
	base *url.URL

	auth      authenticator
	trustNets []netip.Prefix // VITRINE_TRUSTED_PROXIES
	trustAny  bool

	mu          sync.Mutex
	mcpSessions map[string]*mcpSession
	limiters    map[string]*bucket
	sampleBusy  map[string]int
}

func newServer(cfg *Config, st *Store) (*Server, error) {
	b, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	s := &Server{cfg: cfg, st: st, base: b, mcpSessions: map[string]*mcpSession{}, limiters: map[string]*bucket{}, sampleBusy: map[string]int{}}
	st.requireSignIn, st.noPublic = cfg.RequireSignIn, cfg.NoPublicSharing
	s.hub = newHub(s)
	if s.trustNets, s.trustAny, err = parsePrefixes(cfg.TrustedProxies); err != nil {
		return nil, err
	}
	if s.auth, err = newAuthenticator(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Server) gcLoop() {
	for range time.Tick(5 * time.Minute) {
		s.st.gc()
		s.mu.Lock()
		for k, ms := range s.mcpSessions {
			if time.Since(ms.seen) > 24*time.Hour {
				delete(s.mcpSessions, k)
			}
		}
		for k, b := range s.limiters {
			if time.Since(b.last) > time.Hour {
				delete(s.limiters, k)
			}
		}
		s.mu.Unlock()
	}
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(static)))))
	mux.HandleFunc("GET /speculationrules", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/speculationrules+json")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write([]byte(speculationRules))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	// pages
	mux.HandleFunc("GET /{$}", s.page("index.html"))
	mux.HandleFunc("GET /settings", s.page("index.html"))
	mux.HandleFunc("GET /a/{id}", s.handleShell)
	mux.HandleFunc("GET /a/{id}/v/{n}", s.handleShell)
	mux.HandleFunc("GET /a/{id}/raw", s.handleRaw)
	mux.HandleFunc("GET /a/{id}/raw/v/{n}", s.handleRaw)
	mux.HandleFunc("GET /a/{id}/source", s.handleSource)
	mux.HandleFunc("GET /a/{id}/source/v/{n}", s.handleSource)
	mux.HandleFunc("GET /c/{id}/{n}", s.handleFrame)
	mux.HandleFunc("GET /a/{id}/thumb", s.handleThumb)

	// browser auth
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	s.auth.routes(mux)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	// shell / UI API (cookie + CSRF)
	mux.HandleFunc("GET /api/me", s.apiMe)
	mux.HandleFunc("GET /api/artifacts", s.apiList)
	mux.HandleFunc("GET /api/artifacts/{id}", s.apiArtifact)
	mux.HandleFunc("GET /api/artifacts/{id}/history", s.apiHistory)
	mux.HandleFunc("POST /api/artifacts/{id}/restore", s.apiRestore)
	mux.HandleFunc("POST /api/artifacts/{id}/share", s.apiShare)
	mux.HandleFunc("POST /api/artifacts/{id}/meta", s.apiMeta)
	mux.HandleFunc("POST /api/artifacts/{id}/duplicate", s.apiDuplicate)
	mux.HandleFunc("POST /api/artifacts/{id}/thumb", s.apiThumb)
	mux.HandleFunc("POST /api/artifacts/{id}/folder", s.apiFile)
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.apiDelete)
	mux.HandleFunc("GET /api/folders", s.apiFolders)
	mux.HandleFunc("POST /api/folders", s.apiFolderSave)
	mux.HandleFunc("POST /api/folders/{fid}", s.apiFolderSave)
	mux.HandleFunc("DELETE /api/folders/{fid}", s.apiFolderDelete)
	mux.HandleFunc("GET /api/tokens", s.apiTokens)
	mux.HandleFunc("POST /api/tokens", s.apiCreateToken)
	mux.HandleFunc("DELETE /api/tokens/{tid}", s.apiRevokeToken)
	mux.HandleFunc("POST /api/sample/{id}", s.apiSample)
	mux.HandleFunc("POST /api/db/{id}", s.apiDB)
	mux.HandleFunc("GET /ws/{id}", s.handleWS)

	// agent REST API: the MCP tools over plain HTTP, bearer token
	mux.HandleFunc("POST /api/v1/tools/{tool}", s.apiTool)
	mux.HandleFunc("GET /api/v1/tools", s.apiToolList)

	// MCP + OAuth
	mux.HandleFunc("POST /mcp", s.handleMCP)
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.bearer(r); !ok {
			s.unauthorized(w)
			return
		}
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "this server does not offer a server-to-client stream", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("DELETE /mcp", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.bearer(r)
		if !ok {
			s.unauthorized(w)
			return
		}
		sid := r.Header.Get("Mcp-Session-Id")
		s.mu.Lock()
		if ms := s.mcpSessions[sid]; ms != nil && ms.userID == p.User.ID {
			delete(s.mcpSessions, sid)
		}
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handlePRM)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/{rest...}", s.handlePRM)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleASM)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/{rest...}", s.handleASM)
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleASM)
	mux.HandleFunc("POST /oauth/register", s.handleRegister)
	mux.HandleFunc("GET /oauth/authorize", s.handleAuthorize)
	mux.HandleFunc("POST /oauth/authorize", s.handleAuthorizePost)
	mux.HandleFunc("POST /oauth/token", s.handleToken)
	mux.HandleFunc("POST /oauth/revoke", s.handleRevoke)
	return s.logMW(gzipMW(mux))
}

// speculationRules has the browser fetch an artifact's viewer while the
// pointer rests on its link, so the click shows it at once.
const speculationRules = `{"prefetch":[{"where":{"href_matches":"/a/*"},"eagerness":"moderate"}]}`

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") == assetVer { // the URL changes with the content
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=300")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

// gzipPath picks the responses worth compressing. The app's own pages and
// /api/me are left out: they carry the CSRF token, and compressing a secret
// next to text an attacker can choose leaks it (BREACH).
func gzipPath(p string) bool {
	return strings.HasPrefix(p, "/static/") || strings.HasPrefix(p, "/c/") || strings.HasPrefix(p, "/api/artifacts") ||
		strings.HasPrefix(p, "/api/v1/") || p == "/mcp"
}

var gzipPool = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

func gzipMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gzipPath(r.URL.Path) {
			h.ServeHTTP(w, r)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) || r.Header.Get("Range") != "" {
			h.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		h.ServeHTTP(g, r)
	})
}

// acceptsGzip reads Accept-Encoding properly: "gzip;q=0" refuses gzip.
func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		name, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if k, v, ok := strings.Cut(p, "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
				return err == nil && q > 0
			}
		}
		return true
	}
	return false
}

// gzipMin is the smallest body worth compressing. Small replies (a save's
// JSON acknowledgement, a HEAD, a 304) go out as they are: compressing saves
// nothing there, and some TLS-inspecting proxies mangle small fixed-length
// gzip replies so that the browser waits for bytes that never come.
const gzipMin = 1024

// gzipWriter holds back the status and the first gzipMin bytes, then
// compresses a 200 with a text body that reached gzipMin; anything else
// passes through unchanged.
type gzipWriter struct {
	http.ResponseWriter
	gz   *gzip.Writer
	code int    // status held back until the decision; 0 = none yet
	buf  []byte // body held back until the decision
	sent bool   // the decision is made and the header is out
}

func (g *gzipWriter) WriteHeader(code int) {
	if !g.sent && g.code == 0 {
		g.code = code
	}
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if g.sent {
		if g.gz != nil {
			return g.gz.Write(b)
		}
		return g.ResponseWriter.Write(b)
	}
	g.buf = append(g.buf, b...)
	if len(g.buf) >= gzipMin {
		if err := g.send(true); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

// send makes the decision, writes the header and whatever body was held back.
func (g *gzipWriter) send(big bool) error {
	g.sent = true
	if g.code == 0 {
		g.code = http.StatusOK
	}
	h := g.Header()
	if h.Get("Content-Type") == "" && len(g.buf) > 0 {
		h.Set("Content-Type", http.DetectContentType(g.buf))
	}
	ct := h.Get("Content-Type")
	if big && g.code == http.StatusOK && h.Get("Content-Encoding") == "" && !strings.HasPrefix(ct, "text/event-stream") &&
		(strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "javascript") || strings.HasPrefix(ct, "image/svg")) {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
		g.gz = gzipPool.Get().(*gzip.Writer)
		g.gz.Reset(g.ResponseWriter)
	}
	g.ResponseWriter.WriteHeader(g.code)
	b := g.buf
	g.buf = nil
	if len(b) == 0 {
		return nil
	}
	var err error
	if g.gz != nil {
		_, err = g.gz.Write(b)
	} else {
		_, err = g.ResponseWriter.Write(b)
	}
	return err
}

// Flush sends what is held back (uncompressed if still small), so streams keep flowing.
func (g *gzipWriter) Flush() {
	if !g.sent {
		g.send(len(g.buf) >= gzipMin)
	}
	if g.gz != nil {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) close() {
	if !g.sent && (g.code != 0 || len(g.buf) > 0) { // a small body, a HEAD or a 304: as it is
		g.send(false)
	}
	if g.gz != nil {
		g.gz.Close()
		gzipPool.Put(g.gz)
	}
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) logMW(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		h.ServeHTTP(rec, r)
		observeHTTP(r, rec.code, time.Since(start))
		if r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/static/") {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

// ---- sessions ----

const sessionCookie = "vit_s"

func (s *Server) sessionOf(r *http.Request) *Session {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	ss, err := s.st.session(c.Value)
	if err != nil {
		return nil
	}
	return ss
}

func (s *Server) userOf(r *http.Request) *User {
	if ss := s.sessionOf(r); ss != nil {
		return ss.User
	}
	return nil
}

func (s *Server) sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		sf := r.Header.Get("Sec-Fetch-Site")
		return sf == "" || sf == "same-origin"
	}
	return o == s.base.Scheme+"://"+s.base.Host
}

// apiSession requires a session; mutating requests also need X-CSRF and same origin.
func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) *Session {
	ss := s.sessionOf(r)
	if ss == nil {
		writeJSON(w, 401, map[string]string{"error": "signed_out"})
		return nil
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Header.Get("X-CSRF") != ss.CSRF || !s.sameOrigin(r) {
			writeJSON(w, 403, map[string]string{"error": "csrf"})
			return nil
		}
	}
	return ss
}

func (s *Server) setSession(w http.ResponseWriter, u *User) error {
	tok, _, err := s.st.createSession(u.ID, s.cfg.SessionTTL)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: s.base.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: int(s.cfg.SessionTTL.Seconds())})
	return nil
}

func listMatch(list []string, email string) bool {
	email = strings.ToLower(email)
	for _, e := range list {
		if e == email || (strings.HasPrefix(e, "@") && strings.HasSuffix(email, e)) {
			return true
		}
	}
	return false
}

func (s *Server) mayCreate(u *User) bool {
	return u != nil && (len(s.cfg.Creators) == 0 || listMatch(s.cfg.Creators, u.Email))
}

// safeNext keeps a post-login redirect on this site. Browsers drop tabs and
// newlines from URLs and read "\" as "/", so "/\t/evil.com" means "//evil.com":
// refuse those characters anywhere, not just after the first slash.
func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") ||
		strings.ContainsFunc(n, func(r rune) bool { return r == '\\' || unicode.IsControl(r) }) {
		return "/"
	}
	return n
}

func (s *Server) loginURL(next string) string { return "/auth/login?next=" + url.QueryEscape(next) }

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.apiSession(w, r) == nil {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.base.Scheme == "https"})
	w.Header().Set("Clear-Site-Data", `"cache"`) // artifact frames are kept in the browser cache, revalidated
	next := "/?signed_out"
	if s.cfg.LogoutURL != "" { // e.g. the proxy's or IdP's sign-out page
		next = s.cfg.LogoutURL
	}
	writeJSON(w, 200, map[string]any{"ok": true, "next": next})
}

// ---- bearer auth for agents ----

type Principal struct {
	User     *User
	ClientID string
	Label    string // author label for versions
}

func (s *Server) bearer(r *http.Request) (*Principal, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return nil, false
	}
	tr, err := s.st.lookupToken(strings.TrimSpace(h[7:]))
	if err != nil || (tr.Kind != "pat" && tr.Kind != "access") || (tr.ExpiresAt != 0 && tr.ExpiresAt < now()) {
		return nil, false
	}
	u, err := s.st.userByID(tr.UserID)
	if err != nil {
		return nil, false
	}
	s.st.touchToken(tr.ID)
	p := &Principal{User: u, ClientID: tr.ClientID, Label: "agent"}
	var name string
	if tr.ClientID != "" {
		s.st.db.QueryRow(`SELECT name FROM oauth_clients WHERE id=?`, tr.ClientID).Scan(&name)
	} else {
		s.st.db.QueryRow(`SELECT name FROM tokens WHERE id=?`, tr.ID).Scan(&name)
	}
	if name != "" {
		p.Label = "agent:" + name
	}
	return p, true
}

func (s *Server) unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp"`, s.cfg.BaseURL))
	writeJSON(w, 401, map[string]string{"error": "invalid_token", "error_description": "sign in with OAuth or send a personal token"})
}

// ---- rate limiting ----

type bucket struct {
	tokens float64
	last   time.Time
}

// allow is a token bucket: rate per second, burst capacity.
func (s *Server) allow(key string, rate, burst float64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.limiters[key]
	if !ok {
		b = &bucket{tokens: burst, last: time.Now()}
		s.limiters[key] = b
	}
	b.tokens = min(burst, b.tokens+time.Since(b.last).Seconds()*rate)
	b.last = time.Now()
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func htmlEsc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

func (s *Server) artifactURL(id string) string { return s.cfg.BaseURL + "/a/" + id }
