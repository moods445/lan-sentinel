package agent

import (
	"strings"
	"testing"
)

func TestCounterDiff(t *testing.T) {
	if v := counterDiff(100, 40); v != 60 {
		t.Fatalf("diff = %d, want 60", v)
	}
	if v := counterDiff(10, 40); v != 0 {
		t.Fatalf("counter reset should be 0, got %d", v)
	}
}

func TestSanitizeProcName(t *testing.T) {
	if got := sanitizeProcName("  postgres\n"); got != "postgres" {
		t.Fatalf("name = %q", got)
	}
	if got := sanitizeProcName("bad\x01name"); strings.ContainsRune(got, 0x01) {
		t.Fatalf("control char not removed: %q", got)
	}
	if got := sanitizeProcName(strings.Repeat("a", 100)); len(got) > 48 {
		t.Fatalf("name too long: %d", len(got))
	}
	if got := sanitizeProcName(""); got != "?" {
		t.Fatalf("empty name = %q", got)
	}
}

func TestTopProcs(t *testing.T) {
	all := []IOProcess{
		{PID: 1, Name: "a", ReadBytesS: 10, CPUPct: 1, RSSMB: 20},
		{PID: 2, Name: "b", WriteBytesS: 90, CPUPct: 2, RSSMB: 10},
		{PID: 3, Name: "c", CPUPct: 30, RSSMB: 5},
		{PID: 4, Name: "d", WriteBytesS: 5, CPUPct: 50, RSSMB: 1},
	}
	got := topProcs(all, 2, 1, 0)
	if len(got) != 3 {
		t.Fatalf("got %d procs, want 3", len(got))
	}
	if got[0].PID != 2 || got[1].PID != 1 {
		t.Fatalf("IO order wrong: %+v", got)
	}
	if got[2].PID != 4 { // CPU 补充取最高 CPU，且不与 IO 列表重复
		t.Fatalf("cpu tail = %+v", got[2])
	}
}

func TestTopProcsMemTail(t *testing.T) {
	all := []IOProcess{
		{PID: 1, Name: "a", ReadBytesS: 100, RSSMB: 1},
		{PID: 2, Name: "b", RSSMB: 300},
		{PID: 3, Name: "c", RSSMB: 200},
	}
	got := topProcs(all, 1, 0, 2)
	if len(got) != 3 {
		t.Fatalf("got %d procs, want 3", len(got))
	}
	if got[0].PID != 1 || got[1].PID != 2 || got[2].PID != 3 {
		t.Fatalf("mem tail order wrong: %+v", got)
	}
}

func TestTopProcsFallbackRSS(t *testing.T) {
	all := []IOProcess{
		{PID: 1, Name: "a", RSSMB: 10},
		{PID: 2, Name: "b", RSSMB: 30},
	}
	got := topProcs(all, 1, 5, 0)
	if len(got) != 1 || got[0].PID != 2 {
		t.Fatalf("first sample should fall back to RSS: %+v", got)
	}
}

func TestSummarizeDevices(t *testing.T) {
	s, devs := summarizeDevices([]IODevice{
		{Name: "sdb", ReadBytesS: 100, UtilPct: 10},
		{Name: "sda", WriteBytesS: 900, UtilPct: 80},
	})
	if s.ReadBytesS != 100 || s.WriteBytesS != 900 || s.UtilPct != 80 {
		t.Fatalf("summary = %+v", s)
	}
	if len(devs) != 2 || devs[0].Name != "sda" {
		t.Fatalf("devices not sorted by throughput: %+v", devs)
	}
}
