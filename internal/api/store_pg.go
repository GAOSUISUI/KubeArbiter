package api

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/slo"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// jsonDoc 是库里所有 jsonb 列的统一信封：{"schema_version":1,"items":[...]}，
// 与 db/schema.sql 的 *_versioned CHECK 约束对应。
const jsonSchemaVersion = 1

type itemsDoc[T any] struct {
	SchemaVersion int `json:"schema_version"`
	Items         []T `json:"items"`
}

type evidenceDoc struct {
	SchemaVersion int                     `json:"schema_version"`
	Warnings      []schedcore.WarningCode `json:"warnings,omitempty"`
	Detail        map[string]any          `json:"detail,omitempty"`
}

// PGStore 是 PolicyReader / DecisionReader / DecisionWriter 的 PostgreSQL 实现。
// M1 的内存实现保留给单测：推开库跑 handler 逻辑不必依赖真实实例。
type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

var (
	_ PolicyReader        = (*PGStore)(nil)
	_ DecisionReader      = (*PGStore)(nil)
	_ DecisionWriter      = (*PGStore)(nil)
	_ TenantReader        = (*PGStore)(nil)
	_ TenantLister        = (*PGStore)(nil)
	_ SnapshotWriter      = (*PGStore)(nil)
	_ SLOReader           = (*PGStore)(nil)
	_ SLODefinitionWriter = (*PGStore)(nil)
	_ IdempotencyStore    = (*PGStore)(nil)
	_ TaskReader          = (*PGStore)(nil)
	_ TaskWriter          = (*PGStore)(nil)
	_ AuditReader         = (*PGStore)(nil)
	_ AuditWriter         = (*PGStore)(nil)
)

// ListTenants 返回全部租户及硬配额，按名称排序。
func (s *PGStore) ListTenants(ctx context.Context) ([]TenantInfo, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id::text, t.name, t.weight::float8, coalesce(qc.fair_share, 0)::float8,
		       coalesce(qc.hard, 0), coalesce(qm.hard, 0)
		FROM tenants t
		LEFT JOIN quotas qc ON qc.tenant_id = t.id AND qc.resource = 'cpu'
		LEFT JOIN quotas qm ON qm.tenant_id = t.id AND qm.resource = 'memory'
		ORDER BY t.name, t.id`)
	if err != nil {
		return nil, fmt.Errorf("查询租户列表: %w", err)
	}
	defer rows.Close()

	out := make([]TenantInfo, 0, 8)
	for rows.Next() {
		var (
			t                TenantInfo
			cpuHard, memHard int64
		)
		if err := rows.Scan(&t.ID, &t.Name, &t.Weight, &t.FairShare, &cpuHard, &memHard); err != nil {
			return nil, fmt.Errorf("扫描租户: %w", err)
		}
		if cpuHard > 0 || memHard > 0 {
			t.Quota = schedcore.Resources{CPUMilli: cpuHard, MemBytes: memHard}
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// snapshotDoc 是 capacity_snapshots.payload 的形状，与 db/schema.sql 的版本约束一致。
type snapshotDoc struct {
	SchemaVersion int               `json:"schema_version"`
	Snapshot      capacity.Snapshot `json:"snapshot"`
}

// SaveSnapshot 幂等写入：同一 hash 已存在则跳过，避免每次模拟都新增一行。
func (s *PGStore) SaveSnapshot(ctx context.Context, snap capacity.Snapshot, hash string) error {
	payload, err := json.Marshal(snapshotDoc{SchemaVersion: jsonSchemaVersion, Snapshot: snap})
	if err != nil {
		return fmt.Errorf("序列化快照: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO capacity_snapshots (ts, payload, hash)
		SELECT $1, $2, $3
		WHERE NOT EXISTS (SELECT 1 FROM capacity_snapshots WHERE hash = $3)`,
		snap.TakenAt, payload, hash)
	if err != nil {
		return fmt.Errorf("写入快照: %w", err)
	}
	return nil
}

