package capacity

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// newTestView 用 fake clientset 起一套真实 informer，验证的是缓存与转换逻辑本身。
func newTestView(t *testing.T, objs ...runtime.Object) *View {
	t.Helper()
	v := NewView(fake.NewClientset(objs...), 0)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- v.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("informer 同步失败: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("informer 同步超时")
	}
	if !v.HasSynced() {
		t.Fatal("HasSynced 应为 true")
	}
	return v
}

func testNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				corev1.LabelHostname:     name,
				corev1.LabelTopologyZone: "z1",
			},
		},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints: []corev1.Taint{
				{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule},
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func testContainer(name, cpu, mem string) corev1.Container {
	return corev1.Container{
		Name: name,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			},
		},
	}
}

func testPod(namespace, name, nodeName string, phase corev1.PodPhase, containers ...corev1.Container) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{"app": name},
		},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: containers,
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	return pod
}

func TestSnapshotNodeState(t *testing.T) {
	v := newTestView(t, testNode("n1"))

	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.Nodes) != 1 {
		t.Fatalf("节点数 = %d, want 1", len(snap.Nodes))
	}
	n := snap.Nodes[0]
	if n.Name != "n1" || !n.Ready || !n.Unschedulable {
		t.Fatalf("节点状态错误: %+v", n)
	}
	if n.Allocatable.CPUMilli != 4000 || n.Allocatable.MemBytes != 8*1024*1024*1024 {
		t.Fatalf("Allocatable = %+v", n.Allocatable)
	}
	if len(n.Taints) != 1 || n.Taints[0].Key != "dedicated" || n.Taints[0].Effect != "NoSchedule" {
		t.Fatalf("Taints = %+v", n.Taints)
	}
	if n.Topology[corev1.LabelHostname] != "n1" || n.Topology[corev1.LabelTopologyZone] != "z1" {
		t.Fatalf("Topology = %+v", n.Topology)
	}
	if n.Labels["kubernetes.io/hostname"] != "n1" {
		t.Fatalf("Labels = %+v", n.Labels)
	}
}

func TestSnapshotAggregatesRequestsAndHostPorts(t *testing.T) {
	web := testPod("prod", "web", "n1", corev1.PodRunning,
		testContainer("app", "500m", "1Gi"),
		testContainer("sidecar", "250m", "512Mi"),
	)
	web.Spec.Containers[0].Ports = []corev1.ContainerPort{{HostPort: 8080, ContainerPort: 80}}
	web.Spec.InitContainers = []corev1.Container{testContainer("init", "1", "2Gi")}

	api := testPod("prod", "api", "n1", corev1.PodRunning, testContainer("app", "500m", "256Mi"))
	api.Spec.Containers[0].Ports = []corev1.ContainerPort{{HostPort: 9090, ContainerPort: 9090}}

	v := newTestView(t, testNode("n1"), web, api)
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	n := snap.Nodes[0]
	if n.PodCount != 2 {
		t.Fatalf("PodCount = %d, want 2", n.PodCount)
	}
	// web 的有效 request = 逐维 max(Σ容器, init) = max(750m, 1) = 1 CPU；max(1.5Gi, 2Gi) = 2Gi。
	wantCPU := int64(1000 + 500)
	wantMem := int64(2*1024*1024*1024 + 256*1024*1024)
	if n.Requested.CPUMilli != wantCPU || n.Requested.MemBytes != wantMem {
		t.Fatalf("Requested = %+v, want cpu=%d mem=%d", n.Requested, wantCPU, wantMem)
	}
	if n.HostPorts[8080] != "prod/web" || n.HostPorts[9090] != "prod/api" {
		t.Fatalf("HostPorts = %+v", n.HostPorts)
	}
}

