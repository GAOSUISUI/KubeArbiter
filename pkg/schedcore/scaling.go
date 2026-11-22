package schedcore

import (
	"errors"
	"math"
	"sort"
)

var (
	ErrInsufficientPoints = errors.New("容量曲线测量点少于 4 个")
	ErrInvalidCurve       = errors.New("容量曲线非法：并发必须递增且 P99 不得显著下降")
	ErrNoSamples          = errors.New("没有可用的验证样本")
	ErrLengthMismatch     = errors.New("预测值与实测值长度不一致")
)

type CurvePoint struct {
	Concurrency int     `json:"concurrency"`
	P99Ms       float64 `json:"p99_ms"`
}

type Confidence string

const (
	ConfidenceHigh     Confidence = "HIGH"
	ConfidenceMedium   Confidence = "MEDIUM"
	ConfidenceLow      Confidence = "LOW"
	ConfidenceUnusable Confidence = "UNUSABLE"
)

const (
	minCurvePoints     = 4
	monotonicTolerance = 0.95
	safetyFactor       = 1.2
)

// InterpolateConcurrency 在 log2(并发) 空间线性插值，求满足 L(c) <= targetP99 的最大并发。
// 返回 extrapolated=true 表示目标超出实测区间上沿，结论只能作为下界参考。
func InterpolateConcurrency(curve []CurvePoint, targetP99 float64) (float64, bool, error) {
	if len(curve) < minCurvePoints {
		return 0, false, ErrInsufficientPoints
	}

	pts := append([]CurvePoint(nil), curve...)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Concurrency < pts[j].Concurrency })

	for i := range pts {
		if pts[i].Concurrency <= 0 || pts[i].P99Ms <= 0 {
			return 0, false, ErrInvalidCurve
		}
		if i > 0 {
			if pts[i].Concurrency <= pts[i-1].Concurrency {
				return 0, false, ErrInvalidCurve
			}
			// 相邻档位 P99 下降超过 5% 视为测量不可信。
			if pts[i].P99Ms < pts[i-1].P99Ms*monotonicTolerance {
				return 0, false, ErrInvalidCurve
			}
		}
	}

	if targetP99 <= pts[0].P99Ms {
		return float64(pts[0].Concurrency), false, nil
	}

	last := pts[len(pts)-1]
	if targetP99 >= last.P99Ms {
		return float64(last.Concurrency), true, nil
	}

	for i := 0; i+1 < len(pts); i++ {
		if targetP99 >= pts[i].P99Ms && targetP99 < pts[i+1].P99Ms {
			logCur := math.Log2(float64(pts[i].Concurrency))
			logNext := math.Log2(float64(pts[i+1].Concurrency))
			frac := (targetP99 - pts[i].P99Ms) / (pts[i+1].P99Ms - pts[i].P99Ms)
			return math.Pow(2, logCur+frac*(logNext-logCur)), false, nil
		}
	}

	return float64(last.Concurrency), true, nil
}

// SuggestReplicas 返回建议副本数：ceil(1.2 × 高峰并发 / 单副本可承受并发)，下限 1。
func SuggestReplicas(cStar float64, peakConcurrency int) int {
	if cStar <= 0 || peakConcurrency <= 0 {
		return 1
	}
	n := int(math.Ceil(safetyFactor * float64(peakConcurrency) / cStar))
	if n < 1 {
		return 1
	}
	return n
}

// MAPE 返回平均绝对百分比误差（百分数）。actual 为 0 的样本被排除。
func MAPE(predicted, actual []float64) (float64, error) {
	if len(predicted) != len(actual) {
		return 0, ErrLengthMismatch
	}
	var sum float64
	var n int
	for i := range predicted {
		a := actual[i]
		if a == 0 {
			continue
		}
		sum += math.Abs(predicted[i]-a) / math.Abs(a)
		n++
	}
	if n == 0 {
		return 0, ErrNoSamples
	}
	return 100 * sum / float64(n), nil
}

// ClassifyConfidence 把误差与是否外推映射为置信等级。
// 外推场景最高只能给 LOW；误差超过 50% 一律 UNUSABLE，此时不得给出副本建议。
func ClassifyConfidence(mape float64, extrapolated bool) Confidence {
	switch {
	case mape > 50:
		return ConfidenceUnusable
	case mape > 25:
		return ConfidenceLow
	case extrapolated:
		return ConfidenceLow
	case mape > 15:
		return ConfidenceMedium
	default:
		return ConfidenceHigh
	}
}
