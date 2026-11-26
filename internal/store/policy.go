package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PolicyRow 一条采集策略：scope=global 为默认，scope=host 为单机覆盖
type PolicyRow struct {
	Scope          string    `json:"scope"`
	HostID         string    `json:"host_id,omitempty"`
	Rev            int       `json:"rev"`
	EnabledMetrics []string  `json:"enabled_metrics"`
	IntervalS      int       `json:"interval_s"`
	Mounts         []string  `json:"mounts"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// EffectivePolicy 下发给 Agent 的最终策略，由全局与主机覆盖合并而成
type EffectivePolicy struct {
	Rev            string   `json:"rev"`
	EnabledMetrics []string `json:"enabled_metrics"`
	IntervalS      int      `json:"interval_s"`
	Mounts         []string `json:"mounts"`
	Source         string   `json:"source"` // global | host
}

const policyCols = `scope, host_id, rev, enabled_metrics, interval_s, mounts, updated_at`

func scanPolicy(row pgx.Row) (PolicyRow, error) {
	var p PolicyRow
	err := row.Scan(&p.Scope, &p.HostID, &p.Rev, &p.EnabledMetrics, &p.IntervalS, &p.Mounts, &p.UpdatedAt)
	return p, err
}

// Policies 返回全局策略与全部主机覆盖项
func (s *Store) Policies(ctx context.Context) (PolicyRow, []PolicyRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+policyCols+` FROM sentinel.collection_policies ORDER BY scope, host_id`)
	if err != nil {
		return PolicyRow{}, nil, err
	}
	defer rows.Close()
	var global PolicyRow
	hosts := []PolicyRow{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return PolicyRow{}, nil, err
		}
		if p.Scope == "global" {
			global = p
			continue
		}
		hosts = append(hosts, p)
	}
	if err := rows.Err(); err != nil {
		return PolicyRow{}, nil, err
	}
	if global.Scope == "" {
		global = PolicyRow{Scope: "global", Rev: 1}
	}
	return global, hosts, nil
}

// SavePolicy 写入策略并递增版本号；scope=global 时 hostID 必须为空
func (s *Store) SavePolicy(ctx context.Context, scope, hostID string, enabled []string, intervalS int, mounts []string) (PolicyRow, error) {
	if scope != "global" && scope != "host" {
		return PolicyRow{}, errors.New("invalid policy scope")
	}
	if scope == "global" {
		hostID = ""
	}
	if enabled == nil {
		enabled = []string{}
	}
	if mounts == nil {
		mounts = []string{}
	}
	row, err := scanPolicy(s.pool.QueryRow(ctx, `
		INSERT INTO sentinel.collection_policies (scope, host_id, rev, enabled_metrics, interval_s, mounts, updated_at)
		VALUES ($1,$2,1,$3,$4,$5, now())
		ON CONFLICT (scope, host_id) DO UPDATE SET
			rev = sentinel.collection_policies.rev + 1,
			enabled_metrics = EXCLUDED.enabled_metrics,
			interval_s = EXCLUDED.interval_s,
			mounts = EXCLUDED.mounts,
			updated_at = now()
		RETURNING `+policyCols, scope, hostID, enabled, intervalS, mounts))
	if err != nil {
		return PolicyRow{}, err
	}
	return row, nil
}

// DeleteHostPolicy 删除主机覆盖，回落到全局策略
func (s *Store) DeleteHostPolicy(ctx context.Context, hostID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM sentinel.collection_policies WHERE scope='host' AND host_id=$1`, hostID)
	return err
}

// EffectivePolicy 合并全局与主机覆盖：主机字段为空时继承全局
func (s *Store) EffectivePolicy(ctx context.Context, hostID string) (EffectivePolicy, error) {
	global, hosts, err := s.Policies(ctx)
	if err != nil {
		return EffectivePolicy{}, err
	}
	eff := EffectivePolicy{
		EnabledMetrics: global.EnabledMetrics,
		IntervalS:      global.IntervalS,
		Mounts:         global.Mounts,
		Source:         "global",
		Rev:            fmt.Sprintf("g%d", global.Rev),
	}
	for _, h := range hosts {
		if h.HostID != hostID {
			continue
		}
		eff.Source = "host"
		eff.Rev = fmt.Sprintf("g%d.h%d", global.Rev, h.Rev)
		if len(h.EnabledMetrics) > 0 {
			eff.EnabledMetrics = h.EnabledMetrics
		}
		if h.IntervalS > 0 {
			eff.IntervalS = h.IntervalS
		}
		if len(h.Mounts) > 0 {
			eff.Mounts = h.Mounts
		}
		break
	}
	return eff, nil
}
