package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/home-aio/lan-sentinel/internal/agent"
)

func main() {
	cfgPath := flag.String("config", agent.DefaultConfigPath(), "agent config file")
	server := flag.String("server", "", "control plane URL, e.g. http://192.168.1.102:8110")
	token := flag.String("token", "", "agent token")
	hostID := flag.String("host-id", "", "override host id (default: hostname)")
	interval := flag.Int("interval", 0, "sync interval seconds (default 15)")
	mounts := flag.String("mounts", "", "comma separated mount points to watch (max 3)")
	dataDir := flag.String("data-dir", "", "spool directory")
	once := flag.Bool("once", false, "collect and sync once, then exit")
	printOnly := flag.Bool("print", false, "print one collection as JSON, then exit")
	verbose := flag.Bool("v", false, "debug logs")
	flag.Parse()

	// 弱机红线：软内存上限 20MiB，GOGC=50，RSS 稳定在 8-12MB
	debug.SetMemoryLimit(20 << 20)
	debug.SetGCPercent(50)

	cfg, err := agent.LoadConfigFile(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
	}
	cfg.ApplyEnv()
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *hostID != "" {
		cfg.HostID = *hostID
	}
	if *interval > 0 {
		cfg.IntervalS = *interval
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *mounts != "" {
		parts := strings.Split(*mounts, ",")
		cfg.Mounts = cfg.Mounts[:0]
		for _, p := range parts {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Mounts = append(cfg.Mounts, p)
			}
		}
		if len(cfg.Mounts) > 3 {
			cfg.Mounts = cfg.Mounts[:3]
		}
	}
	if cfg.Server != "" && !strings.Contains(cfg.Server, "://") {
		cfg.Server = "http://" + cfg.Server
	}

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	coll := agent.NewCollector(cfg.Mounts)
	client := agent.NewClient(cfg, coll, log)

	if *printOnly {
		if err := client.PrintOnce(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		if err := client.Step(ctx, true); err != nil {
			fmt.Fprintln(os.Stderr, "sync failed:", err)
			os.Exit(1)
		}
		return
	}

	log.Info("sentinel-agent started", "host", cfg.HostID, "server", cfg.Server,
		"interval", cfg.Interval(), "mounts", cfg.Mounts)
	if err := client.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}
