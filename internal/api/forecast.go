package api

import (
	"math"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/internal/forecast"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// PodUsageReader 是 rightsizing 需要的最小 per-pod 用量能力。
type PodUsageReader interface {
	Snapshot() map[metrics.PodKey]schedcore.Resources
}

type forecastPoint struct {
	TS       time.Time `json:"ts"`
	CPUMilli float64   `json:"cpu_milli"`
	MemBytes float64   `json:"mem_bytes"`
}

type forecastBandPoint struct {
	TS       time.Time `json:"ts"`
	CPUUpper float64   `json:"cpu_upper"`
	CPULower float64   `json:"cpu_lower"`
	MemUpper float64   `json:"mem_upper"`
	MemLower float64   `json:"mem_lower"`
}

type forecastNode struct {
	Name          string              `json:"name"`
	Confidence    string              `json:"confidence"`
	UsageRatio    float64             `json:"usage_ratio"`
	R2            float64             `json:"r2"`
	SlopeCpu      float64             `json:"slope_cpu_per_min"`
	SlopeMem      float64             `json:"slope_mem_per_min"`
	SampleSpan    int64               `json:"sample_span_seconds"`
	TimeToFull90  *int64              `json:"time_to_full_90_seconds"`
	TimeToFull100 *int64              `json:"time_to_full_100_seconds"`
	Allocatable   schedcore.Resources `json:"allocatable"`
	History       []forecastPoint     `json:"history"`
	Prediction    []forecastPoint     `json:"prediction"`
	Band          []forecastBandPoint `json:"band"`
}

type forecastCapacityResponse struct {
	TakenAt           time.Time      `json:"taken_at"`
	Horizon           string         `json:"horizon"`
	Degraded          bool           `json:"degraded"`
	Confidence        string         `json:"confidence"`
	SampleSpanSeconds int64          `json:"sample_span_seconds"`
	Nodes             []forecastNode `json:"nodes"`
	Cluster           *forecastNode  `json:"cluster"`
}

const forecastMaxPredictionPoints = 24

// forecastCapacity 返回按节点/集群的负载外推：历史（1 分钟桶）、预测点与置信带。
// 数据不足时 degraded=true 且不给预测点，绝不返回编造的数字。
func forecastCapacity(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		horizonRaw := c.Query("horizon")
		if horizonRaw == "" {
			horizonRaw = "30m"
		}
		horizon, ok := forecast.Horizons[horizonRaw]
		if !ok {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "horizon", Reason: "仅支持 15m、30m、1h、3h"}))
			return
		}

		now := time.Now().UTC()
		response := forecastCapacityResponse{
			TakenAt: now,
			Horizon: horizonRaw,
			Nodes:   []forecastNode{},
		}
		if d.Metrics == nil || d.Observer == nil {
			response.Degraded = true
			response.Confidence = string(forecast.ConfidenceNone)
			writeOK(c, response)
			return
		}

		diag, err := d.Observer(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		allocs := make(map[string]schedcore.Resources, len(diag.Nodes))
		for _, node := range diag.Nodes {
			allocs[node.Name] = node.Allocatable
		}

		from := now.Add(-d.Metrics.Retention())
		clusterBuckets := make(map[int64]*pointAcc)
		for _, name := range d.Metrics.Nodes() {
			series := d.Metrics.Series(name, from, now)
			if len(series) == 0 {
				continue
			}
			fn := buildForecastNode(name, series, allocs[name], horizon, now)
			response.Nodes = append(response.Nodes, fn)
			if fn.Confidence != string(forecast.ConfidenceNone) {
				response.SampleSpanSeconds = maxInt64(response.SampleSpanSeconds, fn.SampleSpan)
			}
			// 集群序列：按桶求和（可调度节点容量单独累计）。
			for ts, acc := range aggregateSamples(series, time.Minute) {
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
		sort.Slice(response.Nodes, func(i, j int) bool { return response.Nodes[i].Name < response.Nodes[j].Name })

		var clusterAlloc schedcore.Resources
		for _, node := range diag.Nodes {
			if node.Ready && !node.Unschedulable {
				clusterAlloc = clusterAlloc.Add(node.Allocatable)
			}
		}
		if clusterSeries := samplesFromBuckets(clusterBuckets); len(clusterSeries) > 0 {
			cn := buildForecastNode("cluster", clusterSeries, clusterAlloc, horizon, now)
			response.Cluster = &cn
			if cn.SampleSpan > response.SampleSpanSeconds {
				response.SampleSpanSeconds = cn.SampleSpan
			}
		}

		response.Confidence = worstConfidence(response.Nodes, response.Cluster)
		response.Degraded = response.Confidence == string(forecast.ConfidenceNone)
		writeOK(c, response)
	}
}

// buildForecastNode 对单节点序列做拟合并组装响应结构。
func buildForecastNode(name string, series []metrics.Sample, alloc schedcore.Resources, horizon time.Duration, now time.Time) forecastNode {
	cpu := forecast.FitDim(splitDim(series, true))
	mem := forecast.FitDim(splitDim(series, false))
	node := forecast.NodeForecastFor(name, series, alloc, now)

	fn := forecastNode{
		Name:        name,
		Allocatable: alloc,
		History:     historyPoints(series),
		Prediction:  []forecastPoint{},
		Band:        []forecastBandPoint{},
	}
	fn.SlopeCpu = cpu.SlopePerMin
	fn.SlopeMem = mem.SlopePerMin
	fn.R2 = cpu.R2
	if mem.R2 < fn.R2 {
		fn.R2 = mem.R2
	}
	fn.UsageRatio = node.UsageRatio
	fn.TimeToFull90 = durationPtr(node.TimeToFull90)
	fn.TimeToFull100 = durationPtr(node.TimeToFull100)
	fn.SampleSpan = int64(forecastSpan(series).Seconds())

	confCPU, confMem := forecast.ConfidenceOf(cpu), forecast.ConfidenceOf(mem)
	fn.Confidence = string(worstOf(confCPU, confMem))
	if fn.Confidence == string(forecast.ConfidenceNone) {
		return fn
	}

	// 预测点与置信带：从最后样本起，等间隔覆盖 horizon。
	span := horizon / forecastMaxPredictionPoints
	for i := 1; i <= forecastMaxPredictionPoints; i++ {
		at := cpu.LastAt.Add(time.Duration(i) * span)
		pCPU := linearValue(cpu.LastValue, cpu.SlopePerMin, cpu.LastAt, at)
		pMem := linearValue(mem.LastValue, mem.SlopePerMin, mem.LastAt, at)
		fn.Prediction = append(fn.Prediction, forecastPoint{TS: at, CPUMilli: pCPU, MemBytes: pMem})
		fn.Band = append(fn.Band, forecastBandPoint{
			TS:       at,
			CPUUpper: math.Max(0, pCPU+cpu.StdErr), CPULower: math.Max(0, pCPU-cpu.StdErr),
			MemUpper: math.Max(0, pMem+mem.StdErr), MemLower: math.Max(0, pMem-mem.StdErr),
		})
	}
	return fn
}

func linearValue(last, slopePerMin float64, from, at time.Time) float64 {
	v := last + slopePerMin*at.Sub(from).Minutes()
	if v < 0 {
		return 0
	}
	return math.Round(v*100) / 100
}

func historyPoints(series []metrics.Sample) []forecastPoint {
	buckets := aggregateSamples(series, time.Minute)
	points := pointsFromBuckets(buckets)
	out := make([]forecastPoint, 0, len(points))
	for _, p := range points {
		out = append(out, forecastPoint{TS: p.TS, CPUMilli: float64(p.CPUMilli), MemBytes: float64(p.MemBytes)})
	}
	return out
}

func samplesFromBuckets(buckets map[int64]*pointAcc) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(buckets))
	for _, p := range pointsFromBuckets(buckets) {
		out = append(out, metrics.Sample{At: p.TS, Usage: schedcore.Resources{CPUMilli: p.CPUMilli, MemBytes: p.MemBytes}})
	}
	return out
}

