package rules

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/home-aio/lan-sentinel/internal/config"
	"github.com/home-aio/lan-sentinel/internal/store"
)

type fakeStore struct {
	hosts     []store.HostRow
	latest    map[string]map[string]store.Latest
	states    map[string]store.CollectorState
	open      map[string]store.Event
	decisions []store.Decision
	cooldowns map[string]time.Time
	window    map[string]float64
	seq       int64
}

func newFake() *fakeStore {
	return &fakeStore{
		latest:    map[string]map[string]store.Latest{},
		states:    map[string]store.CollectorState{},
		open:      map[string]store.Event{},
		cooldowns: map[string]time.Time{},
		window:    map[string]float64{},
	}
}

func (f *fakeStore) ListHosts(context.Context) ([]store.HostRow, error) { return f.hosts, nil }
func (f *fakeStore) LatestMetrics(context.Context) (map[string]map[string]store.Latest, error) {
	return f.latest, nil
}
func (f *fakeStore) CollectorStates(context.Context) (map[string]store.CollectorState, error) {
	return f.states, nil
}
func (f *fakeStore) OpenEvent(_ context.Context, d store.EventDraft) (int64, bool, error) {
	if e, ok := f.open[d.Fingerprint]; ok {
		e.Value = &d.Value
		e.Severity = d.Severity
		f.open[d.Fingerprint] = e
		return e.ID, false, nil
	}
	f.seq++
	e := store.Event{ID: f.seq, Fingerprint: d.Fingerprint, HostID: d.HostID, RuleID: d.RuleID,
		Severity: d.Severity, Status: "open", Title: d.Title, Value: &d.Value}
	f.open[d.Fingerprint] = e
	f.decisions = append(f.decisions, store.Decision{ActorKind: "rule", Actor: d.RuleID, RuleID: d.RuleID, Decision: "open"})
	return e.ID, true, nil
}
func (f *fakeStore) ResolveEvent(_ context.Context, fp string, cd time.Duration, _ string) (int64, bool, error) {
	e, ok := f.open[fp]
	if !ok {
		return 0, false, nil
	}
	delete(f.open, fp)
	f.cooldowns[fp] = time.Now().Add(cd)
	f.decisions = append(f.decisions, store.Decision{ActorKind: "rule", Actor: e.RuleID, Decision: "resolve"})
	return e.ID, true, nil
}
func (f *fakeStore) OpenFingerprints(context.Context) (map[string]store.Event, error) {
	out := map[string]store.Event{}
	for k, v := range f.open {
		out[k] = v
	}
	return out, nil
}
func (f *fakeStore) CooldownUntil(_ context.Context, fp string) (time.Time, error) {
	return f.cooldowns[fp], nil
}
func (f *fakeStore) WindowMin(_ context.Context, host, metric string, _ time.Time) (float64, bool, error) {
	v, ok := f.window[host+"|"+metric]
	return v, ok, nil
}
func (f *fakeStore) RecordDecision(_ context.Context, d store.Decision) error {
	f.decisions = append(f.decisions, d)
	return nil
}

