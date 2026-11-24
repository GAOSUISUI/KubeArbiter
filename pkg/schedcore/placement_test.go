package schedcore

import (
	"errors"
	"math"
	"testing"
)

const (
	gi = int64(1) << 30
)

func approx(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.6f, want %.6f (±%v)", what, got, want, tol)
	}
}

func hasWarning(list []WarningCode, want WarningCode) bool {
	for _, w := range list {
		if w == want {
			return true
		}
	}
	return false
}

func baseNode(name string) Node {
	return Node{
		Name:        name,
		Ready:       true,
		Allocatable: Resources{CPUMilli: 4000, MemBytes: 8 * gi},
		Requested:   Resources{CPUMilli: 1000, MemBytes: 2 * gi},
		UsageKnown:  true,
		Usage:       Resources{CPUMilli: 500, MemBytes: 1 * gi},
		Labels:      map[string]string{"zone": "z1"},
		HostPorts:   map[int32]string{},
		Topology:    map[string]string{"zone": "z1", "kubernetes.io/hostname": name},
	}
}

func baseCtx(nodes ...Node) ClusterContext {
	return ClusterContext{
		Nodes:             nodes,
		Capacity:          Resources{CPUMilli: 12000, MemBytes: 24 * gi},
		MinPendingPod:     Resources{CPUMilli: 100, MemBytes: 128 << 20},
		Tenants:           map[string]TenantState{},
		TopologyCounts:    map[string]int{},
		MetricsKnown:      true,
		MetricsAgeSeconds: 5,
	}
}

func balancedWeights(t *testing.T) Weights {
	t.Helper()
	w, err := PresetWeights(Balanced)
	if err != nil {
		t.Fatalf("预设权重不可用: %v", err)
	}
	return w
}

func TestWeightsValidate(t *testing.T) {
	cases := []struct {
		name    string
		weights Weights
		wantErr bool
	}{
		{"合法预设", Weights{CPU: 0.2, Mem: 0.2, Balance: 0.2, Pack: 0.1, Load: 0.1, Fair: 0.1, Frag: 0.1}, false},
		{"总和 0.99", Weights{CPU: 0.2, Mem: 0.2, Balance: 0.2, Pack: 0.1, Load: 0.1, Fair: 0.1, Frag: 0.09}, true},
		{"总和 1.01", Weights{CPU: 0.2, Mem: 0.2, Balance: 0.2, Pack: 0.1, Load: 0.1, Fair: 0.1, Frag: 0.11}, true},
		{"存在负权重", Weights{CPU: -0.1, Mem: 0.2, Balance: 0.2, Pack: 0.2, Load: 0.2, Fair: 0.2, Frag: 0.1}, true},
		{"全零", Weights{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.weights.Validate()
			if c.wantErr && !errors.Is(err, ErrInvalidWeights) {
				t.Fatalf("期望 ErrInvalidWeights，得到 %v", err)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望通过，得到 %v", err)
			}
		})
	}
}

