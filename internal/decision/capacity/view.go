// Package capacity 维护集群资源的只读视图。
// 它把 kubelet 上报的 Node/Pod 对象转成决策核可用的整数资源量，
// 并提供一个可被并发读取的一致性快照。节点实际用量由 MetricsSource 注入
// （见 internal/decision/metrics），未注入时快照里的 UsageKnown 全部为 false。
package capacity

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// MetricsSource 提供按节点聚合后的实际用量。由 internal/decision/metrics.Store 实现。
// capacity 只定义消费侧接口，避免反向依赖采集包。
type MetricsSource interface {
	// NodeUsage 返回节点当前 EWMA 用量与最后样本时间；ok=false 表示该节点暂无指标。
	NodeUsage(node string) (schedcore.Resources, time.Time, bool)
	// Newest 返回全部节点中最新样本的时间；零值表示系统还没有任何指标。
	Newest() time.Time
}

// View 是 Node/Pod/Namespace 三类 informer 的封装。所有读取都走本地缓存，不直接打 API Server。
type View struct {
	factory    informers.SharedInformerFactory
	nodes      coreinformers.NodeInformer
	pods       coreinformers.PodInformer
	namespaces coreinformers.NamespaceInformer
	metrics    MetricsSource
}

// UseMetrics 注入指标来源。应在 Run 之前调用；未注入时快照不携带 usage。
func (v *View) UseMetrics(src MetricsSource) {
	v.metrics = src
}

// NodeView 是单个节点的画像。Requested 为该节点上所有非终态 Pod 的 request 之和。
type NodeView struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Allocatable   schedcore.Resources
	Requested     schedcore.Resources
	// Usage 是节点实际用量（已做 5m EWMA）；UsageKnown 为 false 时该字段无意义。
	Usage      schedcore.Resources
	UsageKnown bool
	PodCount   int
	Labels     map[string]string
	Taints     []schedcore.Taint
	HostPorts  map[int32]string
	Topology   map[string]string
}

// PodView 只保留调度决策需要的字段，避免把完整 Pod 对象长期留在内存里。
type PodView struct {
	Namespace string
	Name      string
	NodeName  string
	Phase     corev1.PodPhase
	Requests  schedcore.Resources
	HostPorts []int32
	Labels    map[string]string
}

// Snapshot 是某一时刻的只读视图。Nodes 按名字升序、PendingPods 按 namespace/name 升序，
// 便于测试与决策结果复现。
type Snapshot struct {
	TakenAt     time.Time
	Nodes       []NodeView
	PendingPods []PodView
	// MetricsAt 是指标存储中最新样本的时间；零值表示完全没有指标。
	MetricsAt time.Time
	// TenantUsage 是按命名空间标签 arbiter.io/tenant 聚合出的已调度资源用量（含目标租户之外的所有租户）。
	TenantUsage map[string]schedcore.Resources
}

func NewView(client kubernetes.Interface, resync time.Duration) *View {
	factory := informers.NewSharedInformerFactory(client, resync)
	nodes := factory.Core().V1().Nodes()
	pods := factory.Core().V1().Pods()
	namespaces := factory.Core().V1().Namespaces()
	// 生成式 informer 是惰性的：不先调用一次 Informer() 注册，factory.Start 不会启动它，
	// WaitForCacheSync 也会直接返回空结果。
	_ = nodes.Informer()
	_ = pods.Informer()
	_ = namespaces.Informer()
	return &View{
		factory:    factory,
		nodes:      nodes,
		pods:       pods,
		namespaces: namespaces,
	}
}

// Run 启动 informer 并等待首次同步完成。ctx 取消后 informer 停止。
func (v *View) Run(ctx context.Context) error {
	v.factory.Start(ctx.Done())
	for typ, ok := range v.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("informer %v 未完成同步", typ)
		}
	}
	return nil
}

func (v *View) HasSynced() bool {
	return v.nodes.Informer().HasSynced() && v.pods.Informer().HasSynced() && v.namespaces.Informer().HasSynced()
}

// TenantLabelKey 把命名空间归属到租户；未打标签的命名空间不参与租户用量统计。
const TenantLabelKey = "arbiter.io/tenant"

// NamespaceTenant 从 informer 缓存读取命名空间的租户归属，供 admission webhook 使用。
func (v *View) NamespaceTenant(namespace string) (string, bool) {
	ns, err := v.namespaces.Lister().Get(namespace)
	if err != nil {
		return "", false
	}
	tenant := ns.Labels[TenantLabelKey]
	return tenant, tenant != ""
}

