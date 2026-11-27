package aiops

import (
	"fmt"
	"math"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
)

// FairShare 缺失时不做"份额超配"扣分。
type HealthInput struct {
	RuleInput
	Findings []Finding
}

// HealthReport 是一次健康评估的结果。
type HealthReport struct {
	Cluster HealthScore   `json:"cluster"`
	Nodes   []HealthScore `json:"nodes"`
	Tenants []HealthScore `json:"tenants"`
}

// ComputeHealth 按可解释的固定规则打分，纯函数。
func ComputeHealth(in HealthInput) HealthReport {
	report := HealthReport{
		Nodes:   make([]HealthScore, 0, len(in.Nodes)),
		Tenants: make([]HealthScore, 0, len(in.Tenants)),
	}

	var weightSum, weighted float64
	for _, node := range in.Nodes {
		score, comps := nodeHealth(node, in)
		report.Nodes = append(report.Nodes, HealthScore{
			Scope: "NODE", Name: node.Name, Score: score, Components: comps,
		})
		weight := float64(node.Allocatable.CPUMilli)
		if weight <= 0 {
			weight = 1
		}
		weightSum += weight
		weighted += *score * weight
	}

	if len(in.Nodes) == 0 {
		report.Cluster = HealthScore{
			Scope: "CLUSTER", Name: "cluster", Score: nil,
			Components: []HealthComponent{{Code: "NO_DATA", Message: "暂无节点数据", Delta: 0}},
		}
	} else {
		base := weighted / weightSum
		comps := make([]HealthComponent, 0)
		if crit := countFindings(in.Findings, SeverityCritical); crit > 0 {
			delta := math.Max(-30, -15*float64(crit))
			base += delta
			comps = append(comps, HealthComponent{
				Code: "CRITICAL_INCIDENTS", Message: fmt.Sprintf("%d 个未解决严重事件", crit), Delta: delta,
			})
		}
		if warn := countFindings(in.Findings, SeverityWarn); warn > 0 {
			delta := math.Max(-15, -5*float64(warn))
			base += delta
			comps = append(comps, HealthComponent{
				Code: "WARN_INCIDENTS", Message: fmt.Sprintf("%d 个未解决告警事件", warn), Delta: delta,
			})
		}
		if stale := metricsStale(in); stale {
			base -= 10
			comps = append(comps, HealthComponent{Code: "METRICS_STALE", Message: "指标已过期", Delta: -10})
		}
		report.Cluster = HealthScore{Scope: "CLUSTER", Name: "cluster", Score: clampScore(base), Components: comps}
	}

	for _, tenant := range in.Tenants {
		report.Tenants = append(report.Tenants, tenantHealth(tenant, in))
	}
	return report
}

func nodeHealth(node capacity.NodeDiag, in HealthInput) (*float64, []HealthComponent) {
	score := 100.0
	comps := make([]HealthComponent, 0)

	if !node.Ready {
		score -= 60
		comps = append(comps, HealthComponent{Code: "NOT_READY", Message: "节点未就绪", Delta: -60})
	}

	pressure := 0
	if node.MemoryPressure {
		pressure++
	}
	if node.DiskPressure {
		pressure++
	}
	if node.PIDPressure {
		pressure++
	}
	if pressure > 0 {
		delta := math.Max(-20, -10*float64(pressure))
		score += delta
		comps = append(comps, HealthComponent{
			Code: "NODE_PRESSURE", Message: fmt.Sprintf("%d 项资源压力", pressure), Delta: delta,
		})
	}

	if node.UsageKnown {
		cpuRatio := ratio(node.Usage.CPUMilli, node.Allocatable.CPUMilli)
		memRatio := ratio(node.Usage.MemBytes, node.Allocatable.MemBytes)
		if cpuRatio >= HighUsageRatio || memRatio >= HighUsageRatio {
			score -= 10
			comps = append(comps, HealthComponent{
				Code: "HIGH_USAGE", Message: fmt.Sprintf("CPU %.0f%% / 内存 %.0f%%", cpuRatio*100, memRatio*100), Delta: -10,
			})
		}
	} else {
		score -= 15
		comps = append(comps, HealthComponent{Code: "METRICS_MISSING", Message: "节点指标缺失", Delta: -15})
	}
	if metricsStale(in) && node.UsageKnown {
		score -= 10
		comps = append(comps, HealthComponent{Code: "METRICS_STALE", Message: "指标已过期", Delta: -10})
	}

	var crit, warn int
	for _, f := range in.Findings {
		if f.Namespace != NamespaceNode || f.Target != node.Name {
			continue
		}
		// 预测耗尽单列扣分项，不并入通用 WARN 计数，避免双重扣分。
		if f.Type == IncidentNodeForecastExhaust {
			continue
		}
		switch f.Severity {
		case SeverityCritical:
			crit++
		case SeverityWarn:
			warn++
		}
	}
	if crit > 0 {
		delta := math.Max(-20, -10*float64(crit))
		score += delta
		comps = append(comps, HealthComponent{
			Code: "CRITICAL_INCIDENTS", Message: fmt.Sprintf("%d 个严重事件", crit), Delta: delta,
		})
	}
	if warn > 0 {
		delta := math.Max(-10, -5*float64(warn))
		score += delta
		comps = append(comps, HealthComponent{
			Code: "WARN_INCIDENTS", Message: fmt.Sprintf("%d 个告警事件", warn), Delta: delta,
		})
	}
	for _, f := range in.Findings {
		if f.Type == IncidentNodeForecastExhaust && f.Namespace == NamespaceNode && f.Target == node.Name {
			score -= 5
			comps = append(comps, HealthComponent{
				Code: "FORECAST_EXHAUST", Message: "负载趋势预计 30 分钟内达到容量 90%", Delta: -5,
			})
		}
	}
	return clampScore(score), comps
}