func TestPlaceZeroCapacity(t *testing.T) {
	node := baseNode("n1")
	node.Requested = node.Allocatable // 剩余为 0

	decision, err := Place(baseCtx(node), Workload{
		Name: "w", Namespace: "default", Replicas: 1,
		Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.Status != StatusNoCandidate {
		t.Fatalf("status = %s, want NO_CANDIDATE", decision.Status)
	}
	if len(decision.Filtered) != 1 || decision.Filtered[0].Reason != ReasonInsufficientCPU {
		t.Fatalf("filtered = %+v, want 单条 INSUFFICIENT_CPU", decision.Filtered)
	}
}

func TestPlaceSingleNode(t *testing.T) {
	decision, err := Place(baseCtx(baseNode("only")), Workload{
		Name: "w", Namespace: "default", Replicas: 1,
		Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.Status != StatusOK || decision.ChosenNode != "only" {
		t.Fatalf("决策 = %+v, want OK/only", decision)
	}
	if decision.Scores[0].Score < 0 || decision.Scores[0].Score > 100 {
		t.Fatalf("总分越界: %v", decision.Scores[0].Score)
	}
}

func TestPlaceNoNodes(t *testing.T) {
	decision, err := Place(baseCtx(), Workload{Replicas: 1}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.Status != StatusNoCandidate || len(decision.Filtered) != 1 ||
		decision.Filtered[0].Reason != ReasonNoNodeInSnapshot {
		t.Fatalf("决策 = %+v, want NO_NODE_IN_SNAPSHOT", decision)
	}
}

func TestPlaceAllTainted(t *testing.T) {
	node := baseNode("n1")
	node.Taints = []Taint{{Key: "dedicated", Value: "db", Effect: "NoSchedule"}}

	decision, err := Place(baseCtx(node), Workload{
		Replicas: 1, Requests: Resources{CPUMilli: 100, MemBytes: 64 << 20},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.Status != StatusNoCandidate || decision.Filtered[0].Reason != ReasonTaintNotTolerated {
		t.Fatalf("决策 = %+v, want TAINT_NOT_TOLERATED", decision)
	}

	// 加了匹配的 Toleration 后应当可放置。
	decision, err = Place(baseCtx(node), Workload{
		Replicas:    1,
		Requests:    Resources{CPUMilli: 100, MemBytes: 64 << 20},
		Tolerations: []Toleration{{Key: "dedicated", Operator: "Equal", Value: "db", Effect: "NoSchedule"}},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.Status != StatusOK {
		t.Fatalf("status = %s, want OK", decision.Status)
	}
}

func TestPlaceTieBreakByName(t *testing.T) {
	wl := Workload{Replicas: 1, Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20}}
	decision, err := Place(baseCtx(baseNode("b-node"), baseNode("a-node")), wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if decision.ChosenNode != "a-node" {
		t.Fatalf("chosen = %s, want a-node（同分按名字升序）", decision.ChosenNode)
	}
	if decision.Scores[0].Rank != 1 || decision.Scores[1].Rank != 2 {
		t.Fatalf("rank 未按顺序赋值: %+v", decision.Scores)
	}
}

func TestPlaceTieBreakIsDeterministic(t *testing.T) {
	wl := Workload{Replicas: 2, Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20}}
	ctx := baseCtx(baseNode("n3"), baseNode("n1"), baseNode("n2"))

	first, err := Place(ctx, wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Place(ctx, wl, balancedWeights(t))
		if err != nil {
			t.Fatalf("Place 返回错误: %v", err)
		}
		if again.ChosenNode != first.ChosenNode {
			t.Fatalf("第 %d 次结果不同: %s vs %s", i, again.ChosenNode, first.ChosenNode)
		}
	}
}

func TestPlaceMetricsStaleDropsLoadWeight(t *testing.T) {
	heavy := baseNode("node-a")
	heavy.Usage = Resources{CPUMilli: 3500, MemBytes: 7 * gi}

	light := baseNode("node-b")
	light.Usage = Resources{CPUMilli: 500, MemBytes: 1 * gi}

	wl := Workload{Replicas: 1, Requests: Resources{CPUMilli: 500, MemBytes: 1 * gi}}

	fresh := baseCtx(heavy, light)
	freshDecision, err := Place(fresh, wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if freshDecision.Degraded {
		t.Fatalf("新鲜指标不应标记降级")
	}
	if freshDecision.ChosenNode != "node-b" {
		t.Fatalf("chosen = %s, want node-b（负载更低）", freshDecision.ChosenNode)
	}

	stale := baseCtx(heavy, light)
	stale.MetricsAgeSeconds = 61
	staleDecision, err := Place(stale, wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if !staleDecision.Degraded {
		t.Fatalf("指标 61s 应标记 degraded")
	}
	if !hasWarning(staleDecision.Warnings, WarnMetricsStale) {
		t.Fatalf("缺少 METRICS_STALE 警告: %+v", staleDecision.Warnings)
	}
	// s_load 权重归零后两个节点其余维度完全相同，应退化为按名字平局。
	if staleDecision.ChosenNode != "node-a" {
		t.Fatalf("chosen = %s, want node-a（忽略负载后按名字平局）", staleDecision.ChosenNode)
	}
}

func TestPlaceMetricsMissing(t *testing.T) {
	ctx := baseCtx(baseNode("n1"))
	ctx.MetricsKnown = false

	decision, err := Place(ctx, Workload{Replicas: 1, Requests: Resources{CPUMilli: 100, MemBytes: 64 << 20}}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if !decision.Degraded || !hasWarning(decision.Warnings, WarnMetricsMissing) {
		t.Fatalf("缺少 METRICS_MISSING 降级标记: %+v", decision)
	}
}

func TestPlaceFairShareMissing(t *testing.T) {
	ctx := baseCtx(baseNode("n1"))
	// 该用例验证基线算法的降级行为，需要显式切回 WEIGHTED_SHARE。
	ctx.FairMode = FairModeWeightedShare
	ctx.Tenants["t1"] = TenantState{ID: "t1", Weight: 1, FairShare: 0}

	decision, err := Place(ctx, Workload{
		TenantID: "t1", Replicas: 1, Requests: Resources{CPUMilli: 100, MemBytes: 64 << 20},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if !hasWarning(decision.Warnings, WarnFairShareMissing) {
		t.Fatalf("缺少 FAIR_SHARE_MISSING 警告: %+v", decision.Warnings)
	}
	approx(t, decision.Scores[0].Breakdown["s_fair"], 0, 1e-9, "s_fair")
}

func TestPlaceFilterReasons(t *testing.T) {
	wl := Workload{Replicas: 1, Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20}}

	// 未就绪
	notReady := baseNode("n1")
	notReady.Ready = false
	got, err := Place(baseCtx(notReady), wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonNodeNotReady {
		t.Fatalf("want NODE_NOT_READY, got %s", got.Filtered[0].Reason)
	}

	// 不可调度
	cordoned := baseNode("n1")
	cordoned.Unschedulable = true
	got, err = Place(baseCtx(cordoned), wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonNodeUnschedulable {
		t.Fatalf("want NODE_UNSCHEDULABLE, got %s", got.Filtered[0].Reason)
	}

	// 内存不足（CPU 足够）
	memFull := baseNode("n1")
	memFull.Requested = Resources{CPUMilli: 0, MemBytes: 8 * gi}
	got, err = Place(baseCtx(memFull), wl, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonInsufficientMemory {
		t.Fatalf("want INSUFFICIENT_MEMORY, got %s", got.Filtered[0].Reason)
	}

	// nodeSelector 不匹配
	got, err = Place(baseCtx(baseNode("n1")), Workload{
		Replicas:     1,
		Requests:     wl.Requests,
		NodeSelector: map[string]string{"zone": "z9"},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonNodeSelectorMismatch {
		t.Fatalf("want NODE_SELECTOR_MISMATCH, got %s", got.Filtered[0].Reason)
	}

	// nodeAffinity 不匹配
	got, err = Place(baseCtx(baseNode("n1")), Workload{
		Replicas: 1,
		Requests: wl.Requests,
		Affinity: []NodeAffinityRequirement{{Key: "zone", Operator: "In", Values: []string{"z2"}}},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonAffinityMismatch {
		t.Fatalf("want AFFINITY_MISMATCH, got %s", got.Filtered[0].Reason)
	}

	// hostPort 冲突
	portNode := baseNode("n1")
	portNode.HostPorts[8080] = "svc-a/pod-1"
	got, err = Place(baseCtx(portNode), Workload{
		Replicas: 1, Requests: wl.Requests, HostPorts: []int32{8080},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonHostPortConflict {
		t.Fatalf("want HOSTPORT_CONFLICT, got %s", got.Filtered[0].Reason)
	}

	// 租户配额超限
	ctx := baseCtx(baseNode("n1"))
	ctx.Tenants["t1"] = TenantState{
		ID: "t1", Weight: 1, FairShare: 0.5,
		Allocated: Resources{CPUMilli: 3700, MemBytes: 1 * gi},
		Quota:     Resources{CPUMilli: 4000, MemBytes: 8 * gi},
	}
	got, err = Place(ctx, Workload{
		TenantID: "t1", Replicas: 2, Requests: Resources{CPUMilli: 500, MemBytes: 256 << 20},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonTenantQuotaExceeded {
		t.Fatalf("want TENANT_QUOTA_EXCEEDED, got %s", got.Filtered[0].Reason)
	}

	// 拓扑分布超限：z1 已有 3 个，z2 为 0，maxSkew=1
	topo := baseNode("n1")
	topoCtx := baseCtx(topo)
	topoCtx.TopologyCounts = map[string]int{"zone=z1": 3, "zone=z2": 0}
	got, err = Place(topoCtx, Workload{
		Replicas:       1,
		Requests:       wl.Requests,
		TopologySpread: &TopologySpread{TopologyKey: "zone", MaxSkew: 1},
	}, balancedWeights(t))
	if err != nil {
		t.Fatalf("Place 错误: %v", err)
	}
	if got.Filtered[0].Reason != ReasonTopologyViolation {
		t.Fatalf("want TOPOLOGY_VIOLATION, got %s", got.Filtered[0].Reason)
	}
}

func TestPlaceInvalidWeights(t *testing.T) {
	_, err := Place(baseCtx(baseNode("n1")), Workload{Replicas: 1}, Weights{Frag: 0.5})
	if !errors.Is(err, ErrInvalidWeights) {
		t.Fatalf("want ErrInvalidWeights, got %v", err)
	}
}

func TestPresetWeightsAllValid(t *testing.T) {
	for _, s := range []Strategy{Balanced, BinPack, FairShare, LoadAware} {
		w, err := PresetWeights(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if err := w.Validate(); err != nil {
			t.Fatalf("%s 预设权重非法: %v", s, err)
		}
	}
	if _, err := PresetWeights("UNKNOWN"); err == nil {
		t.Fatalf("未知策略应当报错")
	}
}

func TestAffinityMatchesOperators(t *testing.T) {
	labels := map[string]string{"zone": "z1", "disk": "ssd"}
	cases := []struct {
		name string
		reqs []NodeAffinityRequirement
		want bool
	}{
		{"无约束", nil, true},
		{"In 命中", []NodeAffinityRequirement{{Key: "zone", Operator: "In", Values: []string{"z1", "z2"}}}, true},
		{"In 未命中", []NodeAffinityRequirement{{Key: "zone", Operator: "In", Values: []string{"z2"}}}, false},
		{"In 但标签不存在", []NodeAffinityRequirement{{Key: "gpu", Operator: "In", Values: []string{"a100"}}}, false},
		{"NotIn 通过", []NodeAffinityRequirement{{Key: "zone", Operator: "NotIn", Values: []string{"z2"}}}, true},
		{"NotIn 拒绝", []NodeAffinityRequirement{{Key: "zone", Operator: "NotIn", Values: []string{"z1"}}}, false},
		{"Exists 命中", []NodeAffinityRequirement{{Key: "disk", Operator: "Exists"}}, true},
		{"Exists 未命中", []NodeAffinityRequirement{{Key: "gpu", Operator: "Exists"}}, false},
		{"DoesNotExist 通过", []NodeAffinityRequirement{{Key: "gpu", Operator: "DoesNotExist"}}, true},
		{"DoesNotExist 拒绝", []NodeAffinityRequirement{{Key: "disk", Operator: "DoesNotExist"}}, false},
		{"未知算子保守拒绝", []NodeAffinityRequirement{{Key: "zone", Operator: "Gt", Values: []string{"1"}}}, false},
		{"多条约束需全部满足", []NodeAffinityRequirement{
			{Key: "zone", Operator: "In", Values: []string{"z1"}},
			{Key: "disk", Operator: "Exists"},
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := affinityMatches(c.reqs, labels); got != c.want {
				t.Errorf("affinityMatches = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTolerated(t *testing.T) {
	taint := Taint{Key: "dedicated", Value: "gpu", Effect: "NoSchedule"}
	cases := []struct {
		name        string
		tolerations []Toleration
		want        bool
	}{
		{"无容忍", nil, false},
		{"Equal 完全匹配", []Toleration{{Key: "dedicated", Operator: "Equal", Value: "gpu", Effect: "NoSchedule"}}, true},
		{"Equal 值不匹配", []Toleration{{Key: "dedicated", Operator: "Equal", Value: "cpu", Effect: "NoSchedule"}}, false},
		{"Effect 不匹配", []Toleration{{Key: "dedicated", Operator: "Equal", Value: "gpu", Effect: "NoExecute"}}, false},
		{"Effect 留空表示全部", []Toleration{{Key: "dedicated", Operator: "Equal", Value: "gpu"}}, true},
		{"Exists 只匹配键", []Toleration{{Key: "dedicated", Operator: "Exists"}}, true},
		{"Exists 空键容忍一切", []Toleration{{Operator: "Exists"}}, true},
		{"Exists 键不匹配", []Toleration{{Key: "other", Operator: "Exists"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := tolerated(c.tolerations, taint); got != c.want {
				t.Errorf("tolerated = %v, want %v", got, c.want)
			}
		})
	}
}

// sFairOf 取出某节点的 s_fair 子分。
func sFairOf(t *testing.T, d Decision, node string) float64 {
	t.Helper()
	for _, s := range d.Scores {
		if s.Node == node {
			return s.Breakdown["s_fair"]
		}
	}
	t.Fatalf("节点 %s 不在评分结果中: %+v", node, d.Scores)
	return 0
}

// TestPlaceDRFPrefersNodeWithLowerTenantShare 验证 DRF 公平维度随节点变化：
// 租户已在 node-a 占用大量资源时，公平维度把决策导向 node-b；基线模式则产出与节点无关的常量。
func TestPlaceDRFPrefersNodeWithLowerTenantShare(t *testing.T) {
	busy := baseNode("node-a")
	busy.Requested = Resources{CPUMilli: 3500, MemBytes: 7 * gi}
	busy.TenantUsage = map[string]Resources{"t1": {CPUMilli: 3500, MemBytes: 7 * gi}}
	idle := baseNode("node-b")
	idle.Requested = Resources{}

	wl := Workload{TenantID: "t1", Replicas: 1, Requests: Resources{CPUMilli: 200, MemBytes: 256 << 20}}
	fairOnly := Weights{Fair: 1}

	drfCtx := baseCtx(busy, idle)
	drfCtx.Tenants["t1"] = TenantState{ID: "t1", Weight: 1}

	drfDecision, err := Place(drfCtx, wl, fairOnly)
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	if drfDecision.ChosenNode != "node-b" {
		t.Fatalf("DRF 应导向占用更少的 node-b，实际 %s", drfDecision.ChosenNode)
	}
	busyFair := sFairOf(t, drfDecision, "node-a")
	idleFair := sFairOf(t, drfDecision, "node-b")
	if idleFair <= busyFair {
		t.Fatalf("空闲节点 s_fair(%.4f) 应高于忙碌节点(%.4f)", idleFair, busyFair)
	}
	// 单租户时权重目标为 100%：忙碌节点份额 0.925 → 0.075；空闲节点份额 0.05 → 0.95。
	approx(t, busyFair, 0.075, 1e-4, "忙碌节点 s_fair")
	approx(t, idleFair, 0.95, 1e-4, "空闲节点 s_fair")

	// 基线模式：s_fair 只依赖全局份额，两节点相同，按名字平局落到 node-a。
	baseFairCtx := baseCtx(busy, idle)
	baseFairCtx.FairMode = FairModeWeightedShare
	baseFairCtx.Tenants["t1"] = TenantState{
		ID: "t1", Weight: 1, FairShare: 0.5,
		Allocated: Resources{CPUMilli: 3500, MemBytes: 7 * gi},
	}
	baseDecision, err := Place(baseFairCtx, wl, fairOnly)
	if err != nil {
		t.Fatalf("基线 Place 返回错误: %v", err)
	}
	if baseDecision.ChosenNode != "node-a" {
		t.Fatalf("基线模式两节点同分，应按名字落到 node-a，实际 %s", baseDecision.ChosenNode)
	}
	approx(t, sFairOf(t, baseDecision, "node-a"), sFairOf(t, baseDecision, "node-b"), 1e-9, "基线 s_fair")
}

// TestPlaceDRFDeterministic 验证多租户场景下重复执行结果一致（不依赖 map 迭代顺序）。
func TestPlaceDRFDeterministic(t *testing.T) {
	busy := baseNode("node-a")
	busy.TenantUsage = map[string]Resources{
		"t1": {CPUMilli: 600, MemBytes: 512 << 20},
		"t2": {CPUMilli: 800, MemBytes: 1 * gi},
		"t3": {CPUMilli: 400, MemBytes: 1 * gi},
	}
	idle := baseNode("node-b")
	idle.TenantUsage = map[string]Resources{"t2": {CPUMilli: 100, MemBytes: 256 << 20}}

	ctx := baseCtx(busy, idle)
	ctx.Tenants["t1"] = TenantState{ID: "t1", Weight: 2}
	ctx.Tenants["t2"] = TenantState{ID: "t2", Weight: 1}
	ctx.Tenants["t3"] = TenantState{ID: "t3", Weight: 1}

	wl := Workload{TenantID: "t1", Replicas: 1, Requests: Resources{CPUMilli: 300, MemBytes: 128 << 20}}
	weights := balancedWeights(t)

	first, err := Place(ctx, wl, weights)
	if err != nil {
		t.Fatalf("Place 返回错误: %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := Place(ctx, wl, weights)
		if err != nil {
			t.Fatalf("第 %d 次 Place 返回错误: %v", i, err)
		}
		if again.ChosenNode != first.ChosenNode {
			t.Fatalf("第 %d 次选中节点不一致: %s vs %s", i, again.ChosenNode, first.ChosenNode)
		}
		for j := range again.Scores {
			if again.Scores[j].Node != first.Scores[j].Node ||
				math.Abs(again.Scores[j].Score-first.Scores[j].Score) > 1e-9 {
				t.Fatalf("第 %d 次评分不一致: %+v vs %+v", i, again.Scores[j], first.Scores[j])
			}
		}
	}
}
