package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestForecastCapacityDegradedWithoutData(t *testing.T) {
	r := NewRouter(Deps{
		Metrics: metrics.NewStore(),
		Observer: func(context.Context) (capacity.Diagnostics, error) {
			return capacity.Diagnostics{}, nil
		},
	})
	rec := do(r, "/api/v1/forecast/capacity?horizon=30m")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var resp forecastCapacityResponse
	if err := json.Unmarshal(body.Data, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Degraded || resp.Confidence != "NONE" || len(resp.Nodes) != 0 {
		t.Fatalf("无数据应 degraded+NONE: %+v", resp)
	}
}

func TestForecastCapacityInvalidHorizon(t *testing.T) {
	r := NewRouter(Deps{Metrics: metrics.NewStore()})
	rec := do(r, "/api/v1/forecast/capacity?horizon=7d")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 horizon 应 400: %d", rec.Code)
	}
}

func TestForecastCapacityWithTrend(t *testing.T) {
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Minute)
	history := metrics.NewStore()
	// 31 个 1 分钟样本（截至当前），CPU 线性 590→890（每分钟 +10 毫核），内存恒定 2GiB。
	for i := 0; i <= 30; i++ {
		history.Observe("n1", schedcore.Resources{
			CPUMilli: 590 + 10*int64(i),
			MemBytes: 2 << 30,
		}, base.Add(time.Duration(i)*time.Minute))
	}
	r := NewRouter(Deps{
		Metrics: history,
		Observer: func(context.Context) (capacity.Diagnostics, error) {
			return capacity.Diagnostics{
				TakenAt:   base.Add(30 * time.Minute),
				MetricsAt: base.Add(30 * time.Minute),
				Nodes: []capacity.NodeDiag{{
					Name:        "n1",
					Ready:       true,
					Allocatable: schedcore.Resources{CPUMilli: 1000, MemBytes: 8 << 30},
					Usage:       schedcore.Resources{CPUMilli: 890, MemBytes: 2 << 30},
					UsageKnown:  true,
				}},
			}, nil
		},
	})

	rec := do(r, "/api/v1/forecast/capacity?horizon=30m")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var resp forecastCapacityResponse
	if err := json.Unmarshal(body.Data, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Degraded || resp.Confidence != "HIGH" {
		t.Fatalf("完美线性应为 HIGH 置信: degraded=%v conf=%s", resp.Degraded, resp.Confidence)
	}
	if len(resp.Nodes) != 1 || len(resp.Nodes[0].Prediction) == 0 || len(resp.Nodes[0].Band) == 0 {
		t.Fatalf("应输出预测点与置信带: %+v", resp.Nodes)
	}
	// CPU 占比 0.89 ≥ 0.6，最后值 890 → 90% 目标 900，约 1 分钟。
	tt90 := resp.Nodes[0].TimeToFull90
	if tt90 == nil || *tt90 > 120 {
		t.Fatalf("90%% 耗尽应约 1 分钟: %v", tt90)
	}
	if resp.Cluster == nil || resp.Cluster.Confidence != "HIGH" {
		t.Fatalf("集群级拟合缺失或置信错误: %+v", resp.Cluster)
	}
}

func TestRightsizingClassification(t *testing.T) {
	podStore := metrics.NewPodUsageStore()
	base := time.Now().UTC()
	podStore.ObservePods("n1", map[metrics.PodKey]schedcore.Resources{
		{Namespace: "prod", Name: "fat"}:   {CPUMilli: 100, MemBytes: 100 << 20}, // request 1000/1Gi → 超配
		{Namespace: "prod", Name: "thin"}:  {CPUMilli: 99, MemBytes: 99 << 20},   // request 100/100Mi → 低配
		{Namespace: "prod", Name: "mixed"}: {CPUMilli: 500, MemBytes: 100 << 20}, // 只有一维超限 → OK
	}, base)

	observer := func(context.Context) (capacity.Diagnostics, error) {
		return capacity.Diagnostics{
			TakenAt: base,
			Nodes: []capacity.NodeDiag{{
				Name: "n1", Ready: true,
				Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
			}},
			Pods: []capacity.PodDiag{
				{Namespace: "prod", Name: "fat", NodeName: "n1", Phase: "Running",
					Requests: schedcore.Resources{CPUMilli: 1000, MemBytes: 1 << 30}},
				{Namespace: "prod", Name: "thin", NodeName: "n1", Phase: "Running",
					Requests: schedcore.Resources{CPUMilli: 100, MemBytes: 100 << 20}},
				{Namespace: "prod", Name: "mixed", NodeName: "n1", Phase: "Running",
					Requests: schedcore.Resources{CPUMilli: 1000, MemBytes: 500 << 20}},
				{Namespace: "prod", Name: "nousetats", NodeName: "n1", Phase: "Running",
					Requests: schedcore.Resources{CPUMilli: 200, MemBytes: 200 << 20}},
				{Namespace: "prod", Name: "norequest", NodeName: "n1", Phase: "Running"},
			},
		}, nil
	}
	r := NewRouter(Deps{Observer: observer, PodUsage: podStore})
	rec := do(r, "/api/v1/rightsizing")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body okBody
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	var resp rightsizingResponse
	if err := json.Unmarshal(body.Data, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byName := make(map[string]rightsizingItem, len(resp.Items))
	for _, item := range resp.Items {
		byName[item.Name] = item
	}
	if len(resp.Items) != 4 {
		t.Fatalf("无 request 的 Pod 应跳过，实际 %d 项", len(resp.Items))
	}
	if got := byName["fat"].Classification; got != "OVER_PROVISIONED" {
		t.Errorf("fat 应为超配: %s", got)
	}
	if got := byName["thin"].Classification; got != "UNDER_PROVISIONED" {
		t.Errorf("thin 应为低配: %s", got)
	}
	if got := byName["mixed"].Classification; got != "OK" {
		t.Errorf("mixed 应为 OK: %s", got)
	}
	if got := byName["nousetats"].Classification; got != "UNKNOWN" || byName["nousetats"].Usage != nil {
		t.Errorf("缺用量应为 UNKNOWN: %s", got)
	}
	if resp.OverProvisioned != 1 || resp.UnderProvisioned != 1 || resp.Unknown != 1 {
		t.Fatalf("分类计数错误: %+v", resp)
	}
	// 超配建议应比 request 明显更小。
	if rec2 := byName["fat"].Recommendation; rec2 == nil || rec2.CPUMilli >= 1000 {
		t.Errorf("超配建议应收缩 request: %+v", rec2)
	}
}
