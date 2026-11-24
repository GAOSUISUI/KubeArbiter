package schedcore

import (
	"math"
	"sort"
	"strings"
)

const (
	// loadWeightDropAgeSeconds 来自算法规范 2.6：指标年龄超过 60s 时 s_load 权重归零。
	loadWeightDropAgeSeconds = 60.0
	epsilon                  = 1e-9
)

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ratio 返回分子占分母的比例；分母非正时返回 0，避免除零。
func ratio(num, den int64) float64 {
	if den <= 0 {
		return 0
	}
	return float64(num) / float64(den)
}

// cv2 是两个数的变异系数闭式解 |a-b|/(a+b)；两者皆为 0 时定义为 0。
func cv2(a, b float64) float64 {
	if a+b == 0 {
		return 0
	}
	return math.Abs(a-b) / (a + b)
}

// dominantShare 返回资源向量在容量上的主导份额 max_r(x_r / C_r)。
func dominantShare(x, capacity Resources) float64 {
	var s float64
	if capacity.CPUMilli > 0 {
		s = math.Max(s, float64(x.CPUMilli)/float64(capacity.CPUMilli))
	}
	if capacity.MemBytes > 0 {
		s = math.Max(s, float64(x.MemBytes)/float64(capacity.MemBytes))
	}
	return s
}

// Place 执行完整放置决策：硬过滤 → 七维评分 → 确定性排序。
// 权重非法时返回 ErrInvalidWeights；其余情况通过 Decision.Status 表达结果。
func Place(ctx ClusterContext, wl Workload, w Weights) (Decision, error) {
	if err := w.Validate(); err != nil {
		return Decision{}, err
	}

	decision := Decision{
		Scores:   []NodeScore{},
		Filtered: []FilteredNode{},
	}

	if len(ctx.Nodes) == 0 {
		decision.Status = StatusNoCandidate
		decision.Filtered = append(decision.Filtered, FilteredNode{
			Node:   "",
			Reason: ReasonNoNodeInSnapshot,
		})
		return decision, nil
	}

	// 指标过期或缺失：s_load 归零后按比例重新归一化，禁止静默使用过期数据。
	if !ctx.MetricsKnown || ctx.MetricsAgeSeconds > loadWeightDropAgeSeconds {
		decision.Degraded = true
		if !ctx.MetricsKnown {
			decision.Warnings = append(decision.Warnings, WarnMetricsMissing)
		} else {
			decision.Warnings = append(decision.Warnings, WarnMetricsStale)
		}
		w.Load = 0
		if s := w.Sum(); s > epsilon {
			w.CPU /= s
			w.Mem /= s
			w.Balance /= s
			w.Pack /= s
			w.Fair /= s
			w.Frag /= s
		}
	}

	// 基线模式依赖 fair_share；DRF 模式用权重份额做目标，不需要该字段。
	if wl.TenantID != "" && ctx.FairMode.orDefault() == FairModeWeightedShare {
		if ts, ok := ctx.Tenants[wl.TenantID]; ok && ts.FairShare <= 0 {
			decision.Warnings = append(decision.Warnings, WarnFairShareMissing)
		}
	}

	totalWeight := w.Sum()
	candidates := make([]Node, 0, len(ctx.Nodes))
	for _, n := range ctx.Nodes {
		ok, reason, evidence := filterNode(ctx, wl, n)
		if !ok {
			decision.Filtered = append(decision.Filtered, FilteredNode{
				Node:     n.Name,
				Reason:   reason,
				Evidence: evidence,
			})
			continue
		}
		candidates = append(candidates, n)
	}

	if len(candidates) == 0 {
		decision.Status = StatusNoCandidate
		return decision, nil
	}

	for _, n := range candidates {
		score := scoreNode(ctx, wl, n, w, totalWeight)
		decision.Scores = append(decision.Scores, score)
	}

	// 得分相同按节点名字节序升序，保证同一快照多次运行结果一致。
	sort.SliceStable(decision.Scores, func(i, j int) bool {
		diff := decision.Scores[i].Score - decision.Scores[j].Score
		if math.Abs(diff) > epsilon {
			return diff > 0
		}
		return decision.Scores[i].Node < decision.Scores[j].Node
	})
	for i := range decision.Scores {
		decision.Scores[i].Rank = i + 1
	}

	decision.Status = StatusOK
	decision.ChosenNode = decision.Scores[0].Node
	return decision, nil
}

