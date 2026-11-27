package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

type staticTenants []TenantInfo

func (s staticTenants) ListTenants(context.Context) ([]TenantInfo, error) { return s, nil }

func TestIncidentsListDetailAndDiagnosis(t *testing.T) {
	store := NewMemoryStore()
	created, err := store.CreateIncident(context.Background(), aiops.Incident{
		Target:    "web",
		Namespace: "prod",
		Type:      aiops.IncidentCrashLoop,
		Severity:  aiops.SeverityCritical,
		Status:    "OPEN",
		FirstSeen: time.Now().UTC(),
	}, aiops.Diagnosis{
		Facts: []aiops.Fact{{
			Stage: "OBSERVED", At: time.Now().UTC(), Object: "pod/prod/web", Summary: "容器反复重启",
		}},
		Inferences: []aiops.Inference{{
			RootCause: "启动即退出", Confidence: "HIGH", Evidence: []string{"restartCount=12"},
		}},
	})
	if err != nil {
		t.Fatalf("seed incident: %v", err)
	}

	r := NewRouter(Deps{Incidents: store, IncidentWriter: store})
	rec := do(r, "/api/v1/incidents?status=OPEN")
	if rec.Code != http.StatusOK {
		t.Fatalf("列表状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var listBody okBody
	if err := json.Unmarshal(rec.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var page incidentPage
	if err := json.Unmarshal(listBody.Data, &page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("事件列表不匹配: %+v", page)
	}

	rec = do(r, "/api/v1/incidents/"+created.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var detailBody okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &detailBody)
	var detail aiops.Detail
	if err := json.Unmarshal(detailBody.Data, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.Diagnosis == nil || len(detail.Diagnosis.Facts) != 1 {
		t.Fatalf("详情应包含诊断事实: %+v", detail)
	}

	// 没有实时视图时，重新诊断应返回"未复现"的低保真报告。
	req := httptest.NewRequest(http.MethodPost, "/api/v1/diagnosis/run",
		strings.NewReader(`{"incident_id":"`+created.ID+`"}`))
	req.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	r.ServeHTTP(postRec, req)
	if postRec.Code != http.StatusOK {
		t.Fatalf("诊断状态码 = %d, body=%s", postRec.Code, postRec.Body.String())
	}
	detail, ok, _ := store.GetIncident(context.Background(), created.ID)
	if !ok || detail.Diagnosis == nil || len(detail.Diagnosis.Inferences) == 0 {
		t.Fatalf("重新诊断未写入: %+v", detail)
	}
}

func TestIncidentsNotFound(t *testing.T) {
	r := NewRouter(Deps{})
	rec := do(r, "/api/v1/incidents/00000000-0000-0000-0000-000000000000")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知事件应返回 404，实际 %d", rec.Code)
	}
}

func TestMonitorOverviewAndSeries(t *testing.T) {
	now := time.Now().UTC()
	base := now.Truncate(time.Minute).Add(2 * time.Second)
	history := metrics.NewStore()
	history.Observe("n1", schedcore.Resources{CPUMilli: 1500, MemBytes: 2 << 30}, base.Add(-time.Second))
	history.Observe("n1", schedcore.Resources{CPUMilli: 1600, MemBytes: 2 << 30}, base)

	observer := func(context.Context) (capacity.Diagnostics, error) {
		return capacity.Diagnostics{
			TakenAt:   now,
			MetricsAt: base,
			Nodes: []capacity.NodeDiag{{
				Name:        "n1",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
				Requested:   schedcore.Resources{CPUMilli: 1000, MemBytes: 1 << 30},
				Usage:       schedcore.Resources{CPUMilli: 1600, MemBytes: 2 << 30},
				UsageKnown:  true,
				PodCount:    3,
			}},
			TenantUsage: map[string]schedcore.Resources{"team-a": {CPUMilli: 1000, MemBytes: 1 << 30}},
		}, nil
	}
	store := NewMemoryStore()
	r := NewRouter(Deps{
		Observer:       observer,
		Metrics:        history,
		TenantCatalog:  staticTenants{{ID: "team-a", Name: "Team A", Weight: 1, Quota: schedcore.Resources{CPUMilli: 2000, MemBytes: 4 << 30}}},
		Incidents:      store,
		IncidentWriter: store,
	})

	rec := do(r, "/api/v1/monitor/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview 状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var overview monitorOverviewResponse
	if err := json.Unmarshal(body.Data, &overview); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if overview.Cluster.NodesTotal != 1 || overview.Cluster.CPUUsageMilli != 1600 {
		t.Fatalf("集群汇总错误: %+v", overview.Cluster)
	}
	if overview.Cluster.Health.Score == nil || *overview.Cluster.Health.Score != 100 {
		t.Fatalf("健康节点集群分应为 100: %+v", overview.Cluster.Health)
	}
	if len(overview.Tenants) != 1 || overview.Tenants[0].QuotaRatio != 0.5 {
		t.Fatalf("租户汇总错误: %+v", overview.Tenants)
	}

	rec = do(r, "/api/v1/monitor/series?window=1h")
	if rec.Code != http.StatusOK {
		t.Fatalf("series 状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var seriesBody okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &seriesBody)
	var series monitorSeriesResponse
	if err := json.Unmarshal(seriesBody.Data, &series); err != nil {
		t.Fatalf("decode series: %v", err)
	}
	if len(series.Nodes) != 1 || len(series.Nodes[0].Points) != 1 || len(series.Cluster) != 1 {
		t.Fatalf("序列聚合错误: %+v", series)
	}
}
