CREATE SCHEMA IF NOT EXISTS sentinel;

CREATE TABLE IF NOT EXISTS sentinel.hosts (
    id          text PRIMARY KEY,
    name        text NOT NULL DEFAULT '',
    os          text NOT NULL DEFAULT '',
    arch        text NOT NULL DEFAULT '',
    kind        text NOT NULL DEFAULT 'agent',   -- agent | snmp | http | probe
    addr        text NOT NULL DEFAULT '',
    token_hash  text NOT NULL DEFAULT '',
    labels      jsonb NOT NULL DEFAULT '{}'::jsonb,
    meta        jsonb NOT NULL DEFAULT '{}'::jsonb,
    frozen      boolean NOT NULL DEFAULT false,
    first_seen  timestamptz NOT NULL DEFAULT now(),
    last_seen   timestamptz
);

-- 原始样本：按天分区，UNLOGGED 降低写放大；48h 后整分区 DROP
CREATE TABLE IF NOT EXISTS sentinel.metric_samples (
    host_id text NOT NULL,
    metric  text NOT NULL,
    ts      timestamptz NOT NULL,
    value   double precision NOT NULL,
    src     text NOT NULL DEFAULT 'agent'
) PARTITION BY RANGE (ts);

CREATE INDEX IF NOT EXISTS metric_samples_ts_brin ON sentinel.metric_samples USING brin (ts);

-- 每主机每指标最新值，供主机墙与规则即时判定
CREATE TABLE IF NOT EXISTS sentinel.metric_latest (
    host_id text NOT NULL,
    metric  text NOT NULL,
    ts      timestamptz NOT NULL,
    value   double precision NOT NULL,
    src     text NOT NULL DEFAULT 'agent',
    PRIMARY KEY (host_id, metric)
);

-- 每次 Agent 低频上报的系统明细快照（IO/CPU/内存/网络/负载），只保留最新值
CREATE TABLE IF NOT EXISTS sentinel.host_io_latest (
    host_id text PRIMARY KEY,
    ts      timestamptz NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE TABLE IF NOT EXISTS sentinel.metric_rollup_1m (
    host_id text NOT NULL,
    metric  text NOT NULL,
    bucket  timestamptz NOT NULL,
    avg     double precision NOT NULL,
    min     double precision NOT NULL,
    max     double precision NOT NULL,
    n       integer NOT NULL,
    PRIMARY KEY (host_id, metric, bucket)
);

CREATE TABLE IF NOT EXISTS sentinel.events (
    id           bigserial PRIMARY KEY,
    fingerprint  text NOT NULL,
    host_id      text NOT NULL,
    rule_id      text NOT NULL,
    severity     text NOT NULL DEFAULT 'warning',  -- warning | critical
    status       text NOT NULL DEFAULT 'open',     -- open | resolved
    title        text NOT NULL DEFAULT '',
    detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
    value        double precision,
    opened_at    timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    resolved_at  timestamptz,
    cooldown_until timestamptz
);
CREATE UNIQUE INDEX IF NOT EXISTS events_one_open ON sentinel.events (fingerprint) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS events_ts ON sentinel.events (opened_at DESC);
CREATE INDEX IF NOT EXISTS events_host ON sentinel.events (host_id, opened_at DESC);

CREATE TABLE IF NOT EXISTS sentinel.decisions (
    id             bigserial PRIMARY KEY,
    ts             timestamptz NOT NULL DEFAULT now(),
    run_id         bigint,
    stage_run_id   bigint,
    actor_kind     text NOT NULL,                 -- rule | ai | human | policy | system
    actor          text NOT NULL,
    rule_id        text,
    rule_rev       text,
    action_id      text,
    risk           text,
    decision       text NOT NULL,                 -- auto_execute | escalate_ai | ask_human | approve | reject | skip | rollback | suppressed | open | resolve
    rationale      text,
    inputs         jsonb,
    evidence       jsonb,
    outcome        text,
    latency_ms     int,
    correlation_id text
);
CREATE INDEX IF NOT EXISTS decisions_ts ON sentinel.decisions (ts DESC);
CREATE INDEX IF NOT EXISTS decisions_host ON sentinel.decisions ((inputs->>'host_id'), ts DESC);

CREATE TABLE IF NOT EXISTS sentinel.stream_events (
    seq     bigserial PRIMARY KEY,
    ts      timestamptz NOT NULL DEFAULT now(),
    type    text NOT NULL,
    host_id text,
    run_id  bigint,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS stream_events_ts ON sentinel.stream_events (ts DESC);

CREATE TABLE IF NOT EXISTS sentinel.config_versions (
    kind        text NOT NULL,
    rev         text NOT NULL,
    content     text NOT NULL,
    loaded_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (kind, rev)
);

CREATE TABLE IF NOT EXISTS sentinel.collector_state (
    host_id     text PRIMARY KEY,
    failures    integer NOT NULL DEFAULT 0,
    last_error  text NOT NULL DEFAULT '',
    last_ok_at  timestamptz,
    next_run_at timestamptz NOT NULL DEFAULT now()
);

-- 面板动态添加的节点（当前为 probe）；与 collectors.yaml 合并后由采集器调度
CREATE TABLE IF NOT EXISTS sentinel.collector_targets (
    host_id     text PRIMARY KEY,
    name        text NOT NULL DEFAULT '',
    type        text NOT NULL DEFAULT 'probe',
    target      text NOT NULL,
    interval_s  integer NOT NULL DEFAULT 30,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- 采集策略：global 为默认，host 行按主机覆盖；空字段表示继承
CREATE TABLE IF NOT EXISTS sentinel.collection_policies (
    scope           text NOT NULL,                 -- global | host
    host_id         text NOT NULL DEFAULT '',
    rev             integer NOT NULL DEFAULT 1,
    enabled_metrics text[] NOT NULL DEFAULT '{}',  -- 空 = 全量采集
    interval_s      integer NOT NULL DEFAULT 0,     -- 0 = 沿用 Agent 本地配置
    mounts          text[] NOT NULL DEFAULT '{}',  -- 空 = 沿用 Agent 本地配置
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, host_id)
);

INSERT INTO sentinel.collection_policies (scope, host_id)
VALUES ('global', '')
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS sentinel.consumer_offsets (
    consumer_id text PRIMARY KEY,
    seq         bigint NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sentinel.audit_log (
    id      bigserial PRIMARY KEY,
    ts      timestamptz NOT NULL DEFAULT now(),
    actor   text NOT NULL,
    action  text NOT NULL,
    target  text,
    detail  jsonb
);

-- 单分区唯一键：spool 补传时按 host+metric+ts 幂等
CREATE OR REPLACE FUNCTION sentinel.ensure_partition(day date) RETURNS void AS $$
DECLARE
    part text := 'metric_samples_' || to_char(day, 'YYYYMMDD');
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_class WHERE relname = part) THEN
        EXECUTE format(
            'CREATE UNLOGGED TABLE IF NOT EXISTS sentinel.%I PARTITION OF sentinel.metric_samples FOR VALUES FROM (%L) TO (%L)',
            part, day::timestamptz, (day + 1)::timestamptz);
        EXECUTE format('CREATE UNIQUE INDEX IF NOT EXISTS %I ON sentinel.%I (host_id, metric, ts)', part || '_uk', part);
        EXECUTE format('CREATE INDEX IF NOT EXISTS %I ON sentinel.%I (host_id, metric, ts DESC)', part || '_q', part);
    END IF;
END;
$$ LANGUAGE plpgsql;
