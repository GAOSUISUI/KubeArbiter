package schedcore

import (
	"errors"
	"math"
	"testing"
)

func sampleCurve() []CurvePoint {
	return []CurvePoint{
		{Concurrency: 1, P99Ms: 100},
		{Concurrency: 2, P99Ms: 120},
		{Concurrency: 4, P99Ms: 160},
		{Concurrency: 8, P99Ms: 220},
		{Concurrency: 16, P99Ms: 300},
	}
}

// log2(2)=1，log2(4)=2，P99 从 120 走到 160 的一半 → 2^1.5。
func TestInterpolateConcurrencyInsideRange(t *testing.T) {
	got, extrapolated, err := InterpolateConcurrency(sampleCurve(), 140)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	if extrapolated {
		t.Errorf("区间内插不应标记外推")
	}
	approx(t, got, math.Pow(2, 1.5), 1e-6, "插值并发")
}

func TestInterpolateConcurrencyBoundaries(t *testing.T) {
	cases := []struct {
		name         string
		target       float64
		want         float64
		extrapolated bool
	}{
		{"恰好命中测点", 120, 2, false},
		{"低于首点返回首点", 50, 1, false},
		{"等于首点", 100, 1, false},
		{"高于末点返回末点并标记外推", 400, 16, true},
		{"等于末点", 300, 16, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, extra, err := InterpolateConcurrency(sampleCurve(), c.target)
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			approx(t, got, c.want, 1e-9, "并发")
			if extra != c.extrapolated {
				t.Errorf("extrapolated = %v, want %v", extra, c.extrapolated)
			}
		})
	}
}

func TestInterpolateConcurrencyInvalidInput(t *testing.T) {
	cases := []struct {
		name  string
		curve []CurvePoint
		want  error
	}{
		{
			name:  "测量点不足 4 个",
			curve: sampleCurve()[:3],
			want:  ErrInsufficientPoints,
		},
		{
			name: "并发重复",
			curve: []CurvePoint{
				{1, 100}, {2, 120}, {2, 130}, {4, 160},
			},
			want: ErrInvalidCurve,
		},
		{
			name: "P99 显著下降",
			curve: []CurvePoint{
				{1, 100}, {2, 90}, {4, 160}, {8, 220},
			},
			want: ErrInvalidCurve,
		},
		{
			name: "并发非正",
			curve: []CurvePoint{
				{0, 100}, {2, 120}, {4, 160}, {8, 220},
			},
			want: ErrInvalidCurve,
		},
		{
			name: "P99 非正",
			curve: []CurvePoint{
				{1, 100}, {2, 0}, {4, 160}, {8, 220},
			},
			want: ErrInvalidCurve,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := InterpolateConcurrency(c.curve, 140)
			if !errors.Is(err, c.want) {
				t.Errorf("错误 = %v, want %v", err, c.want)
			}
		})
	}
}

// 输入乱序时应先排序；且不得修改调用方传入的切片。
func TestInterpolateConcurrencySortsCopy(t *testing.T) {
	curve := []CurvePoint{
		{Concurrency: 16, P99Ms: 300},
		{Concurrency: 1, P99Ms: 100},
		{Concurrency: 8, P99Ms: 220},
		{Concurrency: 2, P99Ms: 120},
		{Concurrency: 4, P99Ms: 160},
	}
	got, _, err := InterpolateConcurrency(curve, 140)
	if err != nil {
		t.Fatalf("意外错误: %v", err)
	}
	approx(t, got, math.Pow(2, 1.5), 1e-6, "乱序输入的插值并发")
	if curve[0].Concurrency != 16 {
		t.Errorf("调用方切片被就地修改: %+v", curve)
	}
}

func TestSuggestReplicas(t *testing.T) {
	cases := []struct {
		name  string
		cStar float64
		peak  int
		want  int
	}{
		// 1.2 × 10 / 2.828427 = 4.2426 → 向上取整 5。
		{"常规缩放", math.Pow(2, 1.5), 10, 5},
		{"单副本可承担全部流量时下限 1", 100, 10, 1},
		{"c* 非法时下限 1", 0, 10, 1},
		{"峰值并发为 0 时下限 1", 4, 0, 1},
		{"安全系数生效", 10, 10, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SuggestReplicas(c.cStar, c.peak); got != c.want {
				t.Errorf("SuggestReplicas(%v,%d) = %d, want %d", c.cStar, c.peak, got, c.want)
			}
		})
	}
}

func TestMAPE(t *testing.T) {
	cases := []struct {
		name      string
		predicted []float64
		actual    []float64
		want      float64
		wantErr   error
	}{
		{"完全准确", []float64{100, 200}, []float64{100, 200}, 0, nil},
		{"平均绝对百分比误差 50%", []float64{100, 200}, []float64{100, 100}, 50, nil},
		{"排除 actual 为 0 的样本", []float64{999, 110}, []float64{0, 100}, 10, nil},
		{"长度不一致", []float64{1}, []float64{1, 2}, 0, ErrLengthMismatch},
		{"全部样本被排除", []float64{1, 2}, []float64{0, 0}, 0, ErrNoSamples},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := MAPE(c.predicted, c.actual)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("错误 = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			approx(t, got, c.want, 1e-6, "MAPE")
		})
	}
}

// 误差超过 50% 必须落到 UNUSABLE，此时不允许对外给出副本数字。
func TestClassifyConfidence(t *testing.T) {
	cases := []struct {
		name         string
		mape         float64
		extrapolated bool
		want         Confidence
	}{
		{"误差 10% 且内插", 10, false, ConfidenceHigh},
		{"误差恰为 15% 仍为 HIGH", 15, false, ConfidenceHigh},
		{"误差 16%", 16, false, ConfidenceMedium},
		{"误差 25% 仍为 MEDIUM", 25, false, ConfidenceMedium},
		{"误差 26%", 26, false, ConfidenceLow},
		{"误差 10% 但外推只能 LOW", 10, true, ConfidenceLow},
		{"误差 50% 仍为 LOW", 50, false, ConfidenceLow},
		{"误差超过 50% 不可用", 50.1, false, ConfidenceUnusable},
		{"误差 60% 不可用", 60, false, ConfidenceUnusable},
		{"外推且误差过大仍不可用", 60, true, ConfidenceUnusable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyConfidence(c.mape, c.extrapolated); got != c.want {
				t.Errorf("ClassifyConfidence(%v,%v) = %s, want %s",
					c.mape, c.extrapolated, got, c.want)
			}
		})
	}
}
