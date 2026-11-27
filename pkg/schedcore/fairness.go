package schedcore

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

type Allocator string

const (
	AllocatorFIFO      Allocator = "FIFO"
	AllocatorHardQuota Allocator = "HARD_QUOTA"
	AllocatorDRF       Allocator = "DRF"
)

var (
	ErrUnknownAllocator = errors.New("未知分配器")
	ErrNegativeCapacity = errors.New("容量不能为负")
)

// TenantDemand 是一次仲裁的输入。
type TenantDemand struct {
	ID         string
	Weight     float64   // 必须 > 0；<= 0 时按 1 处理
	Demand     Resources // 本轮总需求
	Min        Resources // DRF 保底
	Max        *Resources
	ArrivalSeq int // 仅 FIFO 使用
}

type Allocation struct {
	TenantID      string    `json:"tenant_id"`
	Allocated     Resources `json:"allocated"`
	DominantShare float64   `json:"dominant_share"`
}

type Shortfall struct {
	TenantID string    `json:"tenant_id"`
	Missing  Resources `json:"missing"`
}

type Plan struct {
	Allocator   Allocator    `json:"allocator"`
	Allocations []Allocation `json:"allocations"`
	Jain        float64      `json:"jain_index"`
	CPUJain     float64      `json:"cpu_jain"`
	MemJain     float64      `json:"mem_jain"`
	Utilization float64      `json:"utilization"`
	Infeasible  bool         `json:"infeasible"`
	Shortfall   []Shortfall  `json:"shortfall,omitempty"`
}

// Arbitrate 按指定分配器计算租户分配方案。
func Arbitrate(capacity Resources, tenants []TenantDemand, allocator Allocator) (Plan, error) {
	if capacity.CPUMilli < 0 || capacity.MemBytes < 0 || capacity.GPU < 0 {
		return Plan{}, ErrNegativeCapacity
	}

	xs := make([]Resources, len(tenants))
	var infeasible bool
	var shortfalls []Shortfall

	switch allocator {
	case AllocatorFIFO:
		fifoAllocate(capacity, tenants, xs)
	case AllocatorHardQuota:
		hardQuotaAllocate(capacity, tenants, xs)
	case AllocatorDRF:
		// 阶段一：保底。容量不足以覆盖所有 min 时判定为不可行并给出缺口。
		drfAllocate(capacity, tenants, xs, true)
		for i := range tenants {
			if tenants[i].Min.CPUMilli > xs[i].CPUMilli || tenants[i].Min.MemBytes > xs[i].MemBytes || tenants[i].Min.GPU > xs[i].GPU {
				infeasible = true
				shortfalls = append(shortfalls, Shortfall{
					TenantID: tenants[i].ID,
					Missing: Resources{
						CPUMilli: tenants[i].Min.CPUMilli - xs[i].CPUMilli,
						MemBytes: tenants[i].Min.MemBytes - xs[i].MemBytes,
						GPU:      tenants[i].Min.GPU - xs[i].GPU,
					},
				})
			}
		}
		// 阶段二：在保底之上按加权主导份额做最大最小公平分配。
		if !infeasible {
			drfAllocate(capacity, tenants, xs, false)
		}
	default:
		return Plan{}, fmt.Errorf("%w: %q", ErrUnknownAllocator, allocator)
	}

	plan := Plan{
		Allocator:   allocator,
		Allocations: make([]Allocation, len(tenants)),
		Infeasible:  infeasible,
		Shortfall:   shortfalls,
	}

	shares := make([]float64, len(tenants))
	cpuShares := make([]float64, len(tenants))
	memShares := make([]float64, len(tenants))
	var used Resources
	for i := range tenants {
		plan.Allocations[i] = Allocation{
			TenantID:      tenants[i].ID,
			Allocated:     xs[i],
			DominantShare: round4(dominantShare(xs[i], capacity)),
		}
		shares[i] = dominantShare(xs[i], capacity)
		if capacity.CPUMilli > 0 {
			cpuShares[i] = float64(xs[i].CPUMilli) / float64(capacity.CPUMilli)
		}
		if capacity.MemBytes > 0 {
			memShares[i] = float64(xs[i].MemBytes) / float64(capacity.MemBytes)
		}
		used = used.Add(xs[i])
	}
	sort.Slice(plan.Allocations, func(a, b int) bool {
		return plan.Allocations[a].TenantID < plan.Allocations[b].TenantID
	})

	plan.Jain = round4(JainIndex(shares))
	plan.CPUJain = round4(JainIndex(cpuShares))
	plan.MemJain = round4(JainIndex(memShares))

	dims := 0
	var util float64
	if capacity.CPUMilli > 0 {
		util += ratio(used.CPUMilli, capacity.CPUMilli)
		dims++
	}
	if capacity.MemBytes > 0 {
		util += ratio(used.MemBytes, capacity.MemBytes)
		dims++
	}
	if capacity.GPU > 0 {
		util += ratio(used.GPU, capacity.GPU)
		dims++
	}
	if dims > 0 {
		util /= float64(dims)
	}
	plan.Utilization = round4(util)

	return plan, nil
}

// JainIndex 返回 Jain 公平指数 (Σx)² / (n·Σx²)。
// 全零时返回 1，但该值无意义，调用方必须在报告中标注样本为空。
func JainIndex(values []float64) float64 {
	n := len(values)
	if n == 0 {
		return 1
	}
	var sum, squares float64
	for _, v := range values {
		sum += v
		squares += v * v
	}
	if squares == 0 {
		return 1
	}
	return sum * sum / (float64(n) * squares)
}

