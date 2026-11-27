package schedcore

import (
	"errors"
	"fmt"
)

// ErrInvalidGang 表示 gang 参数非法（副本数非正或 min_available 超过副本数）。
var ErrInvalidGang = errors.New("gang 参数非法")

// PlaceGang 执行成组（gang）放置：同一 Workload 的 N 个副本要求至少
// minAvailable 个同时可调度（all-or-nothing 的放宽形式）。
//
// 算法：复用硬过滤与七维评分确定候选与优先级，再按总分降序贪心装箱——
// 每个节点尽可能多放副本（逐维 floor(余量/单副本 request)，节点内可放多份），
// 直到放满 replicas 或候选耗尽。总量 ≥ minAvailable 即成功，输出每节点副本数；
// 否则整体失败（Decision.Status=NO_CANDIDATE），Evidence 记录可行总量，
// 绝不输出"放一半"的部分方案。
//
// GPU 碎片是 AI 任务的典型瓶颈：单节点各剩 1 卡放不下 2 卡副本，而 gang
// 会把它们如实计入 total_feasible 后判失败，正是全有全无语义要暴露的问题。
func PlaceGang(ctx ClusterContext, wl Workload, w Weights, minAvailable int) (Decision, error) {
	if wl.Replicas <= 0 {
		return Decision{}, fmt.Errorf("%w: 副本数必须 > 0", ErrInvalidGang)
	}
	if minAvailable <= 0 {
		minAvailable = wl.Replicas
	}
	if minAvailable > wl.Replicas {
		return Decision{}, fmt.Errorf("%w: min_available(%d) 不能超过副本数(%d)", ErrInvalidGang, minAvailable, wl.Replicas)
	}

	p, err := preparePlacement(ctx, wl, w)
	if err != nil {
		return Decision{}, err
	}

	decision := Decision{
		Scores:   p.scores,
		Filtered: p.filtered,
		Degraded: p.degraded,
		Warnings: p.warnings,
	}
	if len(ctx.Nodes) == 0 {
		decision.Status = StatusNoCandidate
		decision.Filtered = append(decision.Filtered, FilteredNode{
			Node:   "",
			Reason: ReasonNoNodeInSnapshot,
		})
		return decision, nil
	}
	if len(p.scores) == 0 {
		decision.Status = StatusNoCandidate
		return decision, nil
	}

	// 贪心装箱：候选已按总分降序排列，逐节点取 floor(余量/单副本) 的最小维度上限。
	req := wl.Requests
	assigned := make(map[string]int, len(p.scores))
	total := 0
	remain := wl.Replicas
	for _, s := range p.scores {
		if remain <= 0 {
			break
		}
		var n *Node
		for i := range ctx.Nodes {
			if ctx.Nodes[i].Name == s.Node {
				n = &ctx.Nodes[i]
				break
			}
		}
		if n == nil {
			continue
		}
		remaining := n.Allocatable.Sub(n.Requested)
		cap := remain // 单副本 request 全零时可放任意多，上限就是剩余副本数
		if req.CPUMilli > 0 {
			cap = minInt(cap, int(remaining.CPUMilli/req.CPUMilli))
		}
		if req.MemBytes > 0 {
			cap = minInt(cap, int(remaining.MemBytes/req.MemBytes))
		}
		if req.GPU > 0 {
			cap = minInt(cap, int(remaining.GPU/req.GPU))
		}
		if cap <= 0 {
			continue
		}
		assigned[n.Name] = cap
		total += cap
		remain -= cap
	}

	if total < minAvailable {
		decision.Status = StatusNoCandidate
		decision.Evidence = map[string]any{
			"total_feasible": total,
			"min_available":  minAvailable,
		}
		return decision, nil
	}

	decision.Status = StatusOK
	decision.Assignments = make([]NodeAssignment, 0, len(assigned))
	for _, s := range p.scores {
		if count, ok := assigned[s.Node]; ok && count > 0 {
			decision.Assignments = append(decision.Assignments, NodeAssignment{Node: s.Node, Replicas: count})
		}
	}
	decision.ChosenNode = majorityNode(decision.Assignments)
	return decision, nil
}

// majorityNode 返回副本数最多的节点；并列时取名字最小者，保证确定性。
func majorityNode(assignments []NodeAssignment) string {
	best := ""
	bestCount := -1
	for _, a := range assignments {
		if a.Replicas > bestCount || (a.Replicas == bestCount && a.Node < best) {
			best = a.Node
			bestCount = a.Replicas
		}
	}
	return best
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