func TestSnapshotPendingPods(t *testing.T) {
	pending := testPod("prod", "batch", "", corev1.PodPending, testContainer("worker", "2", "1Gi"))
	// 未调度但非 Pending 的 Pod（理论上少见）不计入 PendingPods。
	unknown := testPod("prod", "ghost", "", corev1.PodUnknown, testContainer("worker", "1", "1Gi"))

	v := newTestView(t, testNode("n1"), pending, unknown)
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snap.PendingPods) != 1 {
		t.Fatalf("PendingPods = %+v, want 1 条", snap.PendingPods)
	}
	p := snap.PendingPods[0]
	if p.Name != "batch" || p.Requests.CPUMilli != 2000 || p.Requests.MemBytes != 1024*1024*1024 {
		t.Fatalf("PendingPod = %+v", p)
	}
	if snap.Nodes[0].Requested.CPUMilli != 0 || snap.Nodes[0].PodCount != 0 {
		t.Fatalf("未调度的 Pod 不应计入节点: %+v", snap.Nodes[0])
	}
}

func TestSnapshotSkipsTerminalPods(t *testing.T) {
	succeeded := testPod("prod", "done", "n1", corev1.PodSucceeded, testContainer("app", "1", "1Gi"))
	failed := testPod("prod", "bad", "n1", corev1.PodFailed, testContainer("app", "1", "1Gi"))
	running := testPod("prod", "live", "n1", corev1.PodRunning, testContainer("app", "1", "1Gi"))

	v := newTestView(t, testNode("n1"), succeeded, failed, running)
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Nodes[0].PodCount != 1 || snap.Nodes[0].Requested.CPUMilli != 1000 {
		t.Fatalf("终态 Pod 应被忽略: %+v", snap.Nodes[0])
	}
}

func TestSnapshotIgnoresPodOfUnknownNode(t *testing.T) {
	orphan := testPod("prod", "orphan", "n-missing", corev1.PodRunning, testContainer("app", "1", "1Gi"))
	v := newTestView(t, testNode("n1"), orphan)
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Nodes[0].PodCount != 0 {
		t.Fatalf("节点不存在时不应计入: %+v", snap.Nodes[0])
	}
}

func TestPodRequestsPerDimensionMax(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				testContainer("a", "500m", "2Gi"),
				testContainer("b", "500m", "1Gi"),
			},
			InitContainers: []corev1.Container{
				testContainer("init", "2", "1Gi"),
			},
		},
	}
	got := podRequests(pod)
	// 业务容器内存合计 3Gi，init 为 1Gi，逐维取最大值后仍为 3Gi。
	want := schedcore.Resources{CPUMilli: 2000, MemBytes: 3 * 1024 * 1024 * 1024}
	if got != want {
		t.Fatalf("podRequests = %+v, want %+v", got, want)
	}
}

func TestSnapshotAggregatesTenantUsage(t *testing.T) {
	named := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{TenantLabelKey: "t-a"}},
	}
	plain := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-plain"}}

	v := newTestView(t,
		testNode("ka-w1"),
		named,
		plain,
		testPod("team-a", "api-1", "ka-w1", corev1.PodRunning, testContainer("c", "500m", "1Gi")),
		testPod("team-a", "api-2", "ka-w1", corev1.PodRunning, testContainer("c", "250m", "512Mi")),
		// 未归属租户的命名空间不计入任何租户。
		testPod("team-plain", "job-1", "ka-w1", corev1.PodRunning, testContainer("c", "1", "2Gi")),
	)

	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("快照失败: %v", err)
	}
	got, ok := snap.TenantUsage["t-a"]
	if !ok {
		t.Fatalf("应聚合出 t-a 的用量，实际 %+v", snap.TenantUsage)
	}
	if got.CPUMilli != 750 || got.MemBytes != 1536<<20 {
		t.Fatalf("t-a 用量应为 750m/1.5Gi，实际 %d/%d", got.CPUMilli, got.MemBytes)
	}
	if len(snap.TenantUsage) != 1 {
		t.Fatalf("只有打了租户标签的命名空间才计入，实际 %+v", snap.TenantUsage)
	}
	if len(snap.Nodes) != 1 {
		t.Fatalf("应有 1 个节点，实际 %d", len(snap.Nodes))
	}
	nodeUsage := snap.Nodes[0].TenantUsage["t-a"]
	if nodeUsage.CPUMilli != 750 || nodeUsage.MemBytes != 1536<<20 {
		t.Fatalf("节点级 t-a 用量应为 750m/1.5Gi，实际 %d/%d", nodeUsage.CPUMilli, nodeUsage.MemBytes)
	}
}

