package aiops

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestComputeHealthNodeAndCluster(t *testing.T) {
	now := time.Now()
	in := HealthInput{RuleInput: RuleInput{
		Now:       now,
		MetricsAt: now,
		Nodes: []capacity.NodeDiag{
			{
				Name:        "healthy",
				Ready:       true,
				Allocatable: schedcore.Resources{CPUMilli: 4000},
				Usage:       schedcore.Resources{CPUMilli: 1000},
				UsageKnown:  true,
			},
			{
				Name:           "broken",
				Ready:          false,
				MemoryPressure: true,
				Allocatable:    schedcore.Resources{CPUMilli: 4000},
				Usage:          schedcore.Resources{CPUMilli: 1000},
				UsageKnown:     true,
			},
		},
	}}
	report := ComputeHealth(in)
	if len(report.Nodes) != 2 {
		t.Fatalf("节点健康分数量 = %d", len(report.Nodes))
	}
	if report.Nodes[0].Score == nil || *report.Nodes[0].Score != 100 {
		t.Fatalf("健康节点应为 100 分，实际 %+v", report.Nodes[0].Score)
	}
	if report.Nodes[1].Score == nil || *report.Nodes[1].Score >= 50 {
		t.Fatalf("未就绪节点应低于 50 分，实际 %+v", report.Nodes[1].Score)
	}
	if report.Cluster.Score == nil || *report.Cluster.Score <= 0 || *report.Cluster.Score >= 100 {
		t.Fatalf("集群分应为 0–100 之间，实际 %+v", report.Cluster.Score)
	}
}

func TestComputeHealthNoData(t *testing.T) {
	report := ComputeHealth(HealthInput{})
	if report.Cluster.Score != nil {
		t.Fatal("无节点时集群分应为 null")
	}
	tenant := tenantHealth(TenantInput{ID: "t1"}, HealthInput{})
	if tenant.Score != nil {
		t.Fatal("无配额无事件时租户分应为 null")
	}
}
