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
