package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func doJSON(t *testing.T, handler http.Handler, method, target string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func newSLOWriteRouter() (*MemoryStore, http.Handler) {
	store := NewMemoryStore()
	return store, NewRouter(Deps{SLO: store, SLOWriter: store, Idempotency: store})
}

// doJSONWithKey 与 doJSON 相同，额外带上 Idempotency-Key 头。
func doJSONWithKey(t *testing.T, handler http.Handler, method, target string, payload any, key string) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(idempotencyHeader, key)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCreateSLODefinition(t *testing.T) {
	store, r := newSLOWriteRouter()
	rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service":        "checkout",
		"sli_type":       "AVAILABILITY",
		"target":         0.995,
		"window_seconds": 30 * 24 * 60 * 60,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	created := decodeOK[sloDefinitionResp](t, rec)
	if created.Service != "checkout" || created.Target != 0.995 || created.WindowSeconds != 2592000 {
		t.Fatalf("响应异常: %+v", created)
	}

	defs, err := store.ListSLODefinitions(t.Context())
	if err != nil || len(defs) != 1 {
		t.Fatalf("定义写入失败: %v, %+v", err, defs)
	}
}

func TestCreateSLOConflict(t *testing.T) {
	_, r := newSLOWriteRouter()
	payload := map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}
	if rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", payload); rec.Code != http.StatusOK {
		t.Fatalf("首次创建 status = %d", rec.Code)
	}
	rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", payload)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复创建 status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "SLO_DEFINITION_EXISTS" {
		t.Errorf("message = %q", body.Message)
	}
}

func TestCreateSLOValidation(t *testing.T) {
	_, r := newSLOWriteRouter()
	cases := []struct {
		name      string
		payload   any
		wantField string
	}{
		{
			"缺少延迟阈值",
			map[string]any{"service": "search", "sli_type": "LATENCY", "target": 0.95, "window_seconds": 3600},
			"latency_threshold_ms",
		},
		{
			"可用性不应带延迟阈值",
			map[string]any{"service": "search", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600, "latency_threshold_ms": 300},
			"latency_threshold_ms",
		},
		{
			"目标越界",
			map[string]any{"service": "search", "sli_type": "AVAILABILITY", "target": 1, "window_seconds": 3600},
			"target",
		},
		{
			"窗口非法",
			map[string]any{"service": "search", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 0},
			"window_seconds",
		},
		{
			"未知 SLI",
			map[string]any{"service": "search", "sli_type": "ERROR_RATE", "target": 0.99, "window_seconds": 3600},
			"sli_type",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", c.payload)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
			}
			body := decodeErr(t, rec)
			if len(body.Details) != 1 || body.Details[0].Field != c.wantField {
				t.Errorf("details = %+v, want field %s", body.Details, c.wantField)
			}
		})
	}

	rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("空请求体 status = %d, want 400", rec.Code)
	}
}

func TestUpdateSLODefinition(t *testing.T) {
	_, r := newSLOWriteRouter()
	create := map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}
	if rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", create); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 status = %d", rec.Code)
	}

	rec := doJSON(t, r, http.MethodPut, "/api/v1/slo/checkout/AVAILABILITY", map[string]any{
		"target": 0.999, "window_seconds": 6 * 3600,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("更新 status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	updated := decodeOK[sloDefinitionResp](t, rec)
	if updated.Target != 0.999 || updated.WindowSeconds != 21600 {
		t.Errorf("更新结果异常: %+v", updated)
	}

	detail := decodeOK[sloDetailResp](t, do(r, "/api/v1/slo/checkout"))
	if detail.Target != 0.999 {
		t.Errorf("详情未反映更新: %+v", detail)
	}

	if rec := doJSON(t, r, http.MethodPut, "/api/v1/slo/unknown/AVAILABILITY", map[string]any{
		"target": 0.99, "window_seconds": 3600,
	}); rec.Code != http.StatusNotFound {
		t.Errorf("更新不存在的定义 status = %d, want 404", rec.Code)
	}
	if rec := doJSON(t, r, http.MethodPut, "/api/v1/slo/checkout/ERROR_RATE", map[string]any{
		"target": 0.99, "window_seconds": 3600,
	}); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 SLI 路径 status = %d, want 400", rec.Code)
	}
}

func TestDeleteSLODefinition(t *testing.T) {
	_, r := newSLOWriteRouter()
	create := map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}
	if rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", create); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 status = %d", rec.Code)
	}

	rec := doJSON(t, r, http.MethodDelete, "/api/v1/slo/checkout/AVAILABILITY", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("删除 status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(r, "/api/v1/slo/checkout"); rec.Code != http.StatusNotFound {
		t.Errorf("删除后详情 status = %d, want 404", rec.Code)
	}
	if rec := doJSON(t, r, http.MethodDelete, "/api/v1/slo/checkout/AVAILABILITY", nil); rec.Code != http.StatusNotFound {
		t.Errorf("重复删除 status = %d, want 404", rec.Code)
	}
}

