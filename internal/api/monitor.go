package api

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/internal/observe"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// MetricHistory 是监控接口需要的最小指标历史能力。
type MetricHistory interface {
	Series(node string, from, to time.Time) []metrics.Sample
	Nodes() []string
	Newest() time.Time
	Retention() time.Duration
}

type monitorCluster struct {
	NodesTotal        int               `json:"nodes_total"`
	NodesReady        int               `json:"nodes_ready"`
	CPUAllocMilli     int64             `json:"cpu_alloc_milli"`
	CPURequestedMilli int64             `json:"cpu_requested_milli"`
	CPUUsageMilli     int64             `json:"cpu_usage_milli"`
	MemAllocBytes     int64             `json:"mem_alloc_bytes"`
	MemRequestedBytes int64             `json:"mem_requested_bytes"`
	MemUsageBytes     int64             `json:"mem_usage_bytes"`
	Pods              int               `json:"pods"`
	Health            aiops.HealthScore `json:"health"`
}

type monitorNode struct {
	Name          string              `json:"name"`
	Ready         bool                `json:"ready"`
	Unschedulable bool                `json:"unschedulable"`
	Conditions    map[string]bool     `json:"conditions"`
	Allocatable   schedcore.Resources `json:"allocatable"`
	Requested     schedcore.Resources `json:"requested"`
	Usage         schedcore.Resources `json:"usage"`
	UsageKnown    bool                `json:"usage_known"`
	PodCount      int                 `json:"pod_count"`
	Health        aiops.HealthScore   `json:"health"`
}

type monitorTenant struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Quota      schedcore.Resources `json:"quota"`
	Requested  schedcore.Resources `json:"requested"`
	QuotaRatio float64             `json:"quota_ratio"`
	Share      float64             `json:"share"`
	FairShare  float64             `json:"fair_share"`
	Health     aiops.HealthScore   `json:"health"`
}

type monitorIncidentSummary struct {
	Open     int `json:"open"`
	Critical int `json:"critical"`
	Warn     int `json:"warn"`
	Info     int `json:"info"`
}

type monitorOverviewResponse struct {
	TakenAt           time.Time              `json:"taken_at"`
	Degraded          bool                   `json:"degraded"`
	MetricsAt         *time.Time             `json:"metrics_at"`
	MetricsAgeSeconds *int64                 `json:"metrics_age_seconds"`
	RetentionSeconds  int64                  `json:"retention_seconds"`
	Cluster           monitorCluster         `json:"cluster"`
	Nodes             []monitorNode          `json:"nodes"`
	Tenants           []monitorTenant        `json:"tenants"`
	Incidents         monitorIncidentSummary `json:"incidents"`
}

