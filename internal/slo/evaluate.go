package slo

import (
	"math"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// MinCompleteness 是窗口结论可用的最低数据完整度；低于该值一律视为降级。
const MinCompleteness = 0.95

// MultiWindow 是一组"长窗 + 短窗"的燃尽率判定规则，对应 SRE 多窗口告警表。
// 只有长窗与短窗同时超过阈值才触发，避免短时抖动驱动自动动作。
type MultiWindow struct {
	Long      time.Duration
	Short     time.Duration
	Threshold float64
	Action    Action
}

// StandardWindows 与说明书 5.4 的三行窗口组合逐项对应。
var StandardWindows = []MultiWindow{
	{Long: time.Hour, Short: 5 * time.Minute, Threshold: 14.4, Action: ActionRollback},
	{Long: 6 * time.Hour, Short: 30 * time.Minute, Threshold: 6, Action: ActionAlert},
	{Long: 3 * 24 * time.Hour, Short: 6 * time.Hour, Threshold: 1, Action: ActionTicket},
}

// LongestWindow 返回全部判定规则中最长的窗口，供调用方决定读取多久的预聚合数据。
func LongestWindow() time.Duration {
	var longest time.Duration
	for _, w := range StandardWindows {
		if w.Long > longest {
			longest = w.Long
		}
	}
	return longest
}

// WindowResult 是一个窗口组合的评估结果。BurnRate 取长窗燃尽率（契约只有单个值），
// Completeness 取长窗与短窗中的较小值。
type WindowResult struct {
	LongWindow   string
	ShortWindow  string
	Threshold    float64
	BurnRate     float64
	Firing       bool
	Completeness float64
	Action       Action
}

// Budget 是一个预算窗口内的错误预算状态。
type Budget struct {
	Window          time.Duration
	TotalRequests   int64
	BadRequests     int64
	Current         float64 // 窗口内 SLI 达成率
	BudgetTotal     float64 // 1 - target，即允许的失败比例
	BudgetConsumed  float64 // 窗口内实际失败比例
	BudgetRemaining float64 // 允许为负，表示已透支
	Completeness    float64
	Degraded        bool
}

// Report 是单个 SLO 定义在给定时刻的完整评估结果。
type Report struct {
	Definition Definition
	Budget     Budget
	Windows    []WindowResult
}

// Evaluate 计算预算窗口与全部多窗口燃尽率。
func Evaluate(def Definition, buckets []Bucket, now time.Time) Report {
	return Report{
		Definition: def,
		Budget:     ComputeBudget(def, buckets, now, def.Window),
		Windows:    EvaluateWindows(def, buckets, now),
	}
}

// ComputeBudget 计算指定窗口内的错误预算。window 允许覆盖定义里的默认窗口。
func ComputeBudget(def Definition, buckets []Bucket, now time.Time, window time.Duration) Budget {
	b := Budget{
		Window:       window,
		BudgetTotal:  round4(1 - def.Target),
		Completeness: completeness(buckets, now, window),
	}
	b.Degraded = b.Completeness < MinCompleteness

	var total, bad int64
	for _, bucket := range buckets {
		if !inWindow(bucket.Start, now, window) {
			continue
		}
		total += bucket.Total
		bad += bucket.Bad(def.SLIType)
	}
	b.TotalRequests = total
	b.BadRequests = bad

	if total > 0 {
		b.Current = round4(1 - float64(bad)/float64(total))
		b.BudgetConsumed = round4(float64(bad) / float64(total))
		b.BudgetRemaining = schedcore.BudgetRemaining(def.Target, total, bad)
	} else {
		// 无样本不等于健康，但预算数值仍保持中性；Degraded 已置位，发布会被阻断。
		b.BudgetRemaining = 1
	}
	return b
}

// EvaluateWindows 逐个计算标准窗口组合；数据不完整的窗口不触发。
func EvaluateWindows(def Definition, buckets []Bucket, now time.Time) []WindowResult {
	out := make([]WindowResult, 0, len(StandardWindows))
	for _, w := range StandardWindows {
		longTotal, longBad := sumWindow(def.SLIType, buckets, now, w.Long)
		shortTotal, shortBad := sumWindow(def.SLIType, buckets, now, w.Short)
		longCompleteness := completeness(buckets, now, w.Long)
		shortCompleteness := completeness(buckets, now, w.Short)

		ev := schedcore.EvaluateBurn(def.Target, schedcore.BurnWindow{
			LongWindow:        FormatWindow(w.Long),
			ShortWindow:       FormatWindow(w.Short),
			Threshold:         w.Threshold,
			LongTotal:         longTotal,
			LongBad:           longBad,
			ShortTotal:        shortTotal,
			ShortBad:          shortBad,
			LongCompleteness:  longCompleteness,
			ShortCompleteness: shortCompleteness,
		})
		out = append(out, WindowResult{
			LongWindow:   FormatWindow(w.Long),
			ShortWindow:  FormatWindow(w.Short),
			Threshold:    w.Threshold,
			BurnRate:     ev.LongBurnRate,
			Firing:       ev.Firing,
			Completeness: math.Min(longCompleteness, shortCompleteness),
			Action:       w.Action,
		})
	}
	return out
}

func sumWindow(sli SLIType, buckets []Bucket, now time.Time, window time.Duration) (total, bad int64) {
	for _, bucket := range buckets {
		if !inWindow(bucket.Start, now, window) {
			continue
		}
		total += bucket.Total
		bad += bucket.Bad(sli)
	}
	return total, bad
}

// completeness = 窗口内有数据的分钟数 / 应有分钟数，最大为 1。
func completeness(buckets []Bucket, now time.Time, window time.Duration) float64 {
	expected := window.Minutes()
	if expected <= 0 {
		return 0
	}
	seen := make(map[int64]struct{}, len(buckets))
	for _, bucket := range buckets {
		if !inWindow(bucket.Start, now, window) {
			continue
		}
		seen[bucket.Start.Unix()/60] = struct{}{}
	}
	ratio := float64(len(seen)) / expected
	if ratio > 1 {
		ratio = 1
	}
	return round4(ratio)
}

// inWindow 以整分钟为边界取窗口：预聚合桶按分钟落盘，
// 若直接用 now-窗口 截断会漏掉窗口首分钟，把正常的短窗误判为数据缺失。
func inWindow(start, now time.Time, window time.Duration) bool {
	lower := now.Add(-window).Truncate(time.Minute)
	upper := now.Truncate(time.Minute)
	return !start.Before(lower) && start.Before(upper)
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}
