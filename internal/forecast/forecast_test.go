package forecast

import (
	"math"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func linSamples(start time.Time, n int, step time.Duration, cpuPerStep float64, memPerStep float64) []metrics.Sample {
	out := make([]metrics.Sample, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, metrics.Sample{
			At: start.Add(time.Duration(i) * step),
			Usage: schedcore.Resources{
				CPUMilli: int64(cpuPerStep * float64(i)),
				MemBytes: int64(memPerStep * float64(i)),
			},
		})
	}
	return out
}

func TestFitDimInsufficientData(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		samples []metrics.Sample
	}{
		{"空序列", nil},
		{"桶数不足", linSamples(base, 5, time.Minute, 10, 0)},
		{"跨度不足", linSamples(base, 20, 10*time.Second, 10, 0)},
	}
	for _, tc := range cases {
		fit := FitDim(tc.samples)
		if fit.Prediction != nil {
			t.Fatalf("%s: 不应输出预测值", tc.name)
		}
		if ConfidenceOf(fit) != ConfidenceNone {
			t.Fatalf("%s: 置信应为 NONE", tc.name)
		}
	}
}

func TestFitDimFlatSeries(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	// 完全平稳：斜率≈0、R²=0（ssTot=0），不应给出 time_to_full。
	var samples []metrics.Sample
	for i := 0; i < 30; i++ {
		samples = append(samples, metrics.Sample{
			At:    base.Add(time.Duration(i) * time.Minute),
			Usage: schedcore.Resources{CPUMilli: 500},
		})
	}
	fit := FitDim(samples)
	if fit.Prediction == nil {
		t.Fatal("平稳序列应有预测值")
	}
	if math.Abs(*fit.Prediction-500) > 1 {
		t.Fatalf("平稳序列预测应≈500: %v", *fit.Prediction)
	}
	if math.Abs(fit.SlopePerMin) > 1e-9 {
		t.Fatalf("平稳序列斜率应为 0: %v", fit.SlopePerMin)
	}
	if TimeToFull(fit, 900) != nil {
		t.Fatal("斜率非正时不应计算耗尽时间")
	}
}

func TestFitDimLinearGrowth(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	// 每 1 分钟 +10 毫核，30 分钟：完美线性，R²=1，预测 30m 后 = 590+10*30 ≈ 890。
	samples := linSamples(base, 31, time.Minute, 10, 0)
	fit := FitDim(samples)
	if ConfidenceOf(fit) != ConfidenceHigh {
		t.Fatalf("完美线性应为 HIGH: %v", fit.R2)
	}
	if math.Abs(fit.SlopePerMin-10) > 1e-6 {
		t.Fatalf("斜率应为 10/分钟: %v", fit.SlopePerMin)
	}
	now := base.Add(30 * time.Minute)
	p := Predict(fit, 30*time.Minute, now)
	if p == nil || math.Abs(*p-600) > 1 {
		t.Fatalf("30m 预测应约 600: %v", *p)
	}

	// 从 300 起步：600 目标（90% of 1000 毫核）在 30 分钟后达到。
	grow := linSamples(base, 31, time.Minute, 10, 0)
	for i := range grow {
		grow[i].Usage.CPUMilli += 300
	}
	fit2 := FitDim(grow)
	eta := TimeToFull(fit2, 900)
	if eta == nil {
		t.Fatal("上升序列应给出耗尽时间")
	}
	// 300 + 10/min → 900 需要 60 分钟（从最后值 600 起再 30 分钟）。
	if math.Abs(eta.Minutes()-30) > 0.1 {
		t.Fatalf("预计 30 分钟达到 900: %v", eta)
	}
}

func TestFitDimNoiseDegradesConfidence(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	// 锯齿噪声叠加缓增趋势：R² 应落在 LOW/MEDIUM，不得是 HIGH。
	var samples []metrics.Sample
	for i := 0; i < 40; i++ {
		v := 10*float64(i) + 400*float64(i%2)
		samples = append(samples, metrics.Sample{
			At:    base.Add(time.Duration(i) * time.Minute),
			Usage: schedcore.Resources{CPUMilli: int64(v)},
		})
	}
	fit := FitDim(samples)
	if c := ConfidenceOf(fit); c == ConfidenceHigh || c == ConfidenceNone {
		t.Fatalf("强噪声应为 LOW/MEDIUM: %v (R²=%v)", c, fit.R2)
	}
}

func TestNodeForecastForTimeToFull(t *testing.T) {
	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	now := base.Add(30 * time.Minute)

	// CPU 30 分钟从 590 → 890（每分钟 +10），alloc 1000 毫核；
	// 内存恒为 4GiB（平稳，不产生耗尽时间）。
	var series []metrics.Sample
	for i := 0; i <= 30; i++ {
		series = append(series, metrics.Sample{
			At:    base.Add(time.Duration(i) * time.Minute),
			Usage: schedcore.Resources{CPUMilli: 590 + 10*int64(i), MemBytes: 4 << 30},
		})
	}
	nf := NodeForecastFor("ka-w1", series, schedcore.Resources{CPUMilli: 1000, MemBytes: 8 << 30}, now)
	if nf.CPU.SlopePerMin <= 0 {
		t.Fatalf("CPU 斜率应为正: %v", nf.CPU.SlopePerMin)
	}
	if nf.TimeToFull90 == nil {
		t.Fatal("CPU 占比>0.6 且上升，应给出 90% 耗尽时间")
	}
	// 900-890=10 毫核 ÷10/分钟 = 1 分钟。
	if math.Abs(nf.TimeToFull90.Minutes()-1) > 0.2 {
		t.Fatalf("90%% 耗尽应约 1 分钟: %v", *nf.TimeToFull90)
	}
	if nf.TimeToFull100 == nil || math.Abs(nf.TimeToFull100.Minutes()-11) > 0.2 {
		t.Fatalf("100%% 耗尽应约 11 分钟: %v", nf.TimeToFull100)
	}

	// 占比过低（起点 50）：即使上升，也不给 time_to_full。
	var low []metrics.Sample
	for i := 0; i <= 30; i++ {
		low = append(low, metrics.Sample{
			At:    base.Add(time.Duration(i) * time.Minute),
			Usage: schedcore.Resources{CPUMilli: 50 + 5*int64(i)},
		})
	}
	nfLow := NodeForecastFor("ka-w1", low, schedcore.Resources{CPUMilli: 1000}, now)
	if nfLow.TimeToFull90 != nil {
		t.Fatalf("占比 <0.6 不应给出耗尽时间: %v", nfLow.TimeToFull90)
	}

	// alloc=0：跳过占比与耗尽计算。
	nfZero := NodeForecastFor("ka-w1", series, schedcore.Resources{}, now)
	if nfZero.UsageRatio != 0 || nfZero.TimeToFull90 != nil {
		t.Fatal("allocatable 为 0 时不应计算占比/耗尽")
	}
}

func TestHorizonWhitelist(t *testing.T) {
	for _, h := range []string{"15m", "30m", "1h", "3h"} {
		if _, ok := Horizons[h]; !ok {
			t.Fatalf("标准视界 %s 缺失", h)
		}
	}
}
