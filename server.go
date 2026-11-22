package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg  *Config
	st   *Store
	hub  *Hub
	base *url.URL

	oidcVerifier *oidc.IDTokenVerifier
	oauthCfg     *oauth2.Config

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
	s := &Server{cfg: cfg, st: st, base: b, mcpSessions: map[string]*mcpSession{}, limiters: map[string]*bucket{}, sampleBusy: map[string]int{}}
	s.hub = newHub(s)
	if cfg.Auth == "oidc" {
		p, err := oidc.NewProvider(context.Background(), cfg.OIDCIssuer)
		if err != nil {
			return nil, fmt.Errorf("oidc discovery: %w", err)
		}
		s.oidcVerifier = p.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
		s.oauthCfg = &oauth2.Config{
			ClientID: cfg.OIDCClientID, ClientSecret: cfg.OIDCSecret, Endpoint: p.Endpoint(),
			RedirectURL: cfg.BaseURL + "/auth/callback", Scopes: strings.Fields(cfg.OIDCScopes),
		}
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })

	// pages
	mux.HandleFunc("GET /{$}", s.page("index.html"))
	mux.HandleFunc("GET /settings", s.page("index.html"))
	mux.HandleFunc("GET /a/{id}", s.handleShell)
	mux.HandleFunc("GET /a/{id}/v/{n}", s.handleShell)
	mux.HandleFunc("GET /a/{id}/raw", s.handleRaw)
	mux.HandleFunc("GET /a/{id}/raw/v/{n}", s.handleRaw)
	mux.HandleFunc("GET /c/{id}/{n}", s.handleFrame)

	// browser auth
	mux.HandleFunc("GET /auth/login", s.handleLogin)
	mux.HandleFunc("GET /auth/exedev", s.handleExeDevLogin)
	mux.HandleFunc("GET /auth/callback", s.handleOIDCCallback)
	mux.HandleFunc("POST /auth/logout", s.handleLogout)

	// shell / UI API (cookie + CSRF)
	mux.HandleFunc("GET /api/me", s.apiMe)
	mux.HandleFunc("GET /api/artifacts", s.apiList)
	mux.HandleFunc("GET /api/artifacts/{id}", s.apiArtifact)
	mux.HandleFunc("GET /api/artifacts/{id}/history", s.apiHistory)
	mux.HandleFunc("POST /api/artifacts/{id}/restore", s.apiRestore)
	mux.HandleFunc("POST /api/artifacts/{id}/share", s.apiShare)
	mux.HandleFunc("POST /api/artifacts/{id}/meta", s.apiMeta)
	mux.HandleFunc("DELETE /api/artifacts/{id}", s.apiDelete)
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
		s.mu.Lock()
		delete(s.mcpSessions, r.Header.Get("Mcp-Session-Id"))
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
	return s.logMW(mux)
}

func cacheStatic(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

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
	tok, _, err := s.st.createSession(u.ID, 30*24*time.Hour)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true,
		Secure: s.base.Scheme == "https", SameSite: http.SameSiteLaxMode, MaxAge: 30 * 24 * 3600})
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

func safeNext(n string) string {
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.HasPrefix(n, "/\\") {
		return "/"
	}
	return n
}

func (s *Server) loginURL(next string) string { return "/auth/login?next=" + url.QueryEscape(next) }

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	switch s.cfg.Auth {
	case "exedev":
		http.Redirect(w, r, "/__exe.dev/login?redirect="+url.QueryEscape("/auth/exedev?next="+url.QueryEscape(next)), http.StatusFound)
	case "oidc":
		state, nonce, verifier := randToken(""), randToken(""), oauth2.GenerateVerifier()
		s.st.db.Exec(`INSERT INTO login_states(state, nonce, verifier, next, expires_at) VALUES(?,?,?,?,?)`, state, nonce, verifier, next, now()+600)
		http.Redirect(w, r, s.oauthCfg.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
	case "dev":
		email := r.URL.Query().Get("email")
		if email == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, `<form><input name=email placeholder=email><input type=hidden name=next value="%s"><button>sign in (dev)</button></form>`, htmlEsc(next))
			return
		}
		s.finishLogin(w, r, "dev|"+strings.ToLower(email), email, "", next)
	default:
		http.Error(w, "unknown auth mode", 500)
	}
}

// handleExeDevLogin trusts exe.dev's proxy identity headers. Only safe when the
// server is reachable solely through the exe.dev HTTPS proxy.
func (s *Server) handleExeDevLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth != "exedev" {
		http.NotFound(w, r)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	uid := strings.TrimSpace(r.Header.Get("X-ExeDev-UserID"))
	email := strings.TrimSpace(r.Header.Get("X-ExeDev-Email"))
	if uid == "" || email == "" {
		if r.URL.Query().Get("retry") == "" {
			http.Redirect(w, r, "/__exe.dev/login?redirect="+url.QueryEscape("/auth/exedev?retry=1&next="+url.QueryEscape(next)), http.StatusFound)
			return
		}
		http.Error(w, "exe.dev login did not complete", http.StatusUnauthorized)
		return
	}
	s.finishLogin(w, r, "exedev|"+uid, email, "", next)
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Auth != "oidc" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "sign-in failed: "+e+" "+q.Get("error_description"), http.StatusUnauthorized)
		return
	}
	var nonce, verifier, next string
	err := s.st.db.QueryRow(`SELECT nonce, verifier, next FROM login_states WHERE state=? AND expires_at>?`, q.Get("state"), now()).Scan(&nonce, &verifier, &next)
	if err != nil {
		http.Error(w, "sign-in expired, try again", http.StatusBadRequest)
		return
	}
	s.st.db.Exec(`DELETE FROM login_states WHERE state=?`, q.Get("state"))
	tok, err := s.oauthCfg.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("oidc exchange: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := s.oidcVerifier.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != nonce {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	var cl struct {
		Email    string `json:"email"`
		Verified *bool  `json:"email_verified"`
		Name     string `json:"name"`
		UPN      string `json:"preferred_username"`
	}
	idt.Claims(&cl)
	if cl.Email == "" {
		cl.Email = cl.UPN
	}
	if cl.Email == "" || (cl.Verified != nil && !*cl.Verified) {
		http.Error(w, "the identity provider gave no verified email", http.StatusUnauthorized)
		return
	}
	s.finishLogin(w, r, "oidc|"+idt.Issuer+"|"+idt.Subject, cl.Email, cl.Name, next)
}

func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, sub, email, name, next string) {
	if len(s.cfg.AllowSignIn) > 0 && !listMatch(s.cfg.AllowSignIn, email) {
		log.Printf("login denied for %s", email)
		http.Error(w, "this account is not allowed to sign in here", http.StatusForbidden)
		return
	}
	u, err := s.st.upsertUser(sub, email, name)
	if err != nil {
		http.Error(w, "user: "+err.Error(), 500)
		return
	}
	if err := s.setSession(w, u); err != nil {
		http.Error(w, "session: "+err.Error(), 500)
		return
	}
	log.Printf("login %s (%s)", u.Email, u.ID)
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.apiSession(w, r) == nil {
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.st.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.base.Scheme == "https"})
	writeJSON(w, 200, map[string]bool{"ok": true})
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
