package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/placement"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	defaultLimit = 20
	maxLimit     = 200
)

// Deps 是 API 层的依赖集合，由 cmd/arbiter-api 注入。
type Deps struct {
	Policies    PolicyReader
	PolicyWriter PolicyWriter
	Decisions   DecisionReader
	Writer      DecisionWriter
	// Tenants 提供租户权重与配额；未接入库时为空，模拟接口按权重 1、无配额兜底。
	Tenants TenantReader
	// TenantCatalog 列出全部租户，供容量明细接口使用；为空时该接口返回 503。
	TenantCatalog TenantLister
	// Snapshots 把决策所用快照留档；为空时跳过留档。
	Snapshots SnapshotWriter
	// TenantOfNamespace 由 informer 缓存解析命名空间上的 arbiter.io/tenant 标签；
	// 仅 webhook 路径使用，为空时 webhook 直接放行。
	TenantOfNamespace func(namespace string) (string, bool)
	// Snapshotter 返回当前集群只读快照；未接入 informer 时为 nil，模拟接口返回 503。
	Snapshotter func(context.Context) (capacity.Snapshot, error)
	// Checks 为健康检查集合，key 为依赖名（如 "postgres"、"k8s"）。
	Checks map[string]func(context.Context) error
}

// NewRouter 注册 M1 阶段的只读路由。/healthz 之外的路由均为契约中已定义路径的子集，
// 未实现的写接口不注册，避免出现"看起来能用但返回假数据"的端点。
func NewRouter(d Deps) *gin.Engine {
	if d.Policies == nil && d.Decisions == nil {
		store := NewMemoryStore()
		d.Policies, d.Decisions = store, store
		if d.Writer == nil {
			d.Writer = store
		}
	}
	if d.Policies == nil {
		d.Policies = NewMemoryStore()
	}
	if d.Decisions == nil {
		d.Decisions = NewMemoryStore()
	}

	r := gin.New()
	r.Use(CORS(), TraceID(), Recover())

	r.GET("/healthz", healthz(d))

	v1 := r.Group("/api/v1")
	v1.GET("/policies", listPolicies(d))
	v1.POST("/policies", createPolicy(d))
	v1.PUT("/policies/:id", updatePolicy(d))
	v1.DELETE("/policies/:id", deletePolicy(d))
	v1.GET("/capacity/overview", capacityOverview(d))
	v1.GET("/capacity/nodes", nodeList(d))
	v1.GET("/capacity/tenants", listTenantCapacity(d))
	v1.GET("/placement/decisions", listDecisions(d))
	v1.GET("/placement/decisions/:id", getDecision(d))
	v1.POST("/placement/simulate", simulatePlacement(d))

	// 准入 webhook 正式路径走独立端口的 TLS 服务，这里同二进制内也注册一份，便于本地调试与单测。
	r.POST("/webhook/mutate-pods", webhookMutate(d))

	// 未命中 API 的请求交给内嵌前端（SPA 回退）；未构建前端时返回 JSON 404。
	registerUIRoutes(r, d)
	return r
}

// healthz 为存活探针：任一依赖检查失败返回 503。契约中该路径无响应体 schema，
// 因此只返回最少的状态字段，不套信封。
func healthz(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		failed := make([]string, 0, len(d.Checks))
		for name, check := range d.Checks {
			if err := check(ctx); err != nil {
				failed = append(failed, name)
			}
		}
		if len(failed) > 0 {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable", "failed": failed})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func listPolicies(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		policies, err := d.Policies.ListPolicies(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if policies == nil {
			policies = []Policy{}
		}
		writeOK(c, policies)
	}
}

// createPolicy / updatePolicy / deletePolicy 实现策略 CRUD。
// 权重必须走 Validate()；ErrNotFound 映射 404，其它存储错误映射 503。
type policyRequest struct {
	Name      string             `json:"name"`
	Strategy  schedcore.Strategy `json:"strategy"`
	Weights   schedcore.Weights  `json:"weights"`
	IsDefault bool               `json:"is_default"`
}

