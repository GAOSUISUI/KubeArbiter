-- KubeArbiter 数据库 schema（M0 冻结草案）
-- 目标：PostgreSQL 16
-- 约定：
--   1. 主键统一 uuid（gen_random_uuid），日志类表用 bigserial
--   2. 时间统一 timestamptz，服务端以 UTC 写入
--   3. 所有决策类 jsonb 必须携带 {"schema_version": 1, ...}
--   4. 枚举用 VARCHAR + CHECK，取值与 docs/01-algorithm-spec.md 第 7 节逐字一致
--   5. 资源量统一整数：cpu = millicore，memory = byte

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ============================================================
-- 1. 租户与配额
-- ============================================================

CREATE TABLE tenants (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        varchar(63)  NOT NULL UNIQUE,
    weight      numeric(6,3) NOT NULL DEFAULT 1.0 CHECK (weight > 0),
    labels      jsonb        NOT NULL DEFAULT '{"schema_version":1}'::jsonb,
    created_at  timestamptz  NOT NULL DEFAULT now(),
    updated_at  timestamptz  NOT NULL DEFAULT now()
);
COMMENT ON TABLE tenants IS '租户；weight 用于加权 DRF';

CREATE TABLE quotas (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    resource    varchar(16) NOT NULL CHECK (resource IN ('cpu','memory','gpu')),
    hard        bigint      NOT NULL CHECK (hard >= 0),           -- 硬上限（millicore / byte / GPU 整卡数）
    fair_share  numeric(8,6) NOT NULL CHECK (fair_share >= 0 AND fair_share <= 1), -- 占集群比例
    min_share   bigint      NOT NULL DEFAULT 0 CHECK (min_share >= 0),  -- DRF 保底
    max_share   bigint      CHECK (max_share IS NULL OR max_share >= min_share),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, resource)
);
COMMENT ON COLUMN quotas.hard IS 'ResourceQuota 语义的硬上限；DRF 的 max_share 可小于 hard';
COMMENT ON COLUMN quotas.fair_share IS '主导份额的目标占比，放置评分 s_fair 的分母';

-- ============================================================
-- 2. 调度策略
-- ============================================================

CREATE TABLE policies (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        varchar(63) NOT NULL UNIQUE,
    strategy    varchar(16) NOT NULL
                CHECK (strategy IN ('BALANCED','BIN_PACK','FAIR_SHARE','LOAD_AWARE')),
    weights     jsonb       NOT NULL,
    fairness_mode varchar(16) NOT NULL DEFAULT 'DRF'
                CHECK (fairness_mode IN ('DRF','WEIGHTED_SHARE')),
    is_default  boolean     NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT policies_weights_versioned CHECK (weights ? 'schema_version')
);
-- 同一个 strategy 只允许一个默认策略
CREATE UNIQUE INDEX uq_policies_default ON policies (strategy) WHERE is_default;

-- 四组预设权重（与算法规范 2.4 一致）
INSERT INTO policies (name, strategy, weights, is_default) VALUES
('balanced-default', 'BALANCED',
 '{"schema_version":1,"s_cpu":0.18,"s_mem":0.18,"s_balance":0.25,"s_pack":0.10,"s_load":0.15,"s_fair":0.10,"s_frag":0.04}', true),
('binpack-default', 'BIN_PACK',
 '{"schema_version":1,"s_cpu":0.10,"s_mem":0.10,"s_balance":0.05,"s_pack":0.35,"s_load":0.05,"s_fair":0.05,"s_frag":0.30}', true),
('fairshare-default', 'FAIR_SHARE',
 '{"schema_version":1,"s_cpu":0.10,"s_mem":0.10,"s_balance":0.08,"s_pack":0.05,"s_load":0.05,"s_fair":0.52,"s_frag":0.10}', true),
('loadaware-default', 'LOAD_AWARE',
 '{"schema_version":1,"s_cpu":0.10,"s_mem":0.10,"s_balance":0.12,"s_pack":0.06,"s_load":0.50,"s_fair":0.07,"s_frag":0.05}', true);

-- ============================================================
-- 3. 容量快照与放置决策
-- ============================================================

CREATE TABLE capacity_snapshots (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts          timestamptz NOT NULL,
    payload     jsonb       NOT NULL,
    hash        char(64)    NOT NULL,          -- sha256(canonical_json)
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT snapshots_payload_versioned CHECK (payload ? 'schema_version')
);
CREATE INDEX idx_snapshots_ts   ON capacity_snapshots (ts DESC);
CREATE INDEX idx_snapshots_hash ON capacity_snapshots (hash);

