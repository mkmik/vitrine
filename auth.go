package main

// Sign-in is pluggable. An authenticator only turns a browser into an
// Identity; vitrine then keeps its own session cookie. Agents sign in through
// vitrine's own OAuth server (or use personal tokens), and that OAuth flow
// sends the user through the same authenticator, so every deployment gets MCP
// OAuth for free. Chosen by VITRINE_AUTH:
//
//	exedev  the exe.dev HTTPS proxy login (X-ExeDev-* headers, which the proxy
//	        sets and strips from clients)
//	oidc    any OpenID Connect provider: Google, Entra ID, Okta, Keycloak, Dex...
//	proxy   an authenticating reverse proxy in front (oauth2-proxy, Pomerium,
//	        Cloudflare Access, Teleport, an ingress with auth): identity headers
//	        from trusted addresses, or a signed JWT header checked against the
//	        proxy's published keys
//	dev     type any email; loopback listeners only, for tests

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Identity is what a sign-in method hands back.
type Identity struct {
	Subject string // stable per user within the method, e.g. "oidc|<iss>|<sub>"
	Email   string
	Name    string
	Groups  []string
}

type authenticator interface {
	// begin starts sign-in for a browser; the flow ends in Server.finishLogin.
	begin(w http.ResponseWriter, r *http.Request, next string)
	// routes registers the method's own endpoints.
	routes(mux *http.ServeMux)
	describe() string
}

func newAuthenticator(s *Server) (authenticator, error) {
	switch s.cfg.Auth {
	case "exedev":
		return &exedevAuth{s}, nil
	case "oidc":
		return newOIDCAuth(s)
	case "proxy":
		return newProxyAuth(s)
	case "dev":
		if !loopbackAddr(s.cfg.Addr) {
			return nil, fmt.Errorf("VITRINE_AUTH=dev lets anyone sign in as anyone: it only runs on a loopback address, not %q", s.cfg.Addr)
		}
		return &devAuth{s}, nil
	case "":
		return nil, errors.New("VITRINE_AUTH is not set: choose exedev, oidc, proxy or dev")
	}
	return nil, fmt.Errorf("unknown VITRINE_AUTH %q: choose exedev, oidc, proxy or dev", s.cfg.Auth)
}

func loopbackAddr(addr string) bool {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if h == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.IsLoopback()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	s.auth.begin(w, r, safeNext(r.URL.Query().Get("next")))
}

// finishLogin applies the sign-in allowlists and starts a vitrine session.
func (s *Server) finishLogin(w http.ResponseWriter, r *http.Request, id Identity, next string) {
	id.Email = strings.ToLower(strings.TrimSpace(id.Email))
	if id.Subject == "" || id.Email == "" {
		http.Error(w, "sign-in gave no user id or email", http.StatusUnauthorized)
		return
	}
	if len(s.cfg.AllowSignIn) > 0 && !listMatch(s.cfg.AllowSignIn, id.Email) {
		log.Printf("login denied for %s: not in VITRINE_ALLOW_SIGNIN", id.Email)
		http.Error(w, "this account is not allowed to sign in here", http.StatusForbidden)
		return
	}
	if len(s.cfg.AllowGroups) > 0 && !anyGroup(s.cfg.AllowGroups, id.Groups) {
		log.Printf("login denied for %s: in none of VITRINE_ALLOW_GROUPS", id.Email)
		http.Error(w, "this account is not in a group allowed to sign in here", http.StatusForbidden)
		return
	}
	u, err := s.st.upsertUser(id.Subject, id.Email, id.Name)
	if err != nil {
		http.Error(w, "user: "+err.Error(), 500)
		return
	}
	if err := s.setSession(w, u); err != nil {
		http.Error(w, "session: "+err.Error(), 500)
		return
	}
	log.Printf("login %s (%s) via %s", u.Email, u.ID, s.cfg.Auth)
	http.Redirect(w, r, next, http.StatusFound)
}

func anyGroup(allowed, have []string) bool {
	for _, h := range have {
		h = strings.ToLower(strings.TrimSpace(h))
		for _, a := range allowed {
			if h == a {
				return true
			}
		}
	}
	return false
}

