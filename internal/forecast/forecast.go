// Package forecast 提供基于最小二乘线性回归的短程容量预测。
// 纯函数实现，不依赖任何数学库；输入是 metrics.Store 的原始样本序列，
// 输出带置信分档的预测值与预计耗尽时间。所有结果都是建议性质。
package forecast

import (
	"math"
	"sort"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	// BucketSeconds 是回归前下采样的桶宽：1 分钟，压掉 15s 采样的毛刺。
	BucketSeconds = 60
	// MinBuckets 是回归所需的最少有效桶数，低于此值视为数据不足。
	MinBuckets = 10
	// MinSpan 是回归所需的最短样本跨度；不足时同样视为数据不足。
	MinSpan = 10 * time.Minute
	// HighR2 / MediumR2 是置信分档阈值。
	HighR2   = 0.8
	MediumR2 = 0.6
	// MeaningfulUsageRatio 是 time_to_full 有参考意义的最低当前占比。
	// 占比过低时即使斜率为正，预计耗尽时间也远超预测可信范围。
	MeaningfulUsageRatio = 0.6
)

// Confidence 描述一次拟合的可信程度。NONE 表示数据不足，不得外推。
type Confidence string

const (
	ConfidenceNone   Confidence = "NONE"
	ConfidenceLow    Confidence = "LOW"
	ConfidenceMedium Confidence = "MEDIUM"
	ConfidenceHigh   Confidence = "HIGH"
)

// DimFit 是单一资源维度（CPU 或内存）的一次线性拟合结果。
// 数值单位与输入一致（CPU 毫核、内存字节），斜率单位是"每分钟"。
type DimFit struct {
	SlopePerMin float64
	LastValue   float64
	R2          float64
	StdErr      float64
	LastAt      time.Time
	Points      int
	// Predictions 为空表示数据不足。
	Prediction *float64
}

// NodeForecast 是一个节点两个维度拟合的汇总。
type NodeForecast struct {
	Name string
	CPU  DimFit
	Mem  DimFit
	// TimeToFull90/100 是按当前斜率达到 allocatable 90%/100% 的预计剩余时间；
	// nil 表示不适用（斜率非正、占比过低或数据不足）。
	TimeToFull90  *time.Duration
	TimeToFull100 *time.Duration
	// UsageRatio 是回归终点（最后样本）的 CPU/内存占比中的较大者。
	UsageRatio float64
}

// Horizon 白名单：预测视界受限于 6h 历史，超过 3h 的外推不可信。
var Horizons = map[string]time.Duration{
	"15m": 15 * time.Minute,
	"30m": 30 * time.Minute,
	"1h":  time.Hour,
	"3h":  3 * time.Hour,
}

// FitDim 对单维序列做"1 分钟桶均值 + 最小二乘"拟合。
// 样本不足时返回 Confidence=NONE 且 Prediction=nil。
func FitDim(samples []metrics.Sample) DimFit {
	buckets := bucketize(samples)
	fit := DimFit{LastAt: lastTime(samples), Points: len(buckets)}
	if len(buckets) < MinBuckets || spanOf(buckets) < MinSpan {
		return fit
	}

	// 以"距首桶的分钟数"为自变量做 OLS。
	n := float64(len(buckets))
	var sx, sy, sxx, sxy float64
	for i, b := range buckets {
		x := float64(i)
		sx += x
		sy += b.value
		sxx += x * x
		sxy += x * b.value
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return fit
	}
	slope := (n*sxy - sx*sy) / denom
	intercept := (sy - slope*sx) / n

	// R² 与残差标准误。
	var ssRes, ssTot float64
	meanY := sy / n
	for i, b := range buckets {
		pred := intercept + slope*float64(i)
		ssRes += (b.value - pred) * (b.value - pred)
		ssTot += (b.value - meanY) * (b.value - meanY)
	}
	r2 := 1.0
	if ssTot > 0 {
		r2 = 1 - ssRes/ssTot
	}
	// 方差为 0（完全平稳）的序列是完美可预测的，不应因 R² 公式退化而判为低置信。
	stdErr := math.Sqrt(ssRes / n)

	last := buckets[len(buckets)-1]
	fit.SlopePerMin = slope
	fit.LastValue = last.value
	fit.R2 = r2
	fit.StdErr = stdErr
	fit.LastAt = last.at
	pred := intercept + slope*float64(len(buckets)-1)
	fit.Prediction = &pred
	return fit
}

// ConfidenceOf 依据样本量与 R² 给出置信分档。
func ConfidenceOf(fit DimFit) Confidence {
	if fit.Prediction == nil {
		return ConfidenceNone
	}
	switch {
	case fit.R2 >= HighR2:
		return ConfidenceHigh
	case fit.R2 >= MediumR2:
		return ConfidenceMedium
	default:
		return ConfidenceLow
	}
}

