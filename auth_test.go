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

// VITRINE_DISABLE_PUBLIC_SHARING: a public grant opens nothing, even for a
// signed-in user, and sharing with the public is refused (revoking still works).
func TestNoPublicSharing(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "https://x", Auth: "dev", Addr: "127.0.0.1:0", NoPublicSharing: true}, st)
	must(t, err)
	owner, _ := st.upsertUser("dev|o", "o@example.com", "O")
	other, _ := st.upsertUser("dev|x", "x@example.com", "X")
	a, _ := st.createArtifact(owner.ID, kindHTML, "t", "", "<p>hi</p>", nil, "x")
	st.setGrant(a.ID, Grant{Audience: "public", Level: "view"}) // made before the switch
	if l := st.accessFor(a, other).Level; l != lvNone {
		t.Fatalf("old public grant still opens it: level %d", l)
	}
	if _, err := s.opShare(a, "public", "view", 0); err == nil {
		t.Fatal("sharing with the public was allowed")
	}
	if _, err := s.opShare(a, "public", "none", 0); err != nil {
		t.Fatalf("revoking the old public grant: %v", err)
	}
	if _, err := s.opShare(a, "users", "view", 0); err != nil || st.accessFor(a, other).Level != lvView {
		t.Fatalf("users grant: %v", err)
	}
	// a link grant opens for anyone signed in, but stays out of their list
	b, _ := st.createArtifact(owner.ID, kindHTML, "u", "", "<p>hi</p>", nil, "x")
	if _, err := s.opShare(b, "link", "view", 0); err != nil || st.accessFor(b, other).Level != lvView || st.accessFor(b, nil).Level != lvNone {
		t.Fatalf("link grant: %v", err)
	}
	items, _ := st.listFor(other, "", 0, 0, ListFilter{})
	for _, it := range items {
		if it.ID == b.ID {
			t.Fatal("link-shared artifact is listed")
		}
	}
}
