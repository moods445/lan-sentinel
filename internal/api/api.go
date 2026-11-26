// Package api 提供控制面 HTTP 接口：Agent 同步、查询、拉流、热加载与内嵌主机墙。
package api

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/home-aio/lan-sentinel/internal/rollup"
	"github.com/home-aio/lan-sentinel/internal/rules"
	"github.com/home-aio/lan-sentinel/internal/store"
)

//go:embed ui.html
var uiHTML []byte

var metricNameRe = regexp.MustCompile(`^[A-Za-z0-9_:./-]{1,120}$`)
var metricPatternRe = regexp.MustCompile(`^[A-Za-z0-9_:./*-]{1,120}$`)
var mountRe = regexp.MustCompile(`^(/|[A-Za-z]:[\\/]).{0,118}$`)
var hostIDRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type Deps struct {
	Store        *store.Store
	Engine       *rules.Engine
	Rollup       *rollup.Rollup
	Log          *slog.Logger
	PanelToken   string
	AgentToken   string
	Reload       func(ctx context.Context) (rulesRev, collectorsRev string, err error)
	RunCollector func(ctx context.Context, host string) error
}

type Server struct {
	d Deps
}

func New(d Deps) *Server { return &Server{d: d} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/v1/agent/sync", s.handleSync)
	mux.HandleFunc("GET /api/v1/hosts", s.handleHosts)
	mux.HandleFunc("GET /api/v1/hosts/{id}", s.handleHost)
	mux.HandleFunc("GET /api/v1/hosts/{id}/metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/decisions", s.handleDecisions)
	mux.HandleFunc("GET /api/v1/stream", s.handleStream)
	mux.HandleFunc("POST /api/v1/admin/reload", s.handleReload)
	mux.HandleFunc("GET /api/v1/policies", s.handlePolicies)
	mux.HandleFunc("PUT /api/v1/policies/global", s.handleSaveGlobalPolicy)
	mux.HandleFunc("PUT /api/v1/policies/hosts/{id}", s.handleSaveHostPolicy)
	mux.HandleFunc("DELETE /api/v1/policies/hosts/{id}", s.handleDeleteHostPolicy)
	mux.HandleFunc("POST /api/v1/collectors/{id}/run", s.handleCollectorRun)
	mux.HandleFunc("GET /", s.handleUI)
	return s.recoverMW(s.logMW(s.authMW(mux)))
}

// ---------- middleware ----------

func (s *Server) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/api/v1/agent/sync" {
			next.ServeHTTP(w, r) // 由 handler 内按主机令牌校验
			return
		}
		if s.d.PanelToken != "" && !s.matchPanelToken(r) {
			writeErr(w, http.StatusUnauthorized, "invalid or missing panel token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) matchPanelToken(r *http.Request) bool {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	return tok != "" && constantEqual(tok, s.d.PanelToken)
}

func constantEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		s.d.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.code,
			"ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.d.Log.Error("panic", "path", r.URL.Path, "panic", rec)
				writeErr(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.d.Store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "db": "down", "err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "db": "up", "time": time.Now().Format(time.RFC3339)})
}

type syncHost struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	OS   string         `json:"os"`
	Arch string         `json:"arch"`
	Kind string         `json:"kind"`
	Meta map[string]any `json:"meta"`
}

