// Package bench 用确定性离线仿真比较调度策略与公平模式。
package bench

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	gi                  = int64(1) << 30
	mi                  = int64(1) << 20
	targetDemandRatio   = 0.83
	hotLateFraction     = 0.05
	hotLateDemandFactor = 18.0
)

var (
	benchmarkPod = schedcore.Resources{CPUMilli: 100, MemBytes: 128 * mi}
	cpuChoices   = [...]int64{100, 200, 500, 1000}
	tenantSpecs  = []struct {
		id     string
		weight float64
	}{
		{id: "tenant-a", weight: 4},
		{id: "tenant-b", weight: 2},
		{id: "tenant-c", weight: 1},
		{id: "tenant-d", weight: 1},
	}
)

// Metrics 是一个变体完成全部放置轮次后的汇总指标。
type Metrics struct {
	Jain                float64 `json:"jain"`
	CPUUtilization      float64 `json:"cpu_utilization"`
	MemUtilization      float64 `json:"mem_utilization"`
	Fragmentation       float64 `json:"fragmentation"`
	InfeasibleRate      float64 `json:"infeasible_rate"`
	MeanTenantHHI       float64 `json:"mean_tenant_hhi"`
	MaxNodeReplicaShare float64 `json:"max_node_replica_share"`
	AverageDecisionUS   float64 `json:"average_decision_us"`
	P99US               float64 `json:"p99_us"`
}

// TenantShare 记录 Jain 的输入，便于定位全局公平性差异来自哪个租户。
type TenantShare struct {
	TenantID        string  `json:"tenant_id"`
	DominantShare   float64 `json:"dominant_share"`
	Weight          float64 `json:"weight"`
	NormalizedShare float64 `json:"dominant_share_over_weight"`
}

// VariantResult 保存一个策略与公平模式组合的结果。
type VariantResult struct {
	Name                 string             `json:"name"`
	Strategy             schedcore.Strategy `json:"strategy"`
	FairMode             schedcore.FairMode `json:"fair_mode"`
	Metrics              Metrics            `json:"metrics"`
	TenantShares         []TenantShare      `json:"tenant_shares"`
	FirstInfeasibleRound int                `json:"first_infeasible_round,omitempty"`
	// NodeSequence 用于验证变体确实走出了不同放置路径，不写入报告。
	NodeSequence []string `json:"-"`
}

// ScenarioResult 是同一份到达序列下所有变体的结果。
type ScenarioResult struct {
	Name     string          `json:"name"`
	Variants []VariantResult `json:"variants"`
}

// Result 是一次完整基准评测的结构化结果。
type Result struct {
	Seed              int64            `json:"seed"`
	Rounds            int              `json:"rounds"`
	TargetDemandRatio float64          `json:"target_demand_ratio"`
	Scenarios         []ScenarioResult `json:"scenarios"`
	Conclusions       []string         `json:"conclusions"`
}

type variantSpec struct {
	name     string
	strategy schedcore.Strategy
	fairMode schedcore.FairMode
}

var variantSpecs = []variantSpec{
	{name: "balanced-drf", strategy: schedcore.Balanced, fairMode: schedcore.FairModeDRF},
	{name: "balanced-baseline", strategy: schedcore.Balanced, fairMode: schedcore.FairModeWeightedShare},
	{name: "fairshare-drf", strategy: schedcore.FairShare, fairMode: schedcore.FairModeDRF},
	{name: "fairshare-baseline", strategy: schedcore.FairShare, fairMode: schedcore.FairModeWeightedShare},
	{name: "binpack-drf", strategy: schedcore.BinPack, fairMode: schedcore.FairModeDRF},
}

// Run 执行 balanced 与 hot-tenant 两个场景，计时仅覆盖进程内 Place 调用。
func Run(seed int64, rounds int) (Result, error) {
	return run(seed, rounds, time.Now)
}

