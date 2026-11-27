package schedcore

import (
	"errors"
	"testing"
)

func gangCtx(nodes []Node) ClusterContext {
	var capacity Resources
	for _, n := range nodes {
		capacity = capacity.Add(n.Allocatable)
	}
	return ClusterContext{
		Nodes:             nodes,
		Capacity:          capacity,
		MetricsKnown:      false, // gang 测试不关注负载维，s_load 归零走降级路径
		MinPendingPod:     Resources{CPUMilli: 100},
		Tenants:           map[string]TenantState{},
		TopologyCounts:    map[string]int{},
		MetricsAgeSeconds: 0,
	}
}

// TestPlaceGangFitsByGPU 验证 GPU 碎片下的整组装箱：8 卡总量、每副本 1 卡、
// 要 6 个副本，应成功并把副本按"整卡优先"分布在两个节点上。
func TestPlaceGangFitsByGPU(t *testing.T) {
	nodes := []Node{
		{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 4}},
		{Name: "b", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 4}},
	}
	wl := Workload{Replicas: 6, Requests: Resources{CPUMilli: 500, MemBytes: 1 * gi, GPU: 1}}

	d, err := PlaceGang(gangCtx(nodes), wl, Weights{CPU: 1}, 6)
	if err != nil {
		t.Fatalf("PlaceGang: %v", err)
	}
	if d.Status != StatusOK {
		t.Fatalf("Status = %s, 想要 OK", d.Status)
	}
	var total int
	for _, a := range d.Assignments {
		total += a.Replicas
	}
	if total != 6 {
		t.Fatalf("assignments 总副本 = %d, 想要 6（%v）", total, d.Assignments)
	}
	if len(d.Assignments) != 2 {
		t.Fatalf("分布节点数 = %d, 想要 2（两节点各放 3 份，CPU 是瓶颈）", len(d.Assignments))
	}
	if d.ChosenNode == "" {
		t.Fatal("ChosenNode 未设置")
	}
}

// TestPlaceGangInfeasibleFragmentation 验证碎片：8 卡分散为 4 节点各 2 卡，
// 每副本要 4 卡 → 没有任何节点能容纳单副本，全部以 INSUFFICIENT_GPU 被过滤，
// 整组失败且无 assignments（原因细节由 Filtered 承载）。
func TestPlaceGangInfeasibleFragmentation(t *testing.T) {
	nodes := []Node{
		{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 2}},
		{Name: "b", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 2}},
		{Name: "c", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 2}},
		{Name: "d", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 2}},
	}
	wl := Workload{Replicas: 4, Requests: Resources{CPUMilli: 500, MemBytes: 1 * gi, GPU: 4}}

	d, err := PlaceGang(gangCtx(nodes), wl, Weights{CPU: 1}, 2)
	if err != nil {
		t.Fatalf("PlaceGang: %v", err)
	}
	if d.Status != StatusNoCandidate {
		t.Fatalf("Status = %s, 想要 NO_CANDIDATE", d.Status)
	}
	if len(d.Assignments) != 0 {
		t.Fatalf("失败时不应输出 assignments: %v", d.Assignments)
	}
	if len(d.Filtered) != 4 {
		t.Fatalf("Filtered = %d 项, 想要 4", len(d.Filtered))
	}
	for _, f := range d.Filtered {
		if f.Reason != ReasonInsufficientGPU {
			t.Fatalf("节点 %s reason = %s, 想要 INSUFFICIENT_GPU", f.Node, f.Reason)
		}
	}
}

// TestPlaceGangPartialFeasibleBelowMin 验证可行总量不足 min_available 时整体失败：
// 每节点 CPU 余量只够 2 份，min_available=5 > 4 → 失败并给出 total_feasible=4。
func TestPlaceGangPartialFeasibleBelowMin(t *testing.T) {
	nodes := []Node{
		{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 1000, MemBytes: 16 * gi}},
		{Name: "b", Ready: true, Allocatable: Resources{CPUMilli: 1000, MemBytes: 16 * gi}},
	}
	wl := Workload{Replicas: 6, Requests: Resources{CPUMilli: 500, MemBytes: 1 * gi}}

	d, err := PlaceGang(gangCtx(nodes), wl, Weights{CPU: 1}, 5)
	if err != nil {
		t.Fatalf("PlaceGang: %v", err)
	}
	if d.Status != StatusNoCandidate {
		t.Fatalf("Status = %s, 想要 NO_CANDIDATE", d.Status)
	}
	if d.Evidence["total_feasible"] != 4 {
		t.Fatalf("total_feasible = %v, 想要 4", d.Evidence["total_feasible"])
	}
}