func createPolicy(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.PolicyWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		var req policyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()}))
			return
		}
		if req.Name == "" {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "name", Reason: "必须非空"}))
			return
		}
		if err := req.Weights.Validate(); err != nil {
			writeError(c, errBusinessRejected("POLICY_WEIGHTS_INVALID", ErrorDetail{Field: "weights", Reason: err.Error()}))
			return
		}
		if err := req.Strategy.Validate(); err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "strategy", Reason: err.Error()}))
			return
		}
		p, err := d.PolicyWriter.CreatePolicy(c.Request.Context(), Policy{
			Name:      req.Name,
			Strategy:  req.Strategy,
			Weights:   req.Weights,
			IsDefault: req.IsDefault,
		})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(c, errNotFound())
				return
			}
			writeError(c, errUnavailable())
			return
		}
		writeOK(c, p)
	}
}

func updatePolicy(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.PolicyWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		id := c.Param("id")
		var req policyRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()}))
			return
		}
		if err := req.Weights.Validate(); err != nil {
			writeError(c, errBusinessRejected("POLICY_WEIGHTS_INVALID", ErrorDetail{Field: "weights", Reason: err.Error()}))
			return
		}
		p, err := d.PolicyWriter.UpdatePolicy(c.Request.Context(), Policy{
			ID:        id,
			Name:      req.Name,
			Strategy:  req.Strategy,
			Weights:   req.Weights,
			IsDefault: req.IsDefault,
		})
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(c, errNotFound())
				return
			}
			writeError(c, errUnavailable())
			return
		}
		writeOK(c, p)
	}
}

func deletePolicy(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.PolicyWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		id := c.Param("id")
		if err := d.PolicyWriter.DeletePolicy(c.Request.Context(), id); err != nil {
			if errors.Is(err, ErrNotFound) {
				writeError(c, errNotFound())
				return
			}
			writeError(c, errUnavailable())
			return
		}
		c.Status(http.StatusNoContent)
	}
}

func listDecisions(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}

		filter := DecisionFilter{}
		for field, raw := range map[string]string{
			"tenant_id": c.Query("tenant_id"),
			"policy_id": c.Query("policy_id"),
		} {
			if raw == "" {
				continue
			}
			if _, err := uuid.Parse(raw); err != nil {
				writeError(c, errInvalidArgument(ErrorDetail{Field: field, Reason: "必须是 UUID"}))
				return
			}
			if field == "tenant_id" {
				filter.TenantID = raw
			} else {
				filter.PolicyID = raw
			}
		}

		items, err := d.Decisions.ListDecisions(c.Request.Context(), filter)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		start := 0
		if raw := c.Query("cursor"); raw != "" {
			id, ok := decodeCursor(raw)
			if !ok {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标格式非法"}))
				return
			}
			idx := -1
			for i := range items {
				if items[i].ID == id {
					idx = i
					break
				}
			}
			if idx < 0 {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标已失效"}))
				return
			}
			start = idx + 1
		}

		rest := items[start:]
		page := decisionPage{Items: []PlacementDecision{}}
		if len(rest) > limit {
			page.Items = append(page.Items, rest[:limit]...)
			next := encodeCursor(rest[limit-1].ID)
			page.NextCursor = &next
		} else {
			page.Items = append(page.Items, rest...)
		}
		writeOK(c, page)
	}
}

func getDecision(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		decision, ok, err := d.Decisions.GetDecision(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}
		writeOK(c, decision)
	}
}

func parseLimit(raw string) (int, error) {
	if raw == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		return 0, errors.New("必须是 1..200 的整数")
	}
	return n, nil
}

func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeCursor(raw string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) == 0 {
		return "", false
	}
	return string(b), true
}

// simulateRequest 对应 openapi.yaml 的 PlacementSimulateRequest。
// Weights 可选：传了就覆盖策略预设（前端权重滑杆直接透传），不传则使用 policy_id 对应策略的权重。
type simulateRequest struct {
	Workload WorkloadSpec       `json:"workload"`
	TenantID *string            `json:"tenant_id"`
	PolicyID string             `json:"policy_id"`
	Weights  *schedcore.Weights `json:"weights,omitempty"`
}

