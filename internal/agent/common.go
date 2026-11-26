package agent

import (
	"math"
	"strings"
)

const agentVersion = "0.2.0"

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func bps(cur, prev, dt float64) float64 {
	d := cur - prev
	if d < 0 { // 计数器回绕 / 网卡重置
		d = 0
	}
	return d * 8 / dt
}

// diskMetricLabel 把挂载点转成指标名后缀：Windows 反斜杠不能出现在指标名里
func diskMetricLabel(mp string) string {
	s := strings.TrimRight(mp, `\/`)
	if s == "" {
		s = mp
	}
	return strings.ReplaceAll(s, `\`, `/`)
}

// normalizeMounts 去空、去重、最多 3 个；为空时回落到平台默认
func normalizeMounts(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) == 3 {
			break
		}
	}
	if len(out) == 0 {
		return defaultMounts()
	}
	return out
}
