package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

type Client struct {
	cfg   Config
	coll  Collector
	log   *slog.Logger
	hc    *http.Client
	spool *Spool
	meta  Meta

	pending   [][]byte
	persisted int

	interval  time.Duration
	throttled bool
	over      int

	pol        Policy
	polPath    string
	applyMount func([]string)

	tick       int
	prevSelf   float64
	prevSelfTS time.Time
	failStreak int

	zbuf bytes.Buffer
	zw   *gzip.Writer
}

func NewClient(cfg Config, coll Collector, log *slog.Logger) *Client {
	tr := &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: cfg.Insecure},
		MaxIdleConns:       2,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
	}
	c := &Client{
		cfg:      cfg,
		coll:     coll,
		log:      log,
		hc:       &http.Client{Timeout: 10 * time.Second, Transport: tr},
		spool:    NewSpool(cfg.SpoolPath()),
		meta:     coll.Meta(),
		interval: cfg.Interval(),
		polPath:  policyFilePath(cfg.DataDir),
	}
	if ms, ok := coll.(MountSetter); ok {
		c.applyMount = ms.SetMounts
	}
	c.zw, _ = gzip.NewWriterLevel(&c.zbuf, gzip.BestSpeed)
	return c
}

type syncPayload struct {
	Host struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		OS   string         `json:"os"`
		Arch string         `json:"arch"`
		Kind string         `json:"kind"`
		Meta map[string]any `json:"meta"`
	} `json:"host"`
	TS      time.Time          `json:"ts"`
	Metrics map[string]float64 `json:"metrics"`
	Self    struct {
		IntervalS int     `json:"interval_s"`
		Throttled bool    `json:"throttled"`
		CPU       float64 `json:"cpu_pct"`
	} `json:"self"`
}

type syncResponse struct {
	OK        bool   `json:"ok"`
	IntervalS int    `json:"interval_s,omitempty"`
	Tasks     []any  `json:"tasks"`
	Policy    Policy `json:"policy"`
}

func (c *Client) Step(ctx context.Context, forceCollect bool) error {
	now := time.Now()
	metrics := map[string]float64{}
	if forceCollect || c.tick%2 == 0 {
		m, err := c.coll.Collect(now)
		if err != nil {
			c.log.Warn("collect failed", "err", err)
		} else {
			metrics = filterMetrics(m, c.pol)
		}
	}
	c.tick++

	var p syncPayload
	p.Host.ID = c.cfg.HostID
	p.Host.Name = c.meta.Hostname
	p.Host.OS = c.meta.OS
	p.Host.Arch = c.meta.Arch
	p.Host.Kind = "agent"
	p.Host.Meta = map[string]any{"kernel": c.meta.Kernel, "agent": c.meta.Agent}
	p.TS = now
	p.Metrics = metrics
	p.Self.IntervalS = int(c.interval / time.Second)
	p.Self.Throttled = c.throttled

	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	c.pending = append(c.pending, body)
	if len(c.pending) > 200 {
		c.pending = c.pending[len(c.pending)-200:]
		c.persisted = 0
	}

	err = c.flush(ctx)
	c.measureSelf(now)
	if err == nil {
		c.failStreak = 0
	}
	return err
}

