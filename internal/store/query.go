package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------- metrics ----------

type Point struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"v"`
}

type Latest struct {
	Metric string
	Value  float64
	TS     time.Time
	Src    string
}

func (s *Store) LatestMetrics(ctx context.Context) (map[string]map[string]Latest, error) {
	rows, err := s.pool.Query(ctx, `SELECT host_id, metric, value, ts, src FROM sentinel.metric_latest`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]Latest{}
	for rows.Next() {
		var host string
		var l Latest
		if err := rows.Scan(&host, &l.Metric, &l.Value, &l.TS, &l.Src); err != nil {
			return nil, err
		}
		if out[host] == nil {
			out[host] = map[string]Latest{}
		}
		out[host][l.Metric] = l
	}
	return out, rows.Err()
}

// QueryMetrics 短窗口读原始样本，长窗口读 1m 汇总并降采样
func (s *Store) QueryMetrics(ctx context.Context, host, metric string, from, to time.Time, limit int) ([]Point, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	span := to.Sub(from)
	var rows pgx.Rows
	var err error
	if span <= 6*time.Hour {
		rows, err = s.pool.Query(ctx, `
			SELECT ts, value FROM sentinel.metric_samples
			WHERE host_id=$1 AND metric=$2 AND ts >= $3 AND ts <= $4
			ORDER BY ts`, host, metric, from, to)
	} else {
		stepSec := int(math.Ceil(span.Seconds() / float64(limit)))
		if stepSec < 60 {
			stepSec = 60
		}
		q := fmt.Sprintf(`SELECT date_bin(interval '%d seconds', bucket, timestamptz '2000-01-01') AS b,
			avg(avg) FROM sentinel.metric_rollup_1m
			WHERE host_id=$1 AND metric=$2 AND bucket >= $3 AND bucket <= $4
			GROUP BY b ORDER BY b`, stepSec)
		rows, err = s.pool.Query(ctx, q, host, metric, from, to)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.TS, &p.Value); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WindowMin 用于 `for` 持续时长判定：窗口内最小值仍越线才算持续
func (s *Store) WindowMin(ctx context.Context, host, metric string, since time.Time) (float64, bool, error) {
	var v *float64
	err := s.pool.QueryRow(ctx,
		`SELECT min(value) FROM sentinel.metric_samples WHERE host_id=$1 AND metric=$2 AND ts >= $3`,
		host, metric, since).Scan(&v)
	if err != nil || v == nil {
		return 0, false, err
	}
	return *v, true, nil
}

func (s *Store) InsertRollup(ctx context.Context, host, metric string, bucket time.Time, avg, min, max float64, n int) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sentinel.metric_rollup_1m (host_id, metric, bucket, avg, min, max, n)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (host_id, metric, bucket) DO UPDATE SET
			avg = (sentinel.metric_rollup_1m.avg * sentinel.metric_rollup_1m.n + EXCLUDED.avg * EXCLUDED.n)
			      / (sentinel.metric_rollup_1m.n + EXCLUDED.n),
			min = LEAST(sentinel.metric_rollup_1m.min, EXCLUDED.min),
			max = GREATEST(sentinel.metric_rollup_1m.max, EXCLUDED.max),
			n   = sentinel.metric_rollup_1m.n + EXCLUDED.n`,
		host, metric, bucket, avg, min, max, n)
	return err
}

// ---------- events ----------

