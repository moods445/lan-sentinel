// Package rules 是规则优先处置的判定内核：每 10s 求值一次，命中即开事件，
// 条件消失即关事件；每次开/关/抑制都写 decisions，便于回放「为什么这么做」。
package rules

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/home-aio/lan-sentinel/internal/config"
	"github.com/home-aio/lan-sentinel/internal/store"
)

type Store interface {
	ListHosts(ctx context.Context) ([]store.HostRow, error)
	LatestMetrics(ctx context.Context) (map[string]map[string]store.Latest, error)
	CollectorStates(ctx context.Context) (map[string]store.CollectorState, error)
	OpenEvent(ctx context.Context, d store.EventDraft) (int64, bool, error)
	ResolveEvent(ctx context.Context, fingerprint string, cooldown time.Duration, rationale string) (int64, bool, error)
	OpenFingerprints(ctx context.Context) (map[string]store.Event, error)
	CooldownUntil(ctx context.Context, fingerprint string) (time.Time, error)
	WindowMin(ctx context.Context, host, metric string, since time.Time) (float64, bool, error)
	WindowMax(ctx context.Context, host, metric string, since time.Time) (float64, bool, error)
	RecordDecision(ctx context.Context, d store.Decision) error
}

type Engine struct {
	st         Store
	log        *slog.Logger
	mu         sync.RWMutex
	rules      []config.Rule
	rev        string
	suppressed map[string]time.Time
}

func New(st Store, rules []config.Rule, rev string, log *slog.Logger) *Engine {
	return &Engine{st: st, log: log, rules: rules, rev: rev, suppressed: map[string]time.Time{}}
}

func (e *Engine) Reload(rules []config.Rule, rev string) {
	e.mu.Lock()
	e.rules = rules
	e.rev = rev
	e.mu.Unlock()
}

func (e *Engine) Rules() ([]config.Rule, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.rules, e.rev
}

func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Evaluate(ctx); err != nil && ctx.Err() == nil {
				e.log.Warn("rule evaluate failed", "err", err)
			}
		}
	}
}

// Evaluate 一次完整求值；active 记录本轮仍成立的条件指纹，用于关闭已消失的事件
func (e *Engine) Evaluate(ctx context.Context) error {
	hosts, err := e.st.ListHosts(ctx)
	if err != nil {
		return err
	}
	latest, err := e.st.LatestMetrics(ctx)
	if err != nil {
		return err
	}
	states, err := e.st.CollectorStates(ctx)
	if err != nil {
		return err
	}
	rules, rev := e.Rules()

	active := map[string]bool{}
	cooldownByRule := map[string]time.Duration{}
	now := time.Now()

	for _, rule := range rules {
		cooldownByRule[rule.ID] = rule.Cooldown.Std()
		for _, h := range hosts {
			switch rule.Type {
			case "heartbeat":
				if hitHeartbeat(h, rule, now) {
					active[fingerprint(h.ID, rule.ID, "")] = true
					if err := e.fire(ctx, h, rule, rev, "", nil, 0, "心跳超过阈值未上报"); err != nil {
						e.log.Warn("open heartbeat event failed", "host", h.ID, "err", err)
					}
				}
			case "collector_failing":
				st, ok := states[h.ID]
				need := rule.Consecutive
				if need <= 0 {
					need = 3
				}
				if ok && st.Failures >= need {
					active[fingerprint(h.ID, rule.ID, "")] = true
					if err := e.fire(ctx, h, rule, rev, "", map[string]any{"failures": st.Failures, "last_error": st.LastError}, float64(st.Failures), "拉取连续失败"); err != nil {
						e.log.Warn("open collector event failed", "host", h.ID, "err", err)
					}
				}
			case "metric_threshold":
				for _, m := range thresholdMetrics(latest[h.ID], rule) {
					sev, hit := classify(m.value, rule)
					if !hit {
						continue
					}
					if rule.For.Std() > 0 {
						since := now.Add(-rule.For.Std())
						if rule.Below {
							max, ok, err := e.windowMax(ctx, h.ID, m.key, since)
							if err != nil {
								e.log.Warn("window max failed", "host", h.ID, "metric", m.key, "err", err)
								continue
							}
							if !ok || max > rule.Warning {
								continue // 窗口内曾回升，不算持续
							}
						} else {
							min, ok, err := e.windowMin(ctx, h.ID, m.key, since)
							if err != nil {
								e.log.Warn("window min failed", "host", h.ID, "metric", m.key, "err", err)
								continue
							}
							if !ok || min < rule.Warning {
								continue // 窗口内曾回落，不算持续
							}
						}
					}
					fp := fingerprint(h.ID, rule.ID, m.scope)
					active[fp] = true
					if err := e.fireWithSeverity(ctx, h, rule, rev, m.scope, m.key, m.value, sev,
						fmt.Sprintf("%s=%s 越过 %s 阈值", m.key, trim(m.value), sev)); err != nil {
						e.log.Warn("open metric event failed", "host", h.ID, "err", err)
					}
				}
			}
		}
	}

	// 条件消失 → 关闭事件
	open, err := e.st.OpenFingerprints(ctx)
	if err != nil {
		return err
	}
	for fp, ev := range open {
		if active[fp] {
			continue
		}
		cd := cooldownByRule[ev.RuleID]
		if cd <= 0 {
			cd = 10 * time.Minute
		}
		if _, resolved, err := e.st.ResolveEvent(ctx, fp, cd, "条件已恢复"); err != nil {
			e.log.Warn("resolve event failed", "fingerprint", fp, "err", err)
		} else if resolved {
			e.log.Info("event resolved", "fingerprint", fp, "host", ev.HostID, "rule", ev.RuleID)
		}
	}
	e.pruneSuppressed(now)
	return nil
}

