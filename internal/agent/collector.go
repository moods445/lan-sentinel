package agent

import "time"

// Meta 随每次上报带去，服务端用于展示与分组
type Meta struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Agent    string `json:"agent"`
}

// Collector 平台采集接口：Linux 读 /proc、/sys，macOS / Windows 走系统 API
type Collector interface {
	Collect(now time.Time) (map[string]float64, error)
	Meta() Meta
}