type syncPayload struct {
	Host    syncHost           `json:"host"`
	TS      time.Time          `json:"ts"`
	Metrics map[string]float64 `json:"metrics"`
	Self    struct {
		IntervalS int     `json:"interval_s"`
		Throttled bool    `json:"throttled"`
		CPU       float64 `json:"cpu_pct"`
	} `json:"self"`
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var rd io.Reader = r.Body
	if strings.Contains(r.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad gzip body")
			return
		}
		defer gr.Close()
		rd = gr
	}
	var p syncPayload
	dec := json.NewDecoder(rd)
	if err := dec.Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if !hostIDRe.MatchString(p.Host.ID) {
		writeErr(w, http.StatusBadRequest, "invalid host id")
		return
	}
	if !s.authorizeAgent(r, p.Host.ID) {
		writeErr(w, http.StatusUnauthorized, "bad agent token")
		return
	}

	now := time.Now()
	ts := p.TS
	if ts.IsZero() || now.Sub(ts) > 48*time.Hour || ts.After(now.Add(5*time.Minute)) {
		ts = now
	}
	samples := make([]store.Sample, 0, len(p.Metrics))
	for name, v := range p.Metrics {
		if !metricNameRe.MatchString(name) || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		samples = append(samples, store.Sample{Metric: name, TS: ts, Value: round2(v), Src: "agent"})
		if len(samples) >= 300 {
			break
		}
	}

	addr := remoteHost(r)
	host := store.HostInfo{
		ID: p.Host.ID, Name: firstNonEmpty(p.Host.Name, p.Host.ID), OS: p.Host.OS,
		Arch: p.Host.Arch, Kind: "agent", Addr: addr, Meta: p.Host.Meta,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.d.Store.Ingest(ctx, host, samples, map[string]any{
		"samples": len(samples), "throttled": p.Self.Throttled, "src": "agent",
	}); err != nil {
		s.d.Log.Warn("agent ingest failed", "host", p.Host.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	s.d.Rollup.Add(p.Host.ID, samples)
	resp := map[string]any{"ok": true, "tasks": []any{}, "interval_s": 15}
	if pol, perr := s.d.Store.EffectivePolicy(ctx, p.Host.ID); perr != nil {
		s.d.Log.Warn("load policy failed", "host", p.Host.ID, "err", perr)
	} else {
		resp["policy"] = pol
		if pol.IntervalS > 0 {
			resp["interval_s"] = pol.IntervalS
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) authorizeAgent(r *http.Request, host string) bool {
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimPrefix(h, "Bearer ")
	}
	if tok == "" {
		return s.d.AgentToken == ""
	}
	if s.d.AgentToken != "" && constantEqual(tok, s.d.AgentToken) {
		return true
	}
	sum := sha256.Sum256([]byte(tok))
	hash := hex.EncodeToString(sum[:])
	want, err := s.d.Store.HostTokenHash(r.Context(), host)
	if err != nil || want == "" {
		return false
	}
	return constantEqual(hash, want)
}

func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	hosts, err := s.d.Store.ListHosts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	counts, sev, err := s.d.Store.OpenEventCounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(hosts))
	online := 0
	for _, h := range hosts {
		st := statusOf(h.LastSeen)
		if st == "online" {
			online++
		}
		out = append(out, map[string]any{
			"host": h, "status": st,
			"open_events": counts[h.ID], "max_severity": sev[h.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts": out, "total": len(out), "online": online, "ts": time.Now(),
	})
}

func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	h, err := s.d.Store.Host(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if h == nil {
		writeErr(w, http.StatusNotFound, "host not found")
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		h, err := s.d.Store.Host(ctx, id)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if h == nil {
			writeErr(w, http.StatusNotFound, "host not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"host": id, "latest": h.Metrics})
		return
	}
	if !metricNameRe.MatchString(metric) {
		writeErr(w, http.StatusBadRequest, "invalid metric")
		return
	}
	to := parseTime(r.URL.Query().Get("to"), time.Now())
	from := parseTime(r.URL.Query().Get("from"), to.Add(-time.Hour))
	if !from.Before(to) {
		writeErr(w, http.StatusBadRequest, "from must be before to")
		return
	}
	pts, err := s.d.Store.QueryMetrics(ctx, id, metric, from, to, queryInt(r, "limit", 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"host": id, "metric": metric, "from": from, "to": to, "points": pts})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	evs, err := s.d.Store.ListEvents(ctx, r.URL.Query().Get("status"), r.URL.Query().Get("host"), queryInt(r, "limit", 100))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "count": len(evs)})
}

func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	ds, err := s.d.Store.ListDecisions(ctx, r.URL.Query().Get("host"), queryInt(r, "limit", 100))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": ds, "count": len(ds)})
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	since := int64(queryInt(r, "since", 0))
	limit := queryInt(r, "limit", 200)
	wait := 30 * time.Second
	if v := r.URL.Query().Get("wait"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 && d <= 60*time.Second {
			wait = d
		}
	}
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		evs, err := s.d.Store.StreamSince(ctx, since, limit)
		cancel()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(evs) > 0 {
			next := evs[len(evs)-1].Seq
			writeJSON(w, http.StatusOK, map[string]any{
				"events": evs, "next_seq": next, "count": len(evs), "has_more": len(evs) == limit,
			})
			return
		}
		if time.Now().After(deadline) {
			writeJSON(w, http.StatusOK, map[string]any{"events": []any{}, "next_seq": since, "count": 0})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.d.Reload == nil {
		writeErr(w, http.StatusNotImplemented, "reload not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	rulesRev, collectorsRev, err := s.d.Reload(ctx)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rules_rev": rulesRev, "collectors_rev": collectorsRev})
}

func (s *Server) handleCollectorRun(w http.ResponseWriter, r *http.Request) {
	if s.d.RunCollector == nil {
		writeErr(w, http.StatusNotImplemented, "collectors disabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.d.RunCollector(ctx, r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(uiHTML)
}

// ---------- helpers ----------

func statusOf(last *time.Time) string {
	if last == nil {
		return "offline"
	}
	d := time.Since(*last)
	switch {
	case d < 60*time.Second:
		return "online"
	case d < 300*time.Second:
		return "stale"
	default:
		return "offline"
	}
}

func parseTime(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if sec, err := strconv.ParseInt(s, 10, 64); err == nil {
		return time.Unix(sec, 0)
	}
	if strings.HasPrefix(s, "-") {
		if d, err := time.ParseDuration(s[1:]); err == nil {
			return time.Now().Add(-d)
		}
	}
	return def
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}
