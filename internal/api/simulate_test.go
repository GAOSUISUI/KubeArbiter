package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const testPolicyID = "11111111-1111-1111-1111-111111111111"

func simulateFixture(t *testing.T) (*ginEngine, *MemoryStore) {
	t.Helper()
	weights, err := schedcore.PresetWeights(schedcore.Balanced)
	if err != nil {
		t.Fatalf("预置权重不可用: %v", err)
	}
	store := NewMemoryStore()
	store.AddPolicy(Policy{ID: testPolicyID, Name: "balanced", Strategy: schedcore.Balanced, Weights: weights, IsDefault: true})

	snap := capacity.Snapshot{
		TakenAt: time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC),
		Nodes: []capacity.NodeView{
			{
				Name:        "ka-w1",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
				Labels:      map[string]string{"kubernetes.io/hostname": "ka-w1"},
				HostPorts:   map[int32]string{},
				Topology:    map[string]string{"kubernetes.io/hostname": "ka-w1"},
			},
			{
				Name:        "ka-w2",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
				Requested:   schedcore.Resources{CPUMilli: 3000, MemBytes: 6 << 30},
				Labels:      map[string]string{"kubernetes.io/hostname": "ka-w2"},
				HostPorts:   map[int32]string{},
				Topology:    map[string]string{"kubernetes.io/hostname": "ka-w2"},
			},
		},
	}

	router := NewRouter(Deps{
		Policies:  store,
		Decisions: store,
		Writer:    store,
		Tenants:   store,
		Snapshots: store,
		Snapshotter: func(context.Context) (capacity.Snapshot, error) {
			return snap, nil
		},
	})
	return &ginEngine{t: t, h: router}, store
}

type ginEngine struct {
	t *testing.T
	h http.Handler
}

func (g *ginEngine) post(path, body string) (*httptest.ResponseRecorder, map[string]any) {
	g.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	g.h.ServeHTTP(rec, req)
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		g.t.Fatalf("响应不是合法 JSON: %v，原始: %s", err, rec.Body.String())
	}
	return rec, payload
}

