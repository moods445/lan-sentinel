//go:build linux

package agent

import "testing"

func TestParseDiskstats(t *testing.T) {
	s := `   8       0 sda 100 0 2000 30 50 0 1000 20 0 40 80
   8       1 sda1 10 0 200 3 5 0 100 2 0 4 8
   7       0 loop0 1 0 2 0 3 0 4 0 0 0 0
`
	m := parseDiskstats(s)
	if len(m) != 3 {
		t.Fatalf("parsed %d devices, want 3", len(m))
	}
	d, ok := m["sda"]
	if !ok {
		t.Fatal("sda missing")
	}
	if d.reads != 100 || d.writes != 50 || d.sectorsRead != 2000 || d.sectorsWrite != 1000 {
		t.Fatalf("counters = %+v", d)
	}
	if d.msIO != 40 || d.weightedMS != 80 {
		t.Fatalf("time counters = %+v", d)
	}
}

func TestShouldSkipDisk(t *testing.T) {
	for _, name := range []string{"loop0", "ram1", "zram0", "fd0", "sr0"} {
		if !shouldSkipDisk(name) {
			t.Fatalf("%s should be skipped", name)
		}
	}
	for _, name := range []string{"sda", "nvme0n1", "vda", "md0", "dm-0"} {
		if shouldSkipDisk(name) {
			t.Fatalf("%s should be kept", name)
		}
	}
}

func TestParseProcStat(t *testing.T) {
	line := "1234 (some proc name) S 1 2 3 0 -1 4194560 0 0 0 0 500 700 0 0\n"
	st, err := parseProcStat(line)
	if err != nil {
		t.Fatal(err)
	}
	if st.name != "some proc name" || st.state != "S" {
		t.Fatalf("stat = %+v", st)
	}
	if st.cpuTicks != 1200 {
		t.Fatalf("cpuTicks = %d, want 1200", st.cpuTicks)
	}
}

func TestParseProcIO(t *testing.T) {
	s := `rchar: 999
read_bytes: 4096
wchar: 888
write_bytes: 8192
cancelled_write_bytes: 0
`
	c := parseProcIO(s)
	if c.readBytes != 4096 || c.writeBytes != 8192 {
		t.Fatalf("io = %+v", c)
	}
}

func TestParseStatmRSS(t *testing.T) {
	if got := parseStatmRSS("12345 678 90 1 0 12 0\n"); got != 678 {
		t.Fatalf("rss pages = %d, want 678", got)
	}
	if got := parseStatmRSS(""); got != 0 {
		t.Fatalf("empty statm = %d", got)
	}
}

func TestParseCPUCores(t *testing.T) {
	s := `cpu  1000 0 500 8000 100 0 0 0 0 0
cpu0 1000 0 500 8000 100 0 0 0 0 0
cpu1 2000 0 300 6000 50 0 0 0 0 0
`
	m := parseCPUCores(s)
	if len(m) != 2 {
		t.Fatalf("got %d cores, want 2", len(m))
	}
	c0 := m["cpu0"]
	if c0.total != 9600 || c0.idle != 8000 || c0.iowait != 100 {
		t.Fatalf("cpu0 = %+v", c0)
	}
}

func TestParseLoadavgDetail(t *testing.T) {
	l1, l5, l15, running, total := parseLoadavgDetail("0.52 0.58 0.59 2/345 12345\n")
	if l1 != 0.52 || l5 != 0.58 || l15 != 0.59 || running != 2 || total != 345 {
		t.Fatalf("loadavg = %v %v %v %d/%d", l1, l5, l15, running, total)
	}
}

func TestParseNetDevDetail(t *testing.T) {
	s := `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1000 10 0 0 0 0 0 0 1000 10 0 0 0 0 0 0
  eth0: 2000 20 1 2 0 0 0 0 3000 30 3 4 0 0 0 0
`
	m := parseNetDev(s)
	if len(m) != 2 {
		t.Fatalf("got %d interfaces, want 2", len(m))
	}
	e := m["eth0"]
	if e.rx != 2000 || e.tx != 3000 || e.rxPkt != 20 || e.txPkt != 30 {
		t.Fatalf("eth0 bytes/packets = %+v", e)
	}
	if e.rxErr != 1 || e.rxDrop != 2 || e.txErr != 3 || e.txDrop != 4 {
		t.Fatalf("eth0 errors = %+v", e)
	}
}

func TestParseMeminfoDetail(t *testing.T) {
	s := `MemTotal:       1000 kB
MemAvailable:    400 kB
Cached:          200 kB
Buffers:          50 kB
SwapTotal:       500 kB
SwapFree:        300 kB
`
	m, err := parseMeminfo(s)
	if err != nil {
		t.Fatal(err)
	}
	if m.total != 1000 || m.available != 400 || m.cached != 200 || m.buffers != 50 {
		t.Fatalf("meminfo = %+v", m)
	}
	if m.swapTotal != 500 || m.swapFree != 300 {
		t.Fatalf("swap = %+v", m)
	}
}
