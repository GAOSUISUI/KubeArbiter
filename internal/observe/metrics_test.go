package observe

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposed(t *testing.T) {
	reg := NewRegistry()
	m := NewMetrics(reg)
	if m == nil {
		t.Fatal("NewMetrics 返回 nil")
	}

	m.TaskStarted()
	m.TaskFinished("PLACEMENT_APPLY", "SUCCEEDED", 150*time.Millisecond)
	m.TaskClaimFailed()
	m.TaskReaped("RETRYING")

	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	wants := []string{
		`kubearbiter_worker_tasks_total{status="SUCCEEDED",type="PLACEMENT_APPLY"} 1`,
		`kubearbiter_worker_task_duration_seconds_count{status="SUCCEEDED",type="PLACEMENT_APPLY"} 1`,
		`kubearbiter_worker_inflight_tasks 0`,
		`kubearbiter_worker_claim_errors_total 1`,
		`kubearbiter_worker_reaped_tasks_total{status="RETRYING"} 1`,
	}
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("指标输出缺少 %q\n---\n%s", want, body)
		}
	}
}

func TestMetricsNilIsNoop(t *testing.T) {
	var m *Metrics
	m.TaskStarted()
	m.TaskFinished("PLACEMENT_APPLY", "FAILED", time.Second)
	m.TaskClaimFailed()
	m.TaskReaped("FAILED")

	if m := NewMetrics(nil); m != nil {
		t.Errorf("registry 为 nil 时应返回 nil, got %+v", m)
	}
}