func TestSimulatePlacementChoosesIdleNode(t *testing.T) {
	eng, store := simulateFixture(t)

	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 2,
		             "requests": {"cpu_milli": 500, "mem_bytes": 536870912}},
		"policy_id": "`+testPolicyID+`"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if payload["code"].(float64) != 0 {
		t.Fatalf("期望 code=0，实际 %v", payload["code"])
	}
	data := payload["data"].(map[string]any)
	// ka-w1 空闲、ka-w2 已用 75%，负载感知下应选 ka-w1。
	if data["chosen_node"] != "ka-w1" {
		t.Fatalf("应选择空闲节点 ka-w1，实际 %v", data["chosen_node"])
	}
	if data["status"] != string(schedcore.StatusOK) {
		t.Fatalf("状态应为 OK，实际 %v", data["status"])
	}
	if data["degraded"] != true {
		t.Fatal("M2 尚未接入指标，degraded 应为 true")
	}
	if data["snapshot_hash"] == "" {
		t.Fatal("应返回快照哈希")
	}
	if id, _ := data["id"].(string); id == "" {
		t.Fatal("落库后应返回决策 ID")
	}

	list, _ := store.ListDecisions(context.Background(), DecisionFilter{})
	if len(list) != 1 {
		t.Fatalf("决策应落库 1 条，实际 %d", len(list))
	}
	if n := store.SnapshotCount(); n != 1 {
		t.Fatalf("快照应留档 1 份，实际 %d", n)
	}

	// 同一状态再模拟一次：快照应按 hash 去重，决策则新增一条。
	eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 2,
		             "requests": {"cpu_milli": 500, "mem_bytes": 536870912}},
		"policy_id": "`+testPolicyID+`"
	}`)
	if n := store.SnapshotCount(); n != 1 {
		t.Fatalf("相同快照不应重复留档，实际 %d 份", n)
	}
	if l, _ := store.ListDecisions(context.Background(), DecisionFilter{}); len(l) != 2 {
		t.Fatalf("决策应累计 2 条，实际 %d", len(l))
	}
}

func TestSimulateRejectsInvalidWorkload(t *testing.T) {
	eng, _ := simulateFixture(t)
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "", "namespace": "", "replicas": 0},
		"policy_id": "not-a-uuid"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if payload["code"].(float64) != CodeInvalidArgument {
		t.Fatalf("期望 40001，实际 %v", payload["code"])
	}
	details := payload["details"].([]any)
	if len(details) < 4 {
		t.Fatalf("应逐字段列出错误，实际 %v", details)
	}
}

func TestSimulateUnknownPolicy(t *testing.T) {
	eng, _ := simulateFixture(t)
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 1,
		             "requests": {"cpu_milli": 100, "mem_bytes": 1048576}},
		"policy_id": "22222222-2222-2222-2222-222222222222"
	}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", rec.Code)
	}
	if payload["code"].(float64) != CodeNotFound {
		t.Fatalf("期望 40401，实际 %v", payload["code"])
	}
}

func TestSimulateWithoutSnapshotterReturns503(t *testing.T) {
	weights, _ := schedcore.PresetWeights(schedcore.Balanced)
	store := NewMemoryStore()
	store.AddPolicy(Policy{ID: testPolicyID, Name: "balanced", Weights: weights})
	router := NewRouter(Deps{Policies: store, Decisions: store, Writer: store})

	eng := &ginEngine{t: t, h: router}
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 1,
		             "requests": {"cpu_milli": 100, "mem_bytes": 1048576}},
		"policy_id": "`+testPolicyID+`"
	}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接 informer 时应 503，实际 %d", rec.Code)
	}
	if payload["code"].(float64) != CodeUnavailable {
		t.Fatalf("期望 50301，实际 %v", payload["code"])
	}
}

func TestSimulateTenantQuotaExceeded(t *testing.T) {
	eng, store := simulateFixture(t)
	const tenantID = "44444444-4444-4444-4444-444444444444"
	// 配额 100m，单副本需求 500m：任何节点都不可能通过硬上限。
	store.AddTenant(schedcore.TenantState{
		ID:     tenantID,
		Weight: 2,
		Quota:  schedcore.Resources{CPUMilli: 100, MemBytes: 1 << 20},
	})

	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 1,
		             "requests": {"cpu_milli": 500, "mem_bytes": 536870912}},
		"tenant_id": "`+tenantID+`",
		"policy_id": "`+testPolicyID+`"
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("配额超限属于可解释的拒绝，应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	data := payload["data"].(map[string]any)
	if data["status"] != string(schedcore.StatusNoCandidate) {
		t.Fatalf("状态应为 NO_CANDIDATE，实际 %v", data["status"])
	}
	if data["chosen_node"] != nil {
		t.Fatalf("拒绝时不应有选中节点，实际 %v", data["chosen_node"])
	}
	filtered := data["filtered"].([]any)
	reasons := make([]string, 0, len(filtered))
	for _, f := range filtered {
		reasons = append(reasons, f.(map[string]any)["reason"].(string))
	}
	for _, r := range reasons {
		if r != string(schedcore.ReasonTenantQuotaExceeded) {
			t.Fatalf("过滤原因应全部为 TENANT_QUOTA_EXCEEDED，实际 %v", reasons)
		}
	}
}

func TestSimulateUnknownTenantReturns404(t *testing.T) {
	eng, _ := simulateFixture(t)
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "api", "namespace": "default", "replicas": 1,
		             "requests": {"cpu_milli": 100, "mem_bytes": 1048576}},
		"tenant_id": "55555555-5555-5555-5555-555555555555",
		"policy_id": "`+testPolicyID+`"
	}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知租户应 404，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if payload["code"].(float64) != CodeNotFound {
		t.Fatalf("期望 40401，实际 %v", payload["code"])
	}
}

// gangFixture 提供两台各 4 卡 GPU 的节点，专供 gang 用例。
func gangFixture(t *testing.T) *ginEngine {
	t.Helper()
	weights, err := schedcore.PresetWeights(schedcore.Balanced)
	if err != nil {
		t.Fatalf("预置权重不可用: %v", err)
	}
	store := NewMemoryStore()
	store.AddPolicy(Policy{ID: testPolicyID, Name: "balanced", Strategy: schedcore.Balanced, Weights: weights, IsDefault: true})

	snap := capacity.Snapshot{
		TakenAt: time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC),
		Nodes: []capacity.NodeView{
			{
				Name:        "gpu-a",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 8000, MemBytes: 16 << 30, GPU: 4},
				Labels:      map[string]string{"kubernetes.io/hostname": "gpu-a"},
				HostPorts:   map[int32]string{},
				Topology:    map[string]string{"kubernetes.io/hostname": "gpu-a"},
			},
			{
				Name:        "gpu-b",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 8000, MemBytes: 16 << 30, GPU: 4},
				Labels:      map[string]string{"kubernetes.io/hostname": "gpu-b"},
				HostPorts:   map[int32]string{},
				Topology:    map[string]string{"kubernetes.io/hostname": "gpu-b"},
			},
		},
	}
	router := NewRouter(Deps{
		Policies:  store,
		Decisions: store,
		Writer:    store,
		Tenants:   store,
		Snapshots: store,
		Snapshotter: func(context.Context) (capacity.Snapshot, error) {
			return snap, nil
		},
	})
	return &ginEngine{t: t, h: router}
}

// TestSimulateGangPlacement 验证 gang 模拟：6 副本各 1 卡，两节点共 8 卡，
// 应成功并输出节点分布（CPU 是瓶颈，每节点 8000/500=16 份 → 6 份全在 gpu-a）。
func TestSimulateGangPlacement(t *testing.T) {
	eng := gangFixture(t)

	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "train", "namespace": "ai", "replicas": 6,
		             "requests": {"cpu_milli": 500, "mem_bytes": 1073741824, "gpu": 1}},
		"policy_id": "`+testPolicyID+`",
		"gang": {"min_available": 4}
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	data := payload["data"].(map[string]any)
	if data["status"] != string(schedcore.StatusOK) {
		t.Fatalf("状态应为 OK，实际 %v", data["status"])
	}
	assignments, ok := data["assignments"].([]any)
	if !ok || len(assignments) == 0 {
		t.Fatalf("gang 决策应带 assignments，实际 %v", data["assignments"])
	}
	var total int
	for _, a := range assignments {
		total += int(a.(map[string]any)["replicas"].(float64))
	}
	if total != 6 {
		t.Fatalf("assignments 总副本 = %d, 想要 6（%v）", total, assignments)
	}
	// 落库后 evidence 里也能看到 assignments 与 gang 参数。
	if ev, ok := data["evidence"].(map[string]any); ok {
		if ev["gang_min_available"].(float64) != 4 {
			t.Fatalf("evidence.gang_min_available = %v, 想要 4", ev["gang_min_available"])
		}
		if _, ok := ev["assignments"]; !ok {
			t.Fatal("evidence 应记录 assignments")
		}
	} else {
		t.Fatalf("evidence 缺失: %v", data["evidence"])
	}
}

// TestSimulateGangInfeasible 验证可行总量不足 min_available 时整体失败并给出证据。
func TestSimulateGangInfeasible(t *testing.T) {
	eng := gangFixture(t)

	// 每副本 4 卡 → 每节点 1 份，总量 2 < min_available 3。
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "train", "namespace": "ai", "replicas": 3,
		             "requests": {"cpu_milli": 500, "mem_bytes": 1073741824, "gpu": 4}},
		"policy_id": "`+testPolicyID+`",
		"gang": {"min_available": 3}
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("gang 不可行属于可解释拒绝，应 200，实际 %d", rec.Code)
	}
	data := payload["data"].(map[string]any)
	if data["status"] != string(schedcore.StatusNoCandidate) {
		t.Fatalf("状态应为 NO_CANDIDATE，实际 %v", data["status"])
	}
	if data["assignments"] != nil {
		t.Fatalf("失败时不应有 assignments，实际 %v", data["assignments"])
	}
	ev := data["evidence"].(map[string]any)
	if ev["total_feasible"].(float64) != 2 {
		t.Fatalf("total_feasible = %v, 想要 2", ev["total_feasible"])
	}
	if ev["min_available"].(float64) != 3 {
		t.Fatalf("min_available = %v, 想要 3", ev["min_available"])
	}
}

// TestSimulateGangInvalidArgs 验证 min_available 超过副本数返回 400。
func TestSimulateGangInvalidArgs(t *testing.T) {
	eng := gangFixture(t)
	rec, payload := eng.post("/api/v1/placement/simulate", `{
		"workload": {"name": "train", "namespace": "ai", "replicas": 2,
		             "requests": {"cpu_milli": 500, "mem_bytes": 1073741824}},
		"policy_id": "`+testPolicyID+`",
		"gang": {"min_available": 3}
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("期望 400，实际 %d", rec.Code)
	}
	if payload["code"].(float64) != CodeInvalidArgument {
		t.Fatalf("期望 40001，实际 %v", payload["code"])
	}
}
