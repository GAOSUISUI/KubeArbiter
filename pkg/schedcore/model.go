// Package schedcore 是 KubeArbiter 的纯决策核。
// 约束：本包只允许 import 标准库，不得 import client-go 或任何 k8s 包，
// 也不得访问数据库、网络或时间以外的外部状态。所有函数必须是纯函数或近似纯函数，
// 以便离线仿真与表驱动单测。
package schedcore

import (
	"errors"
	"fmt"
)

// Resources 表示资源量。CPU 单位为 millicore，内存单位为 byte，
// GPU 为整卡数（对应 Kubernetes extended resource `nvidia.com/gpu`）。
// 内部一律用整数运算，不做浮点累加。
type Resources struct {
	CPUMilli int64 `json:"cpu_milli"`
	MemBytes int64 `json:"mem_bytes"`
	GPU      int64 `json:"gpu"`
}

func (r Resources) Add(o Resources) Resources {
	return Resources{CPUMilli: r.CPUMilli + o.CPUMilli, MemBytes: r.MemBytes + o.MemBytes, GPU: r.GPU + o.GPU}
}

func (r Resources) Sub(o Resources) Resources {
	return Resources{CPUMilli: r.CPUMilli - o.CPUMilli, MemBytes: r.MemBytes - o.MemBytes, GPU: r.GPU - o.GPU}
}

func (r Resources) Mul(n int) Resources {
	return Resources{CPUMilli: r.CPUMilli * int64(n), MemBytes: r.MemBytes * int64(n), GPU: r.GPU * int64(n)}
}

func (r Resources) IsZero() bool {
	return r.CPUMilli == 0 && r.MemBytes == 0 && r.GPU == 0
}

// FitsIn 判断 r 是否不超过 o（逐维）。
func (r Resources) FitsIn(o Resources) bool {
	return r.CPUMilli <= o.CPUMilli && r.MemBytes <= o.MemBytes && r.GPU <= o.GPU
}

// ---------- 集群视图 ----------

type Taint struct {
	Key    string
	Value  string
	Effect string // NoSchedule / PreferNoSchedule / NoExecute
}

type Node struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Allocatable   Resources
	Requested     Resources // Σrequest
	Usage         Resources // 实际用量（已由采集层做 5m EWMA）
	UsageKnown    bool
	Labels        map[string]string
	Taints        []Taint
	// HostPorts 记录节点上已占用的 hostPort，key 为端口，value 为占用者描述。
	HostPorts map[int32]string
	// Topology 记录拓扑域，如 {"zone": "z1", "kubernetes.io/hostname": "w1"}。
	Topology map[string]string
	// TenantUsage 是该节点上按租户聚合的已分配资源，DRF 试算的输入。
	TenantUsage map[string]Resources
}

type Toleration struct {
	Key      string
	Operator string // Equal / Exists
	Value    string
	Effect   string
}

// NodeAffinityRequirement 只实现调度中最常用的四种算子，不追求完整语义。
type NodeAffinityRequirement struct {
	Key      string
	Operator string // In / NotIn / Exists / DoesNotExist
	Values   []string
}

type TopologySpread struct {
	TopologyKey string
	MaxSkew     int
}

type Workload struct {
	Name           string
	Namespace      string
	TenantID       string
	Replicas       int
	Requests       Resources // 单副本 request
	NodeSelector   map[string]string
	Affinity       []NodeAffinityRequirement
	Tolerations    []Toleration
	HostPorts      []int32
	TopologySpread *TopologySpread
}

// Demand 返回本次放置的总需求（单副本 × 副本数）。
func (w Workload) Demand() Resources {
	return w.Requests.Mul(w.Replicas)
}

type ClusterContext struct {
	Nodes []Node
	// Capacity 是集群可仲裁容量 C_r，供公平份额计算使用。
	Capacity Resources
	// MinPendingPod 是待调度队列中最小的 Pod 需求，用于判定"不可用碎片"。
	MinPendingPod Resources
	// Tenants 以租户 ID 索引。
	Tenants map[string]TenantState
	// TopologyCounts 以 "<key>=<value>" 为键，记录该拓扑域现有的 Pod 数。
	TopologyCounts map[string]int
	// MetricsAgeSeconds 为指标最新样本距今的秒数。
	MetricsAgeSeconds float64
	// MetricsKnown 为 false 表示完全取不到指标。
	MetricsKnown bool
	// FairMode 决定公平维度 s_fair 的计算方式；零值按 DRF 处理。
	FairMode FairMode
}

