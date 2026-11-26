package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCPUStat(t *testing.T) {
	s := `cpu  100 0 100 800 50 0 0 0 0 0
cpu0 50 0 50 400 25 0 0 0 0 0
intr 123
`
	st, err := parseCPUStat(s)
	if err != nil {
		t.Fatal(err)
	}
	if st.total != 1050 {
		t.Fatalf("total = %v, want 1050", st.total)
	}
	if st.idle != 800 || st.iowait != 50 {
		t.Fatalf("idle/iowait = %v/%v", st.idle, st.iowait)
	}
	if st.idleAll() != 850 {
		t.Fatalf("idleAll = %v", st.idleAll())
	}
}

func TestParseMeminfo(t *testing.T) {
	s := `MemTotal:       16000000 kB
MemAvailable:    4000000 kB
SwapTotal:       2000000 kB
SwapFree:        1500000 kB
`
	m, err := parseMeminfo(s)
	if err != nil {
		t.Fatal(err)
	}
	if m.total != 16000000 || m.available != 4000000 || m.swapTotal != 2000000 || m.swapFree != 1500000 {
		t.Fatalf("unexpected meminfo: %+v", m)
	}
}

func TestParseLoadavg(t *testing.T) {
	l, err := parseLoadavg("0.52 0.61 0.73 1/345 12345\n")
	if err != nil {
		t.Fatal(err)
	}
	if l[0] != 0.52 || l[2] != 0.73 {
		t.Fatalf("load = %v", l)
	}
}

func TestParseNetDev(t *testing.T) {
	s := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000       10    0    0    0     0          0         0     1000       10    0    0    0     0       0          0
  eth0: 2000       20    0    0    0     0          0         0     3000       30    0    0    0     0       0          0
`
	n := parseNetDev(s)
	if n["eth0"].rx != 2000 || n["eth0"].tx != 3000 {
		t.Fatalf("eth0 = %+v", n["eth0"])
	}
	if _, ok := n["lo"]; !ok {
		t.Fatal("lo should be parsed")
	}
}

func TestSelfCPUSeconds(t *testing.T) {
	p := filepath.Join(t.TempDir(), "stat")
	line := "1234 (some proc name) S 1 2 3 0 -1 4194560 " + strings.Repeat("0 ", 4) + "500 700 0 0\n"
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	sec, err := selfCPUSeconds(p)
	if err != nil {
		t.Fatal(err)
	}
	if sec != 12 { // (500+700)/100
		t.Fatalf("self cpu seconds = %v, want 12", sec)
	}
}

func TestSpoolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sp := NewSpool(filepath.Join(dir, "spool.jsonl"))
	lines := [][]byte{[]byte(`{"n":1}`), []byte(`{"n":2}`), []byte(`{"n":3}`)}
	if err := sp.Replace(lines); err != nil {
		t.Fatal(err)
	}
	got, err := sp.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[0]) != `{"n":1}` || string(got[2]) != `{"n":3}` {
		t.Fatalf("spool order wrong: %q", got)
	}
	if err := sp.Clear(); err != nil {
		t.Fatal(err)
	}
	if got, _ := sp.Load(); len(got) != 0 {
		t.Fatalf("spool should be empty, got %d", len(got))
	}
}

func TestSpoolLineCap(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "spool.jsonl")
	sp := NewSpool(p)
	var lines [][]byte
	for i := 0; i < 250; i++ {
		lines = append(lines, []byte(`{"n":1}`))
	}
	if err := sp.Replace(lines); err != nil {
		t.Fatal(err)
	}
	// Replace 不裁剪行数，Load 时裁剪到最后 200 条
	got, err := sp.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 200 {
		t.Fatalf("loaded %d lines, want 200", len(got))
	}
}

func TestBps(t *testing.T) {
	if v := bps(2000, 1000, 2); v != 4000 {
		t.Fatalf("bps = %v, want 4000", v)
	}
	if v := bps(10, 1000, 1); v != 0 {
		t.Fatalf("counter reset should be 0, got %v", v)
	}
}
