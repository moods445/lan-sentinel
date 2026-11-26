package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Config 来自 /etc/lan-sentinel/agent.json，环境变量可覆盖
type Config struct {
	Server    string   `json:"server"`
	Token     string   `json:"token"`
	HostID    string   `json:"host_id"`
	IntervalS int      `json:"interval_s"`
	Mounts    []string `json:"mounts"`
	DataDir   string   `json:"data_dir"`
	Insecure  bool     `json:"insecure_skip_verify"`
}

func DefaultConfig() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	return Config{
		Server:    "http://127.0.0.1:8110",
		HostID:    host,
		IntervalS: 15,
		Mounts:    defaultMounts(),
		DataDir:   defaultDataDir(),
	}
}

func defaultMounts() []string {
	if runtime.GOOS == "windows" {
		return []string{`C:\`}
	}
	return []string{"/"}
}

func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\ProgramData\LanSentinel`
	case "darwin":
		return "/Library/Application Support/LanSentinel"
	default:
		return "/var/lib/lan-sentinel"
	}
}

// DefaultConfigPath 各平台默认配置文件位置
func DefaultConfigPath() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\LanSentinel\agent.json`
	}
	return "/etc/lan-sentinel/agent.json"
}

func LoadConfigFile(path string) (Config, error) {
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, err
	}
	if cfg.IntervalS <= 0 {
		cfg.IntervalS = 15
	}
	if len(cfg.Mounts) == 0 {
		cfg.Mounts = defaultMounts()
	}
	if len(cfg.Mounts) > 3 {
		cfg.Mounts = cfg.Mounts[:3]
	}
	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	return cfg, nil
}

func (c *Config) ApplyEnv() {
	if v := os.Getenv("SENTINEL_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("SENTINEL_TOKEN"); v != "" {
		c.Token = v
	}
	if v := os.Getenv("SENTINEL_HOST_ID"); v != "" {
		c.HostID = v
	}
}

func (c Config) Interval() time.Duration {
	if c.IntervalS <= 0 {
		return 15 * time.Second
	}
	return time.Duration(c.IntervalS) * time.Second
}

func (c Config) SpoolPath() string {
	return filepath.Join(c.DataDir, "spool.jsonl")
}