// run 允许单测注入单调时钟，从而连延迟字段也能逐项验证可复现性。
func run(seed int64, rounds int, now func() time.Time) (Result, error) {
	if rounds <= 0 {
		return Result{}, fmt.Errorf("轮数必须大于 0，当前为 %d", rounds)
	}

	result := Result{Seed: seed, Rounds: rounds, TargetDemandRatio: targetDemandRatio}
	for _, scenario := range []struct {
		name string
		hot  bool
	}{{"balanced", false}, {"hot-tenant", true}} {
		initial := initialContext()
		workloads := generateWorkloads(seed, rounds, scenario.hot, initial.Capacity)
		scenarioResult := ScenarioResult{Name: scenario.name}
		for _, spec := range variantSpecs {
			weights, err := schedcore.PresetWeights(spec.strategy)
			if err != nil {
				return Result{}, fmt.Errorf("读取变体 %s 的权重: %w", spec.name, err)
			}
			scenarioResult.Variants = append(scenarioResult.Variants,
				simulate(initial, workloads, spec, weights, now))
		}
		result.Scenarios = append(result.Scenarios, scenarioResult)
	}
	result.Conclusions = buildConclusions(result)
	return result, nil
}

// generateWorkloads 预先生成整段到达序列，所有变体因而面对完全相同的输入。
func generateWorkloads(seed int64, rounds int, hot bool, capacity schedcore.Resources) []schedcore.Workload {
	rng := rand.New(rand.NewSource(seed))
	meanCPU := 0.0
	for _, cpu := range cpuChoices {
		meanCPU += float64(cpu)
	}
	meanCPU /= float64(len(cpuChoices))
	lateStart := rounds - int(math.Ceil(float64(rounds)*hotLateFraction))
	if hot {
		// 对末期突发做分段期望修正，避免放大热点需求时顺带抬高集群总负载。
		meanHotFactor := (float64(lateStart) + float64(rounds-lateStart)*hotLateDemandFactor) / float64(rounds)
		meanCPU *= 0.7*meanHotFactor + 0.3
	}
	// 由采样分布的期望反推缩放系数，使 rounds 轮的总期望需求达到目标集群利用率。
	scale := targetDemandRatio * float64(capacity.CPUMilli) / (float64(rounds) * meanCPU)
	workloads := make([]schedcore.Workload, rounds)
	for i := range workloads {
		tenantID := pickTenant(rng, hot)
		demandFactor := 1.0
		if hot && tenantID == tenantSpecs[0].id && i >= lateStart {
			demandFactor = hotLateDemandFactor
		}
		cpu := int64(math.Round(float64(cpuChoices[rng.Intn(len(cpuChoices))]) * scale * demandFactor))
		if cpu < 1 {
			cpu = 1
		}
		// 每 1000m 对应 1~2Gi，再乘 0.9~1.1 抖动；取整到 Mi 避免浮点尾数进入调度核。
		ratio := 1 + rng.Float64()
		jitter := 0.9 + 0.2*rng.Float64()
		mem := math.Round(float64(cpu) / 1000 * ratio * jitter * float64(gi) / float64(mi))
		workloads[i] = schedcore.Workload{
			Name:     fmt.Sprintf("wl-%04d", i+1),
			TenantID: tenantID,
			Replicas: 1,
			Requests: schedcore.Resources{CPUMilli: cpu, MemBytes: int64(mem) * mi},
		}
	}
	return workloads
}

// pickTenant 在 hot 场景把 70% 到达固定给权重 4 的租户，其余 30% 仍按 2:1:1 分配。
func pickTenant(rng *rand.Rand, hot bool) string {
	draw := rng.Float64()
	start := 0
	totalWeight := 8.0
	if hot {
		if draw < 0.7 {
			return tenantSpecs[0].id
		}
		// 复用同一个随机数，避免两个场景的资源需求序列被额外随机调用打散。
		draw = (draw - 0.7) / 0.3
		start = 1
		totalWeight = 4
	}
	target := draw * totalWeight
	for i := start; i < len(tenantSpecs); i++ {
		target -= tenantSpecs[i].weight
		if target < 0 {
			return tenantSpecs[i].id
		}
	}
	return tenantSpecs[len(tenantSpecs)-1].id
}

