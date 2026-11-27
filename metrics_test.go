package main

import (
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetrics(t *testing.T) {
	st, err := openStore(filepath.Join(t.TempDir(), "v.db"))
	must(t, err)
	defer st.close()
	s, err := newServer(&Config{BaseURL: "https://x", Auth: "dev", Addr: "127.0.0.1:0"}, st)
	must(t, err)
	owner, _ := st.upsertUser("dev|o", "o@example.com", "O")
	a, _ := st.createArtifact(owner.ID, kindHTML, "t", "", "<p>hi</p>", nil, "x")

	// Routes are labeled by pattern, never by the artifact id in the path.
	s.routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/a/"+a.ID+"/raw", nil))
	s.routes().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/nope/"+a.ID, nil))
	for _, l := range [][2]string{{"GET /a/{id}/raw", "302"}, {"unmatched", "404"}} {
		if n := testutil.ToFloat64(httpRequests.WithLabelValues(l[0], l[1])); n != 1 {
			t.Errorf("%v: %v requests, want 1", l, n)
		}
	}

	// Tool names from the client are bounded.
	p := &Principal{User: owner}
	s.callTool(p, nil, "guide", nil)
	s.callTool(p, &mcpSession{}, a.ID, nil)
	if testutil.ToFloat64(toolCalls.WithLabelValues("guide", "api", "ok")) != 1 ||
		testutil.ToFloat64(toolCalls.WithLabelValues("unknown", "mcp", "error")) != 1 {
		t.Error("tool calls not counted as expected")
	}

	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(inventory{st})
	want := `
# HELP vitrine_artifacts Artifacts, by kind.
# TYPE vitrine_artifacts gauge
vitrine_artifacts{kind="html"} 1
# HELP vitrine_users Users who ever signed in or were created.
# TYPE vitrine_users gauge
vitrine_users 1
`
	must(t, testutil.GatherAndCompare(reg, strings.NewReader(want), "vitrine_artifacts", "vitrine_users"))
	if n, _ := testutil.GatherAndCount(reg, "vitrine_active_owners"); n != 3 {
		t.Errorf("active owners: %d series, want 3", n)
	}
}
