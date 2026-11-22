package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	m.Run()
}

type okBody struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	TraceID string          `json:"trace_id"`
}

type errBody struct {
	Code    int           `json:"code"`
	Message string        `json:"message"`
	Details []ErrorDetail `json:"details"`
	TraceID string        `json:"trace_id"`
}

func do(r http.Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var body errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v, body=%s", err, rec.Body.String())
	}
	return body
}

func seedDecisions(t *testing.T, s *MemoryStore, n int, tenant string) []PlacementDecision {
	t.Helper()
	base := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	out := make([]PlacementDecision, 0, n)
	for i := 0; i < n; i++ {
		tenantID := tenant
		d := PlacementDecision{
			ID:           uuid.NewString(),
			Status:       schedcore.StatusOK,
			TenantID:     &tenantID,
			PolicyID:     "11111111-1111-1111-1111-111111111111",
			SnapshotHash: "sha256:seed",
			Scores:       []schedcore.NodeScore{},
			Filtered:     []schedcore.FilteredNode{},
			CreatedAt:    base.Add(time.Duration(i) * time.Minute),
		}
		s.AddDecision(d)
		out = append(out, d)
	}
	return out
}

func TestHealthzOK(t *testing.T) {
	rec := do(NewRouter(Deps{}), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("status 字段 = %v, want ok", body["status"])
	}
}

func TestHealthzDependencyFailure(t *testing.T) {
	d := Deps{Checks: map[string]func(context.Context) error{
		"postgres": func(context.Context) error { return errors.New("connection refused") },
		"k8s":      func(context.Context) error { return nil },
	}}
	rec := do(NewRouter(d), "/healthz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	var body struct {
		Status string   `json:"status"`
		Failed []string `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Failed) != 1 || body.Failed[0] != "postgres" {
		t.Fatalf("failed = %v, want [postgres]", body.Failed)
	}
}

func TestTraceIDPropagation(t *testing.T) {
	r := NewRouter(Deps{})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req.Header.Set(traceIDHeader, "trace-abc.123")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if got := rec.Header().Get(traceIDHeader); got != "trace-abc.123" {
		t.Fatalf("响应头 trace_id = %q, want trace-abc.123", got)
	}
	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.TraceID != "trace-abc.123" {
		t.Fatalf("body.trace_id = %q", body.TraceID)
	}
}

func TestTraceIDGeneratedWhenMissing(t *testing.T) {
	rec := do(NewRouter(Deps{}), "/api/v1/policies")
	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if _, err := uuid.Parse(body.TraceID); err != nil {
		t.Fatalf("自动生成的 trace_id 不是 UUID: %q", body.TraceID)
	}
}

func TestTraceIDRejectsIllegalValue(t *testing.T) {
	r := NewRouter(Deps{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/policies", nil)
	req.Header.Set(traceIDHeader, "bad value with spaces")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.TraceID == "bad value with spaces" {
		t.Fatal("非法 trace_id 应被替换，而不是原样回显")
	}
	if _, err := uuid.Parse(body.TraceID); err != nil {
		t.Fatalf("替换后的 trace_id 不是 UUID: %q", body.TraceID)
	}
}

func TestListPoliciesEmpty(t *testing.T) {
	rec := do(NewRouter(Deps{}), "/api/v1/policies")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Code != CodeOK {
		t.Fatalf("code = %d, want 0", body.Code)
	}
	if string(body.Data) != "[]" {
		t.Fatalf("data = %s, want []", body.Data)
	}
}

func TestListPolicies(t *testing.T) {
	store := NewMemoryStore()
	store.AddPolicy(Policy{
		ID:        uuid.NewString(),
		Name:      "balanced-default",
		Strategy:  schedcore.Balanced,
		Weights:   schedcore.Weights{CPU: 0.18, Mem: 0.18, Balance: 0.25, Pack: 0.10, Load: 0.15, Fair: 0.10, Frag: 0.04},
		IsDefault: true,
	})
	rec := do(NewRouter(Deps{Policies: store}), "/api/v1/policies")

	var body okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	var policies []Policy
	if err := json.Unmarshal(body.Data, &policies); err != nil {
		t.Fatalf("data 不是 Policy 数组: %v", err)
	}
	if len(policies) != 1 || policies[0].Name != "balanced-default" {
		t.Fatalf("policies = %+v", policies)
	}
	if err := policies[0].Weights.Validate(); err != nil {
		t.Fatalf("预设权重未通过校验: %v", err)
	}
}

func TestListDecisionsPagination(t *testing.T) {
	store := NewMemoryStore()
	seeded := seedDecisions(t, store, 5, "22222222-2222-2222-2222-222222222222")
	r := NewRouter(Deps{Decisions: store})

	var ids []string
	cursor := ""
	for page := 0; page < 3; page++ {
		target := "/api/v1/placement/decisions?limit=2"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(r, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 页 status = %d, body=%s", page, rec.Code, rec.Body.String())
		}
		var body struct {
			Code int          `json:"code"`
			Data decisionPage `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是合法 JSON: %v", err)
		}
		for _, item := range body.Data.Items {
			ids = append(ids, item.ID)
		}
		if page < 2 {
			if body.Data.NextCursor == nil {
				t.Fatalf("第 %d 页应返回 next_cursor", page)
			}
			cursor = *body.Data.NextCursor
		} else if body.Data.NextCursor != nil {
			t.Fatalf("最后一页 next_cursor 应为 null, got %q", *body.Data.NextCursor)
		}
	}

	if len(ids) != len(seeded) {
		t.Fatalf("共取回 %d 条, want %d", len(ids), len(seeded))
	}
	for i := range ids {
		want := seeded[len(seeded)-1-i].ID // 按 created_at 倒序
		if ids[i] != want {
			t.Fatalf("第 %d 条 = %s, want %s（应为倒序且不重不漏）", i, ids[i], want)
		}
	}
}