func testEngine(f *fakeStore, rules ...config.Rule) *Engine {
	return New(f, rules, "rev1", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func hostWithCPU(id string, cpu float64) store.HostRow {
	now := time.Now()
	return store.HostRow{ID: id, LastSeen: &now}
}

func TestThresholdOpenResolveAndSuppress(t *testing.T) {
	f := newFake()
	f.hosts = []store.HostRow{hostWithCPU("h1", 96)}
	f.latest["h1"] = map[string]store.Latest{"cpu_pct": {Metric: "cpu_pct", Value: 96}}
	e := testEngine(f, config.Rule{
		ID: "cpu_high", Type: "metric_threshold", Metric: "cpu_pct",
		Warning: 85, Critical: 95, Cooldown: config.Duration(20 * time.Minute), Title: "CPU 高",
	})

	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.open) != 1 {
		t.Fatalf("want 1 open event, got %d", len(f.open))
	}
	for _, ev := range f.open {
		if ev.Severity != "critical" {
			t.Fatalf("severity = %s", ev.Severity)
		}
	}
	if len(f.decisions) != 1 || f.decisions[0].Decision != "open" {
		t.Fatalf("decisions = %+v", f.decisions)
	}

	// 重复求值不应重复开事件
	_ = e.Evaluate(context.Background())
	if len(f.open) != 1 || len(f.decisions) != 1 {
		t.Fatalf("duplicated: open=%d decisions=%d", len(f.open), len(f.decisions))
	}

	// 指标恢复 → 关事件并进入冷却
	f.latest["h1"]["cpu_pct"] = store.Latest{Metric: "cpu_pct", Value: 10}
	_ = e.Evaluate(context.Background())
	if len(f.open) != 0 {
		t.Fatalf("event should be resolved")
	}
	if len(f.decisions) != 2 || f.decisions[1].Decision != "resolve" {
		t.Fatalf("decisions = %+v", f.decisions)
	}

	// 冷却期内再次越线 → 只记 suppressed，不开事件
	f.latest["h1"]["cpu_pct"] = store.Latest{Metric: "cpu_pct", Value: 99}
	_ = e.Evaluate(context.Background())
	if len(f.open) != 0 {
		t.Fatalf("cooldown should suppress event")
	}
	if len(f.decisions) != 3 || f.decisions[2].Decision != "suppressed" {
		t.Fatalf("decisions = %+v", f.decisions)
	}
}

func TestPerScopeFingerprint(t *testing.T) {
	f := newFake()
	f.hosts = []store.HostRow{hostWithCPU("h1", 0)}
	f.latest["h1"] = map[string]store.Latest{
		"disk_used_pct:/":     {Metric: "disk_used_pct:/", Value: 91},
		"disk_used_pct:/data": {Metric: "disk_used_pct:/data", Value: 50},
	}
	e := testEngine(f, config.Rule{
		ID: "disk_high", Type: "metric_threshold", Metric: "disk_used_pct",
		PerScope: true, Warning: 88, Critical: 95, Cooldown: config.Duration(time.Minute),
	})
	if err := e.Evaluate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.open) != 1 {
		t.Fatalf("want only root disk event, got %d", len(f.open))
	}
	if _, ok := f.open["h1|disk_high|/"]; !ok {
		t.Fatalf("fingerprint = %v", f.open)
	}
}

func TestHeartbeat(t *testing.T) {
	f := newFake()
	old := time.Now().Add(-5 * time.Minute)
	f.hosts = []store.HostRow{{ID: "h1", LastSeen: &old}}
	e := testEngine(f, config.Rule{
		ID: "hb", Type: "heartbeat", Severity: "critical",
		Timeout: config.Duration(150 * time.Second), Title: "心跳丢失",
	})
	_ = e.Evaluate(context.Background())
	if len(f.open) != 1 || f.open["h1|hb"].Severity != "critical" {
		t.Fatalf("heartbeat not raised: %+v", f.open)
	}
	now := time.Now()
	f.hosts[0].LastSeen = &now
	_ = e.Evaluate(context.Background())
	if len(f.open) != 0 {
		t.Fatalf("heartbeat should resolve")
	}
}

func TestSustainedWindow(t *testing.T) {
	f := newFake()
	f.hosts = []store.HostRow{hostWithCPU("h1", 90)}
	f.latest["h1"] = map[string]store.Latest{"cpu_pct": {Metric: "cpu_pct", Value: 90}}
	rule := config.Rule{
		ID: "cpu_for", Type: "metric_threshold", Metric: "cpu_pct",
		Warning: 85, Critical: 95, For: config.Duration(3 * time.Minute), Cooldown: config.Duration(time.Minute),
	}
	e := testEngine(f, rule)

	// 窗口内曾回落到 60 → 不算持续，不开事件
	f.window["h1|cpu_pct"] = 60
	_ = e.Evaluate(context.Background())
	if len(f.open) != 0 {
		t.Fatalf("should not fire when window dipped")
	}

	// 窗口内持续高于阈值 → 开事件
	f.window["h1|cpu_pct"] = 86
	_ = e.Evaluate(context.Background())
	if len(f.open) != 1 {
		t.Fatalf("should fire when sustained, got %d", len(f.open))
	}
}

func TestCollectorFailing(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.hosts = []store.HostRow{{ID: "router", Kind: "snmp", LastSeen: &now}}
	f.states["router"] = store.CollectorState{HostID: "router", Failures: 3, LastError: "timeout"}
	e := testEngine(f, config.Rule{ID: "cf", Type: "collector_failing", Consecutive: 3, Cooldown: config.Duration(time.Minute)})
	_ = e.Evaluate(context.Background())
	if len(f.open) != 1 {
		t.Fatalf("collector_failing not raised")
	}
}

func TestClassify(t *testing.T) {
	r := config.Rule{Warning: 85, Critical: 95}
	if s, ok := classify(84, r); ok {
		t.Fatalf("84 should not hit, got %s", s)
	}
	if s, _ := classify(85, r); s != "warning" {
		t.Fatalf("85 -> %s", s)
	}
	if s, _ := classify(95, r); s != "critical" {
		t.Fatalf("95 -> %s", s)
	}
}