// filterNode 按算法规范 2.2 的固定顺序做硬过滤，命中即返回。
func filterNode(ctx ClusterContext, wl Workload, n Node) (bool, ReasonCode, map[string]any) {
	if !n.Ready {
		return false, ReasonNodeNotReady, nil
	}
	if n.Unschedulable {
		return false, ReasonNodeUnschedulable, nil
	}

	d := wl.Requests
	if remaining := n.Allocatable.Sub(n.Requested); !d.FitsIn(remaining) {
		// 两个维度都可能不足，先报 CPU 再报内存，保证原因是确定的。
		if d.CPUMilli > remaining.CPUMilli {
			return false, ReasonInsufficientCPU, map[string]any{
				"remaining_cpu_milli": remaining.CPUMilli,
				"request_cpu_milli":   d.CPUMilli,
			}
		}
		return false, ReasonInsufficientMemory, map[string]any{
			"remaining_mem_bytes": remaining.MemBytes,
			"request_mem_bytes":   d.MemBytes,
		}
	}

	for _, t := range n.Taints {
		if t.Effect != "NoSchedule" && t.Effect != "NoExecute" {
			continue
		}
		if !tolerated(wl.Tolerations, t) {
			return false, ReasonTaintNotTolerated, map[string]any{
				"taint_key":    t.Key,
				"taint_value":  t.Value,
				"taint_effect": t.Effect,
			}
		}
	}

	for k, v := range wl.NodeSelector {
		if n.Labels[k] != v {
			return false, ReasonNodeSelectorMismatch, map[string]any{
				"key": k, "want": v, "got": n.Labels[k],
			}
		}
	}

	if !affinityMatches(wl.Affinity, n.Labels) {
		return false, ReasonAffinityMismatch, nil
	}

	if wl.TopologySpread != nil {
		if ok, ev := topologyOK(ctx, n, *wl.TopologySpread); !ok {
			return false, ReasonTopologyViolation, ev
		}
	}

	for _, p := range wl.HostPorts {
		if holder, used := n.HostPorts[p]; used {
			return false, ReasonHostPortConflict, map[string]any{
				"port": p, "holder": holder,
			}
		}
	}

	if wl.TenantID != "" {
		if ts, ok := ctx.Tenants[wl.TenantID]; ok && !ts.Quota.IsZero() {
			after := ts.Allocated.Add(wl.Demand())
			if !after.FitsIn(ts.Quota) {
				return false, ReasonTenantQuotaExceeded, map[string]any{
					"quota_cpu_milli": ts.Quota.CPUMilli,
					"quota_mem_bytes": ts.Quota.MemBytes,
					"after_cpu_milli": after.CPUMilli,
					"after_mem_bytes": after.MemBytes,
				}
			}
		}
	}

	return true, "", nil
}

func tolerated(tolerations []Toleration, t Taint) bool {
	for _, tol := range tolerations {
		if tol.Effect != "" && tol.Effect != t.Effect {
			continue
		}
		switch tol.Operator {
		case "Exists":
			if tol.Key == "" || tol.Key == t.Key {
				return true
			}
		case "Equal", "":
			if tol.Key == t.Key && tol.Value == t.Value {
				return true
			}
		}
	}
	return false
}

