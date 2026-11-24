package slo

import (
	"math"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

func makeBuckets(now time.Time, minutes int, total, errors int64) []Bucket {
	out := make([]Bucket, 0, minutes)
	for i := minutes; i >= 1; i-- {
		out = append(out, Bucket{
			Start:  now.Add(-time.Duration(i) * time.Minute),
			Total:  total,
			Errors: errors,
		})
	}
	return out
}

func approx(t *testing.T, got, want float64, tol float64, name string) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", name, got, want, tol)
	}
}

func TestComputeBudgetAvailability(t *testing.T) {
	def := Definition{Service: "checkout", SLIType: SLIAvailability, Target: 0.99, Window: time.Hour}
	budget := ComputeBudget(def, makeBuckets(testNow, 60, 100, 50), testNow, time.Hour)

	if budget.TotalRequests != 6000 || budget.BadRequests != 3000 {
		t.Fatalf("样本统计 = %d/%d, want 3000/6000", budget.BadRequests, budget.TotalRequests)
	}
	approx(t, budget.Current, 0.5, 1e-9, "SLI 达成率")
	approx(t, budget.BudgetConsumed, 0.5, 1e-9, "预算消耗")
	approx(t, budget.BudgetRemaining, -49, 1e-6, "剩余预算")
	approx(t, budget.Completeness, 1, 1e-9, "完整度")
	if budget.Degraded {
		t.Error("完整数据不应标记降级")
	}
}

func TestComputeBudgetLatency(t *testing.T) {
	def := Definition{Service: "search", SLIType: SLILatency, Target: 0.95, Window: time.Hour}
	buckets := makeBuckets(testNow, 60, 100, 0)
	for i := range buckets {
		buckets[i].LatencyOK = 96
	}
	budget := ComputeBudget(def, buckets, testNow, time.Hour)

	if budget.BadRequests != 240 {
		t.Fatalf("延迟未达标数 = %d, want 240", budget.BadRequests)
	}
	approx(t, budget.Current, 0.96, 1e-9, "延迟达标率")
	approx(t, budget.BudgetRemaining, 0.2, 1e-9, "剩余预算")
}

func TestComputeBudgetDegradedWhenIncomplete(t *testing.T) {
	def := Definition{Service: "checkout", SLIType: SLIAvailability, Target: 0.99, Window: time.Hour}
	budget := ComputeBudget(def, makeBuckets(testNow, 30, 100, 0), testNow, time.Hour)

	approx(t, budget.Completeness, 0.5, 1e-9, "完整度")
	if !budget.Degraded {
		t.Fatal("完整度 0.5 应标记降级")
	}
}

func TestEvaluateWindowsRequireBothWindows(t *testing.T) {
	def := Definition{Service: "checkout", SLIType: SLIAvailability, Target: 0.99, Window: time.Hour}
	// 错误率 15%，预算 1% → 燃尽率 15，长窗与短窗同时超过 14.4。
	windows := EvaluateWindows(def, makeBuckets(testNow, 60, 100, 15), testNow)

	if len(windows) != len(StandardWindows) {
		t.Fatalf("窗口数 = %d, want %d", len(windows), len(StandardWindows))
	}
	first := windows[0]
	if first.LongWindow != "1h" || first.ShortWindow != "5m" || first.Threshold != 14.4 {
		t.Fatalf("首个窗口 = %+v", first)
	}
	approx(t, first.BurnRate, 15, 1e-6, "长窗燃尽率")
	if !first.Firing {
		t.Error("长短窗均超阈值应触发")
	}
	if first.Action != ActionRollback {
		t.Errorf("14.4 窗口的动作 = %s, want %s", first.Action, ActionRollback)
	}

	// 只有长窗异常：最后 5 分钟恢复，短窗燃尽率回落到 0。
	recovered := makeBuckets(testNow, 60, 100, 15)
	for i := len(recovered) - 5; i < len(recovered); i++ {
		recovered[i].Errors = 0
	}
	if EvaluateWindows(def, recovered, testNow)[0].Firing {
		t.Error("短窗已恢复时不应触发")
	}
}