CREATE TABLE placement_decisions (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workload      varchar(253) NOT NULL,
    namespace     varchar(63)  NOT NULL,
    tenant_id     uuid REFERENCES tenants(id) ON DELETE SET NULL,
    policy_id     uuid REFERENCES policies(id) ON DELETE SET NULL,
    status        varchar(16)  NOT NULL CHECK (status IN ('OK','NO_CANDIDATE')),
    chosen_node   varchar(253),
    degraded      boolean      NOT NULL DEFAULT false,
    snapshot_hash char(64)     NOT NULL,
    scores        jsonb        NOT NULL,       -- [{node, rank, score, breakdown{}}]
    filtered      jsonb        NOT NULL,       -- [{node, reason, evidence}]
    evidence      jsonb        NOT NULL,       -- 指标快照摘要、权重、耗时
    created_at    timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT decisions_scores_versioned   CHECK (scores   ? 'schema_version'),
    CONSTRAINT decisions_filtered_versioned CHECK (filtered ? 'schema_version'),
    CONSTRAINT decisions_evidence_versioned CHECK (evidence ? 'schema_version'),
    CONSTRAINT decisions_chosen_required CHECK (status <> 'OK' OR chosen_node IS NOT NULL)
);
CREATE INDEX idx_decisions_created ON placement_decisions (created_at DESC);
CREATE INDEX idx_decisions_tenant  ON placement_decisions (tenant_id, created_at DESC);

CREATE TABLE arbitration_plans (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ts             timestamptz NOT NULL,
    allocator      varchar(16) NOT NULL CHECK (allocator IN ('FIFO','HARD_QUOTA','DRF')),
    allocations    jsonb       NOT NULL,       -- [{tenant_id, cpu, memory, dominant_share}]
    jain_index     numeric(6,5) NOT NULL CHECK (jain_index >= 0 AND jain_index <= 1),
    cpu_jain       numeric(6,5) NOT NULL,
    mem_jain       numeric(6,5) NOT NULL,
    utilization    numeric(6,5) NOT NULL CHECK (utilization >= 0),
    infeasible     boolean     NOT NULL DEFAULT false,
    preemption     jsonb       NOT NULL DEFAULT '{"schema_version":1,"candidates":[]}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT arbitration_allocations_versioned  CHECK (allocations  ? 'schema_version'),
    CONSTRAINT arbitration_preemption_versioned   CHECK (preemption   ? 'schema_version')
);
CREATE INDEX idx_arbitration_ts ON arbitration_plans (ts DESC);

-- ============================================================
-- 4. SLO 与容量建议
-- ============================================================

CREATE TABLE slo_definitions (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service              varchar(253) NOT NULL,
    sli_type             varchar(16)  NOT NULL CHECK (sli_type IN ('AVAILABILITY','LATENCY')),
    target               numeric(6,5) NOT NULL CHECK (target > 0 AND target < 1),
    window_seconds       integer      NOT NULL CHECK (window_seconds > 0),
    latency_threshold_ms integer      CHECK (latency_threshold_ms IS NULL OR latency_threshold_ms > 0),
    created_at           timestamptz  NOT NULL DEFAULT now(),
    updated_at           timestamptz  NOT NULL DEFAULT now(),
    UNIQUE (service, sli_type),
    CONSTRAINT slo_latency_threshold CHECK (sli_type <> 'LATENCY' OR latency_threshold_ms IS NOT NULL)
);
COMMENT ON COLUMN slo_definitions.target IS '如 0.995 表示 99.5%';

-- 1 分钟粒度预聚合；UI 查询只走这张表
CREATE TABLE slo_windows (
    id            bigserial PRIMARY KEY,
    service       varchar(253) NOT NULL,
    window_start  timestamptz  NOT NULL,
    total         bigint       NOT NULL CHECK (total >= 0),
    errors        bigint       NOT NULL CHECK (errors >= 0),
    latency_ok    bigint       NOT NULL DEFAULT 0 CHECK (latency_ok >= 0),
    created_at    timestamptz  NOT NULL DEFAULT now(),
    UNIQUE (service, window_start),
    CONSTRAINT slo_window_counts_valid CHECK (errors <= total AND latency_ok <= total)
);
CREATE INDEX idx_slo_windows_service_ts ON slo_windows (service, window_start DESC);

CREATE TABLE scaling_recommendations (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    service             varchar(253) NOT NULL,
    predicted_p99_ms    numeric(10,2),
    current_p99_ms      numeric(10,2),
    mape                numeric(6,3)  CHECK (mape IS NULL OR mape >= 0),
    replicas_current    integer     NOT NULL CHECK (replicas_current >= 0),
    replicas_suggested  integer     CHECK (replicas_suggested IS NULL OR replicas_suggested >= 0),
    confidence          varchar(16) NOT NULL
                        CHECK (confidence IN ('HIGH','MEDIUM','LOW','UNUSABLE')),
    note                text,                     -- 不可信原因、外推标记
    curve               jsonb       NOT NULL,     -- 原始 (c, p99) 测量点
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT scaling_curve_versioned CHECK (curve ? 'schema_version'),
    -- 不可信时不得给出副本数
    CONSTRAINT scaling_unusable_no_replicas
        CHECK (confidence <> 'UNUSABLE' OR replicas_suggested IS NULL)
);
CREATE INDEX idx_scaling_service ON scaling_recommendations (service, created_at DESC);