// simulatePlacement 用当前集群快照跑一次放置决策，并把结果落库。
// 它只做决策不触碰集群对象：真正改写 nodeAffinity 的是 admission webhook（M4）。
func simulatePlacement(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if d.Snapshotter == nil {
			writeError(c, errUnavailable())
			return
		}

		var req simulateRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: "JSON 解析失败: " + err.Error()}))
			return
		}
		if details := validateSimulate(req); len(details) > 0 {
			writeError(c, errInvalidArgument(details...))
			return
		}

		policy, ok, err := d.Policies.GetPolicy(ctx, req.PolicyID)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}
		if err := policy.Weights.Validate(); err != nil {
			writeError(c, errBusinessRejected("POLICY_WEIGHTS_INVALID", ErrorDetail{Field: "policy_id", Reason: err.Error()}))
			return
		}
		// 前端可直接带 weights 覆盖策略预设——权重滑杆就是这条路。
		weights := policy.Weights
		if req.Weights != nil {
			if err := req.Weights.Validate(); err != nil {
				writeError(c, errBusinessRejected("REQUEST_WEIGHTS_INVALID", ErrorDetail{Field: "weights", Reason: err.Error()}))
				return
			}
			weights = *req.Weights
		}

		start := time.Now()
		snap, err := d.Snapshotter(ctx)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		hash, err := placement.Hash(snap)
		if err != nil {
			writeError(c, errInternal())
			return
		}
		if d.Snapshots != nil {
			if err := d.Snapshots.SaveSnapshot(ctx, snap, hash); err != nil {
				writeError(c, errUnavailable())
				return
			}
		}

		// 租户权重与配额来自 tenants / quotas 表；未接入库时按权重 1、无配额兜底。
		tenants := map[string]schedcore.TenantState{}
		if req.TenantID != nil {
			state := schedcore.TenantState{ID: *req.TenantID, Weight: 1}
			if d.Tenants != nil {
				loaded, ok, err := d.Tenants.GetTenant(ctx, *req.TenantID)
				if err != nil {
					writeError(c, errUnavailable())
					return
				}
				if !ok {
					writeError(c, errNotFound())
					return
				}
				state = loaded
			}
			tenants[state.ID] = state
		}

		clusterCtx := placement.BuildContext(snap, tenants)
		out, err := schedcore.Place(clusterCtx, toSchedWorkload(req), weights)
		if err != nil {
			writeError(c, errBusinessRejected("PLACEMENT_REJECTED", ErrorDetail{Field: "workload", Reason: err.Error()}))
			return
		}

		decision := PlacementDecision{
			Status:       out.Status,
			Workload:     &req.Workload,
			TenantID:     req.TenantID,
			PolicyID:     policy.ID,
			SnapshotHash: hash,
			Scores:       out.Scores,
			Filtered:     out.Filtered,
			Degraded:     degraded(out.Warnings),
			Warnings:     out.Warnings,
			Evidence: map[string]any{
				"schema_version":      1,
				"weights":             weights,
				"policy_id":           policy.ID,
				"metrics_known":       clusterCtx.MetricsKnown,
				"metrics_age_seconds": clusterCtx.MetricsAgeSeconds,
				"node_count":          len(snap.Nodes),
				"elapsed_ms":          time.Since(start).Milliseconds(),
			},
			CreatedAt: time.Now().UTC(),
		}
		if out.ChosenNode != "" {
			chosen := out.ChosenNode
			decision.ChosenNode = &chosen
		}

		if d.Writer != nil {
			saved, err := d.Writer.CreateDecision(ctx, decision)
			if err != nil {
				writeError(c, errUnavailable())
				return
			}
			decision = saved
		}
		writeOK(c, decision)
	}
}

func validateSimulate(req simulateRequest) []ErrorDetail {
	var details []ErrorDetail
	if strings.TrimSpace(req.Workload.Name) == "" {
		details = append(details, ErrorDetail{Field: "workload.name", Reason: "不能为空"})
	}
	if strings.TrimSpace(req.Workload.Namespace) == "" {
		details = append(details, ErrorDetail{Field: "workload.namespace", Reason: "不能为空"})
	}
	if req.Workload.Replicas < 1 {
		details = append(details, ErrorDetail{Field: "workload.replicas", Reason: "至少为 1"})
	}
	if req.Workload.Requests.CPUMilli < 0 || req.Workload.Requests.MemBytes < 0 {
		details = append(details, ErrorDetail{Field: "workload.requests", Reason: "不能为负数"})
	}
	if _, err := uuid.Parse(req.PolicyID); err != nil {
		details = append(details, ErrorDetail{Field: "policy_id", Reason: "必须是 UUID"})
	}
	if req.TenantID != nil {
		if _, err := uuid.Parse(*req.TenantID); err != nil {
			details = append(details, ErrorDetail{Field: "tenant_id", Reason: "必须是 UUID"})
		}
	}
	return details
}