// 6h 窗口触发但预算仍充足：动作应为告警，而不是阻断。
func TestDecideAlertKeepsPublishingAllowed(t *testing.T) {
	def := Definition{Service: "checkout", SLIType: SLIAvailability, Target: 0.99, Window: 30 * 24 * time.Hour}
	buckets := makeBuckets(testNow, 30*24*60, 100, 0)
	for i := len(buckets) - 6*60; i < len(buckets); i++ {
		buckets[i].Errors = 7
	}
	report := Evaluate(def, buckets, testNow)
	decision := Decide(report, 0)

	if decision.Action != ActionAlert {
		t.Fatalf("动作 = %s, want %s（原因 %v）", decision.Action, ActionAlert, decision.Reasons)
	}
	if !decision.PublishingAllowed {
		t.Error("预算充足时告警不应阻断发布")
	}
}

func TestDecide(t *testing.T) {
	healthyDef := Definition{Service: "checkout", SLIType: SLIAvailability, Target: 0.99, Window: time.Hour}
	healthy := Evaluate(healthyDef, makeBuckets(testNow, 60, 100, 0), testNow)

	fastBurn := Evaluate(healthyDef, makeBuckets(testNow, 60, 100, 15), testNow)
	// 错误率 3%：未触发 14.4，但预算 1% 已透支。
	budgetLow := Evaluate(healthyDef, makeBuckets(testNow, 60, 100, 3), testNow)
	degraded := Evaluate(healthyDef, makeBuckets(testNow, 30, 100, 0), testNow)

	cases := []struct {
		name       string
		report     Report
		spent      float64
		wantAction Action
		wantAllow  bool
		wantReason string
	}{
		{"健康", healthy, 0, ActionNone, true, ""},
		{"快速燃尽回滚", fastBurn, 0, ActionRollback, false, ReasonFastBurn},
		{"发布超支回滚", healthy, 0.51, ActionRollback, false, ReasonReleaseSpend},
		{"预算不足阻断", budgetLow, 0, ActionBlockFullRollout, false, ReasonBudgetLow},
		{"数据降级阻断", degraded, 0, ActionBlockFullRollout, false, ReasonDataDegraded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.report, c.spent)
			if got.Action != c.wantAction {
				t.Fatalf("动作 = %s, want %s（原因 %v）", got.Action, c.wantAction, got.Reasons)
			}
			if got.PublishingAllowed != c.wantAllow {
				t.Errorf("PublishingAllowed = %v, want %v", got.PublishingAllowed, c.wantAllow)
			}
			if c.wantReason != "" {
				found := false
				for _, reason := range got.Reasons {
					if reason == c.wantReason {
						found = true
					}
				}
				if !found {
					t.Errorf("原因 = %v, 缺少 %s", got.Reasons, c.wantReason)
				}
			}
		})
	}
}

func TestParseAndFormatWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"1h":  time.Hour,
		"6h":  6 * time.Hour,
		"3d":  3 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	for raw, want := range cases {
		got, ok := ParseWindow(raw)
		if !ok || got != want {
			t.Errorf("ParseWindow(%q) = %v,%v want %v", raw, got, ok, want)
		}
		if FormatWindow(want) != raw {
			t.Errorf("FormatWindow(%v) = %q, want %q", want, FormatWindow(want), raw)
		}
	}
	if _, ok := ParseWindow("2h"); ok {
		t.Error("2h 不在契约允许的取值内")
	}
}

func TestDefinitionValidate(t *testing.T) {
	threshold := 300
	valid := Definition{Service: "checkout", SLIType: SLILatency, Target: 0.95, Window: time.Hour, LatencyThresholdMS: &threshold}
	if err := valid.Validate(); err != nil {
		t.Fatalf("合法定义被拒绝: %v", err)
	}

	bad := []Definition{
		{Service: "", SLIType: SLIAvailability, Target: 0.99, Window: time.Hour},
		{Service: "checkout", SLIType: "ERROR_RATE", Target: 0.99, Window: time.Hour},
		{Service: "checkout", SLIType: SLIAvailability, Target: 1, Window: time.Hour},
		{Service: "checkout", SLIType: SLIAvailability, Target: 0.99},
		{Service: "checkout", SLIType: SLILatency, Target: 0.95, Window: time.Hour},
	}
	for i, def := range bad {
		if err := def.Validate(); err == nil {
			t.Errorf("第 %d 个非法定义未被拒绝: %+v", i, def)
		}
	}
}