func initialContext() schedcore.ClusterContext {
	ctx := schedcore.ClusterContext{
		Nodes:         make([]schedcore.Node, 6),
		Tenants:       make(map[string]schedcore.TenantState, len(tenantSpecs)),
		MetricsKnown:  true,
		MinPendingPod: benchmarkPod,
	}
	for i := range ctx.Nodes {
		resources := schedcore.Resources{CPUMilli: 4000, MemBytes: 8 * gi}
		if i >= 4 {
			resources = schedcore.Resources{CPUMilli: 8000, MemBytes: 16 * gi}
		}
		ctx.Nodes[i] = schedcore.Node{
			Name:        fmt.Sprintf("n%d", i+1),
			Ready:       true,
			Allocatable: resources,
			UsageKnown:  true,
			TenantUsage: make(map[string]schedcore.Resources),
		}
		ctx.Capacity = ctx.Capacity.Add(resources)
	}
	for _, spec := range tenantSpecs {
		ctx.Tenants[spec.id] = schedcore.TenantState{
			ID:        spec.id,
			Weight:    spec.weight,
			FairShare: spec.weight / 8,
		}
	}
	return ctx
}

// cloneContext 复制仿真会修改的节点切片与 map，确保变体都从同一初始状态出发。
func cloneContext(ctx schedcore.ClusterContext) schedcore.ClusterContext {
	cloned := ctx
	cloned.Nodes = make([]schedcore.Node, len(ctx.Nodes))
	for i, node := range ctx.Nodes {
		cloned.Nodes[i] = node
		cloned.Nodes[i].TenantUsage = make(map[string]schedcore.Resources, len(node.TenantUsage))
		for id, usage := range node.TenantUsage {
			cloned.Nodes[i].TenantUsage[id] = usage
		}
	}
	cloned.Tenants = make(map[string]schedcore.TenantState, len(ctx.Tenants))
	for id, tenant := range ctx.Tenants {
		cloned.Tenants[id] = tenant
	}
	return cloned
}

func simulate(initial schedcore.ClusterContext, workloads []schedcore.Workload, spec variantSpec,
	weights schedcore.Weights, now func() time.Time) VariantResult {
	ctx := cloneContext(initial)
	ctx.FairMode = spec.fairMode
	result := VariantResult{
		Name:         spec.name,
		Strategy:     spec.strategy,
		FairMode:     spec.fairMode,
		NodeSequence: make([]string, len(workloads)),
	}
	latencies := make([]time.Duration, 0, len(workloads))
	replicaCounts := make(map[string]map[string]int, len(tenantSpecs))
	infeasible := 0
	for i, workload := range workloads {
		start := now()
		decision, err := schedcore.Place(ctx, workload, weights)
		latencies = append(latencies, now().Sub(start))
		// 调度核用 NO_CANDIDATE 表达容量不足，而不是返回 error；两者都表示本轮未放置。
		if err != nil || decision.Status != schedcore.StatusOK || decision.ChosenNode == "" {
			infeasible++
			if result.FirstInfeasibleRound == 0 {
				result.FirstInfeasibleRound = i + 1
			}
			result.NodeSequence[i] = "-"
			continue
		}

		result.NodeSequence[i] = decision.ChosenNode
		if replicaCounts[workload.TenantID] == nil {
			replicaCounts[workload.TenantID] = make(map[string]int, len(ctx.Nodes))
		}
		replicaCounts[workload.TenantID][decision.ChosenNode] += workload.Replicas
		demand := workload.Demand()
		for j := range ctx.Nodes {
			node := &ctx.Nodes[j]
			if node.Name != decision.ChosenNode {
				continue
			}
			node.Requested = node.Requested.Add(demand)
			node.TenantUsage[workload.TenantID] = node.TenantUsage[workload.TenantID].Add(demand)
			break
		}
		tenant := ctx.Tenants[workload.TenantID]
		tenant.Allocated = tenant.Allocated.Add(demand)
		ctx.Tenants[workload.TenantID] = tenant
	}
	result.Metrics, result.TenantShares = calculateMetrics(ctx, infeasible, len(workloads), latencies, replicaCounts)
	return result
}

