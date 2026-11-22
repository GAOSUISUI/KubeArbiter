package schedcore

import (
	"errors"
	"reflect"
	"testing"
)

func allocOf(t *testing.T, plan Plan, tenantID string) Allocation {
	t.Helper()
	for _, a := range plan.Allocations {
		if a.TenantID == tenantID {
			return a
		}
	}
	t.Fatalf("分配结果中缺少租户 %q", tenantID)
	return Allocation{}
}

func cpuReq(milli int64) Resources {
	return Resources{CPUMilli: milli}
}

// DRF 在等权重等需求时应均分容量，这是公平性的基线。
func TestDRFEqualSplit(t *testing.T) {
	capacity := Resources{CPUMilli: 90000, MemBytes: 300 * gi}
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Demand: Resources{CPUMilli: 90000, MemBytes: 100 * gi}},
		{ID: "b", Weight: 1, Demand: Resources{CPUMilli: 90000, MemBytes: 100 * gi}},
		{ID: "c", Weight: 1, Demand: Resources{CPUMilli: 90000, MemBytes: 100 * gi}},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if plan.Infeasible {
		t.Fatalf("不应判定为不可行")
	}
	for _, id := range []string{"a", "b", "c"} {
		got := allocOf(t, plan, id)
		if got.Allocated.CPUMilli != 30000 {
			t.Errorf("租户 %s CPU 分配 = %d, want 30000", id, got.Allocated.CPUMilli)
		}
		if got.Allocated.MemBytes != 100*gi {
			t.Errorf("租户 %s 内存分配 = %d, want %d", id, got.Allocated.MemBytes, 100*gi)
		}
		approx(t, got.DominantShare, 1.0/3.0, 1e-4, "租户 "+id+" 主导份额")
	}
	approx(t, plan.Jain, 1.0, 1e-4, "Jain 指数")
	approx(t, plan.Utilization, 1.0, 1e-4, "利用率")
}

// 加权 DRF 的分配比例应等于权重比例。
func TestDRFWeightedProportional(t *testing.T) {
	capacity := Resources{CPUMilli: 60000, MemBytes: 600 * gi}
	tenants := []TenantDemand{
		{ID: "small", Weight: 1, Demand: Resources{CPUMilli: 60000, MemBytes: 100 * gi}},
		{ID: "mid", Weight: 2, Demand: Resources{CPUMilli: 60000, MemBytes: 100 * gi}},
		{ID: "large", Weight: 3, Demand: Resources{CPUMilli: 60000, MemBytes: 100 * gi}},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}

	want := map[string]int64{"small": 10000, "mid": 20000, "large": 30000}
	for id, w := range want {
		if got := allocOf(t, plan, id).Allocated.CPUMilli; got != w {
			t.Errorf("租户 %s CPU 分配 = %d, want %d", id, got, w)
		}
	}
	// 主导份额之比应为 1:2:3，即 1/6 : 2/6 : 3/6。
	approx(t, allocOf(t, plan, "small").DominantShare, 1.0/6.0, 1e-4, "small 主导份额")
	approx(t, allocOf(t, plan, "mid").DominantShare, 2.0/6.0, 1e-4, "mid 主导份额")
	approx(t, allocOf(t, plan, "large").DominantShare, 3.0/6.0, 1e-4, "large 主导份额")
}

// 权重非法时按 1 处理，不能出现除零或 NaN。
func TestDRFNonPositiveWeightTreatedAsOne(t *testing.T) {
	capacity := cpuReq(10000)
	tenants := []TenantDemand{
		{ID: "a", Weight: 0, Demand: cpuReq(10000)},
		{ID: "b", Weight: -1, Demand: cpuReq(10000)},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if got := allocOf(t, plan, "a").Allocated.CPUMilli; got != 5000 {
		t.Errorf("a CPU = %d, want 5000", got)
	}
	if got := allocOf(t, plan, "b").Allocated.CPUMilli; got != 5000 {
		t.Errorf("b CPU = %d, want 5000", got)
	}
}

// Min 是保底：阶段二不得把任何租户压到 min 以下。
func TestDRFRespectsMinFloor(t *testing.T) {
	capacity := cpuReq(10000)
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Min: cpuReq(4000), Demand: cpuReq(10000)},
		{ID: "b", Weight: 1, Min: cpuReq(4000), Demand: cpuReq(10000)},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if plan.Infeasible {
		t.Fatalf("容量可覆盖全部 min，不应判定为不可行")
	}
	for _, id := range []string{"a", "b"} {
		got := allocOf(t, plan, id).Allocated.CPUMilli
		if got < 4000 {
			t.Errorf("租户 %s 分配 %d 低于保底 4000", id, got)
		}
		if got != 5000 {
			t.Errorf("租户 %s CPU = %d, want 5000", id, got)
		}
	}
}

