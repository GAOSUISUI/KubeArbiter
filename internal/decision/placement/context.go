// Package placement 把只读集群快照转换为决策核输入，并产出可落库的放置决策。
// 它只做"取数 + 调用 schedcore"，不含任何调度评分逻辑本身。
package placement

import (
	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// BuildContext 把 informer 快照转换为 schedcore.ClusterContext。
// 指标新鲜度由快照里的 MetricsAt 决定：零值表示完全取不到指标，此时
// MetricsKnown=false，决策核会降权 load 维度并给出 METRICS_MISSING 警告，
// 而不是臆造数值。单节点缺指标只影响该节点的 UsageKnown。
func BuildContext(snap capacity.Snapshot, tenants map[string]schedcore.TenantState) schedcore.ClusterContext {
	nodes := make([]schedcore.Node, 0, len(snap.Nodes))
	topologyCounts := make(map[string]int)
	var capacitySum schedcore.Resources

	for _, n := range snap.Nodes {
		nodes = append(nodes, schedcore.Node{
			Name:          n.Name,
			Ready:         n.Ready,
			Unschedulable: n.Unschedulable,
			Allocatable:   n.Allocatable,
			Requested:     n.Requested,
			Usage:         n.Usage,
			UsageKnown:    n.UsageKnown,
			Labels:        n.Labels,
			Taints:        n.Taints,
			HostPorts:     n.HostPorts,
			Topology:      n.Topology,
		})

		// 只有可参与调度的节点才计入可仲裁容量。
		if n.Ready && !n.Unschedulable {
			capacitySum = capacitySum.Add(n.Allocatable)
		}
		// 拓扑域计数按节点粒度近似：同一域内的 Pod 数由该域下各节点的 PodCount 累加。
		for k, v := range n.Topology {
			topologyCounts[k+"="+v] += n.PodCount
		}
	}

	if tenants == nil {
		tenants = map[string]schedcore.TenantState{}
	}
	// 快照里出现但调用方没提供的租户（例如别的租户正在占用资源）按权重 1 补入，
	// 否则权重总和偏小会把目标租户的公平份额算高。
	states := make(map[string]schedcore.TenantState, len(tenants)+len(snap.TenantUsage))
	for id, ts := range tenants {
		states[id] = ts
	}
	for id := range snap.TenantUsage {
		if _, ok := states[id]; !ok {
			states[id] = schedcore.TenantState{ID: id, Weight: 1}
		}
	}

	// 已用量来自快照聚合；权重份额按 Σweight 归一化，得到本轮公平份额。
	var totalWeight float64
	for _, ts := range states {
		totalWeight += ts.Weight
	}
	for id, ts := range states {
		if ts.Allocated.IsZero() {
			ts.Allocated = snap.TenantUsage[id]
		}
		if ts.FairShare <= 0 && totalWeight > 0 {
			ts.FairShare = ts.Weight / totalWeight
		}
		states[id] = ts
	}

	var metricsAge float64
	if !snap.MetricsAt.IsZero() {
		metricsAge = snap.TakenAt.Sub(snap.MetricsAt).Seconds()
		// 时钟回拨或手动构造的快照可能让 age 为负；按"刚采集"处理，避免误报过期。
		if metricsAge < 0 {
			metricsAge = 0
		}
	}

	return schedcore.ClusterContext{
		Nodes:             nodes,
		Capacity:          capacitySum,
		MinPendingPod:     minPendingRequest(snap.PendingPods),
		Tenants:           states,
		TopologyCounts:    topologyCounts,
		MetricsKnown:      !snap.MetricsAt.IsZero(),
		MetricsAgeSeconds: metricsAge,
	}
}

// minPendingRequest 返回待调度 Pod 中最小的单副本 request，用于判定"不可用碎片"。
// 没有待调度 Pod 时返回零值，语义为"不存在最小需求约束"。
func minPendingRequest(pods []capacity.PodView) schedcore.Resources {
	var min schedcore.Resources
	first := true
	for _, p := range pods {
		if p.Requests.IsZero() {
			continue
		}
		if first {
			min = p.Requests
			first = false
			continue
		}
		if p.Requests.CPUMilli < min.CPUMilli {
			min.CPUMilli = p.Requests.CPUMilli
		}
		if p.Requests.MemBytes < min.MemBytes {
			min.MemBytes = p.Requests.MemBytes
		}
	}
	return min
}