// GetTenant 读取租户权重与配额。配额的 cpu / memory / gpu 三行分别映射到 Resources，
// FairShare 取 cpu 行（算法规范中主导份额按 CPU 归一化）；缺少配额行时按"不限"处理。
func (s *PGStore) GetTenant(ctx context.Context, id string) (schedcore.TenantState, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return schedcore.TenantState{}, false, nil
	}
	var (
		t       schedcore.TenantState
		weight  float64
		cpuHard int64
		memHard int64
		gpuHard int64
		fair    float64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.weight::float8,
		       coalesce(qc.hard, 0), coalesce(qm.hard, 0), coalesce(qg.hard, 0), coalesce(qc.fair_share, 0)::float8
		FROM tenants t
		LEFT JOIN quotas qc ON qc.tenant_id = t.id AND qc.resource = 'cpu'
		LEFT JOIN quotas qm ON qm.tenant_id = t.id AND qm.resource = 'memory'
		LEFT JOIN quotas qg ON qg.tenant_id = t.id AND qg.resource = 'gpu'
		WHERE t.id = $1`, id).
		Scan(&t.ID, &weight, &cpuHard, &memHard, &gpuHard, &fair)
	if err == pgx.ErrNoRows {
		return schedcore.TenantState{}, false, nil
	}
	if err != nil {
		return schedcore.TenantState{}, false, fmt.Errorf("查询租户: %w", err)
	}
	t.Weight = weight
	t.FairShare = fair
	if cpuHard > 0 || memHard > 0 || gpuHard > 0 {
		t.Quota = schedcore.Resources{CPUMilli: cpuHard, MemBytes: memHard, GPU: gpuHard}
	}
	return t, true, nil
}

func (s *PGStore) ListPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, strategy, weights, COALESCE(fairness_mode,'DRF'), is_default
		FROM policies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("查询策略: %w", err)
	}
	defer rows.Close()

	out := make([]Policy, 0, 4)
	for rows.Next() {
		var (
			p        Policy
			strategy string
			fairMode string
			raw      []byte
		)
		if err := rows.Scan(&p.ID, &p.Name, &strategy, &raw, &fairMode, &p.IsDefault); err != nil {
			return nil, fmt.Errorf("扫描策略: %w", err)
		}
		p.Strategy = schedcore.Strategy(strategy)
		p.FairnessMode = schedcore.FairMode(fairMode)
		if p.Weights, err = decodeWeights(raw); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *PGStore) GetPolicy(ctx context.Context, id string) (Policy, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Policy{}, false, nil
	}
	var (
		p        Policy
		strategy string
		fairMode string
		raw      []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, strategy, weights, COALESCE(fairness_mode,'DRF'), is_default
		FROM policies WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &strategy, &raw, &fairMode, &p.IsDefault)
	if err == pgx.ErrNoRows {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("查询策略: %w", err)
	}
	p.Strategy = schedcore.Strategy(strategy)
	p.FairnessMode = schedcore.FairMode(fairMode)
	if p.Weights, err = decodeWeights(raw); err != nil {
		return Policy{}, false, err
	}
	return p, true, nil
}

func (s *PGStore) ListDecisions(ctx context.Context, f DecisionFilter) ([]PlacementDecision, error) {
	q := `
		SELECT id::text, workload, namespace, tenant_id::text, policy_id::text,
		       status, chosen_node, degraded, snapshot_hash, scores, filtered, evidence, created_at
		FROM placement_decisions
		WHERE ($1::uuid IS NULL OR tenant_id = $1::uuid)
		  AND ($2::uuid IS NULL OR policy_id = $2::uuid)
		ORDER BY created_at DESC, id DESC`
	rows, err := s.pool.Query(ctx, q, nullableUUID(f.TenantID), nullableUUID(f.PolicyID))
	if err != nil {
		return nil, fmt.Errorf("查询决策: %w", err)
	}
	defer rows.Close()

	out := make([]PlacementDecision, 0, 20)
	for rows.Next() {
		d, err := scanDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *PGStore) GetDecision(ctx context.Context, id string) (PlacementDecision, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PlacementDecision{}, false, nil
	}
	q := `
		SELECT id::text, workload, namespace, tenant_id::text, policy_id::text,
		       status, chosen_node, degraded, snapshot_hash, scores, filtered, evidence, created_at
		FROM placement_decisions WHERE id = $1`
	d, err := scanDecision(s.pool.QueryRow(ctx, q, id))
	if err == pgx.ErrNoRows {
		return PlacementDecision{}, false, nil
	}
	if err != nil {
		return PlacementDecision{}, false, err
	}
	return d, true, nil
}

func (s *PGStore) CreateDecision(ctx context.Context, in PlacementDecision) (PlacementDecision, error) {
	workload, namespace := "", ""
	if in.Workload != nil {
		workload, namespace = in.Workload.Name, in.Workload.Namespace
	}
	scores, err := json.Marshal(itemsDoc[schedcore.NodeScore]{SchemaVersion: jsonSchemaVersion, Items: in.Scores})
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("序列化 scores: %w", err)
	}
	filtered, err := json.Marshal(itemsDoc[schedcore.FilteredNode]{SchemaVersion: jsonSchemaVersion, Items: in.Filtered})
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("序列化 filtered: %w", err)
	}
	evidence, err := json.Marshal(evidenceDoc{SchemaVersion: jsonSchemaVersion, Warnings: in.Warnings, Detail: in.Evidence})
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("序列化 evidence: %w", err)
	}

	var policyID any
	if in.PolicyID != "" {
		policyID = in.PolicyID
	}
	q := `
		INSERT INTO placement_decisions
		    (workload, namespace, tenant_id, policy_id, status, chosen_node,
		     degraded, snapshot_hash, scores, filtered, evidence)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id::text, created_at`
	err = s.pool.QueryRow(ctx, q,
		workload, namespace, in.TenantID, policyID, string(in.Status), in.ChosenNode,
		in.Degraded, in.SnapshotHash, scores, filtered, evidence,
	).Scan(&in.ID, &in.CreatedAt)
	if err != nil {
		return PlacementDecision{}, fmt.Errorf("写入决策: %w", err)
	}
	return in, nil
}

// scanDecision 同时适配 pgx.Row 与 pgx.Rows，避免两份重复的 Scan 代码。
type scannable interface {
	Scan(dest ...any) error
}

func scanDecision(row scannable) (PlacementDecision, error) {
	var (
		d                          PlacementDecision
		workload, namespace        string
		tenantID, policyID, chosen *string
		status                     string
		scores, filtered, evidence []byte
	)
	if err := row.Scan(&d.ID, &workload, &namespace, &tenantID, &policyID,
		&status, &chosen, &d.Degraded, &d.SnapshotHash, &scores, &filtered, &evidence, &d.CreatedAt); err != nil {
		return PlacementDecision{}, err
	}
	d.Workload = &WorkloadSpec{Name: workload, Namespace: namespace}
	d.Status = schedcore.Status(status)
	d.TenantID = tenantID
	d.ChosenNode = chosen
	if policyID != nil {
		d.PolicyID = *policyID
	}
	if err := decodeItems(scores, &d.Scores); err != nil {
		return PlacementDecision{}, err
	}
	if err := decodeItems(filtered, &d.Filtered); err != nil {
		return PlacementDecision{}, err
	}
	var ev evidenceDoc
	if err := json.Unmarshal(evidence, &ev); err != nil {
		return PlacementDecision{}, fmt.Errorf("解析 evidence: %w", err)
	}
	d.Warnings = ev.Warnings
	d.Evidence = ev.Detail
	return d, nil
}

func decodeItems[T any](raw []byte, dst *[]T) error {
	if len(raw) == 0 {
		return nil
	}
	var doc itemsDoc[T]
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("解析 jsonb items: %w", err)
	}
	*dst = doc.Items
	return nil
}

func decodeWeights(raw []byte) (schedcore.Weights, error) {
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		schedcore.Weights
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return schedcore.Weights{}, fmt.Errorf("解析策略权重: %w", err)
	}
	return doc.Weights, nil
}

func encodeWeights(w schedcore.Weights) ([]byte, error) {
	// 与 decode 对称：schema_version 固定为 1，CHECK 约束要求必须存在。
	return json.Marshal(struct {
		SchemaVersion int `json:"schema_version"`
		schedcore.Weights
	}{SchemaVersion: 1, Weights: w})
}

func (s *PGStore) CreatePolicy(ctx context.Context, p Policy) (Policy, error) {
	raw, err := encodeWeights(p.Weights)
	if err != nil {
		return Policy{}, fmt.Errorf("编码策略权重: %w", err)
	}
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	mode := p.FairnessMode
	if mode == "" {
		mode = schedcore.FairModeDRF
	}
	p.FairnessMode = mode
	_, err = s.pool.Exec(ctx, `
		INSERT INTO policies (id, name, strategy, weights, fairness_mode, is_default)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6)
		ON CONFLICT DO NOTHING`,
		p.ID, p.Name, string(p.Strategy), raw, string(mode), p.IsDefault)
	if err != nil {
		return Policy{}, fmt.Errorf("写入策略: %w", err)
	}
	return p, nil
}

func (s *PGStore) UpdatePolicy(ctx context.Context, p Policy) (Policy, error) {
	if _, err := uuid.Parse(p.ID); err != nil {
		return Policy{}, ErrNotFound
	}
	raw, err := encodeWeights(p.Weights)
	if err != nil {
		return Policy{}, fmt.Errorf("编码策略权重: %w", err)
	}
	mode := p.FairnessMode
	if mode == "" {
		mode = schedcore.FairModeDRF
	}
	p.FairnessMode = mode
	ct, err := s.pool.Exec(ctx, `
		UPDATE policies SET name=$2, strategy=$3, weights=$4::jsonb, fairness_mode=$5, is_default=$6, updated_at=now()
		WHERE id=$1`,
		p.ID, p.Name, string(p.Strategy), raw, string(mode), p.IsDefault)
	if err != nil {
		return Policy{}, fmt.Errorf("更新策略: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return Policy{}, ErrNotFound
	}
	return p, nil
}

func (s *PGStore) DeletePolicy(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	ct, err := s.pool.Exec(ctx, `DELETE FROM policies WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("删除策略: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListSLODefinitions 返回全部 SLO 定义，按服务与 SLI 类型排序。
func (s *PGStore) ListSLODefinitions(ctx context.Context) ([]slo.Definition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT service, sli_type, target::float8, window_seconds, latency_threshold_ms
		FROM slo_definitions ORDER BY service, sli_type`)
	if err != nil {
		return nil, fmt.Errorf("查询 SLO 定义: %w", err)
	}
	defer rows.Close()

	out := make([]slo.Definition, 0, 8)
	for rows.Next() {
		def, err := scanSLODefinition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, def)
	}
	return out, rows.Err()
}

// GetSLODefinition 同服务存在多个 SLI 定义时优先返回可用性，
// 与契约 SLODetail 只有单个 sli_type 的限制保持一致。
func (s *PGStore) GetSLODefinition(ctx context.Context, service string) (slo.Definition, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT service, sli_type, target::float8, window_seconds, latency_threshold_ms
		FROM slo_definitions WHERE service = $1
		ORDER BY CASE sli_type WHEN 'AVAILABILITY' THEN 0 ELSE 1 END
		LIMIT 1`, service)
	def, err := scanSLODefinition(row)
	if err == pgx.ErrNoRows {
		return slo.Definition{}, false, nil
	}
	if err != nil {
		return slo.Definition{}, false, err
	}
	return def, true, nil
}

