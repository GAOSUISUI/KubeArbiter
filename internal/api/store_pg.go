package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
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
	SchemaVersion int              `json:"schema_version"`
	Warnings      []schedcore.WarningCode `json:"warnings,omitempty"`
	Detail        map[string]any   `json:"detail,omitempty"`
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
	_ PolicyReader   = (*PGStore)(nil)
	_ DecisionReader = (*PGStore)(nil)
	_ DecisionWriter = (*PGStore)(nil)
	_ TenantReader   = (*PGStore)(nil)
	_ TenantLister   = (*PGStore)(nil)
	_ SnapshotWriter = (*PGStore)(nil)
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
	SchemaVersion int              `json:"schema_version"`
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

// GetTenant 读取租户权重与配额。配额的 cpu / memory 两行分别映射到 Resources，
// FairShare 取 cpu 行（算法规范中主导份额按 CPU 归一化）；缺少配额行时按"不限"处理。
func (s *PGStore) GetTenant(ctx context.Context, id string) (schedcore.TenantState, bool, error) {
	if _, err := uuid.Parse(id); err != nil {
		return schedcore.TenantState{}, false, nil
	}
	var (
		t        schedcore.TenantState
		weight   float64
		cpuHard  int64
		memHard  int64
		fair     float64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.weight::float8,
		       coalesce(qc.hard, 0), coalesce(qm.hard, 0), coalesce(qc.fair_share, 0)::float8
		FROM tenants t
		LEFT JOIN quotas qc ON qc.tenant_id = t.id AND qc.resource = 'cpu'
		LEFT JOIN quotas qm ON qm.tenant_id = t.id AND qm.resource = 'memory'
		WHERE t.id = $1`, id).
		Scan(&t.ID, &weight, &cpuHard, &memHard, &fair)
	if err == pgx.ErrNoRows {
		return schedcore.TenantState{}, false, nil
	}
	if err != nil {
		return schedcore.TenantState{}, false, fmt.Errorf("查询租户: %w", err)
	}
	t.Weight = weight
	t.FairShare = fair
	if cpuHard > 0 || memHard > 0 {
		t.Quota = schedcore.Resources{CPUMilli: cpuHard, MemBytes: memHard}
	}
	return t, true, nil
}

func (s *PGStore) ListPolicies(ctx context.Context) ([]Policy, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, strategy, weights, is_default
		FROM policies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("查询策略: %w", err)
	}
	defer rows.Close()

	out := make([]Policy, 0, 4)
	for rows.Next() {
		var (
			p       Policy
			strategy string
			raw     []byte
		)
		if err := rows.Scan(&p.ID, &p.Name, &strategy, &raw, &p.IsDefault); err != nil {
			return nil, fmt.Errorf("扫描策略: %w", err)
		}
		p.Strategy = schedcore.Strategy(strategy)
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
		raw      []byte
	)
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, name, strategy, weights, is_default
		FROM policies WHERE id = $1`, id).
		Scan(&p.ID, &p.Name, &strategy, &raw, &p.IsDefault)
	if err == pgx.ErrNoRows {
		return Policy{}, false, nil
	}
	if err != nil {
		return Policy{}, false, fmt.Errorf("查询策略: %w", err)
	}
	p.Strategy = schedcore.Strategy(strategy)
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
	_, err = s.pool.Exec(ctx, `
		INSERT INTO policies (id, name, strategy, weights, is_default)
		VALUES ($1, $2, $3, $4::jsonb, $5)
		ON CONFLICT DO NOTHING`,
		p.ID, p.Name, string(p.Strategy), raw, p.IsDefault)
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
	ct, err := s.pool.Exec(ctx, `
		UPDATE policies SET name=$2, strategy=$3, weights=$4::jsonb, is_default=$5, updated_at=now()
		WHERE id=$1`,
		p.ID, p.Name, string(p.Strategy), raw, p.IsDefault)
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

func nullableUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