func calculateMetrics(ctx schedcore.ClusterContext, infeasible, rounds int,
	latencies []time.Duration, replicaCounts map[string]map[string]int) (Metrics, []TenantShare) {
	shares := make([]float64, 0, len(tenantSpecs))
	tenantShares := make([]TenantShare, 0, len(tenantSpecs))
	var allocated schedcore.Resources
	for _, spec := range tenantSpecs {
		tenant := ctx.Tenants[spec.id]
		allocated = allocated.Add(tenant.Allocated)
		dominant := math.Max(
			float64(tenant.Allocated.CPUMilli)/float64(ctx.Capacity.CPUMilli),
			float64(tenant.Allocated.MemBytes)/float64(ctx.Capacity.MemBytes),
		)
		normalized := dominant / spec.weight
		shares = append(shares, normalized)
		tenantShares = append(tenantShares, TenantShare{
			TenantID:        spec.id,
			DominantShare:   dominant,
			Weight:          spec.weight,
			NormalizedShare: normalized,
		})
	}

	fragmentation := 0.0
	for _, node := range ctx.Nodes {
		remaining := node.Allocatable.Sub(node.Requested)
		if benchmarkPod.FitsIn(remaining) {
			continue
		}
		// 节点已放不下基准 Pod 时，只把最先耗尽的资源维度计为不可用残余。
		fragmentation += math.Min(
			float64(remaining.CPUMilli)/float64(ctx.Capacity.CPUMilli),
			float64(remaining.MemBytes)/float64(ctx.Capacity.MemBytes),
		)
	}

	meanHHI, maxNodeShare := calculateDispersion(ctx.Nodes, replicaCounts)
	sorted := append([]time.Duration(nil), latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	totalLatency := time.Duration(0)
	for _, latency := range latencies {
		totalLatency += latency
	}
	return Metrics{
		Jain:                schedcore.JainIndex(shares),
		CPUUtilization:      float64(allocated.CPUMilli) / float64(ctx.Capacity.CPUMilli),
		MemUtilization:      float64(allocated.MemBytes) / float64(ctx.Capacity.MemBytes),
		Fragmentation:       fragmentation,
		InfeasibleRate:      float64(infeasible) / float64(rounds),
		MeanTenantHHI:       meanHHI,
		MaxNodeReplicaShare: maxNodeShare,
		AverageDecisionUS:   durationUS(totalLatency) / float64(rounds),
		P99US:               durationUS(percentile(sorted, 0.99)),
	}, tenantShares
}

// calculateDispersion 只统计成功放置的副本；没有副本的租户两项均计 0。
func calculateDispersion(nodes []schedcore.Node, replicaCounts map[string]map[string]int) (float64, float64) {
	meanHHI := 0.0
	maxNodeShare := 0.0
	for _, spec := range tenantSpecs {
		total := 0
		for _, node := range nodes {
			total += replicaCounts[spec.id][node.Name]
		}
		if total == 0 {
			continue
		}
		tenantHHI := 0.0
		for _, node := range nodes {
			share := float64(replicaCounts[spec.id][node.Name]) / float64(total)
			tenantHHI += share * share
			maxNodeShare = math.Max(maxNodeShare, share)
		}
		meanHHI += tenantHHI / float64(len(tenantSpecs))
	}
	return meanHHI, maxNodeShare
}

// percentile 使用 nearest-rank 定义，避免插值制造出并未观测到的延迟样本。
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	return sorted[index]
}

func durationUS(d time.Duration) float64 {
	return float64(d) / float64(time.Microsecond)
}