// ---- exe.dev ----

type exedevAuth struct{ s *Server }

func (a *exedevAuth) describe() string { return "exe.dev proxy login" }

func (a *exedevAuth) begin(w http.ResponseWriter, r *http.Request, next string) {
	http.Redirect(w, r, "/__exe.dev/login?redirect="+url.QueryEscape("/auth/exedev?next="+url.QueryEscape(next)), http.StatusFound)
}

func (a *exedevAuth) routes(mux *http.ServeMux) { mux.HandleFunc("GET /auth/exedev", a.callback) }

// callback trusts exe.dev's identity headers. Only safe when the server is
// reachable solely through the exe.dev HTTPS proxy, which strips them from
// clients.
func (a *exedevAuth) callback(w http.ResponseWriter, r *http.Request) {
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
	a.s.finishLogin(w, r, Identity{Subject: "exedev|" + uid, Email: email}, next)
}

// ---- OpenID Connect ----

type oidcAuth struct {
	s        *Server
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
}

func newOIDCAuth(s *Server) (*oidcAuth, error) {
	c := s.cfg
	if c.OIDCIssuer == "" || c.OIDCClientID == "" {
		return nil, errors.New("VITRINE_AUTH=oidc needs VITRINE_OIDC_ISSUER and VITRINE_OIDC_CLIENT_ID")
	}
	p, err := oidc.NewProvider(context.Background(), c.OIDCIssuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", c.OIDCIssuer, err)
	}
	return &oidcAuth{
		s:        s,
		verifier: p.Verifier(&oidc.Config{ClientID: c.OIDCClientID}),
		oauth: &oauth2.Config{ClientID: c.OIDCClientID, ClientSecret: c.OIDCSecret, Endpoint: p.Endpoint(),
			RedirectURL: c.BaseURL + "/auth/callback", Scopes: strings.Fields(c.OIDCScopes)},
	}, nil
}

func (a *oidcAuth) describe() string { return "OpenID Connect via " + a.s.cfg.OIDCIssuer }

func (a *oidcAuth) routes(mux *http.ServeMux) { mux.HandleFunc("GET /auth/callback", a.callback) }

func (a *oidcAuth) begin(w http.ResponseWriter, r *http.Request, next string) {
	state, nonce, verifier := randToken(""), randToken(""), oauth2.GenerateVerifier()
	a.s.st.db.Exec(`INSERT INTO login_states(state, nonce, verifier, next, expires_at) VALUES(?,?,?,?,?)`, state, nonce, verifier, next, now()+600)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *oidcAuth) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "sign-in failed: "+e+" "+q.Get("error_description"), http.StatusUnauthorized)
		return
	}
	var nonce, verifier, next string
	err := a.s.st.db.QueryRow(`SELECT nonce, verifier, next FROM login_states WHERE state=? AND expires_at>?`, q.Get("state"), now()).Scan(&nonce, &verifier, &next)
	if err != nil {
		http.Error(w, "sign-in expired, try again", http.StatusBadRequest)
		return
	}
	a.s.st.db.Exec(`DELETE FROM login_states WHERE state=?`, q.Get("state"))
	tok, err := a.oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("oidc exchange: %v", err)
		http.Error(w, "token exchange failed", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), raw)
	if err != nil || idt.Nonce != nonce {
		http.Error(w, "invalid id_token", http.StatusUnauthorized)
		return
	}
	var all map[string]any
	idt.Claims(&all)
	id, err := identityFromClaims(all, "oidc|"+idt.Issuer+"|"+idt.Subject, a.s.cfg.GroupsClaim)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	a.s.finishLogin(w, r, id, next)
}

