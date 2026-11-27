package main

import (
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Usage metrics in Prometheus format. Labels only ever hold values from a
// fixed set (route patterns, tool names, kinds), never a user, an email or
// an artifact id.
var (
	httpRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vitrine_http_requests_total",
		Help: "HTTP requests, by route pattern and status code.",
	}, []string{"route", "code"})
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vitrine_http_request_duration_seconds",
		Help:    "HTTP request duration, by route pattern.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})
	toolCalls = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "vitrine_tool_calls_total",
		Help: "Agent tool calls, by tool, transport (mcp or api) and result (ok or error).",
	}, []string{"tool", "via", "result"})
	liveViewers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "vitrine_live_connections",
		Help: "Open WebSocket connections (viewers with an artifact open).",
	})
	signIns = promauto.NewCounter(prometheus.CounterOpts{
		Name: "vitrine_sign_ins_total",
		Help: "Completed browser sign-ins.",
	})
)

var knownTools = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range toolDefs() {
		m[d["name"].(string)] = true
	}
	return m
}()

func countToolCall(name string, viaMCP bool, res *toolResult) {
	if !knownTools[name] {
		name = "unknown" // the name comes from the client: keep the label set bounded
	}
	via, result := "api", "ok"
	if viaMCP {
		via = "mcp"
	}
	if res == nil || res.IsError {
		result = "error"
	}
	toolCalls.WithLabelValues(name, via, result).Inc()
}

func observeHTTP(r *http.Request, code int, d time.Duration) {
	route := r.Pattern // set by ServeMux: "GET /a/{id}", never the id itself
	if route == "" {
		route = "unmatched"
	}
	httpRequests.WithLabelValues(route, strconv.Itoa(code)).Inc()
	httpDuration.WithLabelValues(route).Observe(d.Seconds())
}

// inventory reads totals from the store at scrape time.
// ponytail: a few COUNT queries per scrape; fine for one team's instance,
// cache them if scrapes ever show up in database load.
type inventory struct{ st *Store }

var (
	artifactsDesc = prometheus.NewDesc("vitrine_artifacts", "Artifacts, by kind.", []string{"kind"}, nil)
	usersDesc     = prometheus.NewDesc("vitrine_users", "Users who ever signed in or were created.", nil, nil)
	ownersDesc    = prometheus.NewDesc("vitrine_active_owners", "Distinct owners of artifacts updated within the window.", []string{"window"}, nil)
)

func (inventory) Describe(ch chan<- *prometheus.Desc) {
	ch <- artifactsDesc
	ch <- usersDesc
	ch <- ownersDesc
}

func (i inventory) Collect(ch chan<- prometheus.Metric) {
	db := i.st.db
	if rows, err := db.Query(`SELECT kind, COUNT(*) FROM artifacts GROUP BY kind`); err == nil {
		for rows.Next() {
			var kind string
			var n float64
			if rows.Scan(&kind, &n) == nil {
				ch <- prometheus.MustNewConstMetric(artifactsDesc, prometheus.GaugeValue, n, kind)
			}
		}
		rows.Close()
	} else {
		log.Printf("metrics: %v", err)
	}
	var n float64
	if db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n) == nil {
		ch <- prometheus.MustNewConstMetric(usersDesc, prometheus.GaugeValue, n)
	}
	for _, w := range []struct {
		name string
		d    time.Duration
	}{{"1d", 24 * time.Hour}, {"7d", 7 * 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}} {
		since := time.Now().Add(-w.d).Unix()
		if db.QueryRow(`SELECT COUNT(DISTINCT owner_id) FROM artifacts WHERE updated_at > ?`, since).Scan(&n) == nil {
			ch <- prometheus.MustNewConstMetric(ownersDesc, prometheus.GaugeValue, n, w.name)
		}
	}
}

// serveMetrics serves /metrics on its own address, so it stays off the
// public listener.
func serveMetrics(addr string, st *Store) {
	prometheus.MustRegister(inventory{st})
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	log.Printf("metrics on %s/metrics", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("metrics: %v", err)
	}
}