// ListSLOBuckets 返回 [from, to) 内的 1 分钟预聚合窗口。
func (s *PGStore) ListSLOBuckets(ctx context.Context, service string, from, to time.Time) ([]slo.Bucket, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT window_start, total, errors, latency_ok
		FROM slo_windows
		WHERE service = $1 AND window_start >= $2 AND window_start < $3
		ORDER BY window_start`, service, from, to)
	if err != nil {
		return nil, fmt.Errorf("查询 SLO 窗口: %w", err)
	}
	defer rows.Close()

	out := make([]slo.Bucket, 0, 1440)
	for rows.Next() {
		var bucket slo.Bucket
		if err := rows.Scan(&bucket.Start, &bucket.Total, &bucket.Errors, &bucket.LatencyOK); err != nil {
			return nil, fmt.Errorf("扫描 SLO 窗口: %w", err)
		}
		out = append(out, bucket)
	}
	return out, rows.Err()
}

func scanSLODefinition(row scannable) (slo.Definition, error) {
	var (
		def           slo.Definition
		sliType       string
		windowSeconds int32
		threshold     *int32
	)
	if err := row.Scan(&def.Service, &sliType, &def.Target, &windowSeconds, &threshold); err != nil {
		return slo.Definition{}, err
	}
	def.SLIType = slo.SLIType(sliType)
	def.Window = time.Duration(windowSeconds) * time.Second
	if threshold != nil {
		ms := int(*threshold)
		def.LatencyThresholdMS = &ms
	}
	return def, nil
}

// CreateSLODefinition 依赖 (service, sli_type) 唯一约束；冲突时返回 ErrConflict。
func (s *PGStore) CreateSLODefinition(ctx context.Context, def slo.Definition) (slo.Definition, error) {
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO slo_definitions (service, sli_type, target, window_seconds, latency_threshold_ms)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (service, sli_type) DO NOTHING`,
		def.Service, string(def.SLIType), def.Target,
		int64(def.Window/time.Second), nullableInt(def.LatencyThresholdMS))
	if err != nil {
		return slo.Definition{}, fmt.Errorf("创建 SLO 定义: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return slo.Definition{}, ErrConflict
	}
	return def, nil
}

