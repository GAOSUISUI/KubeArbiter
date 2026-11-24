package bench

import (
	"fmt"
	"math"
	"strings"
)

const (
	chartBarCells = 6
	chartFilled   = '█'
	chartEmpty    = '░'
)

// RenderConsole 生成面向终端的横向条形图，与 Markdown/JSON 使用同一份 Result。
// Jain 与 CPU 利用率按 0~1 绝对刻度展示；HHI、最大节点占比与耗时按各列最大值归一化，
// 以便放大变体之间的差异。
func RenderConsole(result Result) string {
	var out strings.Builder
	out.WriteString("M8 基准评测 · 控制台图表\n")
	fmt.Fprintf(&out, "seed=%d  rounds=%d  期望总需求=%.0f%% 集群CPU\n", result.Seed, result.Rounds, result.TargetDemandRatio*100)
	out.WriteString("Jain 越高越好；HHI 与最大节点占比越低越散布；耗时列为单次 Place 均值，斜杠后为 P99。\n")

	for _, scenario := range result.Scenarios {
		renderScenarioChart(&out, scenario)
	}
	renderDispersionComparison(&out, result)
	return out.String()
}

func renderScenarioChart(out *strings.Builder, scenario ScenarioResult) {
	hhiScale, shareScale, latencyScale := 0.0, 0.0, 0.0
	for _, variant := range scenario.Variants {
		hhiScale = math.Max(hhiScale, variant.Metrics.MeanTenantHHI)
		shareScale = math.Max(shareScale, variant.Metrics.MaxNodeReplicaShare)
		latencyScale = math.Max(latencyScale, variant.Metrics.AverageDecisionUS)
	}

	fmt.Fprintf(out, "\n场景 %s\n", scenario.Name)
	barPad := strings.Repeat(" ", chartBarCells)
	fmt.Fprintf(out, "  %-19s %s %-6s  %s %-7s  %s %-6s  %s %-7s  %s %-12s  %s\n",
		"variant", barPad, "Jain", barPad, "CPU%", barPad, "HHI", barPad, "maxNode%", barPad, "us avg/p99", "不可行率")
	for _, variant := range scenario.Variants {
		m := variant.Metrics
		fmt.Fprintf(out, "  %-19s %s %.4f  %s %6.2f%%  %s %.4f  %s %6.2f%%  %s %5.1f/%6.1f  %6.2f%%\n",
			variant.Name,
			chartBar(m.Jain, 1), m.Jain,
			chartBar(m.CPUUtilization, 1), m.CPUUtilization*100,
			chartBar(m.MeanTenantHHI, hhiScale), m.MeanTenantHHI,
			chartBar(m.MaxNodeReplicaShare, shareScale), m.MaxNodeReplicaShare*100,
			chartBar(m.AverageDecisionUS, latencyScale), m.AverageDecisionUS, m.P99US,
			m.InfeasibleRate*100)
	}
}

// renderDispersionComparison 只对比 DRF 与 baseline 的散布和开销，不声称提升 Jain。
func renderDispersionComparison(out *strings.Builder, result Result) {
	out.WriteString("\n节点级散布与开销（相对 baseline，负值表示更散布；末列为单次 Place 均值对比）\n")
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
		scenario, ok := findScenario(result, comparison.scenario)
		if !ok {
			continue
		}
		drf, hasDRF := findVariant(scenario, comparison.drf)
		baseline, hasBaseline := findVariant(scenario, comparison.baseline)
		if !hasDRF || !hasBaseline {
			continue
		}
		hhiChange := chartPercentChange(baseline.Metrics.MeanTenantHHI, drf.Metrics.MeanTenantHHI)
		shareChange := chartPercentChange(baseline.Metrics.MaxNodeReplicaShare, drf.Metrics.MaxNodeReplicaShare)
		fmt.Fprintf(out, "  %-11s %-18s HHI %+7.2f%%   maxNode %+7.2f%%   %6.1fµs vs %5.1fµs\n",
			comparison.scenario, comparison.drf, hhiChange, shareChange,
			drf.Metrics.AverageDecisionUS, baseline.Metrics.AverageDecisionUS)
	}
}

// chartBar 按 scale 归一化后渲染固定宽度条形，非零值至少保留一格。
func chartBar(value, scale float64) string {
	if scale <= 0 || value <= 0 {
		return strings.Repeat(string(chartEmpty), chartBarCells)
	}
	ratio := value / scale
	if ratio > 1 {
		ratio = 1
	}
	filled := int(math.Round(ratio * chartBarCells))
	if filled == 0 {
		filled = 1
	}
	return strings.Repeat(string(chartFilled), filled) +
		strings.Repeat(string(chartEmpty), chartBarCells-filled)
}

func chartPercentChange(before, after float64) float64 {
	if before == 0 {
		return 0
	}
	return (after - before) / before * 100
}
