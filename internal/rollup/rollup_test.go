package rollup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/home-aio/lan-sentinel/internal/store"
)

type flakyStore struct {
	mu       sync.Mutex
	fail     int // 前 N 次 InsertRollup 返回错误
	calls    int
	inserted map[string]float64 // host|metric -> avg
}

func newFlaky(fail int) *flakyStore {
	return &flakyStore{fail: fail, inserted: map[string]float64{}}
}

func (f *flakyStore) InsertRollup(_ context.Context, host, metric string, _ time.Time, avg, _, _ float64, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fail {
		return errors.New("db down")
	}
	f.inserted[host+"|"+metric] = avg
	return nil
}

// TestFlushKeepsAllBucketsOnPartialFailure 单个 bucket 落库失败不能连带丢掉其余 bucket。
func TestFlushKeepsAllBucketsOnPartialFailure(t *testing.T) {
	fs := newFlaky(1)
	r := New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	for _, h := range []string{"h1", "h2", "h3", "h4"} {
		r.Add(h, []store.Sample{{Metric: "cpu_pct", TS: now, Value: 10}})
	}
	if err := r.Flush(context.Background()); err == nil {
		t.Fatal("first flush should report the failure")
	}
	// 第二轮：失败的 bucket 已放回，全部应落库
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("second flush: %v", err)
	}
	if len(fs.inserted) != 4 {
		t.Fatalf("want 4 buckets persisted, got %d (%v)", len(fs.inserted), fs.inserted)
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("third flush: %v", err)
	}
	// 4 个 bucket + 1 次重试；第三轮空 map 直接返回，不再打库
	if fs.calls != 5 {
		t.Fatalf("calls=%d, want 5", fs.calls)
	}
}

// TestAddRetryMergesIntoExistingBucket 放回的失败数据要与新采样合并，而不是覆盖。
func TestAddRetryMergesIntoExistingBucket(t *testing.T) {
	fs := newFlaky(1)
	r := New(fs, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Now()
	r.Add("h1", []store.Sample{{Metric: "mem_pct", TS: now, Value: 40}})
	_ = r.Flush(context.Background())
	r.Add("h1", []store.Sample{{Metric: "mem_pct", TS: now, Value: 60}})
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	// 第二轮把上一轮失败的 40 与本轮 60 合并后写入，均值 50
	if got := fs.inserted["h1|mem_pct"]; got != 50 {
		t.Fatalf("avg = %v, want 50", got)
	}
}
