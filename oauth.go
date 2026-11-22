package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The server is its own OAuth 2.1 authorization server for MCP clients:
// RFC 9728 resource metadata, RFC 8414 server metadata, RFC 7591 dynamic
// registration, PKCE (S256), rotating refresh tokens. Users sign in through the
// same browser login as the UI, then approve the client once.

var defaultRedirectHosts = []string{"claude.ai", "claude.com", "chatgpt.com", "chat.openai.com", "platform.openai.com"}

func (s *Server) handlePRM(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"resource":                 s.cfg.BaseURL + "/mcp",
		"authorization_servers":    []string{s.cfg.BaseURL},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{"artifacts"},
		"resource_name":            "vitrine artifacts",
	})
}

func (s *Server) handleASM(w http.ResponseWriter, r *http.Request) {
	b := s.cfg.BaseURL
	writeJSON(w, 200, map[string]any{
		"issuer":                                b,
		"authorization_endpoint":                b + "/oauth/authorize",
		"token_endpoint":                        b + "/oauth/token",
		"registration_endpoint":                 b + "/oauth/register",
		"revocation_endpoint":                   b + "/oauth/revoke",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{"artifacts"},
	})
}

func (s *Server) redirectAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme == "http" {
		return host == "localhost" || host == "127.0.0.1" || host == "::1"
	}
	if u.Scheme != "https" {
		// native app schemes (cursor://, vscode://) are fine; script-ish ones are not
		switch u.Scheme {
		case "", "javascript", "data", "file", "vbscript", "blob":
			return false
		}
		return true
	}
	for _, h := range append(defaultRedirectHosts, s.cfg.RedirectHosts...) {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

func oauthErr(w http.ResponseWriter, code int, e, desc string) {
	writeJSON(w, code, map[string]string{"error": e, "error_description": desc})
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !s.allow("register:"+clientIP(r), 0.1, 10) {
		oauthErr(w, 429, "slow_down", "too many registrations")
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthErr(w, 400, "invalid_client_metadata", "bad json")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthErr(w, 400, "invalid_redirect_uri", "1-10 redirect_uris required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !s.redirectAllowed(u) {
			log.Printf("oauth register: rejected redirect_uri %q", u)
			oauthErr(w, 400, "invalid_redirect_uri", "redirect_uri not allowed: "+u)
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "MCP client"
	}
	if len(name) > 80 {
		name = name[:80]
	}
	id := "c_" + randID(12)
	ru, _ := json.Marshal(req.RedirectURIs)
	if _, err := s.st.db.Exec(`INSERT INTO oauth_clients(id, name, redirect_uris, created_at) VALUES(?,?,?,?)`, id, name, string(ru), now()); err != nil {
		oauthErr(w, 500, "server_error", err.Error())
		return
	}
	log.Printf("oauth: registered client %s (%s) %v", id, name, req.RedirectURIs)
	writeJSON(w, 201, map[string]any{
		"client_id": id, "client_name": name, "redirect_uris": req.RedirectURIs, "client_id_issued_at": now(),
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	})
}

type oauthClient struct {
	ID, Name     string
	RedirectURIs []string
}

func (s *Server) client(id string) (*oauthClient, error) {
	c := &oauthClient{ID: id}
	var ru string
	if err := s.st.db.QueryRow(`SELECT name, redirect_uris FROM oauth_clients WHERE id=?`, id).Scan(&c.Name, &ru); err != nil {
		return nil, errNotFound
	}
	json.Unmarshal([]byte(ru), &c.RedirectURIs)
	return c, nil
}

// validateAuthorize checks the client before any redirect (no open redirect).
func (s *Server) validateAuthorize(q url.Values) (*oauthClient, string) {
	c, err := s.client(q.Get("client_id"))
	if err != nil {
		return nil, "unknown client_id"
	}
	for _, u := range c.RedirectURIs {
		if u == q.Get("redirect_uri") {
			return c, ""
		}
	}
	return nil, "redirect_uri is not registered for this client"
}

func redirectWith(w http.ResponseWriter, r *http.Request, base string, kv map[string]string) {
	u, _ := url.Parse(base)
	v := u.Query()
	for k, val := range kv {
		if val != "" {
			v.Set(k, val)
		}
	}
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, msg := s.validateAuthorize(q)
	if c == nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	fail := func(e, d string) {
		redirectWith(w, r, q.Get("redirect_uri"), map[string]string{"error": e, "error_description": d, "state": q.Get("state")})
	}
	if q.Get("response_type") != "code" {
		fail("unsupported_response_type", "only code")
		return
	}
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		fail("invalid_request", "PKCE S256 is required")
		return
	}
	if res := strings.TrimRight(q.Get("resource"), "/"); res != "" && res != s.cfg.BaseURL+"/mcp" && res != s.cfg.BaseURL {
		fail("invalid_target", "unknown resource")
		return
	}
	ss := s.sessionOf(r)
	if ss == nil {
		http.Redirect(w, r, s.loginURL(r.URL.RequestURI()), http.StatusFound)
		return
	}
	ru, _ := url.Parse(q.Get("redirect_uri"))
	target := ru.Scheme + ":"
	if ru.Host != "" {
		target = ru.Scheme + "://" + ru.Host
	}
	s.securityHeaders(w)
	// Chrome applies form-action to the redirect that follows the POST too.
	w.Header().Set("Content-Security-Policy", strings.Replace(s.shellCSP(), "form-action 'self'", "form-action 'self' "+target, 1))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta name=viewport content="width=device-width,initial-scale=1"><title>Connect %[1]s</title>
<link rel=stylesheet href="/static/app.css"><main class="consent"><h1>Connect <b>%[1]s</b>?</h1>
<p><b>%[1]s</b> will be able to create, read, edit, share and delete your artifacts, acting as <b>%[2]s</b>.</p>
<p class="muted">After you allow, you return to <code>%[3]s</code>.</p>
<form method=post action="/oauth/authorize"><input type=hidden name=csrf value="%[4]s">`, htmlEsc(c.Name), htmlEsc(ss.User.Email), htmlEsc(target), htmlEsc(ss.CSRF))
	for _, k := range []string{"client_id", "redirect_uri", "state", "code_challenge", "code_challenge_method", "resource", "scope"} {
		fmt.Fprintf(w, `<input type=hidden name="%s" value="%s">`, k, htmlEsc(q.Get(k)))
	}
	fmt.Fprint(w, `<div class=row><button name=decision value=allow class=primary>Allow</button><button name=decision value=deny>Cancel</button></div></form></main>`)
}

func (s *Server) handleAuthorizePost(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	q := r.PostForm
	ss := s.sessionOf(r)
	if ss == nil || subtle.ConstantTimeCompare([]byte(q.Get("csrf")), []byte(ss.CSRF)) != 1 || !s.sameOrigin(r) {
		http.Error(w, "session expired, start again from your app", http.StatusForbidden)
		return
	}
	c, msg := s.validateAuthorize(q)
	if c == nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if q.Get("decision") != "allow" {
		redirectWith(w, r, q.Get("redirect_uri"), map[string]string{"error": "access_denied", "state": q.Get("state")})
		return
	}
	code := randToken("")
	if _, err := s.st.db.Exec(`INSERT INTO oauth_codes(code_hash, client_id, user_id, redirect_uri, challenge, expires_at) VALUES(?,?,?,?,?,?)`,
		hashToken(code), c.ID, ss.User.ID, q.Get("redirect_uri"), q.Get("code_challenge"), now()+60); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	log.Printf("oauth: %s authorized client %s (%s)", ss.User.Email, c.ID, c.Name)
	redirectWith(w, r, q.Get("redirect_uri"), map[string]string{"code": code, "state": q.Get("state"), "iss": s.cfg.BaseURL})
}

const (
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
)

func (s *Server) issueTokens(w http.ResponseWriter, userID, clientID, family string) {
	at, err1 := s.st.insertToken("access", userID, clientID, family, accessTTL)
	rt, err2 := s.st.insertToken("refresh", userID, clientID, family, refreshTTL)
	if err1 != nil || err2 != nil {
		oauthErr(w, 500, "server_error", "token store")
		return
	}
	writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": int(accessTTL.Seconds()), "refresh_token": rt, "scope": "artifacts"})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if !s.allow("token:"+clientIP(r), 1, 30) {
		oauthErr(w, 429, "slow_down", "too many requests")
		return
	}
	r.ParseForm()
	f := r.PostForm
	switch f.Get("grant_type") {
	case "authorization_code":
		var clientID, userID, redirect, challenge string
		var exp int64
		h := hashToken(f.Get("code"))
		err := s.st.db.QueryRow(`SELECT client_id, user_id, redirect_uri, challenge, expires_at FROM oauth_codes WHERE code_hash=?`, h).Scan(&clientID, &userID, &redirect, &challenge, &exp)
		s.st.db.Exec(`DELETE FROM oauth_codes WHERE code_hash=?`, h)
		if err != nil || exp < now() {
			oauthErr(w, 400, "invalid_grant", "unknown or expired code")
			return
		}
		if f.Get("client_id") != clientID || f.Get("redirect_uri") != redirect {
			oauthErr(w, 400, "invalid_grant", "client_id or redirect_uri mismatch")
			return
		}
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) != 1 {
			oauthErr(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		s.issueTokens(w, userID, clientID, "f_"+randID(10))
	case "refresh_token":
		tr, err := s.st.lookupToken(f.Get("refresh_token"))
		if err != nil || tr.Kind != "refresh" || tr.ExpiresAt < now() {
			oauthErr(w, 400, "invalid_grant", "unknown or expired refresh token")
			return
		}
		if f.Get("client_id") != "" && f.Get("client_id") != tr.ClientID {
			oauthErr(w, 400, "invalid_grant", "client mismatch")
			return
		}
		if tr.Used {
			// reuse of a rotated refresh token: revoke the whole family
			s.st.db.Exec(`DELETE FROM tokens WHERE family=?`, tr.Family)
			log.Printf("oauth: refresh token reuse, revoked family %s", tr.Family)
			oauthErr(w, 400, "invalid_grant", "refresh token reuse")
			return
		}
		s.st.db.Exec(`UPDATE tokens SET used=1 WHERE id=?`, tr.ID)
		s.issueTokens(w, tr.UserID, tr.ClientID, tr.Family)
	default:
		oauthErr(w, 400, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if tr, err := s.st.lookupToken(r.PostForm.Get("token")); err == nil && tr.Kind != "pat" && tr.Family != "" {
		s.st.db.Exec(`DELETE FROM tokens WHERE family=?`, tr.Family)
	}
	w.WriteHeader(200)
}

func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	return r.RemoteAddr
}