func TestNamespaceTenant(t *testing.T) {
	named := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: map[string]string{TenantLabelKey: "t-a"}},
	}
	empty := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-empty", Labels: map[string]string{TenantLabelKey: ""}},
	}
	v := newTestView(t, named, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-plain"}}, empty)

	if got, ok := v.NamespaceTenant("team-a"); !ok || got != "t-a" {
		t.Fatalf("team-a 应归属 t-a，实际 %q/%v", got, ok)
	}
	for _, ns := range []string{"team-plain", "team-empty", "missing"} {
		if got, ok := v.NamespaceTenant(ns); ok {
			t.Fatalf("%s 不应有租户归属，实际 %q", ns, got)
		}
	}
}

func TestSnapshotOrdersDeterministically(t *testing.T) {
	v := newTestView(t,
		testNode("n2"), testNode("n1"),
		testPod("b", "p1", "", corev1.PodPending, testContainer("c", "1", "1Gi")),
		testPod("a", "p2", "", corev1.PodPending, testContainer("c", "1", "1Gi")),
	)
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Nodes[0].Name != "n1" || snap.Nodes[1].Name != "n2" {
		t.Fatalf("节点未按名字排序: %+v", snap.Nodes)
	}
	if snap.PendingPods[0].Namespace != "a" {
		t.Fatalf("PendingPods 未按 namespace/name 排序: %+v", snap.PendingPods)
	}
}

// fakeMetrics 是 MetricsSource 的测试替身，只为验证快照的接线逻辑。
type fakeMetrics struct {
	usage  map[string]schedcore.Resources
	newest time.Time
}

func (f fakeMetrics) NodeUsage(node string) (schedcore.Resources, time.Time, bool) {
	u, ok := f.usage[node]
	return u, f.newest, ok
}

func (f fakeMetrics) Newest() time.Time { return f.newest }

func TestSnapshotAttachesNodeUsage(t *testing.T) {
	metricsAt := time.Date(2026, 11, 23, 10, 0, 5, 0, time.UTC)
	v := newTestView(t, testNode("n1"), testNode("n2"))
	v.UseMetrics(fakeMetrics{
		usage: map[string]schedcore.Resources{
			"n1": {CPUMilli: 1500, MemBytes: 6 << 30},
		},
		newest: metricsAt,
	})

	snap, err := v.Snapshot(metricsAt.Add(5 * time.Second))
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.MetricsAt.Equal(metricsAt) {
		t.Fatalf("MetricsAt = %v, want %v", snap.MetricsAt, metricsAt)
	}
	if !snap.Nodes[0].UsageKnown || snap.Nodes[0].Usage.CPUMilli != 1500 {
		t.Fatalf("n1 应带上用量，实际 %+v", snap.Nodes[0])
	}
	// 缺指标的节点仍保留在快照里，只标记为未知，不影响未注入指标时的行为。
	if snap.Nodes[1].UsageKnown {
		t.Fatalf("n2 无指标，UsageKnown 应为 false: %+v", snap.Nodes[1])
	}
}

func TestSnapshotWithoutMetricsSource(t *testing.T) {
	v := newTestView(t, testNode("n1"))
	snap, err := v.Snapshot(time.Now())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !snap.MetricsAt.IsZero() || snap.Nodes[0].UsageKnown {
		t.Fatalf("未注入指标来源时不应携带 usage: %+v", snap)
	}
}
