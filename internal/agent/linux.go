//go:build linux

package agent

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LinuxCollector 只读 /proc、/sys 与 statfs：零子进程、零端口
type LinuxCollector struct {
	procRoot string
	sysRoot  string
	mounts   []string

	prevCPU   cpuStat
	haveCPU   bool
	prevNet   map[string]netCtr
	prevNetTS time.Time

	kernel string
}

func NewLinuxCollector(mounts []string) *LinuxCollector {
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	if len(mounts) > 3 {
		mounts = mounts[:3]
	}
	k, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return &LinuxCollector{
		procRoot: "/proc",
		sysRoot:  "/sys",
		mounts:   mounts,
		kernel:   strings.TrimSpace(string(k)),
	}
}

func NewCollector(mounts []string) Collector { return NewLinuxCollector(mounts) }

// SetMounts 策略热更新挂载点
func (c *LinuxCollector) SetMounts(mounts []string) { c.mounts = normalizeMounts(mounts) }

func (c *LinuxCollector) Meta() Meta {
	host, _ := os.Hostname()
	return Meta{OS: "linux", Arch: runtime.GOARCH, Kernel: c.kernel, Hostname: host, Agent: agentVersion}
}

func (c *LinuxCollector) Collect(now time.Time) (map[string]float64, error) {
	m := map[string]float64{}

	if st, err := readCPUStat(filepath.Join(c.procRoot, "stat")); err == nil {
		if c.haveCPU {
			dt := st.total - c.prevCPU.total
			if dt > 0 {
				busy := (st.total - st.idleAll()) - (c.prevCPU.total - c.prevCPU.idleAll())
				m["cpu_pct"] = round2(busy / dt * 100)
				m["cpu_iowait_pct"] = round2((st.iowait - c.prevCPU.iowait) / dt * 100)
			}
		}
		c.prevCPU = st
		c.haveCPU = true
	}

	if mem, err := readMeminfo(filepath.Join(c.procRoot, "meminfo")); err == nil && mem.total > 0 {
		m["mem_pct"] = round2((mem.total - mem.available) / mem.total * 100)
		if mem.swapTotal > 0 {
			m["swap_pct"] = round2((mem.swapTotal - mem.swapFree) / mem.swapTotal * 100)
		} else {
			m["swap_pct"] = 0
		}
		m["mem_used_gb"] = round2((mem.total - mem.available) / 1024 / 1024)
	}

	if l, err := readLoadavg(filepath.Join(c.procRoot, "loadavg")); err == nil {
		m["load1"], m["load5"], m["load15"] = l[0], l[1], l[2]
	}

	for _, mp := range c.mounts {
		var st syscall.Statfs_t
		if err := syscall.Statfs(mp, &st); err != nil {
			continue
		}
		bs := float64(st.Bsize)
		used := float64(st.Blocks-st.Bfree) * bs
		avail := float64(st.Bavail) * bs
		denom := used + avail
		if denom <= 0 {
			continue
		}
		m["disk_used_pct:"+mp] = round2(used / denom * 100)
		m["disk_free_gb:"+mp] = round2(avail / 1024 / 1024 / 1024)
	}

	if net, err := readNetDev(filepath.Join(c.procRoot, "net", "dev")); err == nil {
		var rx, tx float64
		for name, v := range net {
			if name == "lo" {
				continue
			}
			rx += float64(v.rx)
			tx += float64(v.tx)
		}
		if c.prevNet != nil && !c.prevNetTS.IsZero() {
			dt := now.Sub(c.prevNetTS).Seconds()
			if dt > 0 {
				var prx, ptx float64
				for name, v := range c.prevNet {
					if name == "lo" {
						continue
					}
					prx += float64(v.rx)
					ptx += float64(v.tx)
				}
				m["net_rx_bps"] = round2(bps(rx, prx, dt))
				m["net_tx_bps"] = round2(bps(tx, ptx, dt))
			}
		}
		c.prevNet = net
		c.prevNetTS = now
	}

	if up, err := readUptime(filepath.Join(c.procRoot, "uptime")); err == nil {
		m["uptime_s"] = up
	}

	if n, err := countProcs(c.procRoot); err == nil {
		m["proc_count"] = float64(n)
	}

	for i, t := range readTemps(c.sysRoot, 2) {
		m[fmt.Sprintf("temp:zone%d", i)] = t
	}
	return m, nil
}

