package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestCachingAndCompression(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "https://x", Auth: "dev", Addr: "127.0.0.1:0"}, st)
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
	// thumbnails get the hardening but not the viewer's runtime
	r := httptest.NewRequest("GET", frame, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if b := w.Body.String(); !strings.Contains(b, "page hardening") || strings.Contains(b, "makeDB") || len(b) > 20000 { // hardening ~11KB, full runtime ~42KB more
		t.Fatalf("raw frame carries the full runtime (%d bytes)", len(b))
	}

	w = get("/")
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Speculation-Rules") == "" {
		t.Fatalf("index (carries the CSRF token, must stay uncompressed): %v", w.Header())
	}
}

// gzipMW leaves small replies (a save's JSON ack, a HEAD, a 304) as they are,
// honours "gzip;q=0", and still compresses big text.
func TestGzipSmallAndNegotiation(t *testing.T) {
	big := strings.Repeat("walrus ", 400)
	h := gzipMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/artifacts/small":
			writeJSON(w, 200, map[string]int{"y": 1})
		case "/api/artifacts/big":
			writeJSON(w, 200, map[string]string{"s": big})
		case "/api/artifacts/notmod":
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	do := func(method, path, ae string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Accept-Encoding", ae)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("POST", "/api/artifacts/small", "gzip"); w.Code != 200 || w.Header().Get("Content-Encoding") != "" || w.Body.String() != "{\"y\":1}\n" {
		t.Fatalf("small reply: %d %v %q", w.Code, w.Header(), w.Body)
	}
	w := do("GET", "/api/artifacts/big", "deflate, GZip")
	zr, err := gzip.NewReader(w.Body)
	must(t, err)
	b, _ := io.ReadAll(zr)
	if w.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(string(b), big) {
		t.Fatalf("big reply not compressed: %v", w.Header())
	}
	if w := do("GET", "/api/artifacts/big", "gzip;q=0, identity"); w.Header().Get("Content-Encoding") != "" {
		t.Fatalf("gzip;q=0 ignored: %v", w.Header())
	}
	if w := do("GET", "/api/artifacts/notmod", "gzip"); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("304: %d %q", w.Code, w.Body)
	}

	// HEAD of a static file keeps its real length
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "https://x", Auth: "dev", Addr: "127.0.0.1:0"}, st)
	must(t, err)
	get := httptest.NewRecorder()
	head := httptest.NewRecorder()
	for _, c := range []struct {
		m string
		w *httptest.ResponseRecorder
	}{{"GET", get}, {"HEAD", head}} {
		r := httptest.NewRequest(c.m, "/static/app.js", nil)
		s.routes().ServeHTTP(c.w, r)
	}
	if head.Header().Get("Content-Length") != fmt.Sprint(get.Body.Len()) || head.Header().Get("Content-Encoding") != "" {
		t.Fatalf("HEAD: %v, GET body %d", head.Header(), get.Body.Len())
	}
}

// The index carries its first screen (folders, artifacts, thumbnail preloads)
// so it draws without waiting on the API; the service worker is served at the root.
func TestIndexFirstScreen(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "https://x", Auth: "dev", Addr: "127.0.0.1:0"}, st)
	must(t, err)
	h := s.routes()
	u, _ := st.upsertUser("dev|o", "o@example.com", "O")
	top, _ := st.createArtifact(u.ID, kindHTML, "top level", "", "<p>a</p>", nil, "x")
	inside, _ := st.createArtifact(u.ID, kindHTML, "in folder", "", "<p>b</p>", nil, "x")
	f, err := st.createFolder(u.ID, "demos")
	must(t, err)
	must(t, st.fileArtifact(u.ID, inside.ID, f.ID))
	must(t, st.setThumb(top.ID, 0, "image/png", []byte("png")))
	tok, _, err := st.createSession(u.ID, time.Hour)
	must(t, err)

	boot := func(path string) (map[string]any, *httptest.ResponseRecorder) {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		m := regexp.MustCompile(`data-boot="([^"]*)"`).FindStringSubmatch(w.Body.String())
		if m == nil {
			t.Fatalf("%s: no boot data: %d", path, w.Code)
		}
		var b map[string]any
		must(t, json.Unmarshal([]byte(html.UnescapeString(m[1])), &b))
		return b, w
	}
	titles := func(b map[string]any) (out []string) {
		for _, it := range b["initial"].(map[string]any)["items"].([]any) {
			out = append(out, it.(map[string]any)["title"].(string))
		}
		return
	}

	b, w := boot("/")
	if got := titles(b); len(got) != 1 || got[0] != "top level" {
		t.Fatalf("top level lists %v, want only the unfiled artifact", got)
	}
	if fs := b["initial"].(map[string]any)["folders"].([]any); len(fs) != 1 {
		t.Fatalf("folders: %v", fs)
	}
	if l := w.Header().Get("Link"); !strings.Contains(l, "/a/"+top.ID+"/thumb?v=") || !strings.Contains(l, "rel=preload") {
		t.Fatalf("thumbnail preload: %q", l)
	}
	// the versioned image is kept by the browser, so the list shows it without a round trip
	r := httptest.NewRequest("GET", "/a/"+top.ID+"/thumb?v=1", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: tok})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if cc := w.Header().Get("Cache-Control"); w.Code != 200 || !strings.Contains(cc, "immutable") {
		t.Fatalf("thumb: %d %q", w.Code, cc)
	}
	b, _ = boot("/?folder=" + f.ID)
	if got := titles(b); len(got) != 1 || got[0] != "in folder" {
		t.Fatalf("folder lists %v", got)
	}
	if k := b["initial"].(map[string]any)["key"].(map[string]any); k["folder"] != f.ID || k["q"] != "" {
		t.Fatalf("key: %v", k)
	}
	if b, _ = boot("/settings"); b["initial"] != nil {
		t.Fatal("settings page carries the list")
	}

	r = httptest.NewRequest("GET", "/sw.js", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-cache" || !strings.Contains(w.Body.String(), "vitrine-pages") {
		t.Fatalf("sw.js: %d %v", w.Code, w.Header())
	}
}
