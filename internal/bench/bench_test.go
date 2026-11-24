package bench

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func fakeClock() func() time.Time {
	now := time.Unix(0, 0)
	return func() time.Time {
		now = now.Add(time.Microsecond)
		return now
	}
}

// 同 seed 下，包括节点选择、资源指标和注入时钟产生的延迟在内，结果必须完全一致。
func TestSameSeedIsDeterministic(t *testing.T) {
	first, err := run(42, 80, fakeClock())
	if err != nil {
		t.Fatalf("第一次运行失败: %v", err)
	}
	second, err := run(42, 80, fakeClock())
	if err != nil {
		t.Fatalf("第二次运行失败: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同 seed 的完整结果不一致:\n第一次: %+v\n第二次: %+v", first, second)
	}
}

func TestMetricsAreInValidRanges(t *testing.T) {
	result, err := run(42, 40, fakeClock())
	if err != nil {
		t.Fatalf("运行基准失败: %v", err)
	}
	for _, scenario := range result.Scenarios {
		for _, variant := range scenario.Variants {
			m := variant.Metrics
			assertUnitInterval(t, scenario.Name, variant.Name, "Jain", m.Jain)
			assertUnitInterval(t, scenario.Name, variant.Name, "CPU 利用率", m.CPUUtilization)
			assertUnitInterval(t, scenario.Name, variant.Name, "内存利用率", m.MemUtilization)
			assertUnitInterval(t, scenario.Name, variant.Name, "碎片率", m.Fragmentation)
			assertUnitInterval(t, scenario.Name, variant.Name, "不可行率", m.InfeasibleRate)
			assertUnitInterval(t, scenario.Name, variant.Name, "平均租户 HHI", m.MeanTenantHHI)
			assertUnitInterval(t, scenario.Name, variant.Name, "最大节点副本占比", m.MaxNodeReplicaShare)
			if m.AverageDecisionUS < 0 || m.P99US < m.AverageDecisionUS ||
				math.IsNaN(m.AverageDecisionUS) || math.IsNaN(m.P99US) {
				t.Errorf("%s/%s 延迟非法: 平均=%vµs P99=%vµs",
					scenario.Name, variant.Name, m.AverageDecisionUS, m.P99US)
			}
		}
	}
}

func TestCalibratedDefaultRunStaysBelowSaturation(t *testing.T) {
	result, err := run(42, 200, fakeClock())
	if err != nil {
		t.Fatalf("运行基准失败: %v", err)
	}
	for _, scenario := range result.Scenarios {
		for _, variant := range scenario.Variants {
			m := variant.Metrics
			if m.InfeasibleRate >= 0.10 {
				t.Errorf("%s/%s 不可行率 %.2f%% 未达到 <10%% 的校准目标",
					scenario.Name, variant.Name, m.InfeasibleRate*100)
			}
			if m.CPUUtilization < 0.70 || m.CPUUtilization > 0.90 {
				t.Errorf("%s/%s CPU 利用率 %.2f%% 超出 70%%~90%% 校准区间",
					scenario.Name, variant.Name, m.CPUUtilization*100)
			}
			if len(variant.TenantShares) != len(tenantSpecs) {
				t.Errorf("%s/%s 租户明细数 = %d, want %d",
					scenario.Name, variant.Name, len(variant.TenantShares), len(tenantSpecs))
			}
		}
	}
}

func TestCalculateDispersion(t *testing.T) {
	nodes := []schedcore.Node{{Name: "n1"}, {Name: "n2"}, {Name: "n3"}, {Name: "n4"}}
	counts := map[string]map[string]int{
		"tenant-a": {"n1": 6, "n2": 2, "n3": 2},
		"tenant-b": {"n1": 1, "n2": 1, "n3": 1, "n4": 1},
		"tenant-c": {"n1": 4},
	}
	meanHHI, maxShare := calculateDispersion(nodes, counts)
	if math.Abs(meanHHI-0.4225) > 1e-12 {
		t.Errorf("平均租户 HHI = %v, want 0.4225", meanHHI)
	}
	if maxShare != 1 {
		t.Errorf("最大节点副本占比 = %v, want 1", maxShare)
	}
}

// 控制台图表必须覆盖全部场景与变体，且不再出现已移除的 P50 口径。
func TestRenderConsoleCoversAllVariants(t *testing.T) {
	result, err := run(42, 30, fakeClock())
	if err != nil {
		t.Fatalf("运行基准失败: %v", err)
	}
	out := RenderConsole(result)
	for _, scenario := range result.Scenarios {
		if !strings.Contains(out, "场景 "+scenario.Name) {
			t.Errorf("控制台图表缺少场景 %s", scenario.Name)
		}
		for _, variant := range scenario.Variants {
			if !strings.Contains(out, variant.Name) {
				t.Errorf("控制台图表缺少变体 %s", variant.Name)
			}
		}
	}
	if !strings.Contains(out, string(chartFilled)) {
		t.Error("控制台图表未渲染任何条形")
	}
	if strings.Contains(out, "P50") {
		t.Error("控制台图表不应再输出 P50")
	}
}

func assertUnitInterval(t *testing.T, scenario, variant, name string, value float64) {
	t.Helper()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		t.Errorf("%s/%s 的%s越界: %v", scenario, variant, name, value)
	}
}

// 只验证公平模式会改变路径，不断言 DRF 在任何场景中必然更优。
func TestDRFAndBaselineChooseDifferentNodeSequences(t *testing.T) {
	result, err := run(42, 200, fakeClock())
	if err != nil {
		t.Fatalf("运行基准失败: %v", err)
	}
	for _, scenario := range result.Scenarios {
		for _, pair := range [][2]string{
			{"balanced-drf", "balanced-baseline"},
			{"fairshare-drf", "fairshare-baseline"},
		} {
			drf, ok := findVariant(scenario, pair[0])
			if !ok {
				t.Fatalf("场景 %s 缺少变体 %s", scenario.Name, pair[0])
			}
			baseline, ok := findVariant(scenario, pair[1])
			if !ok {
				t.Fatalf("场景 %s 缺少变体 %s", scenario.Name, pair[1])
			}
			if reflect.DeepEqual(drf.NodeSequence, baseline.NodeSequence) {
				t.Errorf("场景 %s 中 %s 与 %s 的逐节点选择序列完全相同",
					scenario.Name, pair[0], pair[1])
			}
		}
	}
}
