package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/home-aio/lan-sentinel/internal/store"
)

// 与 internal/agent.DetailSnapshot 的 JSON 结构保持一致
// （控制面不依赖 agent 包，避免把平台采集代码带进 server 镜像）

type detailSnapshot struct {
	IO    ioStats     `json:"io"`
	CPU   cpuStats    `json:"cpu"`
	Mem   memStats    `json:"mem"`
	Net   netStats    `json:"net"`
	Load  loadStats   `json:"load"`
	Procs []ioProcess `json:"procs,omitempty"`
}

// ioSnapshot 旧版 Agent 的 io 字段，同时用于 GET /hosts/{id}/io 的响应
type ioSnapshot struct {
	ioStats
	Procs []ioProcess `json:"procs,omitempty"`
}

type ioStats struct {
	Summary ioSummary  `json:"summary"`
	Devices []ioDevice `json:"devices,omitempty"`
}

type ioSummary struct {
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	UtilPct     float64 `json:"util_pct"`
	ProcCount   int     `json:"proc_count"`
}

type ioDevice struct {
	Name        string  `json:"name"`
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	ReadIOPS    float64 `json:"read_iops"`
	WriteIOPS   float64 `json:"write_iops"`
	UtilPct     float64 `json:"util_pct"`
	AwaitMS     float64 `json:"await_ms"`
	Queue       float64 `json:"queue"`
}

type cpuStats struct {
	UsagePct  float64   `json:"usage_pct"`
	IowaitPct float64   `json:"iowait_pct"`
	Cores     []cpuCore `json:"cores,omitempty"`
}

type cpuCore struct {
	Name      string  `json:"name"`
	UsagePct  float64 `json:"usage_pct"`
	IowaitPct float64 `json:"iowait_pct"`
}

type memStats struct {
	TotalMB     float64 `json:"total_mb"`
	UsedMB      float64 `json:"used_mb"`
	AvailableMB float64 `json:"available_mb"`
	CachedMB    float64 `json:"cached_mb"`
	SwapTotalMB float64 `json:"swap_total_mb"`
	SwapUsedMB  float64 `json:"swap_used_mb"`
	UsedPct     float64 `json:"used_pct"`
}

type netStats struct {
	RxBytesS   float64    `json:"rx_bytes_s"`
	TxBytesS   float64    `json:"tx_bytes_s"`
	Interfaces []netIface `json:"interfaces,omitempty"`
}

type netIface struct {
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

type loadStats struct {
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
	Running      int     `json:"running"`
	TotalThreads int     `json:"total_threads"`
	Blocked      int     `json:"blocked"`
}

type ioProcess struct {
	PID         int     `json:"pid"`
	Name        string  `json:"name"`
	ReadBytesS  float64 `json:"read_bytes_s"`
	WriteBytesS float64 `json:"write_bytes_s"`
	CPUPct      float64 `json:"cpu_pct"`
	RSSMB       float64 `json:"rss_mb"`
	State       string  `json:"state,omitempty"`
}

// ioSamples 把摘要写成窄表指标，供节点详情页画 IO 趋势。
func ioSamples(stats *ioStats, ts time.Time) []store.Sample {
	if stats == nil {
		return nil
	}
	pairs := []struct {
		name string
		v    float64
	}{
		{"disk_read_bytes_s", stats.Summary.ReadBytesS},
		{"disk_write_bytes_s", stats.Summary.WriteBytesS},
		{"disk_read_iops", stats.Summary.ReadIOPS},
		{"disk_write_iops", stats.Summary.WriteIOPS},
		{"disk_util_pct", stats.Summary.UtilPct},
	}
	out := make([]store.Sample, 0, len(pairs))
	for _, p := range pairs {
		if math.IsNaN(p.v) || math.IsInf(p.v, 0) || p.v < 0 {
			continue
		}
		out = append(out, store.Sample{Metric: p.name, TS: ts, Value: round2(p.v), Src: "agent"})
	}
	return out
}

// sanitizeDetail 限制单次快照的规模，避免异常 Agent 撑大数据库与面板
func sanitizeDetail(d *detailSnapshot) {
	if d == nil {
		return
	}
	if len(d.IO.Devices) > 32 {
		d.IO.Devices = d.IO.Devices[:32]
	}
	if len(d.Procs) > 64 {
		d.Procs = d.Procs[:64]
	}
	if len(d.CPU.Cores) > 128 {
		d.CPU.Cores = d.CPU.Cores[:128]
	}
	if len(d.Net.Interfaces) > 64 {
		d.Net.Interfaces = d.Net.Interfaces[:64]
	}
	for i := range d.IO.Devices {
		d.IO.Devices[i].Name = trimName(d.IO.Devices[i].Name)
	}
	for i := range d.Procs {
		d.Procs[i].Name = trimName(d.Procs[i].Name)
	}
	for i := range d.CPU.Cores {
		d.CPU.Cores[i].Name = trimName(d.CPU.Cores[i].Name)
	}
	for i := range d.Net.Interfaces {
		d.Net.Interfaces[i].Name = trimName(d.Net.Interfaces[i].Name)
	}
}

func trimName(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// decodeDetail 兼容旧版只含 io 的载荷；新载荷顶层带 io/cpu/mem/net/load。
func decodeDetail(raw []byte) (*detailSnapshot, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["io"]; !ok {
		var old ioSnapshot
		if err := json.Unmarshal(raw, &old); err != nil {
			return nil, err
		}
		return &detailSnapshot{IO: old.ioStats, Procs: old.Procs}, nil
	}
	var d detailSnapshot
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (s *Server) handleHostDetail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, err := s.d.Store.HostDetail(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if row == nil {
		writeJSON(w, http.StatusOK, map[string]any{"host": id, "ts": nil})
		return
	}
	d, err := decodeDetail(row.Payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bad detail snapshot")
		return
	}
	sanitizeDetail(d)
	writeJSON(w, http.StatusOK, struct {
		Host string    `json:"host"`
		TS   time.Time `json:"ts"`
		detailSnapshot
	}{Host: id, TS: row.TS, detailSnapshot: *d})
}

func (s *Server) handleHostIO(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	row, err := s.d.Store.HostDetail(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if row == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"host": id, "ts": nil, "summary": ioSummary{}, "devices": []any{}, "procs": []any{},
		})
		return
	}
	d, err := decodeDetail(row.Payload)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "bad io snapshot")
		return
	}
	sanitizeDetail(d)
	writeJSON(w, http.StatusOK, struct {
		Host string    `json:"host"`
		TS   time.Time `json:"ts"`
		ioSnapshot
	}{Host: id, TS: row.TS, ioSnapshot: ioSnapshot{ioStats: d.IO, Procs: d.Procs}})
}
