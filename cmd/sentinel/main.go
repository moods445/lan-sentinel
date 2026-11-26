package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/home-aio/lan-sentinel/internal/api"
	"github.com/home-aio/lan-sentinel/internal/collectors"
	"github.com/home-aio/lan-sentinel/internal/config"
	"github.com/home-aio/lan-sentinel/internal/rules"
	"github.com/home-aio/lan-sentinel/internal/rollup"
	"github.com/home-aio/lan-sentinel/internal/store"
)

const version = "0.2.0"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		serve()
	case "migrate":
		if err := migrate(); err != nil {
			fmt.Fprintln(os.Stderr, "migrate failed:", err)
			os.Exit(1)
		}
		fmt.Println("migrate ok")
	case "healthcheck":
		os.Exit(healthcheck())
	case "version":
		fmt.Println("lan-sentinel", version)
	default:
		fmt.Fprintf(os.Stderr, "usage: sentinel [serve|migrate|healthcheck|version]\n")
		os.Exit(2)
	}
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}

func serve() {
	// 弱机内存上限：软限制 96MiB，GOGC 50 让 RSS 稳定在 45-60MB
	debug.SetMemoryLimit(96 << 20)
	debug.SetGCPercent(50)

	cfg := config.FromEnv()
	log := newLogger(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := openWithRetry(ctx, cfg.PGDSN, log)
	if err != nil {
		log.Error("postgres unavailable", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		log.Error("migration failed", "err", err)
		os.Exit(1)
	}

	ruleList, ruleRev, err := config.LoadRules(cfg.ConfigDir)
	if err != nil {
		log.Error("load rules failed", "err", err)
		os.Exit(1)
	}
	colList, colRev, err := config.LoadCollectors(cfg.ConfigDir)
	if err != nil {
		log.Error("load collectors failed", "err", err)
		os.Exit(1)
	}
	secrets := config.LoadSecrets(cfg.ConfigDir)
	if err := st.SaveConfigVersion(ctx, "rules", ruleRev, ""); err != nil {
		log.Warn("save config version failed", "err", err)
	}
	_ = st.SaveConfigVersion(ctx, "collectors", colRev, "")

	eng := rules.New(st, ruleList, ruleRev, log)
	rl := rollup.New(st, log)

	var mgr *collectors.Manager
	if cfg.EnableCollectors && len(colList) > 0 {
		mgr = collectors.NewManager(st, colList, secrets, log)
		go mgr.Run(ctx)
		log.Info("collectors enabled", "count", len(colList))
	}

	reload := func(ctx context.Context) (string, string, error) {
		rs, rev, err := config.LoadRules(cfg.ConfigDir)
		if err != nil {
			return "", "", err
		}
		cs, crev, err := config.LoadCollectors(cfg.ConfigDir)
		if err != nil {
			return "", "", err
		}
		eng.Reload(rs, rev)
		if mgr != nil {
			mgr.Reload(cs)
		}
		_ = st.SaveConfigVersion(ctx, "rules", rev, "")
		_ = st.SaveConfigVersion(ctx, "collectors", crev, "")
		log.Info("config reloaded", "rules_rev", rev, "collectors_rev", crev, "rules", len(rs))
		return rev, crev, nil
	}

	srv := api.New(api.Deps{
		Store: st, Engine: eng, Rollup: rl, Log: log,
		PanelToken: cfg.PanelToken, AgentToken: cfg.AgentToken,
		Reload: reload,
		RunCollector: func(ctx context.Context, host string) error {
			if mgr == nil {
				return errors.New("collectors disabled")
			}
			return mgr.RunOnce(ctx, host)
		},
	})

	hs := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	go eng.Run(ctx, 10*time.Second)
	go rl.Run(ctx)
	go maintenance(ctx, st, log)

	go func() {
		log.Info("sentinel listening", "addr", cfg.Listen, "version", version)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	_ = rl.Flush(context.Background())
}

// maintenance 每小时校正分区，每日 04:00 执行保留策略
func maintenance(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	lastRetention := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := st.EnsurePartitions(ctx, now, 2); err != nil {
				log.Warn("ensure partitions failed", "err", err)
			}
			if err := st.DropOldSamples(ctx, 48*time.Hour); err != nil {
				log.Warn("drop old partitions failed", "err", err)
			}
			if now.Hour() == 4 && now.Sub(lastRetention) > 12*time.Hour {
				lastRetention = now
				if err := st.Retention(ctx); err != nil {
					log.Warn("retention failed", "err", err)
				}
			}
		}
	}
}

func migrate() error {
	cfg := config.FromEnv()
	log := newLogger("warn")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := openWithRetry(ctx, cfg.PGDSN, log)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.Migrate(ctx)
}

func openWithRetry(ctx context.Context, dsn string, log *slog.Logger) (*store.Store, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		st, err := store.Open(ctx, dsn)
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err = st.Ping(pctx)
			cancel()
			if err == nil {
				return st, nil
			}
			st.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if log != nil {
		log.Error("postgres retry exhausted", "err", lastErr)
	}
	return nil, lastErr
}

func healthcheck() int {
	addr := os.Getenv("SENTINEL_HEALTH_URL")
	if addr == "" {
		addr = "http://127.0.0.1:8110/healthz"
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck status:", resp.StatusCode)
		return 1
	}
	return 0
}
