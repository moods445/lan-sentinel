package store

import (
	"context"
	"time"
)

// NodeTarget 面板手工添加的节点，目前只支持 probe 拉取。
type NodeTarget struct {
	HostID    string    `json:"host_id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	Target    string    `json:"target"`
	IntervalS int       `json:"interval_s"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Store) ListNodeTargets(ctx context.Context) ([]NodeTarget, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT host_id, name, type, target, interval_s, created_at
		FROM sentinel.collector_targets ORDER BY created_at, host_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeTarget{}
	for rows.Next() {
		var t NodeTarget
		if err := rows.Scan(&t.HostID, &t.Name, &t.Type, &t.Target, &t.IntervalS, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SaveNodeTarget(ctx context.Context, t NodeTarget) (NodeTarget, error) {
	if t.IntervalS <= 0 {
		t.IntervalS = 30
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO sentinel.collector_targets (host_id, name, type, target, interval_s)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (host_id) DO UPDATE SET
			name = EXCLUDED.name, type = EXCLUDED.type,
			target = EXCLUDED.target, interval_s = EXCLUDED.interval_s
		RETURNING created_at`,
		t.HostID, t.Name, t.Type, t.Target, t.IntervalS).Scan(&t.CreatedAt)
	return t, err
}

func (s *Store) DeleteNodeTarget(ctx context.Context, hostID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM sentinel.collector_targets WHERE host_id = $1`, hostID)
	return err
}

// DeleteProbeHost 移除面板添加的 probe 节点及其最新值/采集状态；
// 只删除 kind='probe' 的主机，避免误删同名 Agent 主机。
func (s *Store) DeleteProbeHost(ctx context.Context, hostID string) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM sentinel.metric_latest WHERE host_id = $1`, hostID); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM sentinel.collector_state WHERE host_id = $1`, hostID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM sentinel.hosts WHERE id = $1 AND kind = 'probe'`, hostID)
	return err
}
