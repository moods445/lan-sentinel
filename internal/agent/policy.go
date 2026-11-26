package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Policy 控制面下发的采集策略；零值表示全量采集、沿用 Agent 本地配置
type Policy struct {
	Rev            string   `json:"rev"`
	EnabledMetrics []string `json:"enabled_metrics"`
	IntervalS      int      `json:"interval_s"`
	Mounts         []string `json:"mounts"`
}

// MountSetter 采集器支持运行期调整监控挂载点，策略变更后立即生效
type MountSetter interface{ SetMounts([]string) }

func (p Policy) allows(metric string) bool {
	if len(p.EnabledMetrics) == 0 {
		return true
	}
	for _, pat := range p.EnabledMetrics {
		if matchMetricPattern(pat, metric) {
			return true
		}
	}
	return false
}

// matchMetricPattern 支持精确匹配与末尾 * 前缀匹配，如 temp:*、disk_used_pct:*
func matchMetricPattern(pattern, name string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == name
}

// filterMetrics 按策略裁剪本轮采集结果；未配置时原样返回
func filterMetrics(m map[string]float64, p Policy) map[string]float64 {
	if len(p.EnabledMetrics) == 0 || len(m) == 0 {
		return m
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if p.allows(k) {
			out[k] = v
		}
	}
	return out
}

func policyFilePath(dataDir string) string {
	if dataDir == "" {
		dataDir = defaultDataDir()
	}
	return filepath.Join(dataDir, "policy.json")
}

func loadPolicyFile(path string) (Policy, error) {
	var p Policy
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// savePolicyFile 原子写入，避免断电留下半截 JSON
func savePolicyFile(path string, p Policy) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p Policy) interval(def time.Duration) time.Duration {
	if p.IntervalS > 0 {
		return time.Duration(p.IntervalS) * time.Second
	}
	return def
}