// TestPlaceGangNoGPUClusterRegression 验证 0-GPU 集群上 gang 与 CPU/内存语义正常，
// 且不因 GPU 维度产生除零或错误过滤。
func TestPlaceGangNoGPUClusterRegression(t *testing.T) {
	nodes := []Node{
		{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 4000, MemBytes: 8 * gi}},
		{Name: "b", Ready: true, Allocatable: Resources{CPUMilli: 4000, MemBytes: 8 * gi}},
	}
	wl := Workload{Replicas: 3, Requests: Resources{CPUMilli: 1000, MemBytes: 2 * gi}}

	// min_available 缺省 = replicas（全有全无）。
	d, err := PlaceGang(gangCtx(nodes), wl, Weights{CPU: 1}, 0)
	if err != nil {
		t.Fatalf("PlaceGang: %v", err)
	}
	if d.Status != StatusOK {
		t.Fatalf("Status = %s, 想要 OK", d.Status)
	}
	// 每节点 4000/1000=4 份 CPU、8/2=4 份内存 → 3 份全在第一个节点。
	if len(d.Assignments) != 1 || d.Assignments[0].Replicas != 3 {
		t.Fatalf("assignments = %v, 想要单节点 3 副本", d.Assignments)
	}
}

// TestPlaceGangInvalidArgs 验证参数校验：副本数非正、min_available 超过副本数。
func TestPlaceGangInvalidArgs(t *testing.T) {
	nodes := []Node{{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 4000, MemBytes: 8 * gi}}}
	w := Weights{CPU: 1}

	if _, err := PlaceGang(gangCtx(nodes), Workload{Replicas: 0, Requests: Resources{CPUMilli: 1}}, w, 0); !errors.Is(err, ErrInvalidGang) {
		t.Fatalf("零副本 err = %v, 想要 ErrInvalidGang", err)
	}
	if _, err := PlaceGang(gangCtx(nodes), Workload{Replicas: 2, Requests: Resources{CPUMilli: 1}}, w, 3); !errors.Is(err, ErrInvalidGang) {
		t.Fatalf("min>replicas err = %v, 想要 ErrInvalidGang", err)
	}
}

// TestPlaceGangFilteredNodesReported 验证被过滤节点如实出现在 Filtered 中：
// 无卡节点对需要 GPU 的负载报 INSUFFICIENT_GPU。
func TestPlaceGangFilteredNodesReported(t *testing.T) {
	nodes := []Node{
		{Name: "gpu0", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi}},
		{Name: "gpu1", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 16 * gi, GPU: 4}},
	}
	wl := Workload{Replicas: 1, Requests: Resources{CPUMilli: 500, MemBytes: 1 * gi, GPU: 1}}

	d, err := PlaceGang(gangCtx(nodes), wl, Weights{CPU: 1}, 1)
	if err != nil {
		t.Fatalf("PlaceGang: %v", err)
	}
	if d.Status != StatusOK {
		t.Fatalf("Status = %s, 想要 OK", d.Status)
	}
	var gpuFiltered bool
	for _, f := range d.Filtered {
		if f.Node == "gpu0" && f.Reason == ReasonInsufficientGPU {
			gpuFiltered = true
		}
	}
	if !gpuFiltered {
		t.Fatalf("gpu0 未以 INSUFFICIENT_GPU 被过滤: %v", d.Filtered)
	}
	if len(d.Assignments) != 1 || d.Assignments[0].Node != "gpu1" {
		t.Fatalf("assignments = %v, 想要全部落在 gpu1", d.Assignments)
	}
}

// TestPlaceGangFilterReasonPriority 验证原因顺序：GPU 与内存同时不足时报内存
// （固定顺序 CPU → GPU → 内存…等等，按 placement.go 实现为 CPU → GPU → 内存）。
// 本例 CPU 充足、GPU 与内存都不足 → 按 CPU → GPU 的顺序应报 INSUFFICIENT_GPU。
func TestPlaceGangFilterReasonPriority(t *testing.T) {
	node := Node{Name: "a", Ready: true, Allocatable: Resources{CPUMilli: 8000, MemBytes: 1 * gi, GPU: 0}}
	wl := Workload{Replicas: 1, Requests: Resources{CPUMilli: 100, MemBytes: 2 * gi, GPU: 1}}

	ok, reason, _ := filterNode(ClusterContext{Nodes: []Node{node}}, wl, node)
	if ok {
		t.Fatal("不应通过过滤")
	}
	if reason != ReasonInsufficientGPU {
		t.Fatalf("reason = %s, 想要 INSUFFICIENT_GPU（CPU 充足时 GPU 先于内存判定）", reason)
	}
}