func tenantHealth(tenant TenantInput, in HealthInput) HealthScore {
	score := 100.0
	comps := make([]HealthComponent, 0)
	cpuRatio := ratio(tenant.Requested.CPUMilli, tenant.Quota.CPUMilli)
	memRatio := ratio(tenant.Requested.MemBytes, tenant.Quota.MemBytes)
	used := math.Max(cpuRatio, memRatio)

	switch {
	case used >= 1:
		score -= 45
		comps = append(comps, HealthComponent{Code: "QUOTA_EXHAUSTED", Message: "配额已用尽", Delta: -45})
	case used >= 0.9:
		score -= 25
		comps = append(comps, HealthComponent{Code: "QUOTA_NEAR", Message: "配额使用率超过 90%", Delta: -25})
	case used >= 0.8:
		score -= 10
		comps = append(comps, HealthComponent{Code: "QUOTA_HIGH", Message: "配额使用率超过 80%", Delta: -10})
	}

	var crit, warn int
	for _, f := range in.Findings {
		if f.Namespace != NamespaceTenant || f.Target != tenant.ID {
			continue
		}
		switch f.Severity {
		case SeverityCritical:
			crit++
		case SeverityWarn:
			warn++
		}
	}
	if crit > 0 {
		score -= 15
		comps = append(comps, HealthComponent{
			Code: "CRITICAL_INCIDENTS", Message: fmt.Sprintf("%d 个严重事件", crit), Delta: -15,
		})
	}
	if warn > 0 {
		score -= 5
		comps = append(comps, HealthComponent{
			Code: "WARN_INCIDENTS", Message: fmt.Sprintf("%d 个告警事件", warn), Delta: -5,
		})
	}
	if tenant.FairShare > 0 && tenant.Share > 2*tenant.FairShare {
		score -= 10
		comps = append(comps, HealthComponent{
			Code: "SHARE_OVERSHOT", Message: "主导份额超过公平份额 2 倍", Delta: -10,
		})
	}

	if tenant.Quota.CPUMilli == 0 && tenant.Quota.MemBytes == 0 && len(comps) == 0 {
		return HealthScore{
			Scope: "TENANT", Name: tenant.ID, Score: nil,
			Components: []HealthComponent{{Code: "NO_DATA", Message: "缺少配额与事件数据", Delta: 0}},
		}
	}
	return HealthScore{Scope: "TENANT", Name: tenant.ID, Score: clampScore(score), Components: comps}
}

func countFindings(findings []Finding, sev Severity) int {
	n := 0
	for _, f := range findings {
		if f.Severity == sev {
			n++
		}
	}
	return n
}

func metricsStale(in HealthInput) bool {
	if in.MetricsAt.IsZero() {
		return true
	}
	return in.Now.Sub(in.MetricsAt) > MetricsStaleAfter
}

func clampScore(v float64) *float64 {
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	v = math.Round(v*10) / 10
	return &v
}
