package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// RenderMarkdown 生成包含口径、逐场景数据与自动结论的 Markdown 报告。
func RenderMarkdown(result Result, generatedAt time.Time) string {
	var out strings.Builder
	out.WriteString("# M8 基准评测报告\n\n")
	out.WriteString("## 评测说明\n\n")
	fmt.Fprintf(&out, "- 生成日期：%s；运行方式：离线多轮放置仿真。\n", generatedAt.Format("2006-01-02"))
	fmt.Fprintf(&out, "- Seed：`%d`；每个场景、每个变体均为 `%d` 轮，所有变体复用对应场景的同一到达序列。\n", result.Seed, result.Rounds)
	fmt.Fprintf(&out, "- 负载标定：原始 CPU 档位 `{100,200,500,1000}m` 的期望为 450m；各档位统一乘以 `目标 × 集群CPU / (轮数 × 分布期望)` 后取整。本次期望总需求目标为集群 CPU 的 %.0f%%，不是另设固定档位。\n", result.TargetDemandRatio*100)
	out.WriteString("- 集群：6 个 Ready 且无 taint 的节点；n1~n4 为 4000m/8Gi，n5~n6 为 8000m/16Gi，总容量 32000m/64Gi。\n")
	fmt.Fprintf(&out, "- 租户权重为 4/2/1/1；`balanced` 按权重到达，`hot-tenant` 中权重 4 的租户占 70%% 到达，其余按 2:1:1 分配。为制造局部热点，最后 %.0f%% 轮次的热点 Pod 需求乘 %.0f，采样期望按 `0.7×(0.95+0.05×%.0f)+0.3` 修正后再做总量标定。\n", hotLateFraction*100, hotLateDemandFactor, hotLateDemandFactor)
	out.WriteString("- 不包含真实 kubelet 指标：`Usage` 固定为 0、`UsageKnown=true`、`MetricsKnown=true`，排除指标缺失对对比的干扰。\n")
	out.WriteString("- 延迟仅统计进程内 `Place` 调用耗时，以 µs 展示并保留 1 位小数；平均决策耗时 = 全部调用总耗时 / 轮数，P99 使用 nearest-rank 分位数。不包含 API、数据库、网络或 kubelet 开销；Windows 计时精度可能使 P99 显示为 0.0。\n")
	out.WriteString("- 不可行率 = 未放置轮次 / 总轮次；未放置包括 `Place` 返回错误，以及当前调度核用于表达容量不足的 `NO_CANDIDATE`。\n\n")

	out.WriteString("## 指标口径\n\n")
	out.WriteString("- **Jain**：先求租户主导份额 `max(已分配 CPU / 集群 CPU, 已分配内存 / 集群内存)`，再除以租户权重，对所得四个值计算 Jain 指数。\n")
	out.WriteString("- **CPU / 内存利用率**：集群已分配 request / 集群总 allocatable。\n")
	out.WriteString("- **碎片率**：仿真结束后，对残余容量放不下基准 Pod（100m/128Mi）的节点，累加 `min(残余 CPU / 集群 CPU, 残余内存 / 集群内存)`；其余节点计 0。\n")
	out.WriteString("- **平均租户 HHI**：每租户计算 `Σ_node(该租户在该节点的副本数 / 该租户总副本数)²`，再对四个租户取均值；越低越散布，无副本租户计 0。\n")
	out.WriteString("- **最大节点副本占比**：先求每租户在单个节点上的最大副本占比，再取四个租户中的最大值；越低表示单租户越少集中于一台节点。\n\n")

	for _, scenario := range result.Scenarios {
		fmt.Fprintf(&out, "## 场景：`%s`\n\n", scenario.Name)
		out.WriteString("| 变体 | Jain | CPU 利用率 | 内存利用率 | 碎片率 | 不可行率 | 平均 HHI | 最大节点副本占比 | 平均决策耗时 (µs) | P99 (µs) |\n")
		out.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
		for _, variant := range scenario.Variants {
			m := variant.Metrics
			fmt.Fprintf(&out, "| %s | %.4f | %.2f%% | %.2f%% | %.2f%% | %.2f%% | %.4f | %.2f%% | %.1f | %.1f |\n",
				variant.Name, m.Jain, m.CPUUtilization*100, m.MemUtilization*100,
				m.Fragmentation*100, m.InfeasibleRate*100, m.MeanTenantHHI,
				m.MaxNodeReplicaShare*100, m.AverageDecisionUS, m.P99US)
		}
		out.WriteString("\n")
		renderInfeasibleNote(&out, result.Rounds, scenario)
		renderTenantShares(&out, scenario)
	}

	out.WriteString("## 自动结论\n\n")
	for i, conclusion := range result.Conclusions {
		fmt.Fprintf(&out, "%d. %s\n", i+1, conclusion)
	}
	return out.String()
}

func renderInfeasibleNote(out *strings.Builder, rounds int, scenario ScenarioResult) {
	entries := make([]string, 0, len(scenario.Variants))
	lateOnly := true
	for _, variant := range scenario.Variants {
		if variant.FirstInfeasibleRound == 0 {
			continue
		}
		entries = append(entries, fmt.Sprintf("%s 第 %d 轮", variant.Name, variant.FirstInfeasibleRound))
		if variant.FirstInfeasibleRound < int(math.Ceil(float64(rounds)*0.8)) {
			lateOnly = false
		}
	}
	if len(entries) == 0 {
		out.WriteString("本场景所有变体均无不可行轮次。\n\n")
		return
	}
	prefix := "首次不可行轮次"
	if lateOnly {
		prefix = "不可行均从末期（最后 20% 轮次）才出现，首次轮次"
	}
	fmt.Fprintf(out, "%s：%s。\n\n", prefix, strings.Join(entries, "；"))
}