func affinityMatches(reqs []NodeAffinityRequirement, labels map[string]string) bool {
	for _, r := range reqs {
		val, exists := labels[r.Key]
		switch r.Operator {
		case "In":
			matched := false
			for _, v := range r.Values {
				if exists && v == val {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		case "NotIn":
			for _, v := range r.Values {
				if exists && v == val {
					return false
				}
			}
		case "Exists":
			if !exists {
				return false
			}
		case "DoesNotExist":
			if exists {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// topologyOK 做基础版拓扑分布校验：加入一个副本后，该域与最小域之差不得超过 maxSkew。
func topologyOK(ctx ClusterContext, n Node, ts TopologySpread) (bool, map[string]any) {
	domain := n.Topology[ts.TopologyKey]
	counts := map[string]int{}
	for k, v := range ctx.TopologyCounts {
		prefix := ts.TopologyKey + "="
		if strings.HasPrefix(k, prefix) {
			counts[strings.TrimPrefix(k, prefix)] = v
		}
	}
	minCount := -1
	for _, c := range counts {
		if minCount < 0 || c < minCount {
			minCount = c
		}
	}
	if minCount < 0 {
		return true, nil
	}
	skew := counts[domain] + 1 - minCount
	if skew > ts.MaxSkew {
		return false, map[string]any{
			"topology_key": ts.TopologyKey,
			"domain":       domain,
			"skew":         skew,
			"max_skew":     ts.MaxSkew,
		}
	}
	return true, nil
}

// scoreNode 计算七维得分与总分，全部维度已归一化到 [0,1]。
func scoreNode(ctx ClusterContext, wl Workload, n Node, w Weights, totalWeight float64) NodeScore {
	d := wl.Requests
	a := n.Allocatable
	residual := a.Sub(n.Requested).Sub(d)

	sCPU := clamp01(ratio(residual.CPUMilli, a.CPUMilli))
	sMem := clamp01(ratio(residual.MemBytes, a.MemBytes))

	// 放置后的使用率：有实测用量则用实测，否则退回分配量。
	var postCPU, postMem float64
	if n.UsageKnown {
		postCPU = ratio(n.Usage.CPUMilli+d.CPUMilli, a.CPUMilli)
		postMem = ratio(n.Usage.MemBytes+d.MemBytes, a.MemBytes)
	} else {
		postCPU = ratio(n.Requested.CPUMilli+d.CPUMilli, a.CPUMilli)
		postMem = ratio(n.Requested.MemBytes+d.MemBytes, a.MemBytes)
	}
	sBalance := 1 - clamp01(cv2(postCPU, postMem))

	sPack := 1 - clamp01(0.5*(ratio(residual.CPUMilli, a.CPUMilli)+ratio(residual.MemBytes, a.MemBytes)))

	var usageCPU, usageMem float64
	if n.UsageKnown {
		usageCPU = ratio(n.Usage.CPUMilli, a.CPUMilli)
		usageMem = ratio(n.Usage.MemBytes, a.MemBytes)
	}
	sLoad := 1 - clamp01(math.Max(usageCPU, usageMem))

	// 公平维度：默认 DRF 节点级试算，基线模式退化为全局份额比。
	sFair := fairScore(ctx, wl, n)

	var unusableCPU, unusableMem float64
	if residual.CPUMilli < ctx.MinPendingPod.CPUMilli {
		unusableCPU = ratio(residual.CPUMilli, a.CPUMilli)
	}
	if residual.MemBytes < ctx.MinPendingPod.MemBytes {
		unusableMem = ratio(residual.MemBytes, a.MemBytes)
	}
	sFrag := 1 - clamp01(0.5*(unusableCPU+unusableMem))

	breakdown := map[string]float64{
		"s_cpu":     round4(sCPU),
		"s_mem":     round4(sMem),
		"s_balance": round4(sBalance),
		"s_pack":    round4(sPack),
		"s_load":    round4(sLoad),
		"s_fair":    round4(sFair),
		"s_frag":    round4(sFrag),
	}

	var total float64
	if totalWeight > epsilon {
		total = 100 * (w.CPU*sCPU + w.Mem*sMem + w.Balance*sBalance +
			w.Pack*sPack + w.Load*sLoad + w.Fair*sFair + w.Frag*sFrag) / totalWeight
	}

	return NodeScore{
		Node:      n.Name,
		Score:     total,
		Breakdown: breakdown,
	}
}

func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}

// fairScore 计算公平维度 s_fair。
//
// DRF 模式（默认）对每个候选节点单独做一次加权 DRF 试算：需求集为"本租户
// （含本次放置需求）+ 该节点上其他租户"，可竞争容量为该节点扣除系统占用后的余量，
// 返回本租户需求被满足的比例。节点上已有该租户的 pod 时满足比例下降，
// 于是 s_fair 随节点变化，真正参与排序——这一步修掉了基线算法只产出全局常量的缺陷。
func fairScore(ctx ClusterContext, wl Workload, n Node) float64 {
	if wl.TenantID == "" {
		return 1.0
	}
	ts, ok := ctx.Tenants[wl.TenantID]
	if !ok {
		return 1.0
	}
	if ctx.FairMode.orDefault() == FairModeWeightedShare {
		if ts.FairShare <= 0 {
			return 0
		}
		after := ts.Allocated.Add(wl.Demand())
		return 1 - clamp01(dominantShare(after, ctx.Capacity)/ts.FairShare)
	}
	return drfNodeScore(ctx, wl, n, ts)
}

// drfNodeScore 在单个节点上执行加权 DRF 仲裁，把目标租户分到的主导份额
// 与其权重目标对比：份额接近或低于目标得高分，超出目标得低分。
func drfNodeScore(ctx ClusterContext, wl Workload, n Node, ts TenantState) float64 {
	want := n.TenantUsage[wl.TenantID].Add(wl.Demand())
	if want.IsZero() {
		return 1
	}

	// 可竞争容量 = allocatable − 非租户 pod 占用；租户之间只争这部分资源。
	var tenantSum Resources
	for _, u := range n.TenantUsage {
		tenantSum = tenantSum.Add(u)
	}
	available := nonNegative(n.Allocatable.Sub(nonNegative(n.Requested.Sub(tenantSum))))
	if available.IsZero() {
		return 0
	}

	ids := make([]string, 0, len(n.TenantUsage)+1)
	seen := make(map[string]bool, len(n.TenantUsage)+1)
	for id := range n.TenantUsage {
		ids = append(ids, id)
		seen[id] = true
	}
	if !seen[wl.TenantID] {
		ids = append(ids, wl.TenantID)
	}
	// 排序保证同一快照多次运行得到同一分配，不受 map 迭代顺序影响。
	sort.Strings(ids)

	demands := make([]TenantDemand, 0, len(ids))
	for _, id := range ids {
		weight := 1.0
		if t, ok := ctx.Tenants[id]; ok && t.Weight > 0 {
			weight = t.Weight
		}
		usage := n.TenantUsage[id]
		if id == wl.TenantID {
			usage = want
		}
		demands = append(demands, TenantDemand{ID: id, Weight: weight, Demand: usage})
	}

	plan, err := Arbitrate(available, demands, AllocatorDRF)
	if err != nil {
		return 0
	}
	var got Resources
	for _, a := range plan.Allocations {
		if a.TenantID == wl.TenantID {
			got = a.Allocated
			break
		}
	}

	// 权重目标：该租户权重 / 全部租户权重之和。
	weight := ts.Weight
	if weight <= 0 {
		weight = 1
	}
	totalWeight := 0.0
	for _, t := range ctx.Tenants {
		if t.Weight > 0 {
			totalWeight += t.Weight
		} else {
			totalWeight++
		}
	}
	if totalWeight <= 0 {
		return 0
	}
	target := weight / totalWeight
	if target <= 0 {
		return 0
	}
	return 1 - clamp01(dominantShare(got, available)/target)
}
