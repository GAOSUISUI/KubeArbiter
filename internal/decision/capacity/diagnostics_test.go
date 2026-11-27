package capacity

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestDiagnosticsNodeAndPodState(t *testing.T) {
	node := testNode("n1")
	node.Spec.Unschedulable = false
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse},
	}

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "prod",
			Labels: map[string]string{TenantLabelKey: "team-a"},
		},
	}
	pod := testPod("prod", "web", "n1", corev1.PodRunning, testContainer("app", "500m", "256Mi"))
	start := time.Now().Add(-10 * time.Minute)
	pod.Status.StartTime = &metav1.Time{Time: start}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "app",
		RestartCount: 5,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
		},
	}}
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(start)},
	}

	v := newTestView(t, node, ns, pod)
	diag, err := v.Diagnostics(time.Now())
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diag.Nodes) != 1 {
		t.Fatalf("节点数 = %d, want 1", len(diag.Nodes))
	}
	n := diag.Nodes[0]
	if n.Ready || !n.MemoryPressure || n.DiskPressure {
		t.Fatalf("节点条件解析错误: %+v", n)
	}
	if n.PodCount != 1 || n.Requested.CPUMilli != 500 {
		t.Fatalf("节点请求量聚合错误: %+v", n)
	}
	if got := diag.TenantUsage["team-a"]; got.CPUMilli != 500 {
		t.Fatalf("租户用量错误: %+v", got)
	}
	if len(diag.Pods) != 1 {
		t.Fatalf("Pod 数 = %d, want 1", len(diag.Pods))
	}
	p := diag.Pods[0]
	if p.Restarts != 5 || p.WaitingReason != "CrashLoopBackOff" || p.LastTerminatedReason != "OOMKilled" {
		t.Fatalf("容器状态解析错误: %+v", p)
	}
	if p.Ready || p.StartTime.IsZero() || p.ReadySince.IsZero() {
		t.Fatalf("就绪/启动时间解析错误: %+v", p)
	}
	if p.Requests.CPUMilli != 500 || p.Requests.MemBytes != 256*1024*1024 {
		t.Fatalf("Request 汇总错误: %+v", p.Requests)
	}
}

func TestDiagnosticsSkipsSucceededPods(t *testing.T) {
	pod := testPod("prod", "job", "n1", corev1.PodSucceeded, testContainer("app", "100m", "64Mi"))
	v := newTestView(t, testNode("n1"), pod)
	diag, err := v.Diagnostics(time.Now())
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(diag.Pods) != 0 || diag.Nodes[0].PodCount != 0 {
		t.Fatalf("已完成的 Pod 不应参与诊断: %+v", diag)
	}
}