// 所有租户 min 之和超过容量时必须显式报不可行并给出缺口，不能静默截断。
func TestDRFInfeasibleMinSum(t *testing.T) {
	capacity := cpuReq(5000)
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Min: cpuReq(4000), Demand: cpuReq(10000)},
		{ID: "b", Weight: 1, Min: cpuReq(4000), Demand: cpuReq(10000)},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if !plan.Infeasible {
		t.Fatalf("min 之和 8000 > 容量 5000，应判定为不可行")
	}
	if len(plan.Shortfall) != 2 {
		t.Fatalf("缺口条目数 = %d, want 2", len(plan.Shortfall))
	}
	gaps := map[string]int64{}
	for _, s := range plan.Shortfall {
		gaps[s.TenantID] = s.Missing.CPUMilli
	}
	if gaps["a"] != 1500 || gaps["b"] != 1500 {
		t.Errorf("缺口 = %v, want a/b 各 1500", gaps)
	}
}

// Max 是硬上限，DRF 不得越过。
func TestDRFRespectsMax(t *testing.T) {
	capacity := cpuReq(10000)
	max := cpuReq(3000)
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Demand: cpuReq(10000), Max: &max},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if got := allocOf(t, plan, "a").Allocated.CPUMilli; got != 3000 {
		t.Errorf("CPU = %d, want 3000", got)
	}
}

// 容量为空时不做任何分配，且不得产生 NaN。
func TestDRFZeroCapacity(t *testing.T) {
	plan, err := Arbitrate(Resources{}, []TenantDemand{
		{ID: "a", Weight: 1, Demand: cpuReq(1000)},
	}, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if got := allocOf(t, plan, "a"); got.Allocated != (Resources{}) {
		t.Errorf("零容量下的分配 = %+v, want 全零", got.Allocated)
	}
}

// FIFO 按到达顺序满足，先到者占满后后来者只能拿到残余。
func TestFIFOByArrival(t *testing.T) {
	capacity := cpuReq(10000)
	tenants := []TenantDemand{
		{ID: "late", Weight: 1, Demand: cpuReq(8000), ArrivalSeq: 2},
		{ID: "early", Weight: 1, Demand: cpuReq(8000), ArrivalSeq: 1},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorFIFO)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if got := allocOf(t, plan, "early").Allocated.CPUMilli; got != 8000 {
		t.Errorf("early CPU = %d, want 8000", got)
	}
	if got := allocOf(t, plan, "late").Allocated.CPUMilli; got != 2000 {
		t.Errorf("late CPU = %d, want 2000", got)
	}
}

// 硬配额分配器按租户 ID 升序满足，受 Max 与剩余容量双重限制。
func TestHardQuotaByTenantID(t *testing.T) {
	capacity := cpuReq(10000)
	quotaA := cpuReq(3000)
	quotaB := cpuReq(8000)
	tenants := []TenantDemand{
		{ID: "b", Weight: 1, Demand: cpuReq(10000), Max: &quotaB},
		{ID: "a", Weight: 1, Demand: cpuReq(10000), Max: &quotaA},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorHardQuota)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if got := allocOf(t, plan, "a").Allocated.CPUMilli; got != 3000 {
		t.Errorf("a CPU = %d, want 3000（受硬配额限制）", got)
	}
	if got := allocOf(t, plan, "b").Allocated.CPUMilli; got != 7000 {
		t.Errorf("b CPU = %d, want 7000（受剩余容量限制）", got)
	}
}

// CPU 先耗尽时内存维度不应被串扰，两个资源维度相互独立。
func TestDRFCPUBindsFirst(t *testing.T) {
	capacity := Resources{CPUMilli: 10000, MemBytes: 100 * gi}
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Demand: Resources{CPUMilli: 10000, MemBytes: 10 * gi}},
		{ID: "b", Weight: 1, Demand: Resources{CPUMilli: 10000, MemBytes: 10 * gi}},
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	for _, id := range []string{"a", "b"} {
		got := allocOf(t, plan, id).Allocated
		if got.CPUMilli != 5000 {
			t.Errorf("租户 %s CPU = %d, want 5000", id, got.CPUMilli)
		}
		if got.MemBytes != 10*gi {
			t.Errorf("租户 %s 内存 = %d, want %d（应完整满足）", id, got.MemBytes, 10*gi)
		}
	}
}

