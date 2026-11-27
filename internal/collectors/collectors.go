// Package collectors 是「装不了 Agent 的设备」的兜底拉取：probe / http / snmp。
// 约束：全局并发 ≤2、单次超时 ≤4s、失败退避、只读（GET / SNMP GET）。
package collectors

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosnmp/gosnmp"

	"github.com/home-aio/lan-sentinel/internal/config"
	"github.com/home-aio/lan-sentinel/internal/store"
)

const (
	oidSysUpTime     = "1.3.6.1.2.1.1.3.0"
	oidHrProcessor   = "1.3.6.1.2.1.25.3.3.1.2"
	oidIfOperStatus  = "1.3.6.1.2.1.2.2.1.8"
	oidIfHCInOctets  = "1.3.6.1.2.1.31.1.1.1.6"
	oidIfHCOutOctets = "1.3.6.1.2.1.31.1.1.1.10"
)

// Store 是调度器需要的存储能力；用接口声明便于单测注入假实现
type Store interface {
	Ingest(ctx context.Context, h store.HostInfo, samples []store.Sample, streamPayload map[string]any) error
	SaveCollectorState(ctx context.Context, st store.CollectorState) error
	AppendStream(ctx context.Context, typ, hostID string, payload map[string]any) error
}

// item 的可变状态分两类：
//   - next/failures 用原子量，tick 每秒扫全部 item 时不必加锁（否则会阻塞面板侧的重载/加节点）；
//   - prev 只在 runOne 内读写，靠 mu 让「定时任务」与「手动触发」串行，避免并发写 map 直接 panic。
type item struct {
	cfg      config.Collector
	next     atomic.Int64 // UnixNano
	failures atomic.Int32

	mu   sync.Mutex
	prev map[string]prevSample
}

func (it *item) nextAt() time.Time   { return time.Unix(0, it.next.Load()) }
func (it *item) setNext(t time.Time) { it.next.Store(t.UnixNano()) }
func (it *item) failCount() int      { return int(it.failures.Load()) }
func (it *item) setFailures(n int)   { it.failures.Store(int32(n)) }

type prevSample struct {
	value float64
	ts    time.Time
}

type Manager struct {
	st      Store
	log     *slog.Logger
	secrets map[string]string

	mu    sync.Mutex
	base  []config.Collector
	extra []config.Collector
	items map[string]*item
	sem   chan struct{}
}

func NewManager(st Store, cfg []config.Collector, secrets map[string]string, log *slog.Logger) *Manager {
	m := &Manager{st: st, log: log, secrets: secrets, items: map[string]*item{}, sem: make(chan struct{}, 2)}
	m.Reload(cfg)
	return m
}

// Reload 替换文件配置（collectors.yaml），保留面板动态节点。
func (m *Manager) Reload(cfg []config.Collector) {
	m.mu.Lock()
	m.base = cfg
	m.rebuildLocked()
	m.mu.Unlock()
}

// SetExtra 替换面板动态添加的节点（数据库），保留文件配置。
func (m *Manager) SetExtra(cfg []config.Collector) {
	m.mu.Lock()
	m.extra = cfg
	m.rebuildLocked()
	m.mu.Unlock()
}

func (m *Manager) rebuildLocked() {
	next := map[string]*item{}
	for _, c := range append(append([]config.Collector{}, m.base...), m.extra...) {
		it := &item{cfg: c, prev: map[string]prevSample{}} // next 零值 = 立即可跑
		if old, ok := m.items[c.HostID]; ok {
			it.failures.Store(old.failures.Load())
			it.next.Store(old.next.Load())
			// prev（SNMP 计数器基线）不跨重建继承：旧 item 可能仍在跑，读它要加锁，
			// 而在 m.mu 下等锁会堵住面板侧的重载/加节点；代价只是每个接口少一个 bps 采样点。
		}
		next[c.HostID] = it
	}
	m.items = next
}