func monitorOverview(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Observer == nil {
			writeError(c, errUnavailable())
			return
		}
		diag, err := d.Observer(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		now := time.Now().UTC()
		tenants := TenantInputs(c.Request.Context(), d.TenantCatalog, diag)
		ruleInput := aiops.RuleInput{
			Now:       now,
			Nodes:     diag.Nodes,
			Pods:      diag.Pods,
			Tenants:   tenants,
			MetricsAt: diag.MetricsAt,
		}
		findings := aiops.Detect(ruleInput)
		health := aiops.ComputeHealth(aiops.HealthInput{RuleInput: ruleInput, Findings: findings})
		healthNodes := make(map[string]aiops.HealthScore, len(health.Nodes))
		for _, score := range health.Nodes {
			healthNodes[score.Name] = score
		}
		healthTenants := make(map[string]aiops.HealthScore, len(health.Tenants))
		for _, score := range health.Tenants {
			healthTenants[score.Name] = score
		}

		degraded := diag.MetricsAt.IsZero()
		var metricsAge *int64
		var metricsAt *time.Time
		if !diag.MetricsAt.IsZero() {
			at := diag.MetricsAt
			metricsAt = &at
			age := int64(now.Sub(at).Seconds())
			metricsAge = &age
			if now.Sub(at) > aiops.MetricsStaleAfter {
				degraded = true
			}
		}

		response := monitorOverviewResponse{
			TakenAt:           now,
			Degraded:          degraded,
			MetricsAt:         metricsAt,
			MetricsAgeSeconds: metricsAge,
			Nodes:             make([]monitorNode, 0, len(diag.Nodes)),
			Tenants:           make([]monitorTenant, 0, len(tenants)),
			Cluster:           monitorCluster{Pods: 0},
		}
		if d.Metrics != nil {
			response.RetentionSeconds = int64(d.Metrics.Retention().Seconds())
		}

		for _, node := range diag.Nodes {
			response.Cluster.NodesTotal++
			if node.Ready {
				response.Cluster.NodesReady++
			}
			response.Cluster.CPUAllocMilli += node.Allocatable.CPUMilli
			response.Cluster.CPURequestedMilli += node.Requested.CPUMilli
			response.Cluster.MemAllocBytes += node.Allocatable.MemBytes
			response.Cluster.MemRequestedBytes += node.Requested.MemBytes
			response.Cluster.Pods += node.PodCount
			if node.UsageKnown {
				response.Cluster.CPUUsageMilli += node.Usage.CPUMilli
				response.Cluster.MemUsageBytes += node.Usage.MemBytes
			}
			response.Nodes = append(response.Nodes, monitorNode{
				Name:          node.Name,
				Ready:         node.Ready,
				Unschedulable: node.Unschedulable,
				Conditions: map[string]bool{
					"ready":           node.Ready,
					"memory_pressure": node.MemoryPressure,
					"disk_pressure":   node.DiskPressure,
					"pid_pressure":    node.PIDPressure,
					"unschedulable":   node.Unschedulable,
				},
				Allocatable: node.Allocatable,
				Requested:   node.Requested,
				Usage:       node.Usage,
				UsageKnown:  node.UsageKnown,
				PodCount:    node.PodCount,
				Health:      healthNodes[node.Name],
			})
		}
		response.Cluster.Health = health.Cluster

		clusterCap := clusterCapacityFromDiag(diag)
		for _, tenant := range tenants {
			response.Tenants = append(response.Tenants, monitorTenant{
				ID:         tenant.ID,
				Name:       tenant.Name,
				Quota:      tenant.Quota,
				Requested:  tenant.Requested,
				QuotaRatio: maxResourceRatio(tenant.Requested, tenant.Quota),
				Share:      dominantShare(tenant.Requested, clusterCap),
				FairShare:  tenant.FairShare,
				Health:     healthTenants[tenant.ID],
			})
		}
		sort.Slice(response.Tenants, func(i, j int) bool {
			if response.Tenants[i].Name != response.Tenants[j].Name {
				return response.Tenants[i].Name < response.Tenants[j].Name
			}
			return response.Tenants[i].ID < response.Tenants[j].ID
		})

		if d.Incidents != nil {
			open, err := d.Incidents.ListIncidents(c.Request.Context(), aiops.Filter{Status: "OPEN"})
			if err != nil {
				response.Degraded = true
			} else {
				response.Incidents.Open = len(open)
				for _, inc := range open {
					switch inc.Severity {
					case aiops.SeverityCritical:
						response.Incidents.Critical++
					case aiops.SeverityWarn:
						response.Incidents.Warn++
					default:
						response.Incidents.Info++
					}
				}
			}
		}

		writeOK(c, response)
	}
}

type monitorPoint struct {
	TS       time.Time `json:"ts"`
	CPUMilli int64     `json:"cpu_milli"`
	MemBytes int64     `json:"mem_bytes"`
}

type monitorNodeSeries struct {
	Name   string         `json:"name"`
	Points []monitorPoint `json:"points"`
}

type monitorSeriesResponse struct {
	Window           string              `json:"window"`
	StepSeconds      int                 `json:"step_seconds"`
	RetentionSeconds int64               `json:"retention_seconds"`
	Degraded         bool                `json:"degraded"`
	Nodes            []monitorNodeSeries `json:"nodes"`
	Cluster          []monitorPoint      `json:"cluster"`
}

func monitorSeries(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		window, step, ok := parseSeriesWindow(c.Query("window"))
		if !ok {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "window", Reason: "仅支持 1h、6h、24h"}))
			return
		}
		response := monitorSeriesResponse{
			Window:      window.String(),
			StepSeconds: int(step.Seconds()),
			Nodes:       []monitorNodeSeries{},
			Cluster:     []monitorPoint{},
		}
		if d.Metrics == nil {
			response.Degraded = true
			writeOK(c, response)
			return
		}

		now := time.Now().UTC()
		from := now.Add(-window)
		response.RetentionSeconds = int64(d.Metrics.Retention().Seconds())
		if newest := d.Metrics.Newest(); newest.IsZero() || now.Sub(newest) > aiops.MetricsStaleAfter {
			response.Degraded = true
		}

		nodeNames := d.Metrics.Nodes()
		if len(nodeNames) == 0 {
			response.Degraded = true
		}
		clusterBuckets := make(map[int64]*pointAcc)
		for _, name := range nodeNames {
			buckets := aggregateSamples(d.Metrics.Series(name, from, now), step)
			points := pointsFromBuckets(buckets)
			response.Nodes = append(response.Nodes, monitorNodeSeries{Name: name, Points: points})
			for ts, acc := range buckets {
				avg := acc.average()
				target := clusterBuckets[ts]
				if target == nil {
					target = &pointAcc{}
					clusterBuckets[ts] = target
				}
				target.cpu += avg.CPUMilli
				target.mem += avg.MemBytes
				target.n = 1
			}
		}
		response.Cluster = pointsFromBuckets(clusterBuckets)
		writeOK(c, response)
	}
}