// 分配结果按租户 ID 升序排列，与输入顺序无关。
func TestAllocationsSortedByTenantID(t *testing.T) {
	plan, err := Arbitrate(cpuReq(10000), []TenantDemand{
		{ID: "z", Weight: 1, Demand: cpuReq(1000)},
		{ID: "a", Weight: 1, Demand: cpuReq(1000)},
	}, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if plan.Allocations[0].TenantID != "a" || plan.Allocations[1].TenantID != "z" {
		t.Errorf("排序结果 = %s,%s, want a,z",
			plan.Allocations[0].TenantID, plan.Allocations[1].TenantID)
	}
}

// 同一输入重复仲裁必须得到完全相同的结果，保证可复现。
func TestArbitrateDeterministic(t *testing.T) {
	capacity := Resources{CPUMilli: 60000, MemBytes: 600 * gi}
	tenants := []TenantDemand{
		{ID: "a", Weight: 1, Demand: Resources{CPUMilli: 60000, MemBytes: 100 * gi}},
		{ID: "b", Weight: 2, Demand: Resources{CPUMilli: 60000, MemBytes: 100 * gi}},
	}

	first, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	second, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("两次仲裁结果不一致:\n%+v\n%+v", first, second)
	}
}

func TestArbitrateErrors(t *testing.T) {
	if _, err := Arbitrate(Resources{CPUMilli: -1}, nil, AllocatorDRF); !errors.Is(err, ErrNegativeCapacity) {
		t.Errorf("负容量错误 = %v, want ErrNegativeCapacity", err)
	}
	if _, err := Arbitrate(Resources{CPUMilli: 1}, nil, Allocator("BOGUS")); !errors.Is(err, ErrUnknownAllocator) {
		t.Errorf("未知分配器错误 = %v, want ErrUnknownAllocator", err)
	}
}

func TestJainIndex(t *testing.T) {
	cases := []struct {
		name   string
		values []float64
		want   float64
	}{
		{"空样本返回 1", nil, 1},
		{"完全均分", []float64{0.5, 0.5, 0.5}, 1},
		{"单租户独占", []float64{1, 0, 0}, 1.0 / 3.0},
		{"两租户 1:3", []float64{1, 3}, 0.8},
		{"全零返回 1", []float64{0, 0}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			approx(t, JainIndex(c.values), c.want, 1e-9, "Jain 指数")
		})
	}
}

// 50 租户规模下仲裁结果必须仍然可行，作为 §8 复杂度预算的正确性保护。
func TestDRFManyTenantsFeasible(t *testing.T) {
	capacity := Resources{CPUMilli: 100000, MemBytes: 1000 * gi}
	tenants := make([]TenantDemand, 50)
	for i := range tenants {
		tenants[i] = TenantDemand{
			ID:     string(rune('A' + i%26)),
			Weight: 1,
			Demand: Resources{CPUMilli: 100000, MemBytes: 1000 * gi},
		}
	}
	// 补足唯一 ID，避免重名掩盖问题。
	for i := range tenants {
		tenants[i].ID = tenants[i].ID + string(rune('a'+i/26))
	}

	plan, err := Arbitrate(capacity, tenants, AllocatorDRF)
	if err != nil {
		t.Fatalf("Arbitrate 返回错误: %v", err)
	}
	var used Resources
	for _, a := range plan.Allocations {
		used = used.Add(a.Allocated)
	}
	if !used.FitsIn(capacity) {
		t.Errorf("分配总量 %+v 超出容量 %+v", used, capacity)
	}
	if plan.Utilization < 0.99 {
		t.Errorf("利用率 = %v, want ≥ 0.99", plan.Utilization)
	}
}
