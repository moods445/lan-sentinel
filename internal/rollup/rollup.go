// Package rollup 在内存里按分钟聚合，每 30s 批量落库，避免逐条 UPDATE。
package rollup

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/home-aio/lan-sentinel/internal/store"
)

type key struct {
	host   string
	metric string
	minute time.Time
}

type agg struct {
	sum, min, max float64
	n             int
}

// Store 是聚合落库需要的最小能力；用接口声明便于单测注入失败场景
type Store interface {
	InsertRollup(ctx context.Context, host, metric string, bucket time.Time, avg, min, max float64, n int) error
}

type Rollup struct {
	st  Store
	log *slog.Logger

	mu      sync.Mutex
	buckets map[key]*agg
}

func New(st Store, log *slog.Logger) *Rollup {
	return &Rollup{st: st, log: log, buckets: map[key]*agg{}}
}

func (r *Rollup) Add(host string, samples []store.Sample) {
	if len(samples) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range samples {
		k := key{host: host, metric: s.Metric, minute: s.TS.Truncate(time.Minute)}
		a := r.buckets[k]
		if a == nil {
			if len(r.buckets) > 5000 { // 兜底上限，防止异常指标名把内存撑爆
				continue
			}
			a = &agg{min: s.Value, max: s.Value}
			r.buckets[k] = a
		}
		a.sum += s.Value
		a.n++
		if s.Value < a.min {
			a.min = s.Value
		}
		if s.Value > a.max {
			a.max = s.Value
		}
	}
}

func (r *Rollup) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Flush(ctx); err != nil {
				r.log.Warn("rollup flush failed", "err", err)
			}
		}
	}
}

// Flush 把内存聚合落库；单个 bucket 失败只影响它自己，其余继续写，
// 失败的全部放回内存等下一轮重试（早先的实现在首个错误处 return，会让快照里剩下的 bucket 直接丢失）。
func (r *Rollup) Flush(ctx context.Context) error {
	r.mu.Lock()
	if len(r.buckets) == 0 {
		r.mu.Unlock()
		return nil
	}
	snapshot := r.buckets
	r.buckets = map[key]*agg{}
	r.mu.Unlock()

	var firstErr error
	failed := 0
	for k, a := range snapshot {
		if a.n == 0 {
			continue
		}
		if err := r.st.InsertRollup(ctx, k.host, k.metric, k.minute, a.sum/float64(a.n), a.min, a.max, a.n); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			failed++
			// 失败则放回，下次重试
			r.mu.Lock()
			if cur := r.buckets[k]; cur != nil {
				cur.sum += a.sum
				cur.n += a.n
				if a.min < cur.min {
					cur.min = a.min
				}
				if a.max > cur.max {
					cur.max = a.max
				}
			} else {
				r.buckets[k] = a
			}
			r.mu.Unlock()
		}
	}
	if failed > 0 && r.log != nil {
		r.log.Warn("rollup flush partially failed", "failed", failed, "total", len(snapshot))
	}
	return firstErr
}