type pointAcc struct {
	cpu int64
	mem int64
	n   int64
}

func (a *pointAcc) average() schedcore.Resources {
	if a.n == 0 {
		return schedcore.Resources{}
	}
	return schedcore.Resources{CPUMilli: a.cpu / a.n, MemBytes: a.mem / a.n}
}

func (a *pointAcc) averagePoint() monitorPoint {
	avg := a.average()
	return monitorPoint{CPUMilli: avg.CPUMilli, MemBytes: avg.MemBytes}
}

func aggregateSamples(samples []metrics.Sample, step time.Duration) map[int64]*pointAcc {
	seconds := int64(step.Seconds())
	out := make(map[int64]*pointAcc)
	for _, sample := range samples {
		bucket := sample.At.Unix()
		if seconds > 0 {
			bucket -= bucket % seconds
		}
		acc := out[bucket]
		if acc == nil {
			acc = &pointAcc{}
			out[bucket] = acc
		}
		acc.cpu += sample.Usage.CPUMilli
		acc.mem += sample.Usage.MemBytes
		acc.n++
	}
	return out
}

func pointsFromBuckets(buckets map[int64]*pointAcc) []monitorPoint {
	keys := make([]int64, 0, len(buckets))
	for ts := range buckets {
		keys = append(keys, ts)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]monitorPoint, 0, len(keys))
	for _, ts := range keys {
		point := buckets[ts].averagePoint()
		point.TS = time.Unix(ts, 0).UTC()
		out = append(out, point)
	}
	return out
}

func parseSeriesWindow(raw string) (time.Duration, time.Duration, bool) {
	switch raw {
	case "", "1h":
		return time.Hour, time.Minute, true
	case "6h":
		return 6 * time.Hour, time.Minute, true
	case "24h":
		return 24 * time.Hour, 5 * time.Minute, true
	default:
		return 0, 0, false
	}
}

// TenantInputs 把租户目录与诊断视图拼成检测/健康分所需的输入。
func TenantInputs(ctx context.Context, catalog TenantLister, diag capacity.Diagnostics) []aiops.TenantInput {
	if catalog == nil {
		return nil
	}
	tenants, err := catalog.ListTenants(ctx)
	if err != nil {
		return nil
	}
	clusterCap := clusterCapacityFromDiag(diag)
	var totalWeight float64
	for _, tenant := range tenants {
		if tenant.Weight > 0 {
			totalWeight += tenant.Weight
		}
	}
	out := make([]aiops.TenantInput, 0, len(tenants))
	for _, tenant := range tenants {
		fairShare := tenant.FairShare
		if fairShare <= 0 && totalWeight > 0 && tenant.Weight > 0 {
			fairShare = tenant.Weight / totalWeight
		}
		out = append(out, aiops.TenantInput{
			ID:        tenant.ID,
			Name:      tenant.Name,
			Quota:     tenant.Quota,
			Requested: diag.TenantUsage[tenant.ID],
			Share:     dominantShare(diag.TenantUsage[tenant.ID], clusterCap),
			FairShare: fairShare,
		})
	}
	return out
}

func clusterCapacityFromDiag(diag capacity.Diagnostics) schedcore.Resources {
	var sum schedcore.Resources
	for _, node := range diag.Nodes {
		if node.Ready && !node.Unschedulable {
			sum = sum.Add(node.Allocatable)
		}
	}
	return sum
}

func maxResourceRatio(used, capacity schedcore.Resources) float64 {
	cpu := 0.0
	if capacity.CPUMilli > 0 {
		cpu = float64(used.CPUMilli) / float64(capacity.CPUMilli)
	}
	mem := 0.0
	if capacity.MemBytes > 0 {
		mem = float64(used.MemBytes) / float64(capacity.MemBytes)
	}
	if mem > cpu {
		return mem
	}
	return cpu
}

func recordRequests(rec *observe.Recorder) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if strings.HasPrefix(c.Request.URL.Path, "/api/v1/") {
			rec.Observe(c.Writer.Status())
		}
	}
}
