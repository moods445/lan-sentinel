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

	prevDisk    map[string]diskCtr
	prevDiskTS  time.Time
	prevProc    map[int]procCtr
	prevProcTS  time.Time
	prevCores   map[string]cpuStat
	prevCoresTS time.Time
	prevIface   map[string]netCtr
	prevIfaceTS time.Time

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

// CollectDetail 采样 CPU/内存/网络/负载与磁盘、进程明细；由 Client 低频调用（默认 60s）。
// 只读 /proc 与 /sys，仍然零子进程。
func (c *LinuxCollector) CollectDetail(now time.Time) *DetailSnapshot {
	snap := &DetailSnapshot{}

	if raw, err := os.ReadFile(filepath.Join(c.procRoot, "diskstats")); err == nil {
		cur := parseDiskstats(string(raw))
		dt := 0.0
		if !c.prevDiskTS.IsZero() {
			dt = now.Sub(c.prevDiskTS).Seconds()
		}
		devs := make([]IODevice, 0, len(cur))
		for name, v := range cur {
			if shouldSkipDisk(name) || !c.wholeDisk(name) {
				continue
			}
			d := IODevice{Name: name}
			if prev, ok := c.prevDisk[name]; ok && dt > 0 {
				dr := counterDiff(v.reads, prev.reads)
				dw := counterDiff(v.writes, prev.writes)
				sr := counterDiff(v.sectorsRead, prev.sectorsRead)
				sw := counterDiff(v.sectorsWrite, prev.sectorsWrite)
				mio := counterDiff(v.msIO, prev.msIO)
				wmio := counterDiff(v.weightedMS, prev.weightedMS)
				d.ReadBytesS = roundIO(float64(sr) * 512 / dt)
				d.WriteBytesS = roundIO(float64(sw) * 512 / dt)
				d.ReadIOPS = roundIO(float64(dr) / dt)
				d.WriteIOPS = roundIO(float64(dw) / dt)
				d.UtilPct = clampPct(float64(mio) / dt / 10) // ms/s 除以 10 即百分比
				if ops := dr + dw; ops > 0 {
					d.AwaitMS = roundIO(float64(mio) / float64(ops))
				}
				if mio > 0 {
					d.Queue = roundIO(float64(wmio) / float64(mio))
				}
			}
			devs = append(devs, d)
		}
		c.prevDisk = cur
		c.prevDiskTS = now
		snap.IO.Summary, devs = summarizeDevices(devs)
		snap.IO.Devices = trimDevices(devs)
	}

	snap.CPU = c.collectCPUCores(now)
	snap.Mem = c.collectMem()
	snap.Net = c.collectNetIfaces(now)

	procs, procCount := c.collectProcs(now)
	snap.IO.Summary.ProcCount = procCount
	snap.Procs = topProcs(procs, maxDetailProcsIO, maxDetailProcsCPU, maxDetailProcsMem)
	snap.Load = c.collectLoad(procs)
	return snap
}

// collectCPUCores 按核心做差分；首次采样只给出核心列表，速率为 0
func (c *LinuxCollector) collectCPUCores(now time.Time) CPUSnapshot {
	raw, err := os.ReadFile(filepath.Join(c.procRoot, "stat"))
	if err != nil {
		return CPUSnapshot{}
	}
	cur := parseCPUCores(string(raw))
	snap := CPUSnapshot{}
	dt := 0.0
	if !c.prevCoresTS.IsZero() {
		dt = now.Sub(c.prevCoresTS).Seconds()
	}
	var total, busy, wait float64
	for name, st := range cur {
		if name == "cpu" {
			continue
		}
		core := CPUCore{Name: name}
		if prev, ok := c.prevCores[name]; ok && dt > 0 {
			dTotal := st.total - prev.total
			if dTotal > 0 {
				busyDelta := (st.total - st.idleAll()) - (prev.total - prev.idleAll())
				core.UsagePct = clampPct(busyDelta / dTotal * 100)
				core.IowaitPct = clampPct((st.iowait - prev.iowait) / dTotal * 100)
				total += dTotal
				busy += busyDelta
				wait += st.iowait - prev.iowait
			}
		}
		snap.Cores = append(snap.Cores, core)
	}
	if total > 0 {
		snap.UsagePct = clampPct(busy / total * 100)
		snap.IowaitPct = clampPct(wait / total * 100)
	}
	sortCores(snap.Cores)
	snap.Cores = trimCores(snap.Cores)
	c.prevCores = cur
	c.prevCoresTS = now
	return snap
}

func (c *LinuxCollector) collectMem() MemSnapshot {
	mem, err := readMeminfo(filepath.Join(c.procRoot, "meminfo"))
	if err != nil || mem.total <= 0 {
		return MemSnapshot{}
	}
	used := mem.total - mem.available
	return MemSnapshot{
		TotalMB:     round2(mem.total / 1024),
		UsedMB:      round2(used / 1024),
		AvailableMB: round2(mem.available / 1024),
		CachedMB:    round2((mem.cached + mem.buffers) / 1024),
		SwapTotalMB: round2(mem.swapTotal / 1024),
		SwapUsedMB:  round2((mem.swapTotal - mem.swapFree) / 1024),
		UsedPct:     round2(used / mem.total * 100),
	}
}