type Event struct {
	ID            int64          `json:"id"`
	Fingerprint   string         `json:"fingerprint"`
	HostID        string         `json:"host_id"`
	RuleID        string         `json:"rule_id"`
	Severity      string         `json:"severity"`
	Status        string         `json:"status"`
	Title         string         `json:"title"`
	Detail        map[string]any `json:"detail"`
	Value         *float64       `json:"value,omitempty"`
	OpenedAt      time.Time      `json:"opened_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	ResolvedAt    *time.Time     `json:"resolved_at,omitempty"`
	CooldownUntil *time.Time     `json:"cooldown_until,omitempty"`
}

type EventDraft struct {
	Fingerprint string
	HostID      string
	RuleID      string
	Severity    string
	Title       string
	Value       float64
	Detail      map[string]any
	RuleRev     string
	Rationale   string
}

// OpenEvent 幂等打开事件；同指纹已有 open 时只刷新数值，不重复写决策/流
func (s *Store) OpenEvent(ctx context.Context, d EventDraft) (int64, bool, error) {
	if d.Severity == "" {
		d.Severity = "warning"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	var id int64
	err = tx.QueryRow(ctx, `
		INSERT INTO sentinel.events (fingerprint, host_id, rule_id, severity, title, value, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (fingerprint) WHERE status = 'open' DO NOTHING
		RETURNING id`,
		d.Fingerprint, d.HostID, d.RuleID, d.Severity, d.Title, d.Value, jsonBytes(d.Detail)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// 已存在打开事件：刷新观测值
		if _, err := tx.Exec(ctx, `
			UPDATE sentinel.events SET value=$2, severity=$3, updated_at=now()
			WHERE fingerprint=$1 AND status='open'`, d.Fingerprint, d.Value, d.Severity); err != nil {
			return 0, false, err
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM sentinel.events WHERE fingerprint=$1 AND status='open'`, d.Fingerprint).Scan(&id); err != nil {
			return 0, false, err
		}
		return id, false, tx.Commit(ctx)
	}
	if err != nil {
		return 0, false, err
	}

	payload := map[string]any{"event_id": id, "rule_id": d.RuleID, "host_id": d.HostID, "severity": d.Severity, "title": d.Title}
	if err := appendStreamTx(ctx, tx, "event.opened", d.HostID, 0, payload); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sentinel.decisions (actor_kind, actor, rule_id, rule_rev, decision, rationale, inputs, outcome)
		VALUES ('rule', $1, $1, $2, 'open', $3, $4, 'success')`,
		d.RuleID, d.RuleRev, d.Rationale, jsonBytes(map[string]any{"host_id": d.HostID, "event_id": id, "value": d.Value})); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}

func (s *Store) ResolveEvent(ctx context.Context, fingerprint string, cooldown time.Duration, rationale string) (int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	var id int64
	var hostID, ruleID string
	err = tx.QueryRow(ctx, `
		UPDATE sentinel.events
		SET status='resolved', resolved_at=now(), updated_at=now(), cooldown_until = now() + $2::interval
		WHERE fingerprint=$1 AND status='open'
		RETURNING id, host_id, rule_id`,
		fingerprint, fmt.Sprintf("%d seconds", int(cooldown.Seconds()))).Scan(&id, &hostID, &ruleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if err := appendStreamTx(ctx, tx, "event.resolved", hostID, 0,
		map[string]any{"event_id": id, "rule_id": ruleID}); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO sentinel.decisions (actor_kind, actor, rule_id, decision, rationale, inputs, outcome)
		VALUES ('rule', $1, $1, 'resolve', $2, $3, 'success')`,
		ruleID, rationale, jsonBytes(map[string]any{"host_id": hostID, "event_id": id})); err != nil {
		return 0, false, err
	}
	return id, true, tx.Commit(ctx)
}

func (s *Store) CooldownActive(ctx context.Context, fingerprint string) (bool, error) {
	until, err := s.CooldownUntil(ctx, fingerprint)
	if err != nil {
		return false, err
	}
	return time.Now().Before(until), nil
}