// Snapshot 从本地缓存构造一致性快照。所有字段均为副本，调用方修改不会影响缓存。
func (v *View) Snapshot(now time.Time) (Snapshot, error) {
	nodes, err := v.nodes.Lister().List(labels.Everything())
	if err != nil {
		return Snapshot{}, fmt.Errorf("列出节点: %w", err)
	}
	pods, err := v.pods.Lister().List(labels.Everything())
	if err != nil {
		return Snapshot{}, fmt.Errorf("列出 Pod: %w", err)
	}
	namespaces, err := v.namespaces.Lister().List(labels.Everything())
	if err != nil {
		return Snapshot{}, fmt.Errorf("列出命名空间: %w", err)
	}

	tenantByNS := make(map[string]string, len(namespaces))
	for _, ns := range namespaces {
		if tenant := ns.Labels[TenantLabelKey]; tenant != "" {
			tenantByNS[ns.Name] = tenant
		}
	}

	byName := make(map[string]*NodeView, len(nodes))
	out := Snapshot{
		TakenAt:     now,
		Nodes:       make([]NodeView, 0, len(nodes)),
		PendingPods: make([]PodView, 0),
		TenantUsage: make(map[string]schedcore.Resources),
	}

	for _, node := range nodes {
		nv := &NodeView{
			Name:          node.Name,
			Ready:         nodeReady(node),
			Unschedulable: node.Spec.Unschedulable,
			Allocatable:   resources(node.Status.Allocatable),
			Labels:        copyMap(node.Labels),
			Taints:        convertTaints(node.Spec.Taints),
			HostPorts:     make(map[int32]string),
			Topology:      nodeTopology(node),
		}
		// 指标可能缺某个节点（如 kubelet 暂不可达）：只标记该节点未知，整个快照仍可用。
		if v.metrics != nil {
			if usage, _, ok := v.metrics.NodeUsage(node.Name); ok {
				nv.Usage = usage
				nv.UsageKnown = true
			}
		}
		byName[node.Name] = nv
	}

	for _, pod := range pods {
		if isTerminal(pod) {
			continue
		}
		pv := PodView{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			NodeName:  pod.Spec.NodeName,
			Phase:     pod.Status.Phase,
			Requests:  podRequests(pod),
			HostPorts: podHostPorts(pod),
			Labels:    copyMap(pod.Labels),
		}
		if pod.Spec.NodeName == "" {
			if pod.Status.Phase == corev1.PodPending {
				out.PendingPods = append(out.PendingPods, pv)
			}
			continue
		}
		nv, ok := byName[pod.Spec.NodeName]
		if !ok {
			// 节点对象可能已被删除而 Pod 缓存尚未收敛；不把这类 Pod 计入任何节点。
			continue
		}
		nv.Requested = nv.Requested.Add(pv.Requests)
		nv.PodCount++
		// 已调度 Pod 按所属命名空间的租户标签累计用量；未归属租户的资源不计入公平性。
		if tenant := tenantByNS[pod.Namespace]; tenant != "" {
			out.TenantUsage[tenant] = out.TenantUsage[tenant].Add(pv.Requests)
		}
		for _, port := range pv.HostPorts {
			if _, taken := nv.HostPorts[port]; !taken {
				nv.HostPorts[port] = pod.Namespace + "/" + pod.Name
			}
		}
	}

	for _, nv := range byName {
		out.Nodes = append(out.Nodes, *nv)
	}

	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].Name < out.Nodes[j].Name })
	sort.Slice(out.PendingPods, func(i, j int) bool {
		if out.PendingPods[i].Namespace != out.PendingPods[j].Namespace {
			return out.PendingPods[i].Namespace < out.PendingPods[j].Namespace
		}
		return out.PendingPods[i].Name < out.PendingPods[j].Name
	})
	if v.metrics != nil {
		out.MetricsAt = v.metrics.Newest()
	}
	return out, nil
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func resources(list corev1.ResourceList) schedcore.Resources {
	var r schedcore.Resources
	if q, ok := list[corev1.ResourceCPU]; ok {
		r.CPUMilli = q.MilliValue()
	}
	if q, ok := list[corev1.ResourceMemory]; ok {
		r.MemBytes = q.Value()
	}
	return r
}

// podRequests 按经典语义计算 Pod 的有效 request：逐维取 max(Σ 业务容器, max(init 容器))。
// 未考虑 sidecar 型 init 容器（restartPolicy: Always），接入真实集群后用实测数据校准。
func podRequests(pod *corev1.Pod) schedcore.Resources {
	var app schedcore.Resources
	for _, c := range pod.Spec.Containers {
		app = app.Add(resources(c.Resources.Requests))
	}
	var initMax schedcore.Resources
	for _, c := range pod.Spec.InitContainers {
		r := resources(c.Resources.Requests)
		if r.CPUMilli > initMax.CPUMilli {
			initMax.CPUMilli = r.CPUMilli
		}
		if r.MemBytes > initMax.MemBytes {
			initMax.MemBytes = r.MemBytes
		}
	}
	return schedcore.Resources{
		CPUMilli: maxInt64(app.CPUMilli, initMax.CPUMilli),
		MemBytes: maxInt64(app.MemBytes, initMax.MemBytes),
	}
}

func podHostPorts(pod *corev1.Pod) []int32 {
	var ports []int32
	collect := func(cs []corev1.Container) {
		for _, c := range cs {
			for _, p := range c.Ports {
				if p.HostPort > 0 {
					ports = append(ports, p.HostPort)
				}
			}
		}
	}
	collect(pod.Spec.Containers)
	collect(pod.Spec.InitContainers)
	return ports
}

func isTerminal(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed
}

func convertTaints(taints []corev1.Taint) []schedcore.Taint {
	if len(taints) == 0 {
		return nil
	}
	out := make([]schedcore.Taint, 0, len(taints))
	for _, t := range taints {
		out = append(out, schedcore.Taint{Key: t.Key, Value: t.Value, Effect: string(t.Effect)})
	}
	return out
}

func nodeTopology(node *corev1.Node) map[string]string {
	keys := []string{corev1.LabelHostname, corev1.LabelTopologyZone, corev1.LabelTopologyRegion}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := node.Labels[k]; ok {
			out[k] = v
		}
	}
	return out
}

func copyMap(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
