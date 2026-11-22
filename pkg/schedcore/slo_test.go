package schedcore

import "testing"

func TestBurnRate(t *testing.T) {
	cases := []struct {
		name   string
		target float64
		total  int64
		bad    int64
		want   float64
	}{
		// 错误率 1%，预算 1%（SLO 99%）→ 燃尽率 1。
		{"用尽预算恰好燃尽率 1", 0.99, 10000, 100, 1},
		{"错误率是预算两倍", 0.995, 10000, 100, 2},
		{"无错误", 0.99, 10000, 0, 0},
		{"无样本返回 0", 0.99, 0, 0, 0},
		{"target 为 1 时不可定义", 1, 1000, 10, 0},
		{"target 为 0 时不可定义", 0, 1000, 10, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			approx(t, BurnRate(c.target, c.total, c.bad), c.want, 1e-4, "燃尽率")
		})
	}
}

func TestBudgetRemaining(t *testing.T) {
	cases := []struct {
		name   string
		target float64
		total  int64
		bad    int64
		want   float64
	}{
		{"未消耗", 0.99, 10000, 0, 1},
		{"消耗一半", 0.99, 10000, 50, 0.5},
		{"刚好耗尽", 0.99, 10000, 100, 0},
		// 透支必须返回负值，调用方不得截断为 0。
		{"已透支", 0.99, 10000, 200, -1},
		{"无样本视为未消耗", 0.99, 0, 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			approx(t, BudgetRemaining(c.target, c.total, c.bad), c.want, 1e-4, "剩余预算")
		})
	}
}

// 多窗口燃尽必须长窗与短窗同时超阈值才触发。
func TestEvaluateBurnRequiresBothWindows(t *testing.T) {
	cases := []struct {
		name     string
		window   BurnWindow
		wantFire bool
	}{
		{
			name: "长短窗都超过 14.4",
			window: BurnWindow{
				// 错误率 15%，SLO 99% → 燃尽率 15 > 14.4。
				LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4,
				LongTotal: 10000, LongBad: 1500,
				ShortTotal: 1000, ShortBad: 150,
				LongCompleteness: 1, ShortCompleteness: 1,
			},
			wantFire: true,
		},
		{
			name: "只有长窗超阈值（短窗已恢复）",
			window: BurnWindow{
				LongWindow: "1h", ShortWindow: "5m", Threshold: 14.4,
				LongTotal: 10000, LongBad: 1500,
				ShortTotal: 1000, ShortBad: 1,
				LongCompleteness: 1, ShortCompleteness: 1,
			},
			wantFire: false,
		},
		{
			name: "刚好等于阈值不触发",
			window: BurnWindow{
				// 错误率 1%，SLO 99% → 燃尽率恰为 1，不满足严格大于。
				LongWindow: "1h", ShortWindow: "5m", Threshold: 1,
				LongTotal: 10000, LongBad: 100,
				ShortTotal: 1000, ShortBad: 10,
				LongCompleteness: 1, ShortCompleteness: 1,
			},
			wantFire: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := EvaluateBurn(0.99, c.window)
			if ev.Firing != c.wantFire {
				t.Errorf("Firing = %v, want %v (burn %v/%v)",
					ev.Firing, c.wantFire, ev.LongBurnRate, ev.ShortBurnRate)
			}
			if ev.Degraded {
				t.Errorf("完整数据不应标记降级")
			}
		})
	}
}

// 数据完整度不足时禁止触发，避免用残缺数据驱动自动回滚。
func TestEvaluateBurnDegradedData(t *testing.T) {
	cases := []struct {
		name   string
		window BurnWindow
		reason string
	}{
		{
			name: "长窗完整度 0.94",
			window: BurnWindow{
				Threshold: 14.4,
				LongTotal: 10000, LongBad: 5000,
				ShortTotal: 1000, ShortBad: 500,
				LongCompleteness: 0.94, ShortCompleteness: 1,
			},
			reason: string(WarnSLODataDegraded),
		},
		{
			name: "短窗完整度 0.90",
			window: BurnWindow{
				Threshold: 14.4,
				LongTotal: 10000, LongBad: 5000,
				ShortTotal: 1000, ShortBad: 500,
				LongCompleteness: 1, ShortCompleteness: 0.90,
			},
			reason: string(WarnSLODataDegraded),
		},
		{
			name: "完整度达标但无样本",
			window: BurnWindow{
				Threshold: 14.4,
				LongTotal: 0, LongBad: 0,
				ShortTotal: 1000, ShortBad: 10,
				LongCompleteness: 1, ShortCompleteness: 1,
			},
			reason: "NO_SAMPLES",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := EvaluateBurn(0.99, c.window)
			if !ev.Degraded {
				t.Fatalf("应标记 Degraded")
			}
			if ev.Firing {
				t.Errorf("降级状态不得触发")
			}
			if ev.Reason != c.reason {
				t.Errorf("Reason = %q, want %q", ev.Reason, c.reason)
			}
		})
	}
}

// 预算余量低于 20% 或数据降级时，禁止推进到全量。
func TestPublishingAllowed(t *testing.T) {
	cases := []struct {
		name     string
		target   float64
		total    int64
		bad      int64
		degraded bool
		want     bool
	}{
		// 99% SLO，预算 1%，消耗 0.5% → 余量 50%。
		{"余量 50% 允许", 0.99, 10000, 50, false, true},
		// 消耗 0.9% → 余量 10%。
		{"余量 10% 禁止", 0.99, 10000, 90, false, false},
		// 恰好 20% 属于允许边界。
		{"余量 20% 允许", 0.99, 10000, 80, false, true},
		{"已透支禁止", 0.99, 10000, 200, false, false},
		{"数据降级禁止", 0.99, 10000, 0, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PublishingAllowed(c.target, c.total, c.bad, c.degraded); got != c.want {
				t.Errorf("PublishingAllowed = %v, want %v", got, c.want)
			}
		})
	}
}