-- ============================================================
-- 5. 渐进式发布
-- ============================================================

CREATE TABLE releases (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    app          varchar(253) NOT NULL,
    namespace    varchar(63)  NOT NULL,
    from_rev     integer      NOT NULL CHECK (from_rev > 0),
    to_rev       integer      NOT NULL CHECK (to_rev > 0),
    state        varchar(16)  NOT NULL
                 CHECK (state IN ('PENDING','VALIDATING','CANARY','ANALYZING','PROMOTING',
                                  'COMPLETED','ROLLING_BACK','ROLLED_BACK','FAILED')),
    traffic_mode varchar(16)  NOT NULL DEFAULT 'HTTP_CANARY'
                 CHECK (traffic_mode IN ('HTTP_CANARY','REPLICA_RATIO')),
    started_at   timestamptz  NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    created_at   timestamptz  NOT NULL DEFAULT now(),
    updated_at   timestamptz  NOT NULL DEFAULT now(),
    CONSTRAINT releases_rev_differs CHECK (from_rev <> to_rev)
);
CREATE INDEX idx_releases_state ON releases (state, started_at DESC);

CREATE TABLE release_steps (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    release_id  uuid    NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    step        integer NOT NULL CHECK (step >= 0),
    weight      integer NOT NULL CHECK (weight BETWEEN 0 AND 100),
    verdict     varchar(20) NOT NULL
                CHECK (verdict IN ('PROCEED','ROLLBACK','HOLD','INSUFFICIENT_DATA')),
    samples     bigint  NOT NULL DEFAULT 0 CHECK (samples >= 0),
    evidence    jsonb   NOT NULL,   -- baseline vs canary 的错误率/p99/就绪数/预算消耗
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (release_id, step),
    CONSTRAINT release_steps_evidence_versioned CHECK (evidence ? 'schema_version')
);

-- ============================================================
-- 6. 事件与诊断
-- ============================================================

CREATE TABLE incidents (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    target      varchar(253) NOT NULL,     -- 服务或工作负载名
    namespace   varchar(63)  NOT NULL,
    type        varchar(32)  NOT NULL
                CHECK (type IN ('CRASH_LOOP','IMAGE_PULL_BACKOFF','READINESS_FAIL','OOM_KILLED',
                                'DOWNSTREAM_TIMEOUT','EMPTY_ENDPOINTS','CONFIG_ERROR',
                                'NODE_NOT_READY','NODE_PRESSURE','NODE_HIGH_USAGE','METRICS_STALE',
                                'POD_PENDING_LONG','POD_READINESS_FAIL','QUOTA_NEAR',
                                'NODE_FORECAST_EXHAUST')),
    severity    varchar(16)  NOT NULL CHECK (severity IN ('INFO','WARN','CRITICAL')),
    status      varchar(16)  NOT NULL DEFAULT 'OPEN' CHECK (status IN ('OPEN','RESOLVED')),
    first_seen  timestamptz  NOT NULL,
    resolved_at timestamptz,
    created_at  timestamptz  NOT NULL DEFAULT now()
);
CREATE INDEX idx_incidents_seen ON incidents (first_seen DESC);
CREATE INDEX idx_incidents_status ON incidents (status, severity);

CREATE TABLE diagnosis_reports (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    facts       jsonb NOT NULL,   -- 可直接观测的事实
    inferences  jsonb NOT NULL,   -- [{root_cause, evidence, confidence, counter_evidence}]
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT diagnosis_facts_versioned      CHECK (facts      ? 'schema_version'),
    CONSTRAINT diagnosis_inferences_versioned CHECK (inferences ? 'schema_version')
);
CREATE INDEX idx_diagnosis_incident ON diagnosis_reports (incident_id, created_at DESC);

CREATE TABLE recovery_actions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    action      varchar(64) NOT NULL,      -- ROLLBACK_RELEASE / RAISE_LIMIT_SUGGESTION / ...
    task_id     uuid,
    approved_by varchar(63),
    result      varchar(16) NOT NULL DEFAULT 'PENDING'
                CHECK (result IN ('PENDING','SUCCEEDED','FAILED','SKIPPED')),
    detail      text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);

-- ============================================================
-- 7. 任务与工作流
-- ============================================================