func (s *PGStore) UpdateSLODefinition(ctx context.Context, def slo.Definition) (slo.Definition, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE slo_definitions
		SET target = $3, window_seconds = $4, latency_threshold_ms = $5, updated_at = now()
		WHERE service = $1 AND sli_type = $2`,
		def.Service, string(def.SLIType), def.Target,
		int64(def.Window/time.Second), nullableInt(def.LatencyThresholdMS))
	if err != nil {
		return slo.Definition{}, fmt.Errorf("更新 SLO 定义: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return slo.Definition{}, ErrNotFound
	}
	return def, nil
}

func (s *PGStore) DeleteSLODefinition(ctx context.Context, service string, sli slo.SLIType) error {
	ct, err := s.pool.Exec(ctx, `
		DELETE FROM slo_definitions WHERE service = $1 AND sli_type = $2`,
		service, string(sli))
	if err != nil {
		return fmt.Errorf("删除 SLO 定义: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// GetIdempotentRecord 按幂等键读取已落库的响应；不存在时返回 found=false。
func (s *PGStore) GetIdempotentRecord(ctx context.Context, key string) (IdempotentRecord, bool, error) {
	var (
		record IdempotentRecord
		body   []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT scope, request_hash, status, response
		FROM idempotency_keys WHERE idempotency_key = $1`, key).
		Scan(&record.Scope, &record.RequestHash, &record.Status, &body)
	if err == pgx.ErrNoRows {
		return IdempotentRecord{}, false, nil
	}
	if err != nil {
		return IdempotentRecord{}, false, fmt.Errorf("查询幂等记录: %w", err)
	}
	record.Key = key
	record.Body = body
	return record, true, nil
}

