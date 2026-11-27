package collectors

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/home-aio/lan-sentinel/internal/config"
	"github.com/home-aio/lan-sentinel/internal/store"
)

type fakeStore struct {
	mu      sync.Mutex
	ingests int
	states  []store.CollectorState
}

func (f *fakeStore) Ingest(context.Context, store.HostInfo, []store.Sample, map[string]any) error {
	f.mu.Lock()
	f.ingests++
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) SaveCollectorState(_ context.Context, st store.CollectorState) error {
	f.mu.Lock()
	f.states = append(f.states, st)
	f.mu.Unlock()
	return nil
}

func (f *fakeStore) AppendStream(context.Context, string, string, map[string]any) error { return nil }

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// closedPort 返回一个当前无人监听的本机端口，用于制造一次必然失败的拉取。
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// TestConcurrentRunOnceAndTick 定时调度与面板手动触发会同时操作同一个 item；
// 修复前 tick 在锁外写 it.next、runOne 在另一把锁里写同样的字段（-race 可复现）。
func TestConcurrentRunOnceAndTick(t *testing.T) {
	addr := closedPort(t)
	fs := &fakeStore{}
	m := NewManager(fs, []config.Collector{{
		HostID: "probe1", Type: "probe", Target: addr,
		Interval: config.Duration(time.Millisecond), // 每次成功后立刻再次到期，与手动触发叠加
		Timeout:  config.Duration(200 * time.Millisecond),
	}}, nil, discardLogger())

	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.RunOnce(ctx, "probe1")
		}()
		m.tick(ctx)
	}
	wg.Wait()

	fs.mu.Lock()
	ingests := fs.ingests
	fs.mu.Unlock()
	if ingests == 0 {
		t.Fatal("no successful ingest recorded")
	}
}

// TestFailurePathStateUpdates 失败分支同样要更新 failures/next，且不因并发丢计数。
func TestFailurePathStateUpdates(t *testing.T) {
	m := NewManager(&fakeStore{}, []config.Collector{{
		HostID: "bad", Type: "probe", // 缺 target → collect 直接报错
		Interval: config.Duration(time.Minute),
		Timeout:  config.Duration(100 * time.Millisecond),
	}}, nil, discardLogger())

	if err := m.RunOnce(context.Background(), "bad"); err == nil {
		t.Fatal("missing target should fail")
	}
	m.mu.Lock()
	it := m.items["bad"]
	m.mu.Unlock()
	if got := it.failCount(); got != 1 {
		t.Fatalf("failures = %d, want 1", got)
	}
	if next := it.nextAt(); !next.After(time.Now()) {
		t.Fatalf("next should be pushed into the future, got %v", next)
	}
}

// TestRunOnceSerializes 同一 item 的并发手动触发必须串行执行（rate 基线不被踩踏）。
func TestRunOnceSerializes(t *testing.T) {
	addr := closedPort(t)
	m := NewManager(&fakeStore{}, []config.Collector{{
		HostID: "p", Type: "probe", Target: addr,
		Interval: config.Duration(time.Minute),
		Timeout:  config.Duration(100 * time.Millisecond),
	}}, nil, discardLogger())

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.RunOnce(context.Background(), "p") }()
	}
	wg.Wait()
}
