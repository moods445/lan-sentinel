package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	sqlfs "github.com/home-aio/lan-sentinel/sql"
)

type Store struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	days map[string]bool // 已确认存在的分区
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	if cfg.MaxConns > 8 {
		cfg.MaxConns = 8
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, days: map[string]bool{}}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Migrate(ctx context.Context) error {
	c, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer c.Release()
	// 多语句脚本必须走 simple protocol
	if _, err := c.Conn().Exec(ctx, sqlfs.Init, pgx.QueryExecModeSimpleProtocol); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return s.EnsurePartitions(ctx, time.Now(), 2)
}

// EnsurePartition 按需建当天分区（缓存命中时不查库）
func (s *Store) EnsurePartition(ctx context.Context, ts time.Time) error {
	key := ts.UTC().Format("20060102")
	s.mu.Lock()
	ok := s.days[key]
	s.mu.Unlock()
	if ok {
		return nil
	}
	day := ts.UTC().Format("2006-01-02")
	if _, err := s.pool.Exec(ctx, `SELECT sentinel.ensure_partition($1::date)`, day); err != nil {
		return err
	}
	s.mu.Lock()
	s.days[key] = true
	s.mu.Unlock()
	return nil
}

// ensureSamplePartitions 为一批样本涉及的每个日期建分区。
// 只建 samples[0] 那天的分区时，跨日批次（断网 spool 补传）会因「no partition for row」
// 整批失败，而客户端是 FIFO 重发，失败批次会让整条补传队列卡死。
//
// 分区名按 UTC 日期命名，但 ensure_partition 的分区边界用的是数据库会话时区，
// 所以一个 UTC 日期的行可能落进相邻日期的分区；这里对每个样本日期补建 前/当/后 三天，
// 使写入与数据库时区无关（EnsurePartition 有缓存，命中时不查库）。
func (s *Store) ensureSamplePartitions(ctx context.Context, samples []Sample) error {
	seen := make(map[string]struct{}, 12)
	for _, sm := range samples {
		day := sm.TS.UTC()
		for off := -1; off <= 1; off++ {
			key := day.AddDate(0, 0, off).Format("20060102")
			if _, ok := seen[key]; ok {
				continue
			}
			if len(seen) >= 12 { // 防御伪造的超长时间跨度，最多 4 个样本日期
				return nil
			}
			seen[key] = struct{}{}
			if err := s.EnsurePartition(ctx, day.AddDate(0, 0, off)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) EnsurePartitions(ctx context.Context, now time.Time, aheadDays int) error {
	for i := 0; i <= aheadDays; i++ {
		if err := s.EnsurePartition(ctx, now.AddDate(0, 0, i)); err != nil {
			return err
		}
	}
	return nil
}

// DropOldSamples 删除保留期之前的分区（整分区 DROP，代价恒定）
func (s *Store) DropOldSamples(ctx context.Context, keep time.Duration) error {
	cutoff := time.Now().Add(-keep).UTC()
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'sentinel' AND c.relkind = 'r'
		  AND c.relname ~ '^metric_samples_[0-9]{8}$'`)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			names = append(names, n)
		}
	}
	rows.Close()
	for _, n := range names {
		day, err := time.Parse("20060102", strings.TrimPrefix(n, "metric_samples_"))
		if err != nil {
			continue
		}
		if !day.Add(24 * time.Hour).Before(cutoff) {
			continue
		}
		if _, err := s.pool.Exec(ctx, `DROP TABLE IF EXISTS sentinel.`+pgx.Identifier{n}.Sanitize()); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.days, day.Format("20060102"))
		s.mu.Unlock()
	}
	return nil
}

// ---------- ingest ----------

type HostInfo struct {
	ID   string
	Name string
	OS   string
	Arch string
	Kind string
	Addr string
	Meta map[string]any
}

type Sample struct {
	Metric string
	TS     time.Time
	Value  float64
	Src    string
}

// Ingest 单事务写入：主机心跳 + 样本 + 最新值 + 增量流事件
func (s *Store) Ingest(ctx context.Context, h HostInfo, samples []Sample, streamPayload map[string]any) error {
	if h.ID == "" {
		return errors.New("empty host id")
	}
	if err := s.ensureSamplePartitions(ctx, samples); err != nil {
		return err
	}
	if h.Kind == "" {
		h.Kind = "agent"
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
		INSERT INTO sentinel.hosts (id, name, os, arch, kind, addr, last_seen, meta)
		VALUES ($1,$2,$3,$4,$5,$6, now(), $7)
		ON CONFLICT (id) DO UPDATE SET
			name = COALESCE(NULLIF(EXCLUDED.name,''), sentinel.hosts.name),
			os   = COALESCE(NULLIF(EXCLUDED.os,''), sentinel.hosts.os),
			arch = COALESCE(NULLIF(EXCLUDED.arch,''), sentinel.hosts.arch),
			kind = EXCLUDED.kind,
			addr = COALESCE(NULLIF(EXCLUDED.addr,''), sentinel.hosts.addr),
			-- 上报方带 meta 时刷新（Agent 升级后版本号才能更新），空对象则保留原值
			meta = CASE WHEN EXCLUDED.meta <> '{}'::jsonb THEN EXCLUDED.meta ELSE sentinel.hosts.meta END,
			last_seen = now()`,
		h.ID, h.Name, h.OS, h.Arch, h.Kind, h.Addr, jsonBytes(h.Meta)); err != nil {
		return err
	}

	if len(samples) > 0 {
		b := &pgx.Batch{}
		for _, sm := range samples {
			b.Queue(`INSERT INTO sentinel.metric_samples (host_id, metric, ts, value, src)
				VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`,
				h.ID, sm.Metric, sm.TS, sm.Value, defSrc(sm.Src))
		}
		for _, sm := range samples {
			b.Queue(`INSERT INTO sentinel.metric_latest (host_id, metric, ts, value, src)
				VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (host_id, metric) DO UPDATE SET ts=EXCLUDED.ts, value=EXCLUDED.value, src=EXCLUDED.src
				WHERE EXCLUDED.ts >= sentinel.metric_latest.ts`,
				h.ID, sm.Metric, sm.TS, sm.Value, defSrc(sm.Src))
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return err
		}
	}

	if err := appendStreamTx(ctx, tx, "host.ingested", h.ID, 0, streamPayload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func defSrc(s string) string {
	if s == "" {
		return "agent"
	}
	return s
}

func jsonBytes(m map[string]any) []byte {
	if len(m) == 0 {
		return []byte(`{}`)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

func jsonUnmarshalBytes(b []byte, v any) error {
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// ---------- hosts / latest ----------

type HostRow struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	OS        string             `json:"os"`
	Arch      string             `json:"arch"`
	Kind      string             `json:"kind"`
	Addr      string             `json:"addr"`
	Frozen    bool               `json:"frozen"`
	FirstSeen time.Time          `json:"first_seen"`
	LastSeen  *time.Time         `json:"last_seen"`
	Meta      map[string]any     `json:"meta,omitempty"`
	Metrics   map[string]float64 `json:"metrics"`
}

func (s *Store) ListHosts(ctx context.Context) ([]HostRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT h.id, h.name, h.os, h.arch, h.kind, h.addr, h.frozen, h.first_seen, h.last_seen,
		       COALESCE(h.meta, '{}'::jsonb),
		       COALESCE(jsonb_object_agg(ml.metric, ml.value) FILTER (WHERE ml.metric IS NOT NULL), '{}'::jsonb)
		FROM sentinel.hosts h
		LEFT JOIN sentinel.metric_latest ml ON ml.host_id = h.id
		GROUP BY h.id ORDER BY h.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HostRow
	for rows.Next() {
		var r HostRow
		var meta, metrics []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.OS, &r.Arch, &r.Kind, &r.Addr, &r.Frozen, &r.FirstSeen, &r.LastSeen, &meta, &metrics); err != nil {
			return nil, err
		}
		r.Meta = map[string]any{}
		_ = json.Unmarshal(meta, &r.Meta)
		r.Metrics = map[string]float64{}
		_ = json.Unmarshal(metrics, &r.Metrics)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Host 只查单个主机；早先复用 ListHosts 会把全部主机及其最新值全表拉一遍。
func (s *Store) Host(ctx context.Context, id string) (*HostRow, error) {
	var r HostRow
	var meta, metrics []byte
	err := s.pool.QueryRow(ctx, `
		SELECT h.id, h.name, h.os, h.arch, h.kind, h.addr, h.frozen, h.first_seen, h.last_seen,
		       COALESCE(h.meta, '{}'::jsonb),
		       COALESCE((SELECT jsonb_object_agg(ml.metric, ml.value)
		                 FROM sentinel.metric_latest ml WHERE ml.host_id = h.id), '{}'::jsonb)
		FROM sentinel.hosts h WHERE h.id = $1`, id).
		Scan(&r.ID, &r.Name, &r.OS, &r.Arch, &r.Kind, &r.Addr, &r.Frozen, &r.FirstSeen, &r.LastSeen, &meta, &metrics)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Meta = map[string]any{}
	_ = json.Unmarshal(meta, &r.Meta)
	r.Metrics = map[string]float64{}
	_ = json.Unmarshal(metrics, &r.Metrics)
	return &r, nil
}

// ---------- stream ----------

// AppendStream 业务写 + 流出同一事务（outbox）；本方法用于非事务场景
func (s *Store) AppendStream(ctx context.Context, typ, hostID string, payload map[string]any) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO sentinel.stream_events (type, host_id, payload) VALUES ($1,$2,$3)`,
		typ, nullStr(hostID), jsonBytes(payload))
	return err
}

func appendStreamTx(ctx context.Context, tx pgx.Tx, typ, hostID string, runID int64, payload map[string]any) error {
	var rid any
	if runID > 0 {
		rid = runID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO sentinel.stream_events (type, host_id, run_id, payload) VALUES ($1,$2,$3,$4)`,
		typ, nullStr(hostID), rid, jsonBytes(payload))
	return err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
