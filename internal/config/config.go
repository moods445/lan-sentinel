package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration 让 YAML 里直接写 3m / 150s / 0s
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	s := node.Value
	if s == "" || s == "0" {
		*d = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Listen           string
	PGDSN            string
	PanelToken       string
	AgentToken       string
	InstallURL       string
	ConfigDir        string
	DataDir          string
	LogLevel         string
	EnableCollectors bool
}

func FromEnv() Config {
	return Config{
		Listen:           env("SENTINEL_LISTEN", ":8110"),
		PGDSN:            env("PG_DSN", "postgres://sentinel_app:change-me@127.0.0.1:5432/sentinel?sslmode=disable&pool_max_conns=8"),
		PanelToken:       os.Getenv("PANEL_TOKEN"),
		AgentToken:       os.Getenv("AGENT_TOKEN"),
		InstallURL:       env("SENTINEL_INSTALL_URL", "https://raw.githubusercontent.com/moods445/lan-sentinel/master/install.sh"),
		ConfigDir:        env("CONFIG_DIR", "./config"),
		DataDir:          env("DATA_DIR", "./data"),
		LogLevel:         env("LOG_LEVEL", "info"),
		EnableCollectors: env("ENABLE_COLLECTORS", "1") != "0",
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- rules ----------

type Rule struct {
	ID          string   `yaml:"id"`
	Type        string   `yaml:"type"` // metric_threshold | heartbeat | collector_failing
	Metric      string   `yaml:"metric"`
	Severity    string   `yaml:"severity"`
	Title       string   `yaml:"title"`
	Warning     float64  `yaml:"warning"`
	Critical    float64  `yaml:"critical"`
	Below       bool     `yaml:"below"` // true = 低于阈值触发（如 probe_ok < 0.5）
	PerScope    bool     `yaml:"per_scope"`
	For         Duration `yaml:"for"`
	Cooldown    Duration `yaml:"cooldown"`
	Timeout     Duration `yaml:"timeout"`
	Consecutive int      `yaml:"consecutive"`
}

type RulesFile struct {
	Rules []Rule `yaml:"rules"`
}

func LoadRules(dir string) ([]Rule, string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "rules.yaml"))
	if err != nil {
		return nil, "", err
	}
	var f RulesFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, "", fmt.Errorf("parse rules.yaml: %w", err)
	}
	seen := make(map[string]bool, len(f.Rules))
	for i := range f.Rules {
		r := &f.Rules[i]
		if r.ID == "" {
			return nil, "", fmt.Errorf("rules.yaml: rule #%d missing id", i)
		}
		if seen[r.ID] {
			return nil, "", fmt.Errorf("rules.yaml: duplicate id %q", r.ID)
		}
		seen[r.ID] = true
		switch r.Type {
		case "metric_threshold":
			if r.Metric == "" {
				return nil, "", fmt.Errorf("rules.yaml: %s missing metric", r.ID)
			}
			// 阈值统一成 target > value 语义
			if r.Critical == 0 && r.Warning != 0 {
				r.Critical = r.Warning
			}
		case "heartbeat", "collector_failing":
		default:
			return nil, "", fmt.Errorf("rules.yaml: unknown type %q", r.Type)
		}
		if r.Severity == "" {
			r.Severity = "warning"
		}
	}
	return f.Rules, rev(raw), nil
}

// ---------- collectors ----------

type Collector struct {
	HostID       string            `yaml:"host_id"`
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"` // probe | http | snmp
	Interval     Duration          `yaml:"interval"`
	Timeout      Duration          `yaml:"timeout"`
	Target       string            `yaml:"target"`
	URL          string            `yaml:"url"`
	Fields       map[string]string `yaml:"fields"`
	Interfaces   []int             `yaml:"interfaces"`
	CommunityRef string            `yaml:"community_ref"`
	BearerRef    string            `yaml:"bearer_ref"`
	HeaderRefs   map[string]string `yaml:"header_refs"`
	Insecure     bool              `yaml:"insecure_skip_verify"`
}

type CollectorsFile struct {
	Collectors []Collector `yaml:"collectors"`
}

func LoadCollectors(dir string) ([]Collector, string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "collectors.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, rev(nil), nil
		}
		return nil, "", err
	}
	var f CollectorsFile
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, "", fmt.Errorf("parse collectors.yaml: %w", err)
	}
	for i := range f.Collectors {
		c := &f.Collectors[i]
		if c.HostID == "" || c.Type == "" {
			return nil, "", fmt.Errorf("collectors.yaml: #%d missing host_id/type", i)
		}
		if c.Interval <= 0 {
			c.Interval = Duration(60 * time.Second)
		}
		if c.Timeout <= 0 {
			c.Timeout = Duration(4 * time.Second)
		}
	}
	return f.Collectors, rev(raw), nil
}

// LoadSecrets 只读 config/secrets.yaml（600），键值为明文 secret，不入库、不写日志
func LoadSecrets(dir string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, "secrets.yaml"))
	if err != nil {
		return out
	}
	var m map[string]string
	if yaml.Unmarshal(raw, &m) == nil {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func rev(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}
