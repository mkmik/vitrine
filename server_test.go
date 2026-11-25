package main

import (
	"fmt"
	"testing"
	"time"
)

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                "/",
		"/a/x?v=1":        "/a/x?v=1",
		"//evil.com":      "/",
		"/\\evil.com":     "/",
		"/\t/evil.com":    "/",
		"/\n/evil.com":    "/",
		"/a\\b":           "/",
		"https://evil.co": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMCPSessionBoundToUser(t *testing.T) {
	s := &Server{mcpSessions: map[string]*mcpSession{}}
	alice := &Principal{User: &User{ID: "u_alice"}}
	bob := &Principal{User: &User{ID: "u_bob"}}

	s.mcpSession(alice, "made-up")
	if len(s.mcpSessions) != 0 {
		t.Fatal("an unknown session id was stored")
	}

	s.mcpSessions["sid"] = &mcpSession{userID: alice.User.ID}
	s.mcpSession(alice, "sid").guideShown.Store(true)
	if s.mcpSession(bob, "sid").guideShown.Load() {
		t.Fatal("bob got alice's session")
	}
	if !s.mcpSession(alice, "sid").guideShown.Load() {
		t.Fatal("alice lost her session")
	}
	if s.knownMCPSession(bob, "sid") || !s.knownMCPSession(alice, "sid") {
		t.Fatal("knownMCPSession ignores the owner")
	}
}

func TestMCPSessionCapEvictsOldest(t *testing.T) {
	s := &Server{mcpSessions: map[string]*mcpSession{}}
	alice := &Principal{User: &User{ID: "u_alice"}}
	start := time.Now().Add(-time.Hour)
	for i := range mcpSessionsPerUser {
		s.mcpSessions[fmt.Sprint("s", i)] = &mcpSession{userID: alice.User.ID, seen: start.Add(time.Duration(i) * time.Second)}
	}
	s.mcpSessions["bob"] = &mcpSession{userID: "u_bob", seen: start.Add(-time.Hour)}
	s.newMCPSession(alice, "new")
	if s.knownMCPSession(alice, "s0") || !s.knownMCPSession(alice, "s1") || !s.knownMCPSession(alice, "new") {
		t.Fatal("did not evict exactly alice's oldest session")
	}
	if s.mcpSessions["bob"] == nil {
		t.Fatal("evicted another user's session")
	}
}

func TestProxyPlainHeadersNeedRealTrust(t *testing.T) {
	for _, tc := range []struct {
		trusted, secret string
		ok              bool
	}{
		{"", "", false},
		{"*", "", false},
		{"*,10.0.0.0/8", "", false},
		{"10.0.0.0/8", "", true},
		{"*", "X-Proxy-Secret: s", true},
	} {
		s := &Server{cfg: &Config{TrustedProxies: splitList(tc.trusted), ProxySecret: tc.secret}}
		s.trustNets, s.trustAny, _ = parsePrefixes(s.cfg.TrustedProxies)
		if _, err := newProxyAuth(s); (err == nil) != tc.ok {
			t.Errorf("trusted=%q secret=%q: err=%v, want ok=%v", tc.trusted, tc.secret, err, tc.ok)
		}
	}
}
