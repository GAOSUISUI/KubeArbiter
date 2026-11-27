package aiops

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestDetectForecastExhaust(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   RuleInput
		want bool
	}{
		{
			name: "15 分钟内耗尽且拟合达标",
			in: RuleInput{
				Now: now,
				Forecasts: []NodeForecastSummary{{
					Name: "ka-w1", TimeToFull90: ptrDuration(15 * time.Minute), R2: 0.85, UsageRatio: 0.85,
				}},
			},
			want: true,
		},
		{
			name: "超出预警窗口",
			in: RuleInput{
				Now: now,
				Forecasts: []NodeForecastSummary{{
					Name: "ka-w1", TimeToFull90: ptrDuration(45 * time.Minute), R2: 0.85, UsageRatio: 0.85,
				}},
			},
			want: false,
		},
		{
			name: "R2 过低",
			in: RuleInput{
				Now: now,
				Forecasts: []NodeForecastSummary{{
					Name: "ka-w1", TimeToFull90: ptrDuration(15 * time.Minute), R2: 0.3, UsageRatio: 0.85,
				}},
			},
			want: false,
		},
		{
			name: "当前占比过低",
			in: RuleInput{
				Now: now,
				Forecasts: []NodeForecastSummary{{
					Name: "ka-w1", TimeToFull90: ptrDuration(15 * time.Minute), R2: 0.85, UsageRatio: 0.4,
				}},
			},
			want: false,
		},
		{
			name: "已在 90% 以上（eta=0）由高负载规则负责",
			in: RuleInput{
				Now: now,
				Forecasts: []NodeForecastSummary{{
					Name: "ka-w1", TimeToFull90: ptrDuration(0), R2: 0.85, UsageRatio: 0.95,
				}},
			},
			want: false,
		},
		{
			name: "无预测数据",
			in:   RuleInput{Now: now},
			want: false,
		},
	}

	for _, tc := range cases {
		findings := Detect(tc.in)
		found := false
		for _, f := range findings {
			if f.Type == IncidentNodeForecastExhaust {
				found = true
				if f.Severity != SeverityWarn {
					t.Errorf("%s: 预测事件应为 WARN", tc.name)
				}
				if f.Namespace != NamespaceNode || f.Target != "ka-w1" {
					t.Errorf("%s: 作用域错误: %s/%s", tc.name, f.Namespace, f.Target)
				}
			}
		}
		if found != tc.want {
			t.Errorf("%s: 期望 %v，实际 %v", tc.name, tc.want, found)
		}
	}
}

func TestHealthForecastExhaustDedicatedComponent(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	in := HealthInput{
		RuleInput: RuleInput{
			Now:       now,
			MetricsAt: now.Add(-time.Minute),
			Nodes: []capacity.NodeDiag{{
				Name:        "n1",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
				Usage:       schedcore.Resources{CPUMilli: 1000, MemBytes: 2 << 30},
				UsageKnown:  true,
			}},
		},
		Findings: []Finding{{
			Type: IncidentNodeForecastExhaust, Namespace: NamespaceNode, Target: "n1", Severity: SeverityWarn,
		}},
	}
	report := ComputeHealth(in)
	if len(report.Nodes) != 1 || report.Nodes[0].Score == nil {
		t.Fatal("应有节点健康分")
	}
	var hasComponent bool
	for _, c := range report.Nodes[0].Components {
		if c.Code == "FORECAST_EXHAUST" {
			hasComponent = true
			if c.Delta != -5 {
				t.Errorf("FORECAST_EXHAUST 扣分应为 -5: %v", c.Delta)
			}
		}
		if c.Code == "WARN_INCIDENTS" {
			t.Error("预测事件不应计入通用 WARN 扣分")
		}
	}
	if !hasComponent {
		t.Fatal("缺少 FORECAST_EXHAUST 组件")
	}
	if *report.Nodes[0].Score != 95 {
		t.Errorf("健康分应为 95: %v", *report.Nodes[0].Score)
	}
}

func ptrDuration(d time.Duration) *time.Duration { return &d }