// SaveIdempotentRecord 以主键冲突表达并发：先落库者胜出，后来者收到 ErrConflict 后回读。
func (s *PGStore) SaveIdempotentRecord(ctx context.Context, record IdempotentRecord) error {
	var body any
	if len(record.Body) > 0 {
		body = record.Body
	}
	ct, err := s.pool.Exec(ctx, `
		INSERT INTO idempotency_keys (idempotency_key, scope, request_hash, status, response)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		record.Key, record.Scope, record.RequestHash, record.Status, body)
	if err != nil {
		return fmt.Errorf("写入幂等记录: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// ---------- 任务 ----------

const taskColumns = `id, type, status, params, retry_count, max_retries, error, error_code,
	created_by, approved_by, approved_at, created_at, started_at, finished_at`

func (s *PGStore) ListTasks(ctx context.Context, f TaskFilter) ([]Task, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE ($1::text IS NULL OR status = $1)
		  AND ($2::text IS NULL OR type = $2)
		ORDER BY created_at DESC, id DESC`,
		nullableText(string(f.Status)), nullableText(string(f.Type)))
	if err != nil {
		return nil, fmt.Errorf("查询任务: %w", err)
	}
	defer rows.Close()

	out := make([]Task, 0)
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PGStore) GetTask(ctx context.Context, id string) (Task, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Task{}, false, nil
	}
	t, err := scanTask(s.pool.QueryRow(ctx, `
		SELECT `+taskColumns+` FROM tasks WHERE id = $1`, id))
	if err == pgx.ErrNoRows {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, fmt.Errorf("查询任务: %w", err)
	}
	return t, true, nil
}