// collectNetIfaces 按网卡做差分；速率包含 lo，汇总不含 lo
func (c *LinuxCollector) collectNetIfaces(now time.Time) NetSnapshot {
	raw, err := os.ReadFile(filepath.Join(c.procRoot, "net", "dev"))
	if err != nil {
		return NetSnapshot{}
	}
	cur := parseNetDev(string(raw))
	dt := 0.0
	if !c.prevIfaceTS.IsZero() {
		dt = now.Sub(c.prevIfaceTS).Seconds()
	}
	snap := NetSnapshot{}
	for name, v := range cur {
		item := NetIface{
			Name: name, RxErrors: v.rxErr, TxErrors: v.txErr,
			RxDrops: v.rxDrop, TxDrops: v.txDrop,
		}
		if prev, ok := c.prevIface[name]; ok && dt > 0 {
			item.RxBytesS = roundIO(float64(counterDiff(v.rx, prev.rx)) / dt)
			item.TxBytesS = roundIO(float64(counterDiff(v.tx, prev.tx)) / dt)
			item.RxPacketsS = roundIO(float64(counterDiff(v.rxPkt, prev.rxPkt)) / dt)
			item.TxPacketsS = roundIO(float64(counterDiff(v.txPkt, prev.txPkt)) / dt)
		}
		if name != "lo" {
			snap.RxBytesS += item.RxBytesS
			snap.TxBytesS += item.TxBytesS
		}
		snap.Interfaces = append(snap.Interfaces, item)
	}
	snap.RxBytesS = roundIO(snap.RxBytesS)
	snap.TxBytesS = roundIO(snap.TxBytesS)
	sortIfaces(snap.Interfaces)
	snap.Interfaces = trimIfaces(snap.Interfaces)
	c.prevIface = cur
	c.prevIfaceTS = now
	return snap
}

func (c *LinuxCollector) collectLoad(procs []IOProcess) LoadSnapshot {
	var l LoadSnapshot
	if raw, err := os.ReadFile(filepath.Join(c.procRoot, "loadavg")); err == nil {
		l.Load1, l.Load5, l.Load15, l.Running, l.TotalThreads = parseLoadavgDetail(string(raw))
	}
	for _, p := range procs {
		if p.State == "D" {
			l.Blocked++
		}
	}
	return l
}

// wholeDisk 只保留整块磁盘：/sys/block 下不包含分区（sda1 等）
func (c *LinuxCollector) wholeDisk(name string) bool {
	if _, err := os.Stat(filepath.Join(c.sysRoot, "block")); err == nil {
		_, err := os.Stat(filepath.Join(c.sysRoot, "block", name))
		return err == nil
	}
	return true // 没有 /sys 时只能依赖 shouldSkipDisk 前缀过滤
}

