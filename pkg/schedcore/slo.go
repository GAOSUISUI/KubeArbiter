package schedcore

// BurnWindow 描述一个多窗口燃尽率评估的输入，长窗与短窗必须同时超阈值才算触发。
type BurnWindow struct {
	LongWindow  string
	ShortWindow string
	Threshold   float64
	LongTotal   int64
	LongBad     int64
	ShortTotal  int64
	ShortBad    int64
	// 数据完整度 = 有数据的分钟数 / 应有分钟数，低于 0.95 时该窗口结论不可用。
	LongCompleteness  float64
	ShortCompleteness float64
}

type BurnEvaluation struct {
	Firing        bool
	LongBurnRate  float64
	ShortBurnRate float64
	Degraded      bool
	Reason        string
}

const (
	minCompleteness    = 0.95
	minBudgetRemaining = 0.20
)

// BurnRate 返回窗口燃尽率 = 窗口错误率 / (1 − SLO)。
func BurnRate(target float64, total, bad int64) float64 {
	if total <= 0 || target <= 0 || target >= 1 {
		return 0
	}
	errRate := float64(bad) / float64(total)
	return round4(errRate / (1 - target))
}

// BudgetRemaining 返回剩余预算比例 = 1 − 已消耗 / 预算总额。
// 允许返回负值，表示已透支；调用方不得截断为 0。
func BudgetRemaining(target float64, total, bad int64) float64 {
	if total <= 0 || target <= 0 || target >= 1 {
		return 1
	}
	budget := 1 - target
	consumed := float64(bad) / float64(total)
	return round4(1 - consumed/budget)
}

// EvaluateBurn 评估单个多窗口组合。
// 数据不完整时强制不触发，避免用残缺数据驱动自动回滚。
func EvaluateBurn(target float64, w BurnWindow) BurnEvaluation {
	ev := BurnEvaluation{
		LongBurnRate:  BurnRate(target, w.LongTotal, w.LongBad),
		ShortBurnRate: BurnRate(target, w.ShortTotal, w.ShortBad),
	}
	if w.LongCompleteness < minCompleteness || w.ShortCompleteness < minCompleteness {
		ev.Degraded = true
		ev.Reason = string(WarnSLODataDegraded)
		return ev
	}
	if w.LongTotal <= 0 || w.ShortTotal <= 0 {
		ev.Degraded = true
		ev.Reason = "NO_SAMPLES"
		return ev
	}
	ev.Firing = ev.LongBurnRate > w.Threshold && ev.ShortBurnRate > w.Threshold
	return ev
}

// PublishingAllowed 判定是否允许把灰度推进到全量。
// 规则：预算余量 < 20% 或数据降级时禁止。
func PublishingAllowed(target float64, total, bad int64, degraded bool) bool {
	if degraded {
		return false
	}
	return BudgetRemaining(target, total, bad) >= minBudgetRemaining
}