type metricValue struct {
	key   string
	scope string
	value float64
}

func thresholdMetrics(m map[string]store.Latest, rule config.Rule) []metricValue {
	if len(m) == 0 {
		return nil
	}
	if !rule.PerScope {
		if l, ok := m[rule.Metric]; ok {
			return []metricValue{{key: rule.Metric, value: l.Value}}
		}
		return nil
	}
	var out []metricValue
	for k, l := range m {
		if !strings.HasPrefix(k, rule.Metric+":") {
			continue
		}
		out = append(out, metricValue{key: k, scope: strings.TrimPrefix(k, rule.Metric+":"), value: l.Value})
	}
	return out
}

// classify 返回命中后的最高严重度（critical 优先）
func classify(v float64, rule config.Rule) (string, bool) {
	if rule.Below {
		if rule.Critical != 0 && v <= rule.Critical {
			return "critical", true
		}
		if rule.Warning != 0 && v <= rule.Warning {
			return "warning", true
		}
		return "", false
	}
	if rule.Critical != 0 && v >= rule.Critical {
		return "critical", true
	}
	if rule.Warning != 0 && v >= rule.Warning {
		return "warning", true
	}
	return "", false
}

func hitHeartbeat(h store.HostRow, rule config.Rule, now time.Time) bool {
	timeout := rule.Timeout.Std()
	if timeout <= 0 {
		timeout = 150 * time.Second
	}
	if h.LastSeen == nil {
		return now.Sub(h.FirstSeen) > timeout
	}
	return now.Sub(*h.LastSeen) > timeout
}

func (e *Engine) windowMin(ctx context.Context, host, metric string, since time.Time) (float64, bool, error) {
	return e.st.WindowMin(ctx, host, metric, since)
}

func (e *Engine) windowMax(ctx context.Context, host, metric string, since time.Time) (float64, bool, error) {
	return e.st.WindowMax(ctx, host, metric, since)
}

func (e *Engine) fire(ctx context.Context, h store.HostRow, rule config.Rule, rev, scope string, extra map[string]any, value float64, rationale string) error {
	return e.fireWithSeverity(ctx, h, rule, rev, scope, "", value, rule.Severity, rationale)
}

func (e *Engine) fireWithSeverity(ctx context.Context, h store.HostRow, rule config.Rule, rev, scope, metric string, value float64, severity, rationale string) error {
	fp := fingerprint(h.ID, rule.ID, scope)
	if until, err := e.st.CooldownUntil(ctx, fp); err == nil && time.Now().Before(until) {
		e.recordSuppressed(ctx, fp, h, rule, rev, metric, value, until)
		return nil
	}
	detail := map[string]any{
		"host_id":  h.ID,
		"rule_id":  rule.ID,
		"scope":    scope,
		"metric":   metric,
		"value":    value,
		"warning":  rule.Warning,
		"critical": rule.Critical,
	}
	if rule.For.Std() > 0 {
		detail["for"] = rule.For.Std().String()
	}
	id, created, err := e.st.OpenEvent(ctx, store.EventDraft{
		Fingerprint: fp,
		HostID:      h.ID,
		RuleID:      rule.ID,
		Severity:    severity,
		Title:       rule.Title,
		Value:       value,
		Detail:      detail,
		RuleRev:     rev,
		Rationale:   rationale,
	})
	if err != nil {
		return err
	}
	if created {
		e.log.Info("event opened", "id", id, "host", h.ID, "rule", rule.ID, "severity", severity, "value", value)
	}
	return nil
}

func (e *Engine) recordSuppressed(ctx context.Context, fp string, h store.HostRow, rule config.Rule, rev, metric string, value float64, until time.Time) {
	e.mu.Lock()
	if t, ok := e.suppressed[fp]; ok && t.After(time.Now()) {
		e.mu.Unlock()
		return
	}
	e.suppressed[fp] = until
	e.mu.Unlock()
	_ = e.st.RecordDecision(ctx, store.Decision{
		ActorKind: "rule", Actor: rule.ID, RuleID: rule.ID, RuleRev: rev,
		Decision: "suppressed", Rationale: "冷却期内，不重复处置",
		Inputs:  map[string]any{"host_id": h.ID, "metric": metric, "value": value, "cooldown_until": until.Format(time.RFC3339)},
		Outcome: "success",
	})
}

func (e *Engine) pruneSuppressed(now time.Time) {
	e.mu.Lock()
	for k, v := range e.suppressed {
		if v.Before(now) {
			delete(e.suppressed, k)
		}
	}
	e.mu.Unlock()
}

func fingerprint(host, rule, scope string) string {
	if scope == "" {
		return host + "|" + rule
	}
	return host + "|" + rule + "|" + scope
}

func trim(v float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.2f", v), "0"), ".")
}