// Predict 在 horizon 之后的外推值；数据不足时返回 nil。
func Predict(fit DimFit, horizon time.Duration, now time.Time) *float64 {
	if fit.Prediction == nil {
		return nil
	}
	minutes := now.Sub(fit.LastAt).Minutes() + horizon.Minutes()
	v := fit.LastValue + fit.SlopePerMin*minutes
	return &v
}

// TimeToFull 计算达到 target 绝对值的预计剩余时间。
// 仅在斜率为正且当前占比达 MeaningfulUsageRatio 时有意义；已达/超过返回 0。
func TimeToFull(fit DimFit, target float64) *time.Duration {
	if fit.Prediction == nil || fit.SlopePerMin <= 0 {
		return nil
	}
	minutes := (target - fit.LastValue) / fit.SlopePerMin
	if minutes < 0 {
		minutes = 0
	}
	d := time.Duration(minutes * float64(time.Minute))
	return &d
}

// NodeForecastFor 对单个节点做两维拟合与耗尽估计。
// allocatable 为 0 时跳过 TimeToFull（占比无意义）。
func NodeForecastFor(name string, series []metrics.Sample, alloc schedcore.Resources, now time.Time) NodeForecast {
	cpu := FitDim(cpuSamples(series))
	mem := FitDim(memSamples(series))
	out := NodeForecast{Name: name, CPU: cpu, Mem: mem}

	if cpu.Prediction != nil && alloc.CPUMilli > 0 {
		out.UsageRatio = math.Max(out.UsageRatio, cpu.LastValue/float64(alloc.CPUMilli))
	}
	if mem.Prediction != nil && alloc.MemBytes > 0 {
		ratio := mem.LastValue / float64(alloc.MemBytes)
		if ratio > out.UsageRatio {
			out.UsageRatio = ratio
		}
	}

	ratioOK := out.UsageRatio >= MeaningfulUsageRatio
	if alloc.CPUMilli > 0 && ratioOK {
		out.TimeToFull90 = TimeToFull(cpu, 0.9*float64(alloc.CPUMilli))
		out.TimeToFull100 = TimeToFull(cpu, float64(alloc.CPUMilli))
	}
	if alloc.MemBytes > 0 && ratioOK {
		t90 := TimeToFull(mem, 0.9*float64(alloc.MemBytes))
		t100 := TimeToFull(mem, float64(alloc.MemBytes))
		out.TimeToFull90 = minDuration(out.TimeToFull90, t90)
		out.TimeToFull100 = minDuration(out.TimeToFull100, t100)
	}
	return out
}

func cpuSamples(series []metrics.Sample) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(series))
	for _, s := range series {
		out = append(out, metrics.Sample{At: s.At, Usage: schedcore.Resources{CPUMilli: s.Usage.CPUMilli}})
	}
	return out
}

func memSamples(series []metrics.Sample) []metrics.Sample {
	out := make([]metrics.Sample, 0, len(series))
	for _, s := range series {
		out = append(out, metrics.Sample{At: s.At, Usage: schedcore.Resources{MemBytes: s.Usage.MemBytes}})
	}
	return out
}

type bucket struct {
	at    time.Time
	value float64
	n     int64
}

// bucketize 把原始样本聚合为 1 分钟均值桶，按时间升序。
func bucketize(samples []metrics.Sample) []bucket {
	if len(samples) == 0 {
		return nil
	}
	ordered := append([]metrics.Sample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })

	out := make([]bucket, 0, len(ordered)/4+1)
	var cur *bucket
	for _, s := range ordered {
		bts := time.Unix(s.At.Unix()-s.At.Unix()%BucketSeconds, 0).UTC()
		if cur == nil || !cur.at.Equal(bts) {
			out = append(out, bucket{at: bts})
			cur = &out[len(out)-1]
		}
		cur.value += valueOf(s)
		cur.n++
	}
	for i := range out {
		out[i].value /= float64(out[i].n)
	}
	return out
}

func valueOf(s metrics.Sample) float64 {
	// FitDim 已把另一维置 0，这里直接求和即可区分 CPU/内存序列。
	return float64(s.Usage.CPUMilli) + float64(s.Usage.MemBytes)
}

func spanOf(buckets []bucket) time.Duration {
	if len(buckets) < 2 {
		return 0
	}
	return buckets[len(buckets)-1].at.Sub(buckets[0].at)
}

func lastTime(samples []metrics.Sample) time.Time {
	var latest time.Time
	for _, s := range samples {
		if s.At.After(latest) {
			latest = s.At
		}
	}
	return latest
}

func minDuration(a, b *time.Duration) *time.Duration {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *b < *a {
		return b
	}
	return a
}