// Run 单 goroutine 调度 + 有界并发，弱机上不会堆积
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Manager) tick(ctx context.Context) {
	now := time.Now()
	m.mu.Lock()
	var due []*item
	for _, it := range m.items {
		if !it.nextAt().After(now) {
			due = append(due, it)
		}
	}
	m.mu.Unlock()
	for _, it := range due {
		select {
		case m.sem <- struct{}{}:
		default:
			return // 并发已满，下一跳再试
		}
		it.setNext(now.Add(30 * time.Second)) // 先占位，跑完再按结果重排
		go func(it *item) {
			defer func() { <-m.sem }()
			if err := m.runOne(ctx, it); err != nil {
				m.log.Debug("collector run failed", "host", it.cfg.HostID, "err", err)
			}
		}(it)
	}
}

// RunOnce 手动触发一次拉取，用于接入调试
func (m *Manager) RunOnce(ctx context.Context, host string) error {
	m.mu.Lock()
	it := m.items[host]
	m.mu.Unlock()
	if it == nil {
		return fmt.Errorf("collector %q not configured", host)
	}
	return m.runOne(ctx, it)
}

func (m *Manager) runOne(ctx context.Context, it *item) error {
	// 串行化同一 item：定时任务与手动触发并发跑会让 rate() 里的 it.prev 并发写直接 panic
	it.mu.Lock()
	defer it.mu.Unlock()

	timeout := it.cfg.Timeout.Std()
	if timeout <= 0 || timeout > 4*time.Second {
		timeout = 4 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	samples, err := m.collect(cctx, it, timeout)
	now := time.Now()
	if err != nil {
		it.setFailures(it.failCount() + 1)
		it.setNext(now.Add(backoff(it.failCount())))
	} else {
		it.setFailures(0)
		it.setNext(now.Add(jitter(it.cfg.Interval.Std())))
	}
	failures, next := it.failCount(), it.nextAt()

	kind := it.cfg.Type
	host := store.HostInfo{ID: it.cfg.HostID, Name: nameOr(it.cfg), Kind: kind, Addr: targetOf(it.cfg)}
	if err != nil {
		m.log.Warn("collector failed", "host", host.ID, "type", kind, "failures", failures, "err", err)
		_ = m.st.SaveCollectorState(ctx, store.CollectorState{
			HostID: host.ID, Failures: failures, LastError: truncate(err.Error(), 200), NextRunAt: next,
		})
		if failures == 1 || failures%10 == 0 {
			_ = m.st.AppendStream(ctx, "collector.failed", host.ID,
				map[string]any{"type": kind, "error": truncate(err.Error(), 200), "failures": failures})
		}
		return err
	}

	okAt := now
	if err := m.st.Ingest(ctx, host, samples, map[string]any{"samples": len(samples), "collector": kind}); err != nil {
		m.log.Warn("collector ingest failed", "host", host.ID, "err", err)
		return err
	}
	_ = m.st.SaveCollectorState(ctx, store.CollectorState{
		HostID: host.ID, Failures: 0, LastOKAt: &okAt, NextRunAt: next,
	})
	return nil
}

func (m *Manager) collect(ctx context.Context, it *item, timeout time.Duration) ([]store.Sample, error) {
	now := time.Now()
	switch it.cfg.Type {
	case "probe":
		return m.collectProbe(ctx, it.cfg, timeout, now)
	case "http":
		return m.collectHTTP(ctx, it.cfg, timeout, now)
	case "snmp":
		return m.collectSNMP(ctx, it, timeout, now)
	default:
		return nil, fmt.Errorf("unknown collector type %q", it.cfg.Type)
	}
}

func (m *Manager) collectProbe(ctx context.Context, c config.Collector, timeout time.Duration, now time.Time) ([]store.Sample, error) {
	target := c.Target
	if target == "" {
		return nil, fmt.Errorf("probe target required")
	}
	if !strings.Contains(target, ":") {
		target += ":80"
	}
	start := time.Now()
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", target)
	lat := float64(time.Since(start).Microseconds()) / 1000.0
	ok := 1.0
	if err != nil {
		ok = 0
		lat = float64(timeout.Milliseconds())
	} else {
		conn.Close()
	}
	s := []store.Sample{
		{Metric: "probe_ok", TS: now, Value: ok},
		{Metric: "probe_latency_ms", TS: now, Value: round2(lat)},
	}
	_, port, _ := net.SplitHostPort(target)
	if port != "" {
		s = append(s, store.Sample{Metric: "port_open:" + port, TS: now, Value: ok})
	}
	return s, nil
}

func (m *Manager) collectHTTP(ctx context.Context, c config.Collector, timeout time.Duration, now time.Time) ([]store.Sample, error) {
	if c.URL == "" {
		return nil, fmt.Errorf("http url required")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: c.Insecure}, MaxIdleConns: 2}
	// Transport 每次调用新建，必须显式关闭空闲连接，否则每条连接都会残留到进程退出（fd 泄漏）
	defer tr.CloseIdleConnections()
	client := &http.Client{Timeout: timeout, Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	if c.BearerRef != "" {
		req.Header.Set("Authorization", "Bearer "+m.secret(c.BearerRef))
	}
	for k, ref := range c.HeaderRefs {
		req.Header.Set(k, m.secret(ref))
	}
	start := time.Now()
	resp, err := client.Do(req)
	lat := round2(float64(time.Since(start).Microseconds()) / 1000.0)
	if err != nil {
		return []store.Sample{
			{Metric: "http_ok", TS: now, Value: 0},
			{Metric: "http_latency_ms", TS: now, Value: lat},
		}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	ok := 0.0
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		ok = 1
	}
	out := []store.Sample{
		{Metric: "http_ok", TS: now, Value: ok},
		{Metric: "http_latency_ms", TS: now, Value: lat},
		{Metric: "http_status", TS: now, Value: float64(resp.StatusCode)},
	}
	if len(c.Fields) > 0 {
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			return out, fmt.Errorf("json decode: %w", err)
		}
		for metric, path := range c.Fields {
			if v, ok := lookupPath(doc, path); ok {
				out = append(out, store.Sample{Metric: metric, TS: now, Value: v})
			}
		}
		return out, nil
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		out = append(out, parsePrometheus(body, now)...)
	}
	return out, nil
}