func toSchedWorkload(req simulateRequest) schedcore.Workload {
	spec := req.Workload
	wl := schedcore.Workload{
		Name:         spec.Name,
		Namespace:    spec.Namespace,
		Replicas:     spec.Replicas,
		Requests:     spec.Requests,
		NodeSelector: spec.NodeSelector,
		HostPorts:    spec.HostPorts,
	}
	if req.TenantID != nil {
		wl.TenantID = *req.TenantID
	}
	for _, t := range spec.Tolerations {
		wl.Tolerations = append(wl.Tolerations, schedcore.Toleration{
			Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect,
		})
	}
	if spec.TopologySpread != nil {
		wl.TopologySpread = &schedcore.TopologySpread{
			TopologyKey: spec.TopologySpread.TopologyKey,
			MaxSkew:     spec.TopologySpread.MaxSkew,
		}
	}
	return wl
}

// degraded 只在指标缺失/过期时置位；单纯没有候选节点属于正常拒绝，不算降级。
func degraded(warnings []schedcore.WarningCode) bool {
	for _, w := range warnings {
		if w == schedcore.WarnMetricsMissing || w == schedcore.WarnMetricsStale {
			return true
		}
	}
	return false
}

// capacityTotals 对应 CapacityTotals：节点数、可分配总量、已请求总量。
type capacityTotals struct {
	Count       int               `json:"count"`
	Allocatable schedcore.Resources `json:"allocatable"`
	Requested   schedcore.Resources `json:"requested"`
}

type capacityOverviewResp struct {
	AsOf        time.Time        `json:"as_of"`
	Nodes       capacityTotals   `json:"nodes"`
	TenantCount int              `json:"tenant_count"`
	JainIndex   *float64         `json:"jain_index"`
	SLOSummary  any              `json:"slo_summary"`
}

// capacityOverview 汇总节点容量与租户公平性。SLO 汇总要等指标与 SLO 评估接入，
// 按契约返回 null，前端展示"无数据"而不是 0。
func capacityOverview(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Snapshotter == nil {
			writeError(c, errUnavailable())
			return
		}
		snap, err := d.Snapshotter(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		totals := capacityTotals{Count: len(snap.Nodes)}
		capacitySum := clusterCapacity(snap)
		for _, n := range snap.Nodes {
			totals.Allocatable = totals.Allocatable.Add(n.Allocatable)
			totals.Requested = totals.Requested.Add(n.Requested)
		}

		// 主导份额参与 Jain 指数；没有租户或容量为 0 时无有意义的值。
		var jain *float64
		if len(snap.TenantUsage) > 0 && !capacitySum.IsZero() {
			shares := make([]float64, 0, len(snap.TenantUsage))
			for _, used := range snap.TenantUsage {
				shares = append(shares, dominantShare(used, capacitySum))
			}
			v := schedcore.JainIndex(shares)
			jain = &v
		}

		writeOK(c, capacityOverviewResp{
			AsOf:        snap.TakenAt,
			Nodes:       totals,
			TenantCount: len(snap.TenantUsage),
			JainIndex:   jain,
			SLOSummary:  nil,
		})
	}
}

// tenantCapacityItem 对应 openapi.yaml 的 TenantCapacity。
type tenantCapacityItem struct {
	TenantID  string              `json:"tenant_id"`
	Name      string              `json:"name"`
	Requested schedcore.Resources `json:"requested"`
	Hard      schedcore.Resources `json:"hard"`
	FairShare float64             `json:"fair_share"`
	JainShare float64             `json:"jain_share"`
}

type tenantCapacityPage struct {
	NextCursor *string              `json:"next_cursor"`
	Items      []tenantCapacityItem `json:"items"`
}