// ---------- /proc 解析 ----------

type cpuStat struct {
	total, idle, iowait float64
}

func (c cpuStat) idleAll() float64 { return c.idle + c.iowait }

func readCPUStat(path string) (cpuStat, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cpuStat{}, err
	}
	return parseCPUStat(string(b))
}

func parseCPUStat(s string) (cpuStat, error) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		f := strings.Fields(line)[1:]
		if len(f) < 5 {
			return cpuStat{}, fmt.Errorf("short cpu line")
		}
		v := make([]float64, 0, 8)
		for i := 0; i < len(f) && i < 8; i++ {
			x, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				return cpuStat{}, err
			}
			v = append(v, x)
		}
		var total float64
		for _, x := range v {
			total += x
		}
		return cpuStat{total: total, idle: v[3], iowait: v[4]}, nil
	}
	return cpuStat{}, fmt.Errorf("cpu line not found")
}

type memStats struct {
	total, available, swapTotal, swapFree float64
}

func readMeminfo(path string) (memStats, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return memStats{}, err
	}
	return parseMeminfo(string(b))
}

func parseMeminfo(s string) (memStats, error) {
	var m memStats
	for _, line := range strings.Split(s, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := line[:i]
		f := strings.Fields(line[i+1:])
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(f[0], 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			m.total = v
		case "MemAvailable":
			m.available = v
		case "SwapTotal":
			m.swapTotal = v
		case "SwapFree":
			m.swapFree = v
		}
	}
	return m, nil
}

func readLoadavg(path string) ([3]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return [3]float64{}, err
	}
	return parseLoadavg(string(b))
}

func parseLoadavg(s string) ([3]float64, error) {
	var out [3]float64
	f := strings.Fields(s)
	if len(f) < 3 {
		return out, fmt.Errorf("short loadavg")
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(f[i], 64)
		if err != nil {
			return out, err
		}
		out[i] = v
	}
	return out, nil
}

type netCtr struct{ rx, tx uint64 }

func readNetDev(path string) (map[string]netCtr, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseNetDev(string(b)), nil
}

func parseNetDev(s string) map[string]netCtr {
	out := map[string]netCtr{}
	for _, line := range strings.Split(s, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 16 {
			continue
		}
		rx, err1 := strconv.ParseUint(f[0], 10, 64)
		tx, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[name] = netCtr{rx: rx, tx: tx}
	}
	return out
}

func readUptime(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, fmt.Errorf("empty uptime")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return math.Round(v), err
}

func countProcs(procRoot string) (int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if name[0] < '0' || name[0] > '9' {
			continue
		}
		if _, err := strconv.Atoi(name); err == nil {
			n++
		}
	}
	return n, nil
}

func readTemps(sysRoot string, max int) []float64 {
	zones, _ := filepath.Glob(filepath.Join(sysRoot, "class", "thermal", "thermal_zone*", "temp"))
	var out []float64
	for _, z := range zones {
		b, err := os.ReadFile(z)
		if err != nil {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			continue
		}
		out = append(out, round2(v/1000))
		if len(out) >= max {
			break
		}
	}
	return out
}

// selfCPUPct 读 /proc/self/stat 的 utime+stime 差分，用于自适应降频
func selfCPUSeconds(path string) (float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	// comm 字段可能含空格，从最后一个 ')' 之后再切分
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("bad stat")
	}
	f := strings.Fields(s[i+1:])
	// 剩余字段从第 3 个（state）开始；utime=14, stime=15（从 1 计）
	if len(f) < 13 {
		return 0, fmt.Errorf("short stat")
	}
	utime, _ := strconv.ParseFloat(f[11], 64)
	stime, _ := strconv.ParseFloat(f[12], 64)
	return (utime + stime) / 100, nil // CLK_TCK 固定 100
}

func selfCPU() (float64, error) { return selfCPUSeconds("/proc/self/stat") }