// FairMode 决定公平维度 s_fair 的计算方式。
type FairMode string

const (
	// FairModeDRF 在候选节点上做一次加权 DRF 仲裁，以该租户需求的满足比例作为 s_fair。
	// 节点上已有该租户的用量时得分会下降，从而把租户分散到更"干净"的节点。
	FairModeDRF FairMode = "DRF"
	// FairModeWeightedShare 是 M5 之前的基线算法：用租户全局主导份额除以目标份额，
	// 结果与候选节点无关，仅作为 A/B 对比的对照组保留。
	FairModeWeightedShare FairMode = "WEIGHTED_SHARE"
)

// Validate 校验公平模式取值；空值视为 DRF。
func (m FairMode) Validate() error {
	switch m {
	case "", FairModeDRF, FairModeWeightedShare:
		return nil
	default:
		return fmt.Errorf("非法公平模式 %q（允许 DRF/WEIGHTED_SHARE）", m)
	}
}

// orDefault 返回生效模式，空值回落为 DRF。
func (m FairMode) orDefault() FairMode {
	if m == "" {
		return FairModeDRF
	}
	return m
}

type TenantState struct {
	ID        string
	Weight    float64
	FairShare float64 // 占集群比例
	Allocated Resources
	Quota     Resources // 硬上限，零值表示不限
}

// ---------- 策略 ----------

type Strategy string

const (
	Balanced  Strategy = "BALANCED"
	BinPack   Strategy = "BIN_PACK"
	FairShare Strategy = "FAIR_SHARE"
	LoadAware Strategy = "LOAD_AWARE"
)

// Validate 校验策略值是否属于四种预置枚举之一。
func (s Strategy) Validate() error {
	switch s {
	case Balanced, BinPack, FairShare, LoadAware:
		return nil
	case "":
		return errors.New("必须非空")
	default:
		return fmt.Errorf("非法策略值 %q（允许 BALANCED/BIN_PACK/FAIR_SHARE/LOAD_AWARE）", s)
	}
}

// Weights 为七维权重。全部为 0 时视为非法。
type Weights struct {
	CPU     float64 `json:"s_cpu"`
	Mem     float64 `json:"s_mem"`
	Balance float64 `json:"s_balance"`
	Pack    float64 `json:"s_pack"`
	Load    float64 `json:"s_load"`
	Fair    float64 `json:"s_fair"`
	Frag    float64 `json:"s_frag"`
}

func (w Weights) Sum() float64 {
	return w.CPU + w.Mem + w.Balance + w.Pack + w.Load + w.Fair + w.Frag
}

var ErrInvalidWeights = errors.New("权重非法：每个维度需 >= 0 且总和为 1（误差 1e-6）")

// Validate 校验权重可被接受。零权重允许存在，但总和必须为 1。
func (w Weights) Validate() error {
	vals := []float64{w.CPU, w.Mem, w.Balance, w.Pack, w.Load, w.Fair, w.Frag}
	for _, v := range vals {
		if v < 0 || v > 1 {
			return fmt.Errorf("%w: 存在越界权重 %v", ErrInvalidWeights, v)
		}
	}
	if s := w.Sum(); s < 1-1e-6 || s > 1+1e-6 {
		return fmt.Errorf("%w: 当前总和 %v", ErrInvalidWeights, s)
	}
	return nil
}