func shouldSkipDisk(name string) bool {
	for _, p := range []string{"loop", "ram", "zram", "fd", "sr"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// collectProcs 为每个进程算差分速率；返回明细与进程总数。
// 统计总数不受 maxProcScan 限制，避免 proc_count 失真。
func (c *LinuxCollector) collectProcs(now time.Time) ([]IOProcess, int) {
	entries, err := os.ReadDir(c.procRoot)
	if err != nil {
		return nil, 0
	}
	dt := 0.0
	if !c.prevProcTS.IsZero() {
		dt = now.Sub(c.prevProcTS).Seconds()
	}
	pageSize := float64(os.Getpagesize())
	const clkTck = 100.0 // Linux 用户态 CLK_TCK 通常固定 100
	next := make(map[int]procCtr, len(c.prevProc))
	out := make([]IOProcess, 0, 256)
	count := 0

	for _, e := range entries {
		entryName := e.Name()
		if len(entryName) == 0 || entryName[0] < '0' || entryName[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(entryName)
		if err != nil {
			continue
		}
		count++
		if len(next) >= maxProcScan {
			continue
		}
		dir := filepath.Join(c.procRoot, entryName)
		statRaw, err := os.ReadFile(filepath.Join(dir, "stat"))
		if err != nil {
			continue
		}
		ps, err := parseProcStat(string(statRaw))
		if err != nil {
			continue
		}
		var pio procCtr
		if raw, err := os.ReadFile(filepath.Join(dir, "io")); err == nil {
			pio = parseProcIO(string(raw))
		}
		var rss uint64
		if raw, err := os.ReadFile(filepath.Join(dir, "statm")); err == nil {
			rss = parseStatmRSS(string(raw))
		}
		cur := procCtr{readBytes: pio.readBytes, writeBytes: pio.writeBytes, cpuTicks: ps.cpuTicks}
		next[pid] = cur

		p := IOProcess{
			PID: pid, Name: sanitizeProcName(ps.name), State: ps.state,
			RSSMB: round2(float64(rss) * pageSize / 1024 / 1024),
		}
		if prev, ok := c.prevProc[pid]; ok && dt > 0 {
			p.ReadBytesS = roundIO(float64(counterDiff(cur.readBytes, prev.readBytes)) / dt)
			p.WriteBytesS = roundIO(float64(counterDiff(cur.writeBytes, prev.writeBytes)) / dt)
			p.CPUPct = round2(float64(counterDiff(cur.cpuTicks, prev.cpuTicks)) / clkTck / dt * 100)
		}
		out = append(out, p)
	}
	c.prevProc = next
	c.prevProcTS = now
	return out, count
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
	total, available, cached, buffers, swapTotal, swapFree float64
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
		case "Cached":
			m.cached = v
		case "Buffers":
			m.buffers = v
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

// parseCPUCores 解析每个核心（cpu0…）的累计时间；不含汇总行
func parseCPUCores(s string) map[string]cpuStat {
	out := map[string]cpuStat{}
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 6 || f[0] == "cpu" {
			continue
		}
		v := make([]float64, 0, 8)
		ok := true
		for i := 1; i < len(f) && len(v) < 8; i++ {
			x, err := strconv.ParseFloat(f[i], 64)
			if err != nil {
				ok = false
				break
			}
			v = append(v, x)
		}
		if !ok || len(v) < 5 {
			continue
		}
		var total float64
		for _, x := range v {
			total += x
		}
		out[f[0]] = cpuStat{total: total, idle: v[3], iowait: v[4]}
	}
	return out
}

func parseLoadavgDetail(s string) (l1, l5, l15 float64, running, total int) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	if len(f) >= 4 {
		if a, b, ok := strings.Cut(f[3], "/"); ok {
			running, _ = strconv.Atoi(a)
			total, _ = strconv.Atoi(b)
		}
	}
	return
}

type netCtr struct {
	rx, tx         uint64
	rxPkt, txPkt   uint64
	rxErr, txErr   uint64
	rxDrop, txDrop uint64
}

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
		rxPkt, _ := strconv.ParseUint(f[1], 10, 64)
		rxErr, _ := strconv.ParseUint(f[2], 10, 64)
		rxDrop, _ := strconv.ParseUint(f[3], 10, 64)
		txPkt, _ := strconv.ParseUint(f[9], 10, 64)
		txErr, _ := strconv.ParseUint(f[10], 10, 64)
		txDrop, _ := strconv.ParseUint(f[11], 10, 64)
		out[name] = netCtr{
			rx: rx, tx: tx, rxPkt: rxPkt, txPkt: txPkt,
			rxErr: rxErr, txErr: txErr, rxDrop: rxDrop, txDrop: txDrop,
		}
	}
	return out
}

// ---------- IO 明细解析 ----------

type diskCtr struct {
	reads, writes             uint64
	sectorsRead, sectorsWrite uint64
	msIO, weightedMS          uint64
}

type procStatInfo struct {
	name     string
	state    string
	cpuTicks uint64
}

// parseDiskstats 解析 /proc/diskstats；字段下标见内核 Documentation/iostats.rst
func parseDiskstats(s string) map[string]diskCtr {
	out := map[string]diskCtr{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 14 {
			continue
		}
		name := f[2]
		reads, e1 := strconv.ParseUint(f[3], 10, 64)
		sectorsRead, e2 := strconv.ParseUint(f[5], 10, 64)
		writes, e3 := strconv.ParseUint(f[7], 10, 64)
		sectorsWrite, e4 := strconv.ParseUint(f[9], 10, 64)
		msIO, e5 := strconv.ParseUint(f[12], 10, 64)
		weightedMS, e6 := strconv.ParseUint(f[13], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || e5 != nil || e6 != nil {
			continue
		}
		out[name] = diskCtr{
			reads: reads, writes: writes,
			sectorsRead: sectorsRead, sectorsWrite: sectorsWrite,
			msIO: msIO, weightedMS: weightedMS,
		}
	}
	return out
}

// parseProcStat 解析 /proc/PID/stat：comm 可能含空格，从最后一个 ')' 之后切分
func parseProcStat(s string) (procStatInfo, error) {
	l := strings.IndexByte(s, '(')
	r := strings.LastIndexByte(s, ')')
	if l < 0 || r <= l {
		return procStatInfo{}, fmt.Errorf("bad stat")
	}
	f := strings.Fields(s[r+1:])
	// 剩余字段从 state 开始：utime=11, stime=12（0 基）
	if len(f) < 13 {
		return procStatInfo{}, fmt.Errorf("short stat")
	}
	utime, err1 := strconv.ParseUint(f[11], 10, 64)
	stime, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return procStatInfo{}, fmt.Errorf("bad stat cpu")
	}
	return procStatInfo{name: s[l+1 : r], state: f[0], cpuTicks: utime + stime}, nil
}

// parseProcIO 解析 /proc/PID/io 的 read_bytes / write_bytes
func parseProcIO(s string) procCtr {
	var c procCtr
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "read_bytes:":
			c.readBytes = v
		case "write_bytes:":
			c.writeBytes = v
		}
	}
	return c
}

// parseStatmRSS 取 /proc/PID/statm 的 resident 页数（第 2 个字段）
func parseStatmRSS(s string) uint64 {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// counterDiff 计数器回绕或设备/进程重置时按 0 处理

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
