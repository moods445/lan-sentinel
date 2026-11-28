package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// HostDetail 主机最新系统明细快照（IO/CPU/内存/网络/负载）；payload 为 Agent 上报的原始 JSON。
type HostDetail struct {
	TS      time.Time
	Payload []byte
}

// SaveHostDetail 只保留最新快照：断网补传的旧批次不会覆盖更新的数据。
func (s *Store) SaveHostDetail(ctx context.Context, hostID string, ts time.Time, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sentinel.host_io_latest (host_id, ts, payload)
		VALUES ($1,$2,$3)
		ON CONFLICT (host_id) DO UPDATE SET ts=EXCLUDED.ts, payload=EXCLUDED.payload
		WHERE EXCLUDED.ts >= sentinel.host_io_latest.ts`,
		hostID, ts, payload)
	return err
}

func (s *Store) HostDetail(ctx context.Context, hostID string) (*HostDetail, error) {
	var out HostDetail
	err := s.pool.QueryRow(ctx,
		`SELECT ts, payload FROM sentinel.host_io_latest WHERE host_id=$1`, hostID).
		Scan(&out.TS, &out.Payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}
