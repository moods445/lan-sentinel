package agent

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCollector struct{}

func (fakeCollector) Collect(time.Time) (map[string]float64, error) {
	return map[string]float64{"cpu_pct": 12.5, "mem_pct": 30}, nil
}
func (fakeCollector) Meta() Meta {
	return Meta{OS: "linux", Arch: "amd64", Hostname: "h1", Agent: "test"}
}

func testClient(t *testing.T, server string) *Client {
	t.Helper()
	cfg := Config{
		Server: server, Token: "tok", HostID: "h1", IntervalS: 15,
		Mounts: []string{"/"}, DataDir: t.TempDir(),
	}
	return NewClient(cfg, fakeCollector{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func okServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer gr.Close()
		var p syncPayload
		if err := json.NewDecoder(gr).Decode(&p); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if p.Host.ID != "h1" {
			http.Error(w, "bad host", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "tasks": []any{}, "interval_s": 15})
	}))
}

// TestSyncSpoolDrain 断网期间的批次写入 spool，恢复后按序补传并清空 spool。
func TestSyncSpoolDrain(t *testing.T) {
	ctx := context.Background()
	var hits int32
	srv := okServer(t, &hits)

	c := testClient(t, srv.URL)
	if err := c.Step(ctx, true); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending should be empty after success, got %d", len(c.pending))
	}
	if lines, _ := c.spool.Load(); len(lines) != 0 {
		t.Fatalf("spool should be empty after success, got %d", len(lines))
	}

	// 断网：连续两次上报失败，批次进入 spool
	srv.Close()
	for i := 0; i < 2; i++ {
		if err := c.Step(ctx, true); err == nil {
			t.Fatal("sync should fail while server is down")
		}
	}
	if len(c.pending) != 2 {
		t.Fatalf("pending = %d, want 2", len(c.pending))
	}
	if lines, err := c.spool.Load(); err != nil || len(lines) != 2 {
		t.Fatalf("spool lines = %d (%v), want 2", len(lines), err)
	}

	// 恢复：一次性补传全部积压并清空 spool
	var hits2 int32
	srv2 := okServer(t, &hits2)
	defer srv2.Close()
	c.cfg.Server = srv2.URL
	if err := c.Step(ctx, true); err != nil {
		t.Fatalf("recovery sync: %v", err)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending after recovery = %d, want 0", len(c.pending))
	}
	if lines, err := c.spool.Load(); err != nil || len(lines) != 0 {
		t.Fatalf("spool after recovery = %d (%v), want 0", len(lines), err)
	}
	if got := atomic.LoadInt32(&hits2); got != 3 { // 2 个积压批次 + 本轮新批次
		t.Fatalf("server hits = %d, want 3", got)
	}
}

// TestStepHonorsPolicyFilter 策略生效后上报只包含白名单指标。
func TestStepHonorsPolicyFilter(t *testing.T) {
	var gotMetrics map[string]float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "bad gzip", http.StatusBadRequest)
			return
		}
		defer gr.Close()
		var p syncPayload
		if err := json.NewDecoder(gr).Decode(&p); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		gotMetrics = p.Metrics
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "tasks": []any{}, "interval_s": 20,
			"policy": Policy{Rev: "g2", EnabledMetrics: []string{"cpu_pct"}, IntervalS: 20},
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	if err := c.Step(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(gotMetrics) != 2 { // 首个批次按本地全量采集
		t.Fatalf("first batch metrics = %v", gotMetrics)
	}
	if c.pol.Rev != "g2" || c.interval != 20*time.Second {
		t.Fatalf("policy not applied: rev=%s interval=%v", c.pol.Rev, c.interval)
	}
	if err := c.Step(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if len(gotMetrics) != 1 || gotMetrics["cpu_pct"] != 12.5 {
		t.Fatalf("policy filter not applied: %v", gotMetrics)
	}
}
