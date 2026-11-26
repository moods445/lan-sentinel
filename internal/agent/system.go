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

func isLoopback(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "lo") || strings.Contains(n, "loopback")
}