// identityFromClaims reads email (or preferred_username / upn), name and
// groups from token claims, refusing an email the provider marks unverified.
func identityFromClaims(c map[string]any, subject, groupsClaim string) (Identity, error) {
	str := func(k string) string { v, _ := c[k].(string); return strings.TrimSpace(v) }
	id := Identity{Subject: subject, Email: str("email"), Name: str("name")}
	if v, ok := c["email_verified"].(bool); ok && !v && id.Email != "" {
		return id, errors.New("the identity provider says this email is not verified")
	}
	for _, k := range []string{"preferred_username", "upn"} {
		if id.Email == "" && strings.Contains(str(k), "@") {
			id.Email = str(k)
		}
	}
	if id.Email == "" {
		return id, errors.New("the identity provider gave no email")
	}
	switch g := c[groupsClaim].(type) {
	case []any:
		for _, x := range g {
			if s, ok := x.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	case string:
		id.Groups = splitList(g)
	}
	return id, nil
}

// ---- authenticating reverse proxy ----

type proxyAuth struct {
	s       *Server
	jwt     *oidc.IDTokenVerifier // nil: plain headers
	secretK string
	secretV string
}

func newProxyAuth(s *Server) (*proxyAuth, error) {
	c := s.cfg
	a := &proxyAuth{s: s}
	if c.ProxySecret != "" {
		k, v, ok := strings.Cut(c.ProxySecret, ":")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, errors.New(`VITRINE_PROXY_SECRET must look like "Header-Name: value"`)
		}
		a.secretK, a.secretV = strings.TrimSpace(k), strings.TrimSpace(v)
	}
	if c.ProxyJWTHeader != "" {
		if c.ProxyJWKS == "" || c.ProxyJWTIssuer == "" || c.ProxyJWTAudience == "" {
			return nil, errors.New("a signed proxy header needs VITRINE_PROXY_JWT_JWKS, VITRINE_PROXY_JWT_ISSUER and VITRINE_PROXY_JWT_AUDIENCE")
		}
		keys := oidc.NewRemoteKeySet(context.Background(), c.ProxyJWKS)
		a.jwt = oidc.NewVerifier(c.ProxyJWTIssuer, keys, &oidc.Config{ClientID: c.ProxyJWTAudience,
			SupportedSigningAlgs: []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "EdDSA"}})
	} else if a.secretK == "" && (len(s.trustNets) == 0 || s.trustAny) {
		// plain headers from anyone would let anyone sign in as anyone; "*" trusts anyone
		return nil, errors.New("VITRINE_AUTH=proxy with plain headers needs VITRINE_TRUSTED_PROXIES (the proxy's addresses, not *) or VITRINE_PROXY_SECRET")
	}
	return a, nil
}

func (a *proxyAuth) describe() string {
	if a.jwt != nil {
		return "reverse proxy, signed " + a.s.cfg.ProxyJWTHeader + " header from " + a.s.cfg.ProxyJWTIssuer
	}
	return "reverse proxy, " + a.s.cfg.ProxyEmailHeader + " header"
}

func (a *proxyAuth) routes(mux *http.ServeMux) { mux.HandleFunc("GET /auth/proxy", a.callback) }

// begin sends the browser to /auth/proxy, the one path the proxy must
// protect; VITRINE_PROXY_LOGIN_URL can route it through the proxy's own start
// page first ({rd} is replaced with the escaped return URL).
func (a *proxyAuth) begin(w http.ResponseWriter, r *http.Request, next string) {
	back := "/auth/proxy?next=" + url.QueryEscape(next)
	if u := a.s.cfg.ProxyLoginURL; u != "" {
		http.Redirect(w, r, strings.ReplaceAll(u, "{rd}", url.QueryEscape(a.s.cfg.BaseURL+back)), http.StatusFound)
		return
	}
	http.Redirect(w, r, back, http.StatusFound)
}