func (m *Manager) collectSNMP(ctx context.Context, it *item, timeout time.Duration, now time.Time) ([]store.Sample, error) {
	c := it.cfg
	community := m.secret(c.CommunityRef)
	if community == "" {
		return nil, fmt.Errorf("snmp community not found (community_ref=%q)", c.CommunityRef)
	}
	host, port := splitHostPort(c.Target, 161)
	g := &gosnmp.GoSNMP{
		Target: host, Port: uint16(port), Community: community,
		Version: gosnmp.Version2c, Timeout: timeout, Retries: 0,
		Context: ctx, MaxOids: 20,
	}
	if err := g.Connect(); err != nil {
		return nil, err
	}
	defer g.Conn.Close()

	oids := []string{oidSysUpTime, oidHrProcessor + ".1"}
	for _, idx := range c.Interfaces {
		oids = append(oids,
			fmt.Sprintf("%s.%d", oidIfOperStatus, idx),
			fmt.Sprintf("%s.%d", oidIfHCInOctets, idx),
			fmt.Sprintf("%s.%d", oidIfHCOutOctets, idx))
	}
	pkt, err := g.Get(oids)
	if err != nil {
		return nil, err
	}
	var out []store.Sample
	for _, pv := range pkt.Variables {
		switch pv.Type {
		case gosnmp.NoSuchObject, gosnmp.NoSuchInstance, gosnmp.Null, gosnmp.EndOfMibView:
			continue // 设备未实现该 OID，不落 0 值污染曲线
		}
		name := strings.TrimPrefix(pv.Name, ".")
		switch {
		case name == oidSysUpTime:
			out = append(out, store.Sample{Metric: "snmp_uptime_s", TS: now, Value: toFloat(pv) / 100})
		case strings.HasPrefix(name, oidHrProcessor+"."):
			out = append(out, store.Sample{Metric: "snmp_cpu_pct", TS: now, Value: toFloat(pv)})
		case strings.HasPrefix(name, oidIfOperStatus+"."):
			idx := oidLastIndex(name)
			out = append(out, store.Sample{Metric: "if_oper_up:" + idx, TS: now, Value: toFloat(pv)})
		case strings.HasPrefix(name, oidIfHCInOctets+"."):
			idx := oidLastIndex(name)
			if v, ok := rate(it, "if_in_bps:"+idx, toFloat(pv), now); ok {
				out = append(out, store.Sample{Metric: "if_in_bps:" + idx, TS: now, Value: v})
			}
		case strings.HasPrefix(name, oidIfHCOutOctets+"."):
			idx := oidLastIndex(name)
			if v, ok := rate(it, "if_out_bps:"+idx, toFloat(pv), now); ok {
				out = append(out, store.Sample{Metric: "if_out_bps:" + idx, TS: now, Value: v})
			}
		}
	}
	return out, nil
}