// splitDim 把双维序列拆成单维序列，配合 forecast 包的拟合函数。
func splitDim(series []metrics.Sample, cpu bool) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(series))
	for _, s := range series {
		if cpu {
			out = append(out, metrics.Sample{At: s.At, Usage: schedcore.Resources{CPUMilli: s.Usage.CPUMilli}})
		} else {
			out = append(out, metrics.Sample{At: s.At, Usage: schedcore.Resources{MemBytes: s.Usage.MemBytes}})
		}
	}
	return out
}

func forecastSpan(series []metrics.Sample) time.Duration {
	if len(series) == 0 {
		return 0
	}
	oldest, newest := series[0].At, series[0].At
	for _, s := range series[1:] {
		if s.At.Before(oldest) {
			oldest = s.At
		}
		if s.At.After(newest) {
			newest = s.At
		}
	}
	return newest.Sub(oldest)
}

func worstConfidence(nodes []forecastNode, cluster *forecastNode) string {
	if len(nodes) == 0 && cluster == nil {
		return string(forecast.ConfidenceNone)
	}
	worst := forecast.ConfidenceHigh
	for _, n := range nodes {
		worst = worstOf(worst, forecast.Confidence(n.Confidence))
	}
	if cluster != nil {
		worst = worstOf(worst, forecast.Confidence(cluster.Confidence))
	}
	return string(worst)
}

func worstOf(a, b forecast.Confidence) forecast.Confidence {
	rank := map[forecast.Confidence]int{
		forecast.ConfidenceNone: 0, forecast.ConfidenceLow: 1,
		forecast.ConfidenceMedium: 2, forecast.ConfidenceHigh: 3,
	}
	if rank[a] <= rank[b] {
		return a
	}
	return b
}

func durationPtr(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	v := int64(d.Seconds())
	return &v
}

func maxInt64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}
