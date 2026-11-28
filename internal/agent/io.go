package agent

import (
	"sort"
	"strings"
	"time"
)

// DetailSnapshot 是 Agent 低频上报的系统明细快照，只保留最新值（不进窄表）。
// 速率单位统一为「字节/秒」和「次/秒」，与 net_*_bps（bit/s）区分开。
type DetailSnapshot struct {
	IO    IOSnapshot   `json:"io"`
	CPU   CPUSnapshot  `json:"cpu"`
	Mem   MemSnapshot  `json:"mem"`
	Net   NetSnapshot  `json:"net"`
	Load  LoadSnapshot `json:"load"`
	Procs []IOProcess  `json:"procs,omitempty"`
}

// IOSnapshot 磁盘设备 IO 摘要
type IOSnapshot struct {
	Summary IOSummary  `json:"summary"`
	Devices []IODevice `json:"devices,omitempty"`
}

// IOSummary 聚合值，写入 metric_samples 供趋势图使用
type IOSummary struct {
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	UtilPct     float64 `json:"util_pct"` // 最忙设备的忙碌度
	ProcCount   int     `json:"proc_count"`
}

// IODevice 单块磁盘（不含分区）的实时速率
type IODevice struct {
	Name        string  `json:"name"`
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	UtilPct     float64 `json:"util_pct"`
	AwaitMS     float64 `json:"await_ms"`
	Queue       float64 `json:"queue"`
}

// CPUSnapshot 总体 + 每核使用率
type CPUSnapshot struct {
	UsagePct  float64   `json:"usage_pct"`
	IowaitPct float64   `json:"iowait_pct"`
	Cores     []CPUCore `json:"cores,omitempty"`
}

type CPUCore struct {
	Name      string  `json:"name"`
	UsagePct  float64 `json:"usage_pct"`
	IowaitPct float64 `json:"iowait_pct"`
}

// MemSnapshot 内存/交换明细，单位 MB
type MemSnapshot struct {
	TotalMB     float64 `json:"total_mb"`
	UsedMB      float64 `json:"used_mb"`
	AvailableMB float64 `json:"available_mb"`
	CachedMB    float64 `json:"cached_mb"`
	SwapTotalMB float64 `json:"swap_total_mb"`
	SwapUsedMB  float64 `json:"swap_used_mb"`
	UsedPct     float64 `json:"used_pct"`
}

// NetSnapshot 每网卡速率
type NetSnapshot struct {
	RxBytesS   float64    `json:"rx_bytes_s"`
	TxBytesS   float64    `json:"tx_bytes_s"`
	Interfaces []NetIface `json:"interfaces,omitempty"`
}

type NetIface struct {
	Name       string  `json:"name"`
	RxBytesS   float64 `json:"rx_bytes_s"`
	TxBytesS   float64 `json:"tx_bytes_s"`
	RxPacketsS float64 `json:"rx_packets_s"`
	TxPacketsS float64 `json:"tx_packets_s"`
	RxErrors   uint64  `json:"rx_errors"`
	TxErrors   uint64  `json:"tx_errors"`
	RxDrops    uint64  `json:"rx_drops"`
	TxDrops    uint64  `json:"tx_drops"`
}

// LoadSnapshot 负载与运行/阻塞进程
type LoadSnapshot struct {
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
	Running      int     `json:"running"`
	TotalThreads int     `json:"total_threads"`
	Blocked      int     `json:"blocked"`
}

// IOProcess 单个进程的 IO / CPU / 内存
type IOProcess struct {
	PID         int     `json:"pid"`
	Name        string  `json:"name"`
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	CPUPct      float64 `json:"cpu_pct"`
	RSSMB       float64 `json:"rss_mb"`
	State       string  `json:"state,omitempty"`
}

// DetailReporter 可选能力：Collect 只产出窄表指标，系统明细走独立快照。
type DetailReporter interface {
	CollectDetail(now time.Time) *DetailSnapshot
}

const (
	maxDetailDevices  = 12
	maxDetailProcsIO  = 12
	maxDetailProcsCPU = 5
	maxDetailProcsMem = 5
	maxDetailCores    = 64
	maxDetailIfaces   = 24
	maxProcScan       = 2048
	// detailInterval 明细采集间隔：比指标采样稀疏，兼顾弱机 CPU 预算
	detailInterval = 60 * time.Second
)

// procCtr 进程 IO/CPU 的上一轮计数器，跨平台共用
type procCtr struct {
	readBytes, writeBytes uint64
	cpuTicks              uint64
}

func roundIO(v float64) float64 {
	if v < 0 {
		v = 0
	}
	return round2(v)
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return round2(v)
}

// sanitizeProcName 去掉控制字符并截断，避免异常进程名破坏面板或撑大载荷
func sanitizeProcName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "?"
	}
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		if b.Len()+len(string(r)) > 48 {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return "?"
	}
	return b.String()
}