// PresetWeights 返回四组预设，与 docs/01-algorithm-spec.md 2.4 节逐值一致。
func PresetWeights(s Strategy) (Weights, error) {
	switch s {
	case Balanced:
		return Weights{CPU: 0.18, Mem: 0.18, Balance: 0.25, Pack: 0.10, Load: 0.15, Fair: 0.10, Frag: 0.04}, nil
	case BinPack:
		return Weights{CPU: 0.10, Mem: 0.10, Balance: 0.05, Pack: 0.35, Load: 0.05, Fair: 0.05, Frag: 0.30}, nil
	case FairShare:
		return Weights{CPU: 0.10, Mem: 0.10, Balance: 0.08, Pack: 0.05, Load: 0.05, Fair: 0.52, Frag: 0.10}, nil
	case LoadAware:
		return Weights{CPU: 0.10, Mem: 0.10, Balance: 0.12, Pack: 0.06, Load: 0.50, Fair: 0.07, Frag: 0.05}, nil
	default:
		return Weights{}, fmt.Errorf("未知策略 %q", s)
	}
}

// ---------- 决策输出 ----------

type Status string

const (
	StatusOK          Status = "OK"
	StatusNoCandidate Status = "NO_CANDIDATE"
)

type ReasonCode string

const (
	ReasonNodeNotReady         ReasonCode = "NODE_NOT_READY"
	ReasonNodeUnschedulable    ReasonCode = "NODE_UNSCHEDULABLE"
	ReasonInsufficientCPU      ReasonCode = "INSUFFICIENT_CPU"
	ReasonInsufficientMemory   ReasonCode = "INSUFFICIENT_MEMORY"
	ReasonInsufficientGPU      ReasonCode = "INSUFFICIENT_GPU"
	ReasonTaintNotTolerated    ReasonCode = "TAINT_NOT_TOLERATED"
	ReasonNodeSelectorMismatch ReasonCode = "NODE_SELECTOR_MISMATCH"
	ReasonAffinityMismatch     ReasonCode = "AFFINITY_MISMATCH"
	ReasonTopologyViolation    ReasonCode = "TOPOLOGY_VIOLATION"
	ReasonHostPortConflict     ReasonCode = "HOSTPORT_CONFLICT"
	ReasonTenantQuotaExceeded  ReasonCode = "TENANT_QUOTA_EXCEEDED"
	ReasonNoNodeInSnapshot     ReasonCode = "NO_NODE_IN_SNAPSHOT"
)

type WarningCode string

const (
	WarnMetricsStale          WarningCode = "METRICS_STALE"
	WarnMetricsMissing        WarningCode = "METRICS_MISSING"
	WarnFairShareMissing      WarningCode = "FAIR_SHARE_MISSING"
	WarnExtrapolation         WarningCode = "EXTRAPOLATION"
	WarnInvalidCurve          WarningCode = "INVALID_CURVE"
	WarnInsufficientPoints    WarningCode = "INSUFFICIENT_POINTS"
	WarnBudgetLow             WarningCode = "BUDGET_LOW"
	WarnSLODataDegraded       WarningCode = "SLO_DATA_DEGRADED"
	WarnSingleReplicaExcluded WarningCode = "SINGLE_REPLICA_EXCLUDED"
)

type FilteredNode struct {
	Node     string         `json:"node"`
	Reason   ReasonCode     `json:"reason"`
	Evidence map[string]any `json:"evidence,omitempty"`
}

type NodeScore struct {
	Node      string             `json:"node"`
	Rank      int                `json:"rank"`
	Score     float64            `json:"score"`
	Breakdown map[string]float64 `json:"breakdown"`
}

type Decision struct {
	Status     Status         `json:"status"`
	ChosenNode string         `json:"chosen_node,omitempty"`
	Scores     []NodeScore    `json:"scores"`
	Filtered   []FilteredNode `json:"filtered"`
	Degraded   bool           `json:"degraded"`
	Warnings   []WarningCode  `json:"warnings"`
	// Assignments 是 gang 放置的节点分布（节点 → 副本数）；单 Pod 决策只含一项。
	// 为空表示单副本决策未生成该字段（保持与既有 JSON 兼容）。
	Assignments []NodeAssignment `json:"assignments,omitempty"`
	// Evidence 记录 gang 不可行等场景的聚合证据（total_feasible、min_available）。
	Evidence map[string]any `json:"evidence,omitempty"`
}

// NodeAssignment 是 gang 放置中单个节点分到的副本数。
type NodeAssignment struct {
	Node     string `json:"node"`
	Replicas int    `json:"replicas"`
}
