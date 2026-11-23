package placement

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func nodeView(name string, ready bool, cpu, mem int64, pods int, zone string) capacity.NodeView {
	return capacity.NodeView{
		Name:        name,
		Ready:       ready,
		Allocatable: schedcore.Resources{CPUMilli: cpu, MemBytes: mem},
		Requested:   schedcore.Resources{CPUMilli: cpu / 4, MemBytes: mem / 4},
		PodCount:    pods,
		Labels:      map[string]string{"kubernetes.io/hostname": name},
		HostPorts:   map[int32]string{},
		Topology:    map[string]string{"kubernetes.io/hostname": name, "topology.kubernetes.io/zone": zone},
	}
}

func TestBuildContextMapsNodesAndCapacity(t *testing.T) {
	snap := capacity.Snapshot{
		TakenAt: time.Now(),
		Nodes: []capacity.NodeView{
			nodeView("ka-w1", true, 4000, 8<<30, 3, "z1"),
			nodeView("ka-w2", true, 4000, 8<<30, 5, "z2"),
			nodeView("ka-cp1", false, 2000, 4<<30, 1, "z1"),
		},
	}

	ctx := BuildContext(snap, nil)

	if len(ctx.Nodes) != 3 {
		t.Fatalf("节点数应为 3，实际 %d", len(ctx.Nodes))
	}
	// 未 Ready 的节点不计入可仲裁容量。
	want := schedcore.Resources{CPUMilli: 8000, MemBytes: 16 << 30}
	if ctx.Capacity != want {
		t.Fatalf("容量应为 %+v，实际 %+v", want, ctx.Capacity)
	}
	// 拓扑计数按域累加 PodCount：z1 = 3 + 1（cp1 未 Ready 但仍在快照中）。
	if got := ctx.TopologyCounts["topology.kubernetes.io/zone=z1"]; got != 4 {
		t.Fatalf("z1 计数应为 4，实际 %d", got)
	}
	if got := ctx.TopologyCounts["topology.kubernetes.io/zone=z2"]; got != 5 {
		t.Fatalf("z2 计数应为 5，实际 %d", got)
	}
	if ctx.MetricsKnown {
		t.Fatal("M1 未接指标，MetricsKnown 应为 false")
	}
	if ctx.Tenants == nil {
		t.Fatal("Tenants 不应为 nil，避免调用方再判空")
	}
}

func TestBuildContextMinPendingPod(t *testing.T) {
	snap := capacity.Snapshot{
		PendingPods: []capacity.PodView{
			{Namespace: "a", Name: "big", Requests: schedcore.Resources{CPUMilli: 800, MemBytes: 2 << 30}},
			{Namespace: "a", Name: "zero", Requests: schedcore.Resources{}},
			{Namespace: "a", Name: "small", Requests: schedcore.Resources{CPUMilli: 200, MemBytes: 1 << 30}},
		},
	}

	ctx := BuildContext(snap, nil)

	want := schedcore.Resources{CPUMilli: 200, MemBytes: 1 << 30}
	if ctx.MinPendingPod != want {
		t.Fatalf("最小待调度需求应为 %+v，实际 %+v", want, ctx.MinPendingPod)
	}
}

func TestBuildContextEmptySnapshot(t *testing.T) {
	ctx := BuildContext(capacity.Snapshot{}, nil)
	if len(ctx.Nodes) != 0 || !ctx.Capacity.IsZero() || !ctx.MinPendingPod.IsZero() {
		t.Fatalf("空快照应产生零值上下文，实际 %+v", ctx)
	}
}

func TestBuildContextKeepsTenants(t *testing.T) {
	tenants := map[string]schedcore.TenantState{
		"t1": {ID: "t1", Weight: 2, FairShare: 0.4},
	}
	ctx := BuildContext(capacity.Snapshot{}, tenants)
	if ctx.Tenants["t1"].Weight != 2 {
		t.Fatalf("租户状态应原样透传，实际 %+v", ctx.Tenants)
	}
}