func TestSLOWriteUnavailableWithoutWriter(t *testing.T) {
	r := NewRouter(Deps{SLO: NewMemoryStore()})
	rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("未配置写存储 status = %d, want 503", rec.Code)
	}
}

func TestCreateSLOIdempotentReplay(t *testing.T) {
	store, r := newSLOWriteRouter()
	payload := map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}
	first := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", payload, "create-checkout")
	if first.Code != http.StatusOK {
		t.Fatalf("首次创建 status = %d, body=%s", first.Code, first.Body.String())
	}
	second := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", payload, "create-checkout")
	if second.Code != http.StatusOK {
		t.Fatalf("重放 status = %d, body=%s", second.Code, second.Body.String())
	}
	// 重放必须返回首次的原始响应，trace_id 也不重新生成。
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Errorf("重放响应不一致:\nfirst=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	defs, err := store.ListSLODefinitions(t.Context())
	if err != nil || len(defs) != 1 {
		t.Fatalf("重放不应重复创建: err=%v, defs=%+v", err, defs)
	}
}

func TestIdempotencyKeyReusedWithDifferentBody(t *testing.T) {
	_, r := newSLOWriteRouter()
	first := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}, "shared-key")
	if first.Code != http.StatusOK {
		t.Fatalf("首次创建 status = %d, body=%s", first.Code, first.Body.String())
	}

	rec := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.999, "window_seconds": 3600,
	}, "shared-key")
	if rec.Code != http.StatusConflict {
		t.Fatalf("同键异体 status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeErr(t, rec)
	if body.Code != CodeIdempotencyConflict || body.Message != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("错误体异常: %+v", body)
	}
}

func TestIdempotencyKeyReusedAcrossEndpoints(t *testing.T) {
	_, r := newSLOWriteRouter()
	if rec := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}, "cross-scope"); rec.Code != http.StatusOK {
		t.Fatalf("首次创建 status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rec := doJSONWithKey(t, r, http.MethodPut, "/api/v1/slo/checkout/AVAILABILITY", map[string]any{
		"target": 0.999, "window_seconds": 3600,
	}, "cross-scope")
	if rec.Code != http.StatusConflict {
		t.Fatalf("同键跨接口 status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "IDEMPOTENCY_KEY_REUSED" {
		t.Errorf("错误体异常: %+v", body)
	}
}

func TestUpdateSLOIdempotentReplay(t *testing.T) {
	_, r := newSLOWriteRouter()
	if rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 status = %d", rec.Code)
	}

	payload := map[string]any{"target": 0.999, "window_seconds": 21600}
	first := doJSONWithKey(t, r, http.MethodPut, "/api/v1/slo/checkout/AVAILABILITY", payload, "update-checkout")
	if first.Code != http.StatusOK {
		t.Fatalf("首次更新 status = %d, body=%s", first.Code, first.Body.String())
	}
	second := doJSONWithKey(t, r, http.MethodPut, "/api/v1/slo/checkout/AVAILABILITY", payload, "update-checkout")
	if second.Code != http.StatusOK {
		t.Fatalf("重放 status = %d, body=%s", second.Code, second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Errorf("重放响应不一致:\nfirst=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	if detail := decodeOK[sloDetailResp](t, do(r, "/api/v1/slo/checkout")); detail.Target != 0.999 {
		t.Errorf("详情未反映更新: %+v", detail)
	}
}

func TestDeleteSLOIdempotentReplay(t *testing.T) {
	_, r := newSLOWriteRouter()
	if rec := doJSON(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}); rec.Code != http.StatusOK {
		t.Fatalf("创建失败 status = %d", rec.Code)
	}

	if rec := doJSONWithKey(t, r, http.MethodDelete, "/api/v1/slo/checkout/AVAILABILITY", nil, "delete-checkout"); rec.Code != http.StatusNoContent {
		t.Fatalf("首次删除 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	// 定义已删除，但同键重放仍应返回首次的 204，而不是 404。
	rec := doJSONWithKey(t, r, http.MethodDelete, "/api/v1/slo/checkout/AVAILABILITY", nil, "delete-checkout")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("重放 status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 不应有响应体: %s", rec.Body.String())
	}
}

func TestIdempotencyKeyTooLong(t *testing.T) {
	_, r := newSLOWriteRouter()
	rec := doJSONWithKey(t, r, http.MethodPost, "/api/v1/slo", map[string]any{
		"service": "checkout", "sli_type": "AVAILABILITY", "target": 0.99, "window_seconds": 3600,
	}, strings.Repeat("k", 129))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超长幂等键 status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	body := decodeErr(t, rec)
	if len(body.Details) != 1 || body.Details[0].Field != idempotencyHeader {
		t.Errorf("details = %+v", body.Details)
	}
}