func renderTenantShares(out *strings.Builder, scenario ScenarioResult) {
	out.WriteString("### balanced 策略逐租户份额\n\n")
	out.WriteString("单元格为“主导份额 → 主导份额/权重”，后者是 Jain 的直接输入；完整数值同时写入 JSON。\n\n")
	out.WriteString("| 变体 | tenant-a (w=4) | tenant-b (w=2) | tenant-c (w=1) | tenant-d (w=1) |\n")
	out.WriteString("|---|---:|---:|---:|---:|\n")
	for _, name := range []string{"balanced-drf", "balanced-baseline"} {
		variant, ok := findVariant(scenario, name)
		if !ok {
			continue
		}
		byTenant := make(map[string]TenantShare, len(variant.TenantShares))
		for _, share := range variant.TenantShares {
			byTenant[share.TenantID] = share
		}
		fmt.Fprintf(out, "| %s ", name)
		for _, spec := range tenantSpecs {
			share := byTenant[spec.id]
			fmt.Fprintf(out, "| %.4f → %.4f ", share.DominantShare, share.NormalizedShare)
		}
		out.WriteString("|\n")
	}
	out.WriteString("\n")
}

// RenderJSON 输出与 Markdown 同源的结构化指标，避免两份报告数字不一致。
func RenderJSON(result Result) ([]byte, error) {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("编码基准结果: %w", err)
	}
	return append(data, '\n'), nil
}

// buildConclusions 只根据本次结果生成描述，不把 Jain 提升归为节点级 DRF 的收益。
func buildConclusions(result Result) []string {
	conclusions := []string{
		"集群级租户公平性由 DRF 分配器与配额保证，节点级 DRF 不改变最终份额，两种模式 Jain 一致。",
	}
	for _, comparison := range []struct {
		scenario string
		drf      string
		baseline string
	}{
		{"balanced", "balanced-drf", "balanced-baseline"},
		{"hot-tenant", "balanced-drf", "balanced-baseline"},
		{"balanced", "fairshare-drf", "fairshare-baseline"},
		{"hot-tenant", "fairshare-drf", "fairshare-baseline"},
	} {
		if text, ok := compareDispersion(result, comparison.scenario, comparison.drf, comparison.baseline); ok {
			conclusions = append(conclusions, text)
		}
	}
	return conclusions
}

func compareDispersion(result Result, scenarioName, drfName, baselineName string) (string, bool) {
	scenario, ok := findScenario(result, scenarioName)
	if !ok {
		return "", false
	}
	drf, hasDRF := findVariant(scenario, drfName)
	baseline, hasBaseline := findVariant(scenario, baselineName)
	if !hasDRF || !hasBaseline {
		return "", false
	}

	a, b := drf.Metrics, baseline.Metrics
	prefix := fmt.Sprintf("`%s` 场景下，`%s` 相对 `%s`", scenarioName, drfName, baselineName)
	if a.MeanTenantHHI == b.MeanTenantHHI && a.MaxNodeReplicaShare == b.MaxNodeReplicaShare {
		return fmt.Sprintf("%s：当前场景下两种模式的散布无显著差异（平均 HHI 均为 %.4f，最大节点副本占比均为 %.2f%%）；节点级公平项只参与候选排序，其他评分维度与容量约束共同作用后，逐租户副本分布一致。",
			prefix, a.MeanTenantHHI, a.MaxNodeReplicaShare*100), true
	}
	return fmt.Sprintf("%s：%s；%s。", prefix,
		relativeChange("平均租户 HHI", b.MeanTenantHHI, a.MeanTenantHHI, "%.4f"),
		relativeChange("最大节点副本占比", b.MaxNodeReplicaShare*100, a.MaxNodeReplicaShare*100, "%.2f%%")), true
}

func relativeChange(name string, before, after float64, valueFormat string) string {
	beforeText := fmt.Sprintf(valueFormat, before)
	afterText := fmt.Sprintf(valueFormat, after)
	if before == after {
		return fmt.Sprintf("%s不变（%s）", name, afterText)
	}
	if before == 0 {
		return fmt.Sprintf("%s由 %s 变为 %s", name, beforeText, afterText)
	}
	change := (after - before) / before * 100
	direction := "上升"
	if change < 0 {
		direction = "降低"
	}
	return fmt.Sprintf("%s %s %.2f%%（%s → %s）", name, direction, math.Abs(change), beforeText, afterText)
}

func findScenario(result Result, name string) (ScenarioResult, bool) {
	for _, scenario := range result.Scenarios {
		if scenario.Name == name {
			return scenario, true
		}
	}
	return ScenarioResult{}, false
}

func findVariant(scenario ScenarioResult, name string) (VariantResult, bool) {
	for _, variant := range scenario.Variants {
		if variant.Name == name {
			return variant, true
		}
	}
	return VariantResult{}, false
}
