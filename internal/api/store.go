package api

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// Policy 对应 openapi.yaml 的 Policy。策略权重直接复用 schedcore.Weights，
// 避免 API 层与决策核各定义一份七维权重后悄悄漂移。
type Policy struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Strategy  schedcore.Strategy `json:"strategy"`
	Weights   schedcore.Weights  `json:"weights"`
	IsDefault bool               `json:"is_default"`
}

// WorkloadSpec 对应 openapi.yaml 的 WorkloadSpec（只保留 API 需要的字段）。
type WorkloadSpec struct {
	Name           string              `json:"name"`
	Namespace      string              `json:"namespace"`
	Kind           string              `json:"kind,omitempty"`
	Replicas       int                 `json:"replicas"`
	Requests       schedcore.Resources `json:"requests"`
	NodeSelector   map[string]string   `json:"node_selector,omitempty"`
	Tolerations    []Toleration        `json:"tolerations,omitempty"`
	TopologySpread *TopologySpread     `json:"topology_spread,omitempty"`
	HostPorts      []int32             `json:"host_ports,omitempty"`
}

type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

type TopologySpread struct {
	TopologyKey string `json:"topology_key"`
	MaxSkew     int    `json:"max_skew"`
}

// PlacementDecision 对应 openapi.yaml 的 PlacementDecision。
type PlacementDecision struct {
	ID           string                   `json:"id"`
	Status       schedcore.Status         `json:"status"`
	Workload     *WorkloadSpec            `json:"workload,omitempty"`
	TenantID     *string                  `json:"tenant_id"`
	PolicyID     string                   `json:"policy_id"`
	ChosenNode   *string                  `json:"chosen_node"`
	SnapshotHash string                   `json:"snapshot_hash"`
	Scores       []schedcore.NodeScore    `json:"scores"`
	Filtered     []schedcore.FilteredNode `json:"filtered"`
	Degraded     bool                     `json:"degraded"`
	Warnings     []schedcore.WarningCode  `json:"warnings,omitempty"`
	Evidence     map[string]any           `json:"evidence,omitempty"`
	CreatedAt    time.Time                `json:"created_at"`
}

type decisionPage struct {
	NextCursor *string             `json:"next_cursor"`
	Items      []PlacementDecision `json:"items"`
}

// DecisionFilter 中的字段均已由 handler 校验过格式。
type DecisionFilter struct {
	TenantID string
	PolicyID string
}

// PolicyReader / DecisionReader 是 API 层对存储的最小依赖；
// M1 由内存实现，M2 起由 PostgreSQL 实现，handler 不动。
type PolicyReader interface {
	ListPolicies(ctx context.Context) ([]Policy, error)
	GetPolicy(ctx context.Context, id string) (Policy, bool, error)
}

type DecisionReader interface {
	ListDecisions(ctx context.Context, f DecisionFilter) ([]PlacementDecision, error)
	GetDecision(ctx context.Context, id string) (PlacementDecision, bool, error)
}

// DecisionWriter 只在模拟/落地流程中使用；dry-run 语义由调用方决定是否调用。
type DecisionWriter interface {
	CreateDecision(ctx context.Context, d PlacementDecision) (PlacementDecision, error)
}

// MemoryStore 是只读视图的占位实现：没有数据库时不编造数据，默认返回空列表。
type MemoryStore struct {
	mu        sync.RWMutex
	policies  []Policy
	decisions []PlacementDecision
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		policies:  make([]Policy, 0),
		decisions: make([]PlacementDecision, 0),
	}
}

func (s *MemoryStore) AddPolicy(p Policy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies = append(s.policies, p)
}

func (s *MemoryStore) AddDecision(d PlacementDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, d)
}

func (s *MemoryStore) ListPolicies(_ context.Context) ([]Policy, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Policy{}, s.policies...), nil
}

// ListDecisions 按 created_at 倒序、同时间按 id 倒序返回，保证游标翻页稳定。
func (s *MemoryStore) ListDecisions(_ context.Context, f DecisionFilter) ([]PlacementDecision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]PlacementDecision, 0, len(s.decisions))
	for _, d := range s.decisions {
		if f.TenantID != "" && (d.TenantID == nil || *d.TenantID != f.TenantID) {
			continue
		}
		if f.PolicyID != "" && d.PolicyID != f.PolicyID {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *MemoryStore) GetDecision(_ context.Context, id string) (PlacementDecision, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.decisions {
		if d.ID == id {
			return d, true, nil
		}
	}
	return PlacementDecision{}, false, nil
}

func (s *MemoryStore) GetPolicy(_ context.Context, id string) (Policy, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.policies {
		if p.ID == id {
			return p, true, nil
		}
	}
	return Policy{}, false, nil
}

// CreateDecision 为内存实现补齐 ID 与时间戳，行为与 PostgreSQL 默认值保持一致。
func (s *MemoryStore) CreateDecision(_ context.Context, d PlacementDecision) (PlacementDecision, error) {
	if d.ID == "" {
		d.ID = uuid.NewString()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions = append(s.decisions, d)
	return d, nil
}