// topProcs 取 IO 最高的若干进程，再补 CPU、内存最高的几个，去重后按 IO 降序返回。
// 首次采样所有速率为 0 时退化为按 RSS 排序，面板不会一片空白。
func topProcs(all []IOProcess, maxIO, maxCPU, maxMem int) []IOProcess {
	if len(all) == 0 {
		return nil
	}
	seen := make(map[int]bool, maxIO+maxCPU+maxMem)
	out := make([]IOProcess, 0, maxIO+maxCPU+maxMem)

	byIO := append([]IOProcess(nil), all...)
	ioKey := func(p IOProcess) float64 { return p.ReadBytesS + p.WriteBytesS }
	hasRate := false
	for _, p := range byIO {
		if ioKey(p) > 0 || p.CPUPct > 0 {
			hasRate = true
			break
		}
	}
	sort.Slice(byIO, func(i, j int) bool {
		if hasRate {
			if a, b := ioKey(byIO[i]), ioKey(byIO[j]); a != b {
				return a > b
			}
			return byIO[i].CPUPct > byIO[j].CPUPct
		}
		return byIO[i].RSSMB > byIO[j].RSSMB
	})
	for _, p := range byIO {
		if len(out) >= maxIO {
			break
		}
		if !hasRate && p.RSSMB <= 0 {
			continue
		}
		if seen[p.PID] {
			continue
		}
		seen[p.PID] = true
		out = append(out, p)
	}

	add := func(list []IOProcess, max int, keep func(IOProcess) bool) {
		added := 0
		for _, p := range list {
			if added >= max || !keep(p) {
				break
			}
			if seen[p.PID] {
				continue
			}
			seen[p.PID] = true
			out = append(out, p)
			added++
		}
	}
	byCPU := append([]IOProcess(nil), all...)
	sort.Slice(byCPU, func(i, j int) bool { return byCPU[i].CPUPct > byCPU[j].CPUPct })
	add(byCPU, maxCPU, func(p IOProcess) bool { return p.CPUPct > 0 })

	byMem := append([]IOProcess(nil), all...)
	sort.Slice(byMem, func(i, j int) bool { return byMem[i].RSSMB > byMem[j].RSSMB })
	add(byMem, maxMem, func(p IOProcess) bool { return p.RSSMB > 0 })

	return out
}

// summarizeDevices 汇总所有候选设备（未截断），再按吞吐降序返回展示列表
func summarizeDevices(devs []IODevice) (IOSummary, []IODevice) {
	var s IOSummary
	for _, d := range devs {
		s.ReadBytesS += d.ReadBytesS
		s.WriteBytesS += d.WriteBytesS
		s.ReadIOPS += d.ReadIOPS
		s.WriteIOPS += d.WriteIOPS
		if d.UtilPct > s.UtilPct {
			s.UtilPct = d.UtilPct
		}
	}
	s.ReadBytesS = roundIO(s.ReadBytesS)
	s.WriteBytesS = roundIO(s.WriteBytesS)
	s.ReadIOPS = roundIO(s.ReadIOPS)
	s.WriteIOPS = roundIO(s.WriteIOPS)
	sort.Slice(devs, func(i, j int) bool {
		a := devs[i].ReadBytesS + devs[i].WriteBytesS
		b := devs[j].ReadBytesS + devs[j].WriteBytesS
		if a != b {
			return a > b
		}
		return devs[i].Name < devs[j].Name
	})
	return s, devs
}

// counterDiff 计数器回绕或设备/进程重置时按 0 处理
func counterDiff(cur, prev uint64) uint64 {
	if cur >= prev {
		return cur - prev
	}
	return 0
}

func trimDevices(devs []IODevice) []IODevice {
	if len(devs) > maxDetailDevices {
		return devs[:maxDetailDevices]
	}
	return devs
}

func trimCores(cores []CPUCore) []CPUCore {
	if len(cores) > maxDetailCores {
		return cores[:maxDetailCores]
	}
	return cores
}

func trimIfaces(ifs []NetIface) []NetIface {
	if len(ifs) > maxDetailIfaces {
		return ifs[:maxDetailIfaces]
	}
	return ifs
}

func sortCores(cores []CPUCore) {
	sort.Slice(cores, func(i, j int) bool { return cores[i].UsagePct > cores[j].UsagePct })
}

func sortIfaces(ifs []NetIface) {
	sort.Slice(ifs, func(i, j int) bool {
		a := ifs[i].RxBytesS + ifs[i].TxBytesS
		b := ifs[j].RxBytesS + ifs[j].TxBytesS
		if a != b {
			return a > b
		}
		return ifs[i].Name < ifs[j].Name
	})
}
