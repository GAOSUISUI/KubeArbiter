package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/slo"
)

// seedSLOBuckets 以当前分钟为基准写入 count 个 1 分钟窗口。
func seedSLOBuckets(store *MemoryStore, service string, count int, total, errors, latencyOK int64) {
	now := time.Now().UTC().Truncate(time.Minute)
	for i := count; i >= 1; i-- {
		store.AddSLOBucket(service, slo.Bucket{
			Start:     now.Add(-time.Duration(i) * time.Minute),
			Total:     total,
			Errors:    errors,
			LatencyOK: latencyOK,
		})
	}
}

func seedSLORouter(t *testing.T) *MemoryStore {
	t.Helper()
	threshold := 300
	store := NewMemoryStore()
	store.AddSLODefinition(slo.Definition{
		Service: "checkout", SLIType: slo.SLIAvailability, Target: 0.99, Window: time.Hour,
	})
	store.AddSLODefinition(slo.Definition{
		Service: "search", SLIType: slo.SLIAvailability, Target: 0.99, Window: time.Hour,
	})
	store.AddSLODefinition(slo.Definition{
		Service: "catalog", SLIType: slo.SLILatency, Target: 0.95, Window: time.Hour,
		LatencyThresholdMS: &threshold,
	})

	seedSLOBuckets(store, "checkout", 61, 100, 0, 0)
	seedSLOBuckets(store, "search", 61, 100, 15, 0)
	seedSLOBuckets(store, "catalog", 61, 100, 0, 96)
	return store
}

func decodeOK[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	var data T
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("data 解析失败: %v, data=%s", err, string(body.Data))
	}
	return data
}

func TestListSLO(t *testing.T) {
	r := NewRouter(Deps{SLO: seedSLORouter(t)})
	rec := do(r, "/api/v1/slo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	list := decodeOK[[]sloStatus](t, rec)
	if len(list) != 3 {
		t.Fatalf("定义数 = %d, want 3", len(list))
	}
	byService := map[string]sloStatus{}
	for _, item := range list {
		byService[item.Service] = item
	}

	checkout := byService["checkout"]
	if checkout.Current != 1 || !checkout.PublishingAllowed || checkout.Degraded {
		t.Errorf("checkout 状态异常: %+v", checkout)
	}
	// 15% 错误率对 99% 目标是快速燃尽，必须阻断并建议回滚。
	search := byService["search"]
	if search.PublishingAllowed || search.BudgetRemaining >= 0.2 {
		t.Errorf("search 应被阻断且预算透支: %+v", search)
	}
	catalog := byService["catalog"]
	if catalog.SLIType != slo.SLILatency || catalog.Current != 0.96 {
		t.Errorf("catalog 延迟 SLI 异常: %+v", catalog)
	}
}

func TestGetSLODetail(t *testing.T) {
	r := NewRouter(Deps{SLO: seedSLORouter(t)})
	rec := do(r, "/api/v1/slo/catalog")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	detail := decodeOK[sloDetailResp](t, rec)
	if detail.SLIType != slo.SLILatency || detail.LatencyThresholdMS == nil || *detail.LatencyThresholdMS != 300 {
		t.Fatalf("详情异常: %+v", detail)
	}
	if len(detail.Windows) != len(slo.StandardWindows) {
		t.Fatalf("窗口数 = %d, want %d", len(detail.Windows), len(slo.StandardWindows))
	}
	first := detail.Windows[0]
	if first.LongWindow != "1h" || first.ShortWindow != "5m" || first.Threshold != 14.4 {
		t.Errorf("首个窗口 = %+v", first)
	}
	// 只有 1 小时数据，3d 长窗不完整，详情必须标记降级。
	if !detail.Degraded {
		t.Error("缺少长窗数据时应标记降级")
	}
}

func TestGetSLOBudget(t *testing.T) {
	r := NewRouter(Deps{SLO: seedSLORouter(t)})

	rec := do(r, "/api/v1/slo/checkout/budget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	budget := decodeOK[budgetStatus](t, rec)
	if budget.Window != "1h" || budget.BudgetTotal != 0.01 || budget.BudgetRemaining != 1 {
		t.Errorf("预算数值异常: %+v", budget)
	}
	if !budget.PublishingAllowed || budget.Action != slo.ActionNone || budget.Degraded {
		t.Errorf("健康服务不应阻断: %+v", budget)
	}

	// 显式请求 6h 窗口时数据不完整，必须降级并阻断。
	rec = do(r, "/api/v1/slo/checkout/budget?window=6h")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	sixHour := decodeOK[budgetStatus](t, rec)
	if sixHour.Window != "6h" || !sixHour.Degraded || sixHour.PublishingAllowed {
		t.Errorf("6h 窗口应因完整度不足降级: %+v", sixHour)
	}
	if sixHour.Action != slo.ActionBlockFullRollout {
		t.Errorf("动作 = %s, want %s", sixHour.Action, slo.ActionBlockFullRollout)
	}
}

func TestGetSLODecisionRollback(t *testing.T) {
	r := NewRouter(Deps{SLO: seedSLORouter(t)})
	rec := do(r, "/api/v1/slo/search/budget")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	budget := decodeOK[budgetStatus](t, rec)
	if budget.Action != slo.ActionRollback || budget.PublishingAllowed {
		t.Fatalf("快速燃尽应回滚: %+v", budget)
	}
	hasReason := false
	for _, reason := range budget.Reasons {
		if reason == slo.ReasonFastBurn {
			hasReason = true
		}
	}
	if !hasReason {
		t.Errorf("原因 = %v, 缺少 %s", budget.Reasons, slo.ReasonFastBurn)
	}
}

func TestSLOErrors(t *testing.T) {
	r := NewRouter(Deps{SLO: seedSLORouter(t)})

	if rec := do(r, "/api/v1/slo/unknown"); rec.Code != http.StatusNotFound {
		t.Errorf("未知服务 status = %d, want 404", rec.Code)
	}
	rec := do(r, "/api/v1/slo/checkout/budget?window=2h")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法窗口 status = %d, want 400", rec.Code)
	}
	body := decodeErr(t, rec)
	if len(body.Details) != 1 || body.Details[0].Field != "window" {
		t.Errorf("错误详情 = %+v", body.Details)
	}
}