func (s *PGStore) ListTaskLogs(ctx context.Context, taskID string) ([]TaskLog, error) {
	if _, err := uuid.Parse(taskID); err != nil {
		return []TaskLog{}, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ts, level, message FROM task_logs
		WHERE task_id = $1 ORDER BY ts, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询任务日志: %w", err)
	}
	defer rows.Close()

	out := make([]TaskLog, 0)
	for rows.Next() {
		var log TaskLog
		if err := rows.Scan(&log.TS, &log.Level, &log.Message); err != nil {
			return nil, fmt.Errorf("扫描任务日志: %w", err)
		}
		out = append(out, log)
	}
	return out, rows.Err()
}

func (s *PGStore) CreateTask(ctx context.Context, t Task) (Task, error) {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tasks (`+taskColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		t.ID, string(t.Type), string(t.Status), storageParams(t.Params),
		t.RetryCount, t.MaxRetries, t.Error, t.ErrorCode, t.CreatedBy,
		t.ApprovedBy, t.ApprovedAt, t.CreatedAt, t.StartedAt, t.FinishedAt)
	if err != nil {
		return Task{}, fmt.Errorf("创建任务: %w", err)
	}
	return t, nil
}

// UpdateTask 使用 WHERE status = expected 做乐观并发控制，零行时区分并发冲突与记录消失。
func (s *PGStore) UpdateTask(ctx context.Context, t Task, expected taskengine.Status) (Task, error) {
	ct, err := s.pool.Exec(ctx, `
		UPDATE tasks SET
			status = $2, retry_count = $3, max_retries = $4, error = $5, error_code = $6,
			approved_by = $7, approved_at = $8, started_at = $9, finished_at = $10,
			updated_at = now()
		WHERE id = $1 AND status = $11`,
		t.ID, string(t.Status), t.RetryCount, t.MaxRetries, t.Error, t.ErrorCode,
		t.ApprovedBy, t.ApprovedAt, t.StartedAt, t.FinishedAt, string(expected))
	if err != nil {
		return Task{}, fmt.Errorf("更新任务: %w", err)
	}
	if ct.RowsAffected() > 0 {
		return t, nil
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, t.ID).Scan(&exists); err != nil {
		return Task{}, fmt.Errorf("确认任务存在性: %w", err)
	}
	if !exists {
		return Task{}, ErrNotFound
	}
	return Task{}, ErrConflict
}

func (s *PGStore) AppendTaskLog(ctx context.Context, taskID string, log TaskLog) error {
	if log.TS.IsZero() {
		log.TS = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO task_logs (task_id, ts, level, message) VALUES ($1, $2, $3, $4)`,
		taskID, log.TS, log.Level, log.Message)
	if err != nil {
		return fmt.Errorf("写入任务日志: %w", err)
	}
	return nil
}

