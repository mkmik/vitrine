package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"golang.org/x/oauth2"
)

// Without a client secret, the code exchange must send client_id and the PKCE
// verifier in the body, and no Authorization header or client_secret.
func TestOIDCPublicClientExchange(t *testing.T) {
	var got *http.Request
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer": srv.URL, "authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		got = r
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"a","token_type":"Bearer"}`))
	})

	a, err := newOIDCAuth(&Server{cfg: &Config{OIDCIssuer: srv.URL, OIDCClientID: "pub", BaseURL: "https://v.example"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.oauth.Exchange(t.Context(), "code", oauth2.VerifierOption("ver")); err != nil {
		t.Fatal(err)
	}
	if h := got.Header.Get("Authorization"); h != "" {
		t.Errorf("Authorization header sent: %q", h)
	}
	if got.PostForm.Get("client_id") != "pub" || got.PostForm.Get("code_verifier") != "ver" || got.PostForm.Has("client_secret") {
		t.Errorf("form = %v", got.PostForm)
	}
}

// VITRINE_REQUIRE_SIGNIN: a "public" link still opens for anyone signed in,
// but no longer for an anonymous visitor.
func TestRequireSignInPublicGrant(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	owner, _ := st.upsertUser("dev|o", "o@example.com", "O")
	other, _ := st.upsertUser("dev|x", "x@example.com", "X")
	a, _ := st.createArtifact(owner.ID, kindHTML, "t", "", "<p>hi</p>", nil, "x")
	st.setGrant(a.ID, Grant{Audience: "public", Level: "view"})

	if l := st.accessFor(a, nil).Level; l != lvView {
		t.Fatalf("flag off, anonymous: level %d", l)
	}
	st.requireSignIn = true
	if l := st.accessFor(a, nil).Level; l != lvNone {
		t.Fatalf("flag on, anonymous: level %d", l)
	}
	if l := st.accessFor(a, other).Level; l != lvView {
		t.Fatalf("flag on, signed in: level %d", l)
	}
}