// listTenantCapacity 把「快照里的真实用量」和「库里的租户配额」合成租户容量明细。
// 未打 arbiter.io/tenant 标签的命名空间不归属任何租户，因此这里只出现已登记的租户。
func listTenantCapacity(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if d.Snapshotter == nil || d.TenantCatalog == nil {
			writeError(c, errUnavailable())
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}

		snap, err := d.Snapshotter(ctx)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		tenants, err := d.TenantCatalog.ListTenants(ctx)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		totalCapacity := clusterCapacity(snap)
		var totalWeight float64
		for _, t := range tenants {
			if t.Weight > 0 {
				totalWeight += t.Weight
			}
		}

		items := make([]tenantCapacityItem, 0, len(tenants))
		for _, t := range tenants {
			usage := snap.TenantUsage[t.ID]
			// 优先用租户配置的公平份额；未配置时按权重在全部租户间归一化。
			share := t.FairShare
			if share <= 0 && totalWeight > 0 && t.Weight > 0 {
				share = t.Weight / totalWeight
			}
			items = append(items, tenantCapacityItem{
				TenantID:  t.ID,
				Name:      t.Name,
				Requested: usage,
				Hard:      t.Quota,
				FairShare: share,
				JainShare: dominantShare(usage, totalCapacity),
			})
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Name != items[j].Name {
				return items[i].Name < items[j].Name
			}
			return items[i].TenantID < items[j].TenantID
		})
		if len(items) > limit {
			items = items[:limit]
		}
		writeOK(c, tenantCapacityPage{Items: items})
	}
}

// clusterCapacity 是所有可调度节点的可分配资源之和，与决策核使用的口径一致。
func clusterCapacity(snap capacity.Snapshot) schedcore.Resources {
	var sum schedcore.Resources
	for _, n := range snap.Nodes {
		if n.Ready && !n.Unschedulable {
			sum = sum.Add(n.Allocatable)
		}
	}
	return sum
}

// dominantShare 是租户在 CPU / 内存两维上的最大占用比例，对应契约里的 jain_share。
func dominantShare(used, cap schedcore.Resources) float64 {
	var share float64
	if cap.CPUMilli > 0 {
		if r := float64(used.CPUMilli) / float64(cap.CPUMilli); r > share {
			share = r
		}
	}
	if cap.MemBytes > 0 {
		if r := float64(used.MemBytes) / float64(cap.MemBytes); r > share {
			share = r
		}
	}
	return share
}

// ---------- 节点级明细（临时前端仪表盘用，非正式契约端点）----------

type nodeItem struct {
	Name       string              `json:"name"`
	Ready      bool                `json:"ready"`
	Allocatable schedcore.Resources `json:"allocatable"`
	Requested  schedcore.Resources `json:"requested"`
	Usage      schedcore.Resources `json:"usage"`
	UsageKnown bool                `json:"usage_known"`
	PodCount   int                 `json:"pod_count"`
	Topology   map[string]string   `json:"topology"`
}

type nodeListResp struct {
	AsOf      time.Time  `json:"as_of"`
	MetricsAt *time.Time `json:"metrics_at,omitempty"`
	Nodes     []nodeItem `json:"nodes"`
}

// nodeList 返回当前快照的节点级明细（含实际用量）。临时前端仪表盘依赖它，
// 正式契约端点留待 capacity/overview 扩展后再收敛。
func nodeList(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Snapshotter == nil {
			writeError(c, errUnavailable())
			return
		}
		snap, err := d.Snapshotter(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		out := make([]nodeItem, 0, len(snap.Nodes))
		for _, n := range snap.Nodes {
			out = append(out, nodeItem{
				Name:        n.Name,
				Ready:       n.Ready,
				Allocatable: n.Allocatable,
				Requested:   n.Requested,
				Usage:       n.Usage,
				UsageKnown:  n.UsageKnown,
				PodCount:    n.PodCount,
				Topology:    n.Topology,
			})
		}
		var metricsAt *time.Time
		if !snap.MetricsAt.IsZero() {
			t := snap.MetricsAt
			metricsAt = &t
		}
		writeOK(c, nodeListResp{AsOf: snap.TakenAt, MetricsAt: metricsAt, Nodes: out})
	}
}
