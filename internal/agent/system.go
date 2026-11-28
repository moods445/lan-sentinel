//go:build darwin || windows

package agent

import (
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// systemCollector 用于 macOS / Windows：走各自系统 API，不读 /proc
type systemCollector struct {
	mounts []string
	kernel string

	prevCPU   *cpu.TimesStat
	prevRx    float64
	prevTx    float64
	prevNetTS time.Time
	haveNet   bool

	prevDisk    map[string]sysDiskCtr
	prevDiskTS  time.Time
	prevProc    map[int]procCtr
	prevProcTS  time.Time
	prevCores   map[string]cpu.TimesStat
	prevCoresTS time.Time
	prevIface   map[string]sysNetCtr
	prevIfaceTS time.Time
}

func newSystemCollector(mounts []string) *systemCollector {
	if len(mounts) == 0 {
		mounts = []string{"/"}
	}
	if len(mounts) > 3 {
		mounts = mounts[:3]
	}
	k, _ := host.KernelVersion()
	if len(k) > 120 {
		k = k[:120]
	}
	return &systemCollector{mounts: mounts, kernel: strings.TrimSpace(k)}
}

// SetMounts 策略热更新挂载点
func (c *systemCollector) SetMounts(mounts []string) { c.mounts = normalizeMounts(mounts) }

func (c *systemCollector) Meta() Meta {
	name, _ := os.Hostname()
	return Meta{OS: runtime.GOOS, Arch: runtime.GOARCH, Kernel: c.kernel, Hostname: name, Agent: agentVersion}
}

func (c *systemCollector) Collect(now time.Time) (map[string]float64, error) {
	m := map[string]float64{}

	if ts, err := cpu.Times(false); err == nil && len(ts) > 0 {
		cur := ts[0]
		if c.prevCPU != nil {
			total := cpuTotal(cur) - cpuTotal(*c.prevCPU)
			if total > 0 {
				idle := cur.Idle - c.prevCPU.Idle
				iowait := cur.Iowait - c.prevCPU.Iowait
				busy := total - idle - iowait
				if busy < 0 {
					busy = 0
				}
				m["cpu_pct"] = round2(busy / total * 100)
				m["cpu_iowait_pct"] = round2(iowait / total * 100)
			}
		}
		c.prevCPU = &cur
	}

	if vm, err := mem.VirtualMemory(); err == nil && vm.Total > 0 {
		m["mem_pct"] = round2(vm.UsedPercent)
		m["mem_used_gb"] = round2(float64(vm.Used) / 1024 / 1024 / 1024)
	}
	if sw, err := mem.SwapMemory(); err == nil {
		if sw.Total > 0 {
			m["swap_pct"] = round2(sw.UsedPercent)
		} else {
			m["swap_pct"] = 0
		}
	}
	if runtime.GOOS == "darwin" {
		if l, err := load.Avg(); err == nil {
			m["load1"], m["load5"], m["load15"] = round2(l.Load1), round2(l.Load5), round2(l.Load15)
		}
	}

	for _, mp := range c.mounts {
		u, err := disk.Usage(mp)
		if err != nil || u == nil {
			continue
		}
		label := diskMetricLabel(mp)
		m["disk_used_pct:"+label] = round2(u.UsedPercent)
		m["disk_free_gb:"+label] = round2(float64(u.Free) / 1024 / 1024 / 1024)
	}

	if cs, err := gnet.IOCounters(true); err == nil {
		var rx, tx float64
		for _, v := range cs {
			if isLoopback(v.Name) {
				continue
			}
			rx += float64(v.BytesRecv)
			tx += float64(v.BytesSent)
		}
		if c.haveNet {
			dt := now.Sub(c.prevNetTS).Seconds()
			if dt > 0 {
				m["net_rx_bps"] = round2(bps(rx, c.prevRx, dt))
				m["net_tx_bps"] = round2(bps(tx, c.prevTx, dt))
			}
		}
		c.prevRx, c.prevTx, c.prevNetTS, c.haveNet = rx, tx, now, true
	}

	if up, err := host.Uptime(); err == nil {
		m["uptime_s"] = float64(up)
	}
	if pids, err := process.Pids(); err == nil {
		m["proc_count"] = float64(len(pids))
	}
	return m, nil
}

func cpuTotal(t cpu.TimesStat) float64 {
	return t.User + t.System + t.Idle + t.Nice + t.Iowait + t.Irq + t.Softirq + t.Steal + t.Guest + t.GuestNice
}

// sysDiskCtr 保存 gopsutil 计数器，用于跨采样差分
type sysDiskCtr struct {
	readBytes, writeBytes uint64
	readCount, writeCount uint64
	ioTime, weightedIO    uint64
}

type sysNetCtr struct {
	rx, tx, rxPkt, txPkt         uint64
	rxErr, txErr, rxDrop, txDrop uint64
}

// CollectDetail 用 gopsutil 采集 CPU/内存/网络/负载与磁盘、进程明细。
// 进程 IO 在 macOS 上不可用时留 0。
func (c *systemCollector) CollectDetail(now time.Time) *DetailSnapshot {
	snap := &DetailSnapshot{}

	if cs, err := disk.IOCounters(); err == nil {
		dt := 0.0
		if !c.prevDiskTS.IsZero() {
			dt = now.Sub(c.prevDiskTS).Seconds()
		}
		cur := make(map[string]sysDiskCtr, len(cs))
		devs := make([]IODevice, 0, len(cs))
		for name, v := range cs {
			cur[name] = sysDiskCtr{
				readBytes: v.ReadBytes, writeBytes: v.WriteBytes,
				readCount: v.ReadCount, writeCount: v.WriteCount,
				ioTime: v.IoTime, weightedIO: v.WeightedIO,
			}
			d := IODevice{Name: name}
			if prev, ok := c.prevDisk[name]; ok && dt > 0 {
				dr := counterDiff(v.ReadCount, prev.readCount)
				dw := counterDiff(v.WriteCount, prev.writeCount)
				mio := counterDiff(v.IoTime, prev.ioTime)
				wmio := counterDiff(v.WeightedIO, prev.weightedIO)
				d.ReadBytesS = roundIO(float64(counterDiff(v.ReadBytes, prev.readBytes)) / dt)
				d.WriteBytesS = roundIO(float64(counterDiff(v.WriteBytes, prev.writeBytes)) / dt)
				d.ReadIOPS = roundIO(float64(dr) / dt)
				d.WriteIOPS = roundIO(float64(dw) / dt)
				d.UtilPct = clampPct(float64(mio) / dt / 10)
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

	procs, count := c.collectProcs(now)
	snap.IO.Summary.ProcCount = count
	snap.Procs = topProcs(procs, maxDetailProcsIO, maxDetailProcsCPU, maxDetailProcsMem)
	snap.Load = c.collectLoad(procs)
	return snap
}

func (c *systemCollector) collectCPUCores(now time.Time) CPUSnapshot {
	ts, err := cpu.Times(true)
	if err != nil {
		return CPUSnapshot{}
	}
	dt := 0.0
	if !c.prevCoresTS.IsZero() {
		dt = now.Sub(c.prevCoresTS).Seconds()
	}
	cur := make(map[string]cpu.TimesStat, len(ts))
	snap := CPUSnapshot{}
	var total, busy, wait float64
	for _, t := range ts {
		cur[t.CPU] = t
		core := CPUCore{Name: t.CPU}
		if prev, ok := c.prevCores[t.CPU]; ok && dt > 0 {
			dTotal := cpuTotal(t) - cpuTotal(prev)
			if dTotal > 0 {
				diowait := t.Iowait - prev.Iowait
				busyDelta := dTotal - (t.Idle - prev.Idle) - diowait
				core.UsagePct = clampPct(busyDelta / dTotal * 100)
				core.IowaitPct = clampPct(diowait / dTotal * 100)
				total += dTotal
				busy += busyDelta
				wait += diowait
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

func (c *systemCollector) collectMem() MemSnapshot {
	vm, err := mem.VirtualMemory()
	if err != nil || vm == nil || vm.Total == 0 {
		return MemSnapshot{}
	}
	used := float64(vm.Total) - float64(vm.Available)
	out := MemSnapshot{
		TotalMB:     round2(float64(vm.Total) / 1024 / 1024),
		UsedMB:      round2(used / 1024 / 1024),
		AvailableMB: round2(float64(vm.Available) / 1024 / 1024),
		CachedMB:    round2(float64(vm.Cached+vm.Buffers) / 1024 / 1024),
		UsedPct:     round2(used / float64(vm.Total) * 100),
	}
	if sw, err := mem.SwapMemory(); err == nil && sw != nil {
		out.SwapTotalMB = round2(float64(sw.Total) / 1024 / 1024)
		out.SwapUsedMB = round2(float64(sw.Used) / 1024 / 1024)
	}
	return out
}

func (c *systemCollector) collectNetIfaces(now time.Time) NetSnapshot {
	cs, err := gnet.IOCounters(true)
	if err != nil {
		return NetSnapshot{}
	}
	dt := 0.0
	if !c.prevIfaceTS.IsZero() {
		dt = now.Sub(c.prevIfaceTS).Seconds()
	}
	cur := make(map[string]sysNetCtr, len(cs))
	snap := NetSnapshot{}
	for _, v := range cs {
		cur[v.Name] = sysNetCtr{
			rx: v.BytesRecv, tx: v.BytesSent,
			rxPkt: v.PacketsRecv, txPkt: v.PacketsSent,
			rxErr: v.Errin, txErr: v.Errout, rxDrop: v.Dropin, txDrop: v.Dropout,
		}
		item := NetIface{
			Name: v.Name, RxErrors: v.Errin, TxErrors: v.Errout,
			RxDrops: v.Dropin, TxDrops: v.Dropout,
		}
		if prev, ok := c.prevIface[v.Name]; ok && dt > 0 {
			item.RxBytesS = roundIO(float64(counterDiff(v.BytesRecv, prev.rx)) / dt)
			item.TxBytesS = roundIO(float64(counterDiff(v.BytesSent, prev.tx)) / dt)
			item.RxPacketsS = roundIO(float64(counterDiff(v.PacketsRecv, prev.rxPkt)) / dt)
			item.TxPacketsS = roundIO(float64(counterDiff(v.PacketsSent, prev.txPkt)) / dt)
		}
		if v.Name != "lo0" && v.Name != "lo" {
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

func (c *systemCollector) collectLoad(procs []IOProcess) LoadSnapshot {
	var l LoadSnapshot
	if runtime.GOOS == "darwin" {
		if avg, err := load.Avg(); err == nil {
			l.Load1, l.Load5, l.Load15 = avg.Load1, avg.Load5, avg.Load15
		}
	}
	for _, p := range procs {
		if p.State == "D" {
			l.Blocked++
		}
	}
	return l
}

func (c *systemCollector) collectProcs(now time.Time) ([]IOProcess, int) {
	ps, err := process.Processes()
	if err != nil {
		return nil, 0
	}
	dt := 0.0
	if !c.prevProcTS.IsZero() {
		dt = now.Sub(c.prevProcTS).Seconds()
	}
	total := len(ps)
	if len(ps) > maxProcScan {
		ps = ps[:maxProcScan]
	}
	next := make(map[int]procCtr, len(ps))
	out := make([]IOProcess, 0, len(ps))

	for _, p := range ps {
		name, err := p.Name()
		if err != nil {
			continue
		}
		cur := procCtr{}
		if pio, err := p.IOCounters(); err == nil && pio != nil {
			cur.readBytes, cur.writeBytes = pio.ReadBytes, pio.WriteBytes
		}
		if t, err := p.Times(); err == nil && t != nil {
			// CPU 秒换算成与 Linux 一致的 100 tick/秒
			cur.cpuTicks = uint64((t.User + t.System) * 100)
		}
		var rss uint64
		if mi, err := p.MemoryInfo(); err == nil && mi != nil {
			rss = mi.RSS
		}
		state := ""
		if st, err := p.Status(); err == nil && len(st) > 0 {
			state = st[0]
		}
		pid := int(p.Pid)
		next[pid] = cur

		item := IOProcess{
			PID: pid, Name: sanitizeProcName(name), State: state,
			RSSMB: round2(float64(rss) / 1024 / 1024),
		}
		if prev, ok := c.prevProc[pid]; ok && dt > 0 {
			item.ReadBytesS = roundIO(float64(counterDiff(cur.readBytes, prev.readBytes)) / dt)
			item.WriteBytesS = roundIO(float64(counterDiff(cur.writeBytes, prev.writeBytes)) / dt)
			item.CPUPct = round2(float64(counterDiff(cur.cpuTicks, prev.cpuTicks)) / 100 / dt * 100)
		}
		out = append(out, item)
	}
	c.prevProc = next
	c.prevProcTS = now
	return out, total
}

func isLoopback(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "lo") || strings.Contains(n, "loopback")
}