func (a *proxyAuth) callback(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	c := a.s.cfg
	if len(c.TrustedProxies) > 0 && !a.s.fromTrustedProxy(r) {
		log.Printf("proxy login refused: %s is not in VITRINE_TRUSTED_PROXIES", r.RemoteAddr)
		http.Error(w, "sign-in must come through the configured proxy", http.StatusForbidden)
		return
	}
	if a.secretK != "" && !constEq(r.Header.Get(a.secretK), a.secretV) {
		log.Printf("proxy login refused: missing or wrong %s", a.secretK)
		http.Error(w, "sign-in must come through the configured proxy", http.StatusForbidden)
		return
	}
	var id Identity
	if a.jwt != nil {
		raw := strings.TrimSpace(r.Header.Get(c.ProxyJWTHeader))
		raw = strings.TrimPrefix(raw, "Bearer ")
		t, err := a.jwt.Verify(r.Context(), raw)
		if err != nil {
			log.Printf("proxy login refused: %s: %v", c.ProxyJWTHeader, err)
			http.Error(w, "the proxy's identity token is missing or invalid", http.StatusUnauthorized)
			return
		}
		var all map[string]any
		t.Claims(&all)
		if id, err = identityFromClaims(all, "proxy|"+t.Issuer+"|"+t.Subject, c.GroupsClaim); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
	} else {
		email := strings.TrimSpace(r.Header.Get(c.ProxyEmailHeader))
		user := strings.TrimSpace(r.Header.Get(c.ProxyUserHeader))
		if email == "" {
			http.Error(w, "no "+c.ProxyEmailHeader+" header: the proxy must protect /auth/proxy and pass the signed-in user", http.StatusUnauthorized)
			return
		}
		if user == "" {
			user = email
		}
		id = Identity{Subject: "proxy|" + user, Email: email, Name: strings.TrimSpace(r.Header.Get(c.ProxyNameHeader))}
		if c.ProxyGroupsHeader != "" {
			id.Groups = splitList(r.Header.Get(c.ProxyGroupsHeader))
		}
	}
	a.s.finishLogin(w, r, id, next)
}

// ---- dev ----

type devAuth struct{ s *Server }

func (a *devAuth) describe() string          { return "dev: any email, no password (loopback only)" }
func (a *devAuth) routes(mux *http.ServeMux) {}
func (a *devAuth) begin(w http.ResponseWriter, r *http.Request, next string) {
	email := r.URL.Query().Get("email")
	if email == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<form><input name=email placeholder=email><input type=hidden name=next value="%s"><button>sign in (dev)</button></form>`, htmlEsc(next))
		return
	}
	a.s.finishLogin(w, r, Identity{Subject: "dev|" + strings.ToLower(email), Email: email}, next)
}

// ---- proxies and client addresses ----

func parsePrefixes(list []string) ([]netip.Prefix, bool, error) {
	var out []netip.Prefix
	any := false
	for _, p := range list {
		if p == "*" {
			any = true
			continue
		}
		if !strings.Contains(p, "/") {
			if ip, err := netip.ParseAddr(p); err == nil {
				out = append(out, netip.PrefixFrom(ip, ip.BitLen()))
				continue
			}
		}
		pf, err := netip.ParsePrefix(p)
		if err != nil {
			return nil, false, fmt.Errorf("VITRINE_TRUSTED_PROXIES: %q is not an address or CIDR", p)
		}
		out = append(out, pf.Masked())
	}
	return out, any, nil
}

func (s *Server) trusted(ip string) bool {
	if s.trustAny {
		return true
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	a = a.Unmap()
	for _, p := range s.trustNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func remoteIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func (s *Server) fromTrustedProxy(r *http.Request) bool { return s.trusted(remoteIP(r)) }

// clientIP is the address rate limits key on. X-Forwarded-For counts only
// when the request came from a trusted proxy (on exe.dev, always: everything
// arrives through its proxy); then the right-most address that is not itself
// a trusted proxy is the client.
func (s *Server) clientIP(r *http.Request) string {
	ip := remoteIP(r)
	trustXFF := s.cfg.Auth == "exedev" && len(s.cfg.TrustedProxies) == 0 || len(s.cfg.TrustedProxies) > 0 && s.trusted(ip)
	if !trustXFF {
		return ip
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if h == "" {
			continue
		}
		if s.trustAny || !s.trusted(h) {
			if s.trustAny { // can't tell proxies apart: take the original client
				return strings.TrimSpace(hops[0])
			}
			return h
		}
	}
	return ip
}

func constEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