CREATE TABLE tasks (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    type            varchar(32) NOT NULL
                    CHECK (type IN ('PLACEMENT_APPLY','PREEMPT','SCALE','RELEASE_PROMOTE',
                                    'RELEASE_ABORT','RECOVERY','CONFIG_ROLLBACK')),
    status          varchar(20) NOT NULL DEFAULT 'PENDING'
                    CHECK (status IN ('PENDING','PENDING_APPROVAL','QUEUED','RUNNING','RETRYING',
                                      'SUCCEEDED','FAILED','CANCELLED')),
    params          jsonb       NOT NULL,
    idempotency_key varchar(128),
    request_hash    char(64),              -- 幂等键相同但请求体不同时用于判冲突
    retry_count     integer     NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    max_retries     integer     NOT NULL DEFAULT 3 CHECK (max_retries >= 0),
    timeout_seconds integer     NOT NULL DEFAULT 300 CHECK (timeout_seconds > 0),
    error           text,
    error_code      integer,
    created_by      varchar(63) NOT NULL,
    approved_by     varchar(63),
    approved_at     timestamptz,
    started_at      timestamptz,
    finished_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tasks_params_versioned CHECK (params ? 'schema_version'),
    -- 审批强制：危险类型任务进入 QUEUED 及之后的状态前必须先有 approved_by，
    -- 该规则依赖 params 中的 target_labels（是否为测试工作负载），无法用 CHECK 表达，
    -- 由 taskengine 在状态跃迁时强制校验并用单测覆盖。
    CONSTRAINT tasks_finished_requires_time
        CHECK (status NOT IN ('SUCCEEDED','FAILED','CANCELLED') OR finished_at IS NOT NULL)
);
-- 幂等：同一 key 只允许一条；重复提交返回已有记录
CREATE UNIQUE INDEX uq_tasks_idempotency ON tasks (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
-- 任务领取：FOR UPDATE SKIP LOCKED 的支撑索引
CREATE INDEX idx_tasks_claim ON tasks (status, created_at)
    WHERE status IN ('QUEUED','RETRYING');
CREATE INDEX idx_tasks_status ON tasks (status, updated_at DESC);

CREATE TABLE task_logs (
    id      bigserial PRIMARY KEY,
    task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    ts      timestamptz NOT NULL DEFAULT now(),
    level   varchar(8)  NOT NULL CHECK (level IN ('DEBUG','INFO','WARN','ERROR')),
    message text        NOT NULL
);
CREATE INDEX idx_task_logs_task ON task_logs (task_id, ts);

-- ============================================================
-- 8. 写接口幂等
-- ============================================================

-- 通用写接口幂等记录：scope 为"方法 + 实际路径"，request_hash 为请求体 sha256。
-- 同键同 scope 同 hash 重放 response；任一不符返回 40901。
CREATE TABLE idempotency_keys (
    idempotency_key varchar(128) PRIMARY KEY,
    scope           text         NOT NULL,
    request_hash    char(64)     NOT NULL,
    status          integer      NOT NULL,
    response        jsonb,                       -- 204 等无响应体时为 NULL
    created_at      timestamptz  NOT NULL DEFAULT now()
);

-- ============================================================
-- 9. 审计（只插入，不可修改）
-- ============================================================

CREATE TABLE audit_logs (
    id          bigserial PRIMARY KEY,
    actor       varchar(63) NOT NULL,
    action      varchar(64) NOT NULL,
    object_type varchar(64) NOT NULL,
    object_id   varchar(128),
    before      jsonb,
    after       jsonb,
    trace_id    varchar(64),
    ts          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_ts     ON audit_logs (ts DESC);
CREATE INDEX idx_audit_object ON audit_logs (object_type, object_id);

CREATE OR REPLACE FUNCTION audit_logs_immutable() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is append-only';
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_audit_logs_immutable
    BEFORE UPDATE OR DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION audit_logs_immutable();

-- ============================================================
-- 10. 用户与权限
-- ============================================================

CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    username      varchar(63) NOT NULL UNIQUE,
    password_hash varchar(255) NOT NULL,
    display_name  varchar(63) NOT NULL DEFAULT '',
    status        varchar(16) NOT NULL DEFAULT 'ACTIVE'
                  CHECK (status IN ('ACTIVE','DISABLED')),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE roles (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        varchar(63) NOT NULL UNIQUE,   -- admin / operator / viewer
    description text NOT NULL DEFAULT '',
    permissions jsonb NOT NULL DEFAULT '{"schema_version":1,"items":[]}'::jsonb,
    CONSTRAINT roles_permissions_versioned CHECK (permissions ? 'schema_version')
);

CREATE TABLE role_bindings (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role_id    uuid NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
    scope      varchar(128) NOT NULL DEFAULT '*',   -- tenant:<id> / namespace:<ns> / *
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, role_id, scope)
);

INSERT INTO roles (name, description) VALUES
('admin',    '全部权限，含策略与租户管理'),
('operator', '可创建任务、审批、发布与回滚，不可改策略'),
('viewer',   '只读');
