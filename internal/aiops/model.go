// Package aiops 提供故障检测、事件诊断与健康分的纯逻辑与运行器。
// 检测器只读集群快照，不执行任何恢复动作；所有事件与诊断通过 Store 落库。
package aiops

import "time"

// IncidentType 与 db/schema.sql、api/openapi.yaml 的枚举保持一致。
type IncidentType string

const (
	IncidentCrashLoop         IncidentType = "CRASH_LOOP"
	IncidentImagePullBackOff  IncidentType = "IMAGE_PULL_BACKOFF"
	IncidentReadinessFail     IncidentType = "READINESS_FAIL"
	IncidentOOMKilled         IncidentType = "OOM_KILLED"
	IncidentDownstreamTimeout IncidentType = "DOWNSTREAM_TIMEOUT"
	IncidentEmptyEndpoints    IncidentType = "EMPTY_ENDPOINTS"
	IncidentConfigError       IncidentType = "CONFIG_ERROR"

	IncidentNodeNotReady     IncidentType = "NODE_NOT_READY"
	IncidentNodePressure     IncidentType = "NODE_PRESSURE"
	IncidentNodeHighUsage    IncidentType = "NODE_HIGH_USAGE"
	IncidentMetricsStale     IncidentType = "METRICS_STALE"
	IncidentPodPendingLong   IncidentType = "POD_PENDING_LONG"
	IncidentPodReadinessFail IncidentType = "POD_READINESS_FAIL"
	IncidentQuotaNear        IncidentType = "QUOTA_NEAR"
)

// Severity 沿用契约的三档。
type Severity string

const (
	SeverityInfo     Severity = "INFO"
	SeverityWarn     Severity = "WARN"
	SeverityCritical Severity = "CRITICAL"
)

// Rank 用于"严重度只升不降"的判断。
func (s Severity) Rank() int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityWarn:
		return 2
	default:
		return 1
	}
}

// 事件作用域：节点/集群/租户没有真实命名空间，用保留值区分，避免与业务命名空间混淆。
const (
	NamespaceNode    = "__node__"
	NamespaceCluster = "__cluster__"
	NamespaceTenant  = "__tenant__"
	TargetCluster    = "cluster"
)

// Incident 对应 incidents 表一行。
type Incident struct {
	ID         string       `json:"id"`
	Target     string       `json:"target"`
	Namespace  string       `json:"namespace"`
	Type       IncidentType `json:"type"`
	Severity   Severity     `json:"severity"`
	Status     string       `json:"status"`
	FirstSeen  time.Time    `json:"first_seen"`
	ResolvedAt *time.Time   `json:"resolved_at"`
}

// Fact 是可直接观测的事实，不与推断混在一起。
type Fact struct {
	Stage   string         `json:"stage"`
	At      time.Time      `json:"at"`
	Object  string         `json:"object,omitempty"`
	Summary string         `json:"summary"`
	Values  map[string]any `json:"values,omitempty"`
}

// Inference 是根因假设，必须带证据与反证。
type Inference struct {
	RootCause       string   `json:"root_cause"`
	Confidence      string   `json:"confidence"`
	Evidence        []string `json:"evidence"`
	CounterEvidence []string `json:"counter_evidence,omitempty"`
}

// Diagnosis 对应 diagnosis_reports 表一条记录。
type Diagnosis struct {
	ID         string      `json:"id"`
	IncidentID string      `json:"incident_id"`
	Facts      []Fact      `json:"facts"`
	Inferences []Inference `json:"inferences"`
	CreatedAt  time.Time   `json:"created_at"`
}

// RecoveryAction 当前只读返回空列表，保留契约字段。
type RecoveryAction struct {
	ID         string    `json:"id"`
	Action     string    `json:"action"`
	TaskID     *string   `json:"task_id"`
	ApprovedBy *string   `json:"approved_by"`
	Result     string    `json:"result"`
	CreatedAt  time.Time `json:"created_at"`
}

// Detail 是事件详情，嵌入 Incident 以保持 JSON 扁平。
type Detail struct {
	Incident
	Diagnosis       *Diagnosis       `json:"diagnosis"`
	RecoveryActions []RecoveryAction `json:"recovery_actions"`
}

// Filter 为空字段表示不过滤。
type Filter struct {
	Status    string
	Type      string
	Severity  string
	Namespace string
	Target    string
}

// HealthComponent 解释一次扣分。
type HealthComponent struct {
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Delta   float64 `json:"delta"`
}

// HealthScore 的 Score 为 nil 表示数据不足，不能当成 0 分。
type HealthScore struct {
	Scope      string            `json:"scope"`
	Name       string            `json:"name"`
	Score      *float64          `json:"score"`
	Components []HealthComponent `json:"components"`
}