func TestDecisionFilters(t *testing.T) {
	store := NewMemoryStore()
	const tenantA = "22222222-2222-2222-2222-222222222222"
	const tenantB = "33333333-3333-3333-3333-333333333333"
	seedDecisions(t, store, 3, tenantA)
	seedDecisions(t, store, 2, tenantB)
	r := NewRouter(Deps{Decisions: store})

	rec := do(r, "/api/v1/placement/decisions?tenant_id="+tenantA)
	var body struct {
		Data decisionPage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Data.Items) != 3 {
		t.Fatalf("tenant 过滤后 %d 条, want 3", len(body.Data.Items))
	}
	for _, item := range body.Data.Items {
		if item.TenantID == nil || *item.TenantID != tenantA {
			t.Fatalf("混入了其他租户的数据: %+v", item)
		}
	}
}

func TestListDecisionsRejectsBadQuery(t *testing.T) {
	r := NewRouter(Deps{})
	cases := []struct {
		name  string
		query string
		field string
	}{
		{"limit 为 0", "limit=0", "limit"},
		{"limit 超上限", "limit=201", "limit"},
		{"limit 非数字", "limit=abc", "limit"},
		{"tenant_id 非 UUID", "tenant_id=not-a-uuid", "tenant_id"},
		{"policy_id 非 UUID", "policy_id=not-a-uuid", "policy_id"},
		{"cursor 非 base64", "cursor=%21%21%21", "cursor"},
		{"cursor 已失效", "cursor=" + encodeCursor("missing"), "cursor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(r, "/api/v1/placement/decisions?"+tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			body := decodeErr(t, rec)
			if body.Code != CodeInvalidArgument {
				t.Fatalf("code = %d, want 40001", body.Code)
			}
			if len(body.Details) != 1 || body.Details[0].Field != tc.field {
				t.Fatalf("details = %+v, want field=%s", body.Details, tc.field)
			}
			if body.TraceID == "" {
				t.Fatal("错误响应必须带 trace_id")
			}
		})
	}
}

func TestGetDecision(t *testing.T) {
	store := NewMemoryStore()
	seeded := seedDecisions(t, store, 1, "22222222-2222-2222-2222-222222222222")
	r := NewRouter(Deps{Decisions: store})

	rec := do(r, "/api/v1/placement/decisions/"+seeded[0].ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data PlacementDecision `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body.Data.ID != seeded[0].ID {
		t.Fatalf("id = %s, want %s", body.Data.ID, seeded[0].ID)
	}

	for _, id := range []string{uuid.NewString(), "not-a-uuid"} {
		rec := do(r, "/api/v1/placement/decisions/"+id)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("id=%s status = %d, want 404", id, rec.Code)
		}
		if got := decodeErr(t, rec).Code; got != CodeNotFound {
			t.Fatalf("id=%s code = %d, want 40401", id, got)
		}
	}
}

func TestUnknownRoute(t *testing.T) {
	rec := do(NewRouter(Deps{}), "/api/v1/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	body := decodeErr(t, rec)
	if body.Code != CodeNotFound || body.TraceID == "" {
		t.Fatalf("body = %+v", body)
	}
}

func TestRecoverConvertsPanic(t *testing.T) {
	r := gin.New()
	r.Use(TraceID(), Recover())
	r.GET("/boom", func(*gin.Context) { panic("boom") })

	rec := do(r, "/boom")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	body := decodeErr(t, rec)
	if body.Code != CodeInternal || body.Message != msgInternal {
		t.Fatalf("body = %+v", body)
	}
	if body.TraceID == "" {
		t.Fatal("500 响应必须带 trace_id")
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	cases := map[int]int{
		CodeOK:                  http.StatusOK,
		CodeInvalidArgument:     http.StatusBadRequest,
		CodeUnauthenticated:     http.StatusUnauthorized,
		CodeForbidden:           http.StatusForbidden,
		CodeNotFound:            http.StatusNotFound,
		CodeIdempotencyConflict: http.StatusConflict,
		CodeIllegalTransition:   http.StatusConflict,
		CodeResourceLocked:      http.StatusConflict,
		CodeBusinessRejected:    http.StatusUnprocessableEntity,
		CodeRateLimited:         http.StatusTooManyRequests,
		CodeInternal:            http.StatusInternalServerError,
		CodeUnavailable:         http.StatusServiceUnavailable,
	}
	for code, want := range cases {
		if got := httpStatus(code); got != want {
			t.Errorf("httpStatus(%d) = %d, want %d", code, got, want)
		}
	}
}

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	store := NewMemoryStore()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			store.AddDecision(PlacementDecision{ID: uuid.NewString(), CreatedAt: time.Now()})
		}
	}()
	for i := 0; i < 50; i++ {
		if _, err := store.ListDecisions(context.Background(), DecisionFilter{}); err != nil {
			t.Fatalf("ListDecisions: %v", err)
		}
	}
	<-done
}