// rate 把单调递增计数器换算成 bps；首次只记基线，回绕时按 0 处理
func rate(it *item, key string, counter float64, now time.Time) (float64, bool) {
	prev, ok := it.prev[key]
	it.prev[key] = prevSample{value: counter, ts: now}
	if !ok {
		return 0, false
	}
	dt := now.Sub(prev.ts).Seconds()
	if dt <= 0 {
		return 0, false
	}
	delta := counter - prev.value
	if delta < 0 { // 32/64 位回绕
		delta = 0
	}
	return round2(delta * 8 / dt), true
}

func (m *Manager) secret(ref string) string {
	if ref == "" {
		return ""
	}
	if v, ok := m.secrets[ref]; ok {
		return v
	}
	return strings.TrimSpace(os.Getenv(ref))
}

// ---------- helpers ----------

func lookupPath(doc map[string]any, path string) (float64, bool) {
	var cur any = doc
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return 0, false
			}
			cur = node[i]
		default:
			return 0, false
		}
	}
	switch v := cur.(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func parsePrometheus(body []byte, now time.Time) []store.Sample {
	var out []store.Sample
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if i := strings.IndexByte(name, '{'); i > 0 {
			name = name[:i]
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		out = append(out, store.Sample{Metric: name, TS: now, Value: v})
		if len(out) >= 50 {
			break
		}
	}
	return out
}

func toFloat(pv gosnmp.SnmpPDU) float64 {
	switch v := pv.Value.(type) {
	case uint32:
		return float64(v)
	case uint64:
		return float64(v)
	case int:
		return float64(v)
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case uint:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

func oidLastIndex(oid string) string {
	if i := strings.LastIndexByte(oid, '.'); i >= 0 {
		return oid[i+1:]
	}
	return oid
}

func splitHostPort(target string, defPort int) (string, int) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return target, defPort
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		p = defPort
	}
	return host, p
}

func targetOf(c config.Collector) string {
	if c.Target != "" {
		return c.Target
	}
	return c.URL
}

func nameOr(c config.Collector) string {
	if c.Name != "" {
		return c.Name
	}
	return c.HostID
}

func backoff(failures int) time.Duration {
	switch {
	case failures <= 1:
		return 30 * time.Second
	case failures == 2:
		return 2 * time.Minute
	default:
		return 5 * time.Minute
	}
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// ±10%，避免同一秒齐发
	return d + time.Duration((rand.Float64()*0.2-0.1)*float64(d))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