func scanTask(row scannable) (Task, error) {
	var (
		t         Task
		rawParams []byte
		typeStr   string
		statusStr string
	)
	if err := row.Scan(&t.ID, &typeStr, &statusStr, &rawParams, &t.RetryCount, &t.MaxRetries,
		&t.Error, &t.ErrorCode, &t.CreatedBy, &t.ApprovedBy, &t.ApprovedAt,
		&t.CreatedAt, &t.StartedAt, &t.FinishedAt); err != nil {
		return Task{}, err
	}
	t.Type = taskengine.Type(typeStr)
	t.Status = taskengine.Status(statusStr)
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &t.Params); err != nil {
			return Task{}, fmt.Errorf("解析任务参数: %w", err)
		}
	}
	return t, nil
}

func storageParams(in map[string]any) map[string]any {
	out := cloneParams(in)
	if out == nil {
		out = make(map[string]any, 1)
	}
	if _, ok := out["schema_version"]; !ok {
		out["schema_version"] = jsonSchemaVersion
	}
	return out
}

// ---------- 审计 ----------

func (s *PGStore) AppendAuditLog(ctx context.Context, log AuditLog) error {
	before, err := marshalNullableMap(log.Before)
	if err != nil {
		return err
	}
	after, err := marshalNullableMap(log.After)
	if err != nil {
		return err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO audit_logs (actor, action, object_type, object_id, before, after, trace_id, ts)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		log.Actor, log.Action, log.ObjectType, log.ObjectID, before, after, log.TraceID, log.TS).
		Scan(&log.ID)
	if err != nil {
		return fmt.Errorf("写入审计日志: %w", err)
	}
	return nil
}

func (s *PGStore) ListAuditLogs(ctx context.Context, f AuditFilter) ([]AuditLog, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, actor, action, object_type, object_id, before, after, trace_id, ts
		FROM audit_logs
		WHERE ($1::text IS NULL OR object_type = $1)
		  AND ($2::text IS NULL OR object_id = $2)
		ORDER BY ts DESC, id DESC`,
		nullableText(f.ObjectType), nullableText(f.ObjectID))
	if err != nil {
		return nil, fmt.Errorf("查询审计日志: %w", err)
	}
	defer rows.Close()

	out := make([]AuditLog, 0)
	for rows.Next() {
		var (
			log                 AuditLog
			beforeRaw, afterRaw []byte
		)
		if err := rows.Scan(&log.ID, &log.Actor, &log.Action, &log.ObjectType, &log.ObjectID,
			&beforeRaw, &afterRaw, &log.TraceID, &log.TS); err != nil {
			return nil, fmt.Errorf("扫描审计日志: %w", err)
		}
		if len(beforeRaw) > 0 {
			if err := json.Unmarshal(beforeRaw, &log.Before); err != nil {
				return nil, fmt.Errorf("解析审计 before: %w", err)
			}
		}
		if len(afterRaw) > 0 {
			if err := json.Unmarshal(afterRaw, &log.After); err != nil {
				return nil, fmt.Errorf("解析审计 after: %w", err)
			}
		}
		out = append(out, log)
	}
	return out, rows.Err()
}

func marshalNullableMap(m map[string]any) (any, error) {
	if m == nil {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化审计快照: %w", err)
	}
	return raw, nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