func (c *Client) Run(ctx context.Context) error {
	if lines, err := c.spool.Load(); err == nil && len(lines) > 0 {
		c.pending = lines
		c.persisted = len(lines)
		c.log.Info("spool recovered", "batches", len(lines))
	}
	// 断网重启沿用上次下发的策略
	if p, err := loadPolicyFile(c.polPath); err == nil && p.Rev != "" {
		c.applyPolicy(p, false)
		c.log.Info("policy restored", "rev", p.Rev)
	}
	for {
		err := c.Step(ctx, c.tick == 0)
		if err != nil {
			c.failStreak++
			if c.failStreak == 1 || c.failStreak%10 == 0 {
				c.log.Warn("sync failed", "attempts", c.failStreak, "pending", len(c.pending), "err", err)
			}
		} else if c.failStreak > 0 {
			c.log.Info("sync recovered", "after_failures", c.failStreak)
		}
		t := time.NewTimer(c.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func (c *Client) flush(ctx context.Context) error {
	for len(c.pending) > 0 {
		if err := c.send(ctx, c.pending[0]); err != nil {
			c.persist()
			return err
		}
		c.pending = c.pending[1:]
	}
	// 排空后统一重写一次：逐批重写会在补传积压时放大成上百次全量落盘（弱机 CPU/IO 预算内很贵）。
	// 中途崩溃最多重发未落盘批次，服务端按 host+metric+ts 幂等。
	c.persist()
	return nil
}

// persist 只在待发批次变化时重写，断网期间不会每 15s 刷盘
func (c *Client) persist() {
	if c.persisted == len(c.pending) {
		return
	}
	if len(c.pending) == 0 {
		_ = c.spool.Clear()
		c.persisted = 0
		return
	}
	if err := c.spool.Replace(c.pending); err == nil {
		c.persisted = len(c.pending)
	}
}

func (c *Client) send(ctx context.Context, body []byte) error {
	// 复用 gzip 缓冲，避免每 15s 产生几十 KB 临时分配
	c.zbuf.Reset()
	c.zw.Reset(&c.zbuf)
	if _, err := c.zw.Write(body); err != nil {
		return err
	}
	if err := c.zw.Close(); err != nil {
		return err
	}

	url := strings.TrimRight(c.cfg.Server, "/") + "/api/v1/agent/sync"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(c.zbuf.Bytes()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	if c.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("sync status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var sr syncResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&sr); err != nil {
		return fmt.Errorf("decode sync response: %w", err)
	}
	// M2 尚未启用任务下发；有任务时这里只会打印，执行器在 M5 接入
	if len(sr.Tasks) > 0 {
		c.log.Debug("tasks received", "count", len(sr.Tasks))
	}
	if sr.Policy.Rev != "" {
		c.applyPolicy(sr.Policy, true)
	}
	return nil
}

// applyPolicy 应用控制面策略：指标过滤、上报间隔、监控挂载点
func (c *Client) applyPolicy(p Policy, persist bool) {
	changed := p.Rev != c.pol.Rev
	c.pol = p
	base := p.interval(c.cfg.Interval())
	if !c.throttled && base != c.interval {
		c.interval = base
	}
	if c.applyMount != nil {
		if len(p.Mounts) > 0 {
			c.applyMount(p.Mounts)
		} else {
			c.applyMount(c.cfg.Mounts)
		}
	}
	if !changed {
		return
	}
	c.log.Info("policy applied", "rev", p.Rev, "metrics", len(p.EnabledMetrics),
		"interval_s", int(c.interval/time.Second), "mounts", p.Mounts)
	if persist {
		if err := savePolicyFile(c.polPath, p); err != nil {
			c.log.Warn("save policy failed", "err", err)
		}
	}
}

// measureSelf 连续 3 个周期自身 CPU >0.3% 时降频到 60s（弱机保护）
func (c *Client) measureSelf(now time.Time) {
	cur, err := selfCPU()
	if err != nil {
		return
	}
	if !c.prevSelfTS.IsZero() {
		dt := now.Sub(c.prevSelfTS).Seconds()
		if dt > 0 {
			pct := (cur - c.prevSelf) / dt * 100
			if pct > 0.3 {
				c.over++
			} else {
				c.over = 0
			}
			if c.over >= 3 && !c.throttled {
				c.throttled = true
				if c.interval < 60*time.Second {
					c.interval = 60 * time.Second
				}
				c.log.Warn("self cpu over budget, interval raised to 60s", "cpu_pct", pct)
			}
		}
	}
	c.prevSelf = cur
	c.prevSelfTS = now
}

// PrintOnce 只采集一次并输出，用于安装后自检
func (c *Client) PrintOnce() error {
	// 先采一次建立 CPU/网卡基线，短暂等待后再采，输出里才有 cpu_pct / net_*_bps
	_, _ = c.coll.Collect(time.Now())
	time.Sleep(200 * time.Millisecond)
	m, err := c.coll.Collect(time.Now())
	if err != nil {
		return err
	}
	out := struct {
		Host    string             `json:"host"`
		Meta    Meta               `json:"meta"`
		Metrics map[string]float64 `json:"metrics"`
	}{c.cfg.HostID, c.meta, m}
	b, _ := json.MarshalIndent(out, "", "  ")
	os.Stdout.Write(b)
	fmt.Println()
	return nil
}