func fifoAllocate(capacity Resources, tenants []TenantDemand, xs []Resources) {
	type seqIdx struct{ idx, seq int }
	order := make([]seqIdx, len(tenants))
	for i := range tenants {
		order[i] = seqIdx{idx: i, seq: tenants[i].ArrivalSeq}
	}
	sort.SliceStable(order, func(a, b int) bool { return order[a].seq < order[b].seq })

	remaining := capacity
	for _, item := range order {
		want := tenantTarget(tenants[item.idx], false)
		take := Resources{
			CPUMilli: min64(want.CPUMilli, remaining.CPUMilli),
			MemBytes: min64(want.MemBytes, remaining.MemBytes),
			GPU:      min64(want.GPU, remaining.GPU),
		}
		take = nonNegative(take)
		xs[item.idx] = take
		remaining = remaining.Sub(take)
	}
}

// hardQuotaAllocate 按租户 ID 升序依次满足，超过硬配额或容量即停止。
func hardQuotaAllocate(capacity Resources, tenants []TenantDemand, xs []Resources) {
	order := make([]int, len(tenants))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return tenants[order[a]].ID < tenants[order[b]].ID
	})

	remaining := capacity
	for _, i := range order {
		want := tenantTarget(tenants[i], false)
		take := Resources{
			CPUMilli: min64(want.CPUMilli, remaining.CPUMilli),
			MemBytes: min64(want.MemBytes, remaining.MemBytes),
			GPU:      min64(want.GPU, remaining.GPU),
		}
		take = nonNegative(take)
		xs[i] = take
		remaining = remaining.Sub(take)
	}
}

// drfAllocate 用"水位法"求加权最大最小公平解。
//
// 记份额水位为 λ，租户 i 在资源 r 上最多可拿 λ·w_i·C_r。
// λ 越大，所有租户的加权主导份额越接近；λ 的上界由"总有资源被占满"决定。
// 由于总分配量对 λ 单调不减，用二分法求满足容量约束的最大 λ，再按该水位落量。
//
// 之所以不用"逐轮整段推进"：第一轮会把整段容量给到某个租户，
// 等价于 FIFO，无法得到公平解。水位法与渐进填充（progressive filling）等价，且数值稳定。
func drfAllocate(capacity Resources, tenants []TenantDemand, xs []Resources, useMin bool) {
	n := len(tenants)
	if n == 0 {
		return
	}

	targets := make([]Resources, n)
	floors := make([]Resources, n)
	for i := range tenants {
		targets[i] = tenantTarget(tenants[i], useMin)
		if !useMin {
			// 阶段二以 min 为下限，保证保底份额不被水位法压低。
			floors[i] = minResources(tenants[i].Min, targets[i])
		}
	}

	allocationAt := func(level float64) []Resources {
		out := make([]Resources, n)
		for i := 0; i < n; i++ {
			weight := tenants[i].Weight
			if weight <= 0 {
				weight = 1
			}
			x := Resources{
				CPUMilli: floor64(level * weight * float64(capacity.CPUMilli)),
				MemBytes: floor64(level * weight * float64(capacity.MemBytes)),
				GPU:      floor64(level * weight * float64(capacity.GPU)),
			}
			x = maxResources(x, floors[i])
			x = minResources(x, targets[i])
			out[i] = nonNegative(x)
		}
		return out
	}

	fits := func(alloc []Resources) bool {
		var used Resources
		for _, x := range alloc {
			used = used.Add(x)
		}
		return used.FitsIn(capacity)
	}

	high := 1.0
	for i := range tenants {
		weight := tenants[i].Weight
		if weight <= 0 {
			weight = 1
		}
		if capacity.CPUMilli > 0 {
			high = math.Max(high, float64(targets[i].CPUMilli)/(weight*float64(capacity.CPUMilli)))
		}
		if capacity.MemBytes > 0 {
			high = math.Max(high, float64(targets[i].MemBytes)/(weight*float64(capacity.MemBytes)))
		}
		if capacity.GPU > 0 {
			high = math.Max(high, float64(targets[i].GPU)/(weight*float64(capacity.GPU)))
		}
	}
	high *= 2

	low := 0.0
	for iter := 0; iter < 200; iter++ {
		mid := (low + high) / 2
		if fits(allocationAt(mid)) {
			low = mid
		} else {
			high = mid
		}
	}

	alloc := allocationAt(low)
	for i := range xs {
		xs[i] = alloc[i]
	}
}

func tenantTarget(t TenantDemand, useMin bool) Resources {
	target := t.Demand
	if useMin {
		target = minResources(t.Min, t.Demand)
	}
	if t.Max != nil {
		target = minResources(target, *t.Max)
	}
	return nonNegative(target)
}

func minResources(a, b Resources) Resources {
	return Resources{CPUMilli: min64(a.CPUMilli, b.CPUMilli), MemBytes: min64(a.MemBytes, b.MemBytes), GPU: min64(a.GPU, b.GPU)}
}

func maxResources(a, b Resources) Resources {
	return Resources{CPUMilli: max64(a.CPUMilli, b.CPUMilli), MemBytes: max64(a.MemBytes, b.MemBytes), GPU: max64(a.GPU, b.GPU)}
}

func nonNegative(r Resources) Resources {
	if r.CPUMilli < 0 {
		r.CPUMilli = 0
	}
	if r.MemBytes < 0 {
		r.MemBytes = 0
	}
	if r.GPU < 0 {
		r.GPU = 0
	}
	return r
}

func floor64(v float64) int64 {
	if v <= 0 {
		return 0
	}
	return int64(math.Floor(v))
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
