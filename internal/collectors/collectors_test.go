package collectors

import (
	"testing"
	"time"
)

func TestLookupPath(t *testing.T) {
	doc := map[string]any{
		"status": 1.0,
		"data": map[string]any{
			"cpu": map[string]any{"usage": 42.5},
			"disks": []any{
				map[string]any{"used_pct": 88.0},
			},
			"name": "nas",
			"ok":   true,
		},
	}
	cases := []struct {
		path string
		want float64
		ok   bool
	}{
		{"status", 1, true},
		{"data.cpu.usage", 42.5, true},
		{"data.disks.0.used_pct", 88, true},
		{"data.ok", 1, true},
		{"data.missing", 0, false},
		{"status.deeper", 0, false},
	}
	for _, c := range cases {
		got, ok := lookupPath(doc, c.path)
		if ok != c.ok || (ok && got != c.want) {
			t.Fatalf("%s => %v,%v want %v,%v", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestParsePrometheus(t *testing.T) {
	body := []byte(`# HELP x y
# TYPE x gauge
node_cpu_seconds_total{cpu="0"} 123.5
node_load1 0.42
bad_line
nan_metric NaN
`)
	now := time.Now()
	out := parsePrometheus(body, now)
	if len(out) != 2 {
		t.Fatalf("got %d samples: %+v", len(out), out)
	}
	if out[0].Metric != "node_cpu_seconds_total" || out[0].Value != 123.5 {
		t.Fatalf("first = %+v", out[0])
	}
	if out[1].Metric != "node_load1" || out[1].Value != 0.42 {
		t.Fatalf("second = %+v", out[1])
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 30*time.Second || backoff(2) != 2*time.Minute || backoff(5) != 5*time.Minute {
		t.Fatal("unexpected backoff")
	}
}

func TestSplitHostPort(t *testing.T) {
	h, p := splitHostPort("192.168.0.1:161", 161)
	if h != "192.168.0.1" || p != 161 {
		t.Fatalf("%s %d", h, p)
	}
	h, p = splitHostPort("192.168.0.2", 161)
	if h != "192.168.0.2" || p != 161 {
		t.Fatalf("%s %d", h, p)
	}
}

func TestRate(t *testing.T) {
	it := &item{prev: map[string]prevSample{}}
	now := time.Now()
	if _, ok := rate(it, "if_in_bps:2", 1000, now); ok {
		t.Fatal("first sample must only set baseline")
	}
	v, ok := rate(it, "if_in_bps:2", 2000, now.Add(2*time.Second))
	if !ok || v != 4000 {
		t.Fatalf("rate = %v ok=%v, want 4000", v, ok)
	}
	v, ok = rate(it, "if_in_bps:2", 10, now.Add(4*time.Second))
	if !ok || v != 0 {
		t.Fatalf("counter wrap should be 0, got %v", v)
	}
}
