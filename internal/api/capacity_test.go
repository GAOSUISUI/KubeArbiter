package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestListTenantCapacity(t *testing.T) {
	store := NewMemoryStore()
	store.AddTenant(schedcore.TenantState{
		ID:     "t-a",
		Weight: 3,
		Quota:  schedcore.Resources{CPUMilli: 2000, MemBytes: 4 << 30},
	})
	store.AddTenant(schedcore.TenantState{ID: "t-b", Weight: 1})

	snap := capacity.Snapshot{
		TakenAt: time.Now(),
		Nodes: []capacity.NodeView{{
			Name:        "ka-w1",
			Ready:       true,
			Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
		}},
		TenantUsage: map[string]schedcore.Resources{
			"t-a": {CPUMilli: 500, MemBytes: 1 << 30},
		},
	}

	router := NewRouter(Deps{
		Policies:      store,
		Decisions:     store,
		TenantCatalog: store,
		Snapshotter: func(context.Context) (capacity.Snapshot, error) {
			return snap, nil
		},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capacity/tenants", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Code int `json:"code"`
		Data struct {
			NextCursor *string `json:"next_cursor"`
			Items      []struct {
				TenantID  string              `json:"tenant_id"`
				Name      string              `json:"name"`
				Requested schedcore.Resources `json:"requested"`
				Hard      schedcore.Resources `json:"hard"`
				FairShare float64             `json:"fair_share"`
				JainShare float64             `json:"jain_share"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if payload.Code != 0 || len(payload.Data.Items) != 2 {
		t.Fatalf("应返回两个租户，实际 code=%d items=%d", payload.Code, len(payload.Data.Items))
	}

	first := payload.Data.Items[0]
	if first.TenantID != "t-a" {
		t.Fatalf("按名称排序后 t-a 应在前，实际 %s", first.TenantID)
	}
	if first.Requested.CPUMilli != 500 {
		t.Fatalf("t-a 已用 CPU 应来自快照，实际 %d", first.Requested.CPUMilli)
	}
	if first.FairShare != 0.75 {
		t.Fatalf("t-a 公平份额应为 0.75，实际 %v", first.FairShare)
	}
	// 主导份额 = max(500/4000, 1Gi/8Gi) = 0.125
	if first.JainShare != 0.125 {
		t.Fatalf("t-a 主导份额应为 0.125，实际 %v", first.JainShare)
	}
	if payload.Data.Items[1].JainShare != 0 {
		t.Fatalf("无用量租户主导份额应为 0，实际 %v", payload.Data.Items[1].JainShare)
	}
}

func TestListTenantCapacityRequiresSnapshotter(t *testing.T) {
	store := NewMemoryStore()
	router := NewRouter(Deps{Policies: store, Decisions: store, TenantCatalog: store})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capacity/tenants", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未接 informer 应 503，实际 %d", rec.Code)
	}
}
