package capacity

import (
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// Diagnostics 是面向故障检测与健康分的只读观察结果。
// 它与 Snapshot 分开：Snapshot 参与决策哈希，字段必须保持稳定；
// 这里可以携带 conditions、容器重启与终止原因等运维字段。
type Diagnostics struct {
	TakenAt     time.Time
	Nodes       []NodeDiag
	Pods        []PodDiag
	MetricsAt   time.Time
	TenantUsage map[string]schedcore.Resources
}

// NodeDiag 是单个节点的运维画像。
type NodeDiag struct {
	Name           string
	Ready          bool
	Unschedulable  bool
	MemoryPressure bool
	DiskPressure   bool
	PIDPressure    bool
	Allocatable    schedcore.Resources
	Requested      schedcore.Resources
	Usage          schedcore.Resources
	UsageKnown     bool
	PodCount       int
}

// PodDiag 是单个 Pod 的运维画像，字段只保留检测规则需要的部分。
type PodDiag struct {
	Namespace            string
	Name                 string
	NodeName             string
	Phase                corev1.PodPhase
	CreatedAt            time.Time
	StartTime            time.Time
	Restarts             int32
	WaitingReason        string
	LastTerminatedReason string
	LastTerminatedAt     time.Time
	Ready                bool
	ReadySince           time.Time
}

// Diagnostics 从 informer 缓存构造观察结果。单次遍历 nodes 与 pods，全部为副本。
func (v *View) Diagnostics(now time.Time) (Diagnostics, error) {
	nodes, err := v.nodes.Lister().List(labels.Everything())
	if err != nil {
		return Diagnostics{}, fmt.Errorf("列出节点: %w", err)
	}
	pods, err := v.pods.Lister().List(labels.Everything())
	if err != nil {
		return Diagnostics{}, fmt.Errorf("列出 Pod: %w", err)
	}
	namespaces, err := v.namespaces.Lister().List(labels.Everything())
	if err != nil {
		return Diagnostics{}, fmt.Errorf("列出命名空间: %w", err)
	}

	tenantByNS := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		if tenant := ns.Labels[TenantLabelKey]; tenant != "" {
			tenantByNS[ns.Name] = tenant
		}
	}

	byName := make(map[string]*NodeDiag, len(nodes))
	out := Diagnostics{
		TakenAt:     now,
		Nodes:       make([]NodeDiag, 0, len(nodes)),
		Pods:        make([]PodDiag, 0, len(pods)),
		TenantUsage: make(map[string]schedcore.Resources),
	}
	for _, node := range nodes {
		nd := &NodeDiag{
			Name:           node.Name,
			Ready:          nodeReady(node),
			Unschedulable:  node.Spec.Unschedulable,
			MemoryPressure: nodeConditionTrue(node, corev1.NodeMemoryPressure),
			DiskPressure:   nodeConditionTrue(node, corev1.NodeDiskPressure),
			PIDPressure:    nodeConditionTrue(node, corev1.NodePIDPressure),
			Allocatable:    resources(node.Status.Allocatable),
		}
		if v.metrics != nil {
			if usage, _, ok := v.metrics.NodeUsage(node.Name); ok {
				nd.Usage = usage
				nd.UsageKnown = true
			}
		}
		byName[node.Name] = nd
	}

	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		pd := podDiagnostics(pod)
		out.Pods = append(out.Pods, pd)

		// 只有已调度且未完成的 Pod 计入节点请求量与租户用量。
		if isTerminal(pod) || pod.Spec.NodeName == "" {
			continue
		}
		nd, ok := byName[pod.Spec.NodeName]
		if !ok {
			continue
		}
		req := podRequests(pod)
		nd.Requested = nd.Requested.Add(req)
		nd.PodCount++
		if tenant := tenantByNS[pod.Namespace]; tenant != "" {
			out.TenantUsage[tenant] = out.TenantUsage[tenant].Add(req)
		}
	}

	for _, nd := range byName {
		out.Nodes = append(out.Nodes, *nd)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	sort.Slice(out.Pods, func(i, j int) bool {
		if out.Pods[i].Namespace != out.Pods[j].Namespace {
			return out.Pods[i].Namespace < out.Pods[j].Namespace
		}
		return out.Pods[i].Name < out.Pods[j].Name
	})
	if v.metrics != nil {
		out.MetricsAt = v.metrics.Newest()
	}
	return out, nil
}

func podDiagnostics(pod *corev1.Pod) PodDiag {
	pd := PodDiag{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		NodeName:  pod.Spec.NodeName,
		Phase:     pod.Status.Phase,
		CreatedAt: pod.CreationTimestamp.Time,
	}
	if pod.Status.StartTime != nil {
		pd.StartTime = pod.Status.StartTime.Time
	}
	for _, status := range append(pod.Status.ContainerStatuses, pod.Status.InitContainerStatuses...) {
		pd.Restarts += status.RestartCount
		if pd.WaitingReason == "" && status.State.Waiting != nil {
			pd.WaitingReason = status.State.Waiting.Reason
		}
		if pd.LastTerminatedReason == "" && status.LastTerminationState.Terminated != nil {
			pd.LastTerminatedReason = status.LastTerminationState.Terminated.Reason
			if !status.LastTerminationState.Terminated.FinishedAt.IsZero() {
				pd.LastTerminatedAt = status.LastTerminationState.Terminated.FinishedAt.Time
			}
		}
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			pd.Ready = cond.Status == corev1.ConditionTrue
			if !cond.LastTransitionTime.IsZero() {
				pd.ReadySince = cond.LastTransitionTime.Time
			}
			break
		}
	}
	return pd
}

func nodeConditionTrue(node *corev1.Node, typ corev1.NodeConditionType) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == typ {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}
