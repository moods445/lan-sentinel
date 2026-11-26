package agent

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMatchMetricPattern(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"cpu_pct", "cpu_pct", true},
		{"cpu_pct", "cpu_iowait_pct", false},
		{"temp:*", "temp:zone0", true},
		{"temp:*", "temp:", true},
		{"temp:*", "temps", false},
		{"disk_used_pct:*", "disk_used_pct:/", true},
		{"disk_used_pct:*", "disk_free_gb:/", false},
		{"*", "anything", true},
	}
	for _, c := range cases {
		if got := matchMetricPattern(c.pattern, c.name); got != c.want {
			t.Fatalf("match(%q,%q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestFilterMetrics(t *testing.T) {
	m := map[string]float64{
		"cpu_pct": 12, "mem_pct": 40, "disk_used_pct:/": 55,
		"disk_free_gb:/": 10, "temp:zone0": 44, "net_rx_bps": 100,
	}
	all := filterMetrics(m, Policy{})
	if len(all) != len(m) {
		t.Fatalf("empty policy should keep all metrics, got %d", len(all))
	}
	got := filterMetrics(m, Policy{EnabledMetrics: []string{"cpu_pct", "disk_used_pct:*", "temp:*"}})
	if len(got) != 3 || got["cpu_pct"] != 12 || got["disk_used_pct:/"] != 55 || got["temp:zone0"] != 44 {
		t.Fatalf("unexpected filtered metrics: %+v", got)
	}
}

func TestPolicyFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	in := Policy{Rev: "g2.h1", EnabledMetrics: []string{"cpu_pct"}, IntervalS: 30, Mounts: []string{"/", "/data"}}
	if err := savePolicyFile(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := loadPolicyFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Rev != in.Rev || out.IntervalS != 30 || len(out.EnabledMetrics) != 1 || len(out.Mounts) != 2 {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if got := in.interval(15 * time.Second); got != 30*time.Second {
		t.Fatalf("policy interval = %v", got)
	}
	if got := (Policy{}).interval(15 * time.Second); got != 15*time.Second {
		t.Fatalf("default interval = %v", got)
	}
}
