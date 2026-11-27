package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestCachingAndCompression(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "http://x", Auth: "dev", Addr: "127.0.0.1:0"}, st)
	must(t, err)
	h := s.routes()
	owner, _ := st.upsertUser("dev|o", "o@example.com", "O")
	a, _ := st.createArtifact(owner.ID, kindHTML, "t", "", "<p>hi</p>", nil, "x")
	st.setGrant(a.ID, Grant{Audience: "public", Level: "view"})

	get := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		for i := 0; i < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := get("/static/app.js?v=" + assetVer)
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("static: %d %v", w.Code, w.Header())
	}
	if w := get("/static/app.js?v=old"); w.Header().Get("Cache-Control") != "public, max-age=300" {
		t.Fatalf("stale version cached for good: %v", w.Header())
	}

	frame := "/c/" + a.ID + "/0?raw=1"
	w = get(frame)
	etag := w.Header().Get("ETag")
	if w.Code != 200 || etag == "" || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("frame: %d %v", w.Code, w.Header())
	}
	if w := get(frame, "If-None-Match", etag); w.Code != http.StatusNotModified || w.Body.Len() != 0 || w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("frame revalidation: %d %v", w.Code, w.Header())
	}
	if w := get("/c/"+a.ID+"/0", "If-None-Match", etag); w.Code != 200 {
		t.Fatalf("another mode reused the etag: %d", w.Code)
	}

	w = get("/")
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Speculation-Rules") == "" {
		t.Fatalf("index (carries the CSRF token, must stay uncompressed): %v", w.Header())
	}
}
