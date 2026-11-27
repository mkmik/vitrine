package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