func (s *Store) CooldownUntil(ctx context.Context, fingerprint string) (time.Time, error) {
	var until time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(max(cooldown_until), to_timestamp(0))
		FROM sentinel.events WHERE fingerprint=$1 AND status='resolved'`, fingerprint).Scan(&until)
	return until, err
}

func (s *Store) OpenFingerprints(ctx context.Context) (map[string]Event, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, fingerprint, host_id, rule_id, severity, status, title, opened_at, updated_at
		FROM sentinel.events WHERE status='open'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Fingerprint, &e.HostID, &e.RuleID, &e.Severity, &e.Status, &e.Title, &e.OpenedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out[e.Fingerprint] = e
	}
	return out, rows.Err()
}

// OpenEventCounts 主机墙上每个主机的未关闭事件数与最高严重度
func (s *Store) OpenEventCounts(ctx context.Context) (map[string]int, map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host_id, count(*), bool_or(severity = 'critical')
		FROM sentinel.events WHERE status='open' GROUP BY host_id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	sev := map[string]string{}
	for rows.Next() {
		var h string
		var n int
		var critical bool
		if err := rows.Scan(&h, &n, &critical); err != nil {
			return nil, nil, err
		}
		counts[h] = n
		if critical {
			sev[h] = "critical"
		} else {
			sev[h] = "warning"
		}
	}
	return counts, sev, rows.Err()
}

func (s *Store) ListEvents(ctx context.Context, status, host string, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, fingerprint, host_id, rule_id, severity, status, title, detail, value,
		       opened_at, updated_at, resolved_at, cooldown_until
		FROM sentinel.events
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR host_id = $2)
		ORDER BY opened_at DESC LIMIT $3`, status, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var detail []byte
		if err := rows.Scan(&e.ID, &e.Fingerprint, &e.HostID, &e.RuleID, &e.Severity, &e.Status, &e.Title,
			&detail, &e.Value, &e.OpenedAt, &e.UpdatedAt, &e.ResolvedAt, &e.CooldownUntil); err != nil {
			return nil, err
		}
		_ = jsonUnmarshalBytes(detail, &e.Detail)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- decisions ----------

type Decision struct {
	TS            time.Time      `json:"ts,omitempty"`
	RunID         *int64         `json:"run_id,omitempty"`
	ActorKind     string         `json:"actor_kind"`
	Actor         string         `json:"actor"`
	RuleID        string         `json:"rule_id,omitempty"`
	RuleRev       string         `json:"rule_rev,omitempty"`
	ActionID      string         `json:"action_id,omitempty"`
	Risk          string         `json:"risk,omitempty"`
	Decision      string         `json:"decision"`
	Rationale     string         `json:"rationale,omitempty"`
	Inputs        map[string]any `json:"inputs,omitempty"`
	Outcome       string         `json:"outcome,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
}

func (s *Store) RecordDecision(ctx context.Context, d Decision) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO sentinel.decisions (run_id, actor_kind, actor, rule_id, rule_rev, action_id, risk, decision, rationale, inputs, outcome, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		d.RunID, d.ActorKind, d.Actor, nullStr(d.RuleID), nullStr(d.RuleRev), nullStr(d.ActionID), nullStr(d.Risk),
		d.Decision, d.Rationale, jsonBytes(d.Inputs), nullStr(d.Outcome), nullStr(d.CorrelationID)); err != nil {
		return err
	}
	if err := appendStreamTx(ctx, tx, "decision.recorded", stringAny(d.Inputs["host_id"]), 0,
		map[string]any{"decision": d.Decision, "actor": d.Actor, "rule_id": d.RuleID}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func stringAny(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (s *Store) ListDecisions(ctx context.Context, host string, limit int) ([]Decision, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT run_id, actor_kind, actor, COALESCE(rule_id,''), COALESCE(rule_rev,''), COALESCE(action_id,''),
		       COALESCE(risk,''), decision, COALESCE(rationale,''), COALESCE(inputs,'{}'::jsonb),
		       COALESCE(outcome,''), COALESCE(correlation_id,''), ts
		FROM sentinel.decisions
		WHERE ($1 = '' OR inputs->>'host_id' = $1)
		ORDER BY id DESC LIMIT $2`, host, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Decision{}
	for rows.Next() {
		var r Decision
		var inputs []byte
		if err := rows.Scan(&r.RunID, &r.ActorKind, &r.Actor, &r.RuleID, &r.RuleRev, &r.ActionID, &r.Risk,
			&r.Decision, &r.Rationale, &inputs, &r.Outcome, &r.CorrelationID, &r.TS); err != nil {
			return nil, err
		}
		r.Inputs = map[string]any{}
		_ = jsonUnmarshalBytes(inputs, &r.Inputs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------- stream ----------

type StreamEvent struct {
	Seq     int64          `json:"seq"`
	TS      time.Time      `json:"ts"`
	Type    string         `json:"type"`
	HostID  *string        `json:"host_id,omitempty"`
	RunID   *int64         `json:"run_id,omitempty"`
	Payload map[string]any `json:"payload"`
}

func (s *Store) StreamSince(ctx context.Context, since int64, limit int) ([]StreamEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `
		SELECT seq, ts, type, host_id, run_id, payload FROM sentinel.stream_events
		WHERE seq > $1 ORDER BY seq LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StreamEvent
	for rows.Next() {
		var e StreamEvent
		var payload []byte
		if err := rows.Scan(&e.Seq, &e.TS, &e.Type, &e.HostID, &e.RunID, &payload); err != nil {
			return nil, err
		}
		e.Payload = map[string]any{}
		_ = jsonUnmarshalBytes(payload, &e.Payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- collector state ----------

type CollectorState struct {
	HostID    string     `json:"host_id"`
	Failures  int        `json:"failures"`
	LastError string     `json:"last_error"`
	LastOKAt  *time.Time `json:"last_ok_at"`
	NextRunAt time.Time  `json:"next_run_at"`
}

func (s *Store) SaveCollectorState(ctx context.Context, st CollectorState) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sentinel.collector_state (host_id, failures, last_error, last_ok_at, next_run_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (host_id) DO UPDATE SET failures=EXCLUDED.failures, last_error=EXCLUDED.last_error,
			last_ok_at=EXCLUDED.last_ok_at, next_run_at=EXCLUDED.next_run_at`,
		st.HostID, st.Failures, st.LastError, st.LastOKAt, st.NextRunAt)
	return err
}

func (s *Store) CollectorStates(ctx context.Context) (map[string]CollectorState, error) {
	rows, err := s.pool.Query(ctx, `SELECT host_id, failures, last_error, last_ok_at, next_run_at FROM sentinel.collector_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CollectorState{}
	for rows.Next() {
		var st CollectorState
		if err := rows.Scan(&st.HostID, &st.Failures, &st.LastError, &st.LastOKAt, &st.NextRunAt); err != nil {
			return nil, err
		}
		out[st.HostID] = st
	}
	return out, rows.Err()
}

// ---------- misc ----------

func (s *Store) SaveConfigVersion(ctx context.Context, kind, rev, content string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sentinel.config_versions (kind, rev, content) VALUES ($1,$2,$3)
		ON CONFLICT (kind, rev) DO NOTHING`, kind, rev, content)
	return err
}

func (s *Store) SetHostTokenHash(ctx context.Context, host, hash string) error {
	_, err := s.pool.Exec(ctx, `UPDATE sentinel.hosts SET token_hash=$2 WHERE id=$1`, host, hash)
	return err
}

func (s *Store) HostTokenHash(ctx context.Context, host string) (string, error) {
	var h string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(token_hash,'') FROM sentinel.hosts WHERE id=$1`, host).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return h, err
}

func (s *Store) Audit(ctx context.Context, actor, action, target string, detail map[string]any) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sentinel.audit_log (actor, action, target, detail) VALUES ($1,$2,$3,$4)`,
		actor, action, nullStr(target), jsonBytes(detail))
	return err
}

// Retention 每日清理
func (s *Store) Retention(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sentinel.stream_events WHERE ts < now() - interval '7 days'`); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sentinel.metric_rollup_1m WHERE bucket < now() - interval '90 days'`); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sentinel.metric_latest WHERE ts < now() - interval '7 days'`); err != nil {
		return err
	}
	return nil
}
