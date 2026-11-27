package aiops

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestDetectRuleCoverage(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	in := RuleInput{
		Now:       now,
		MetricsAt: now.Add(-2 * time.Minute),
		Nodes: []capacity.NodeDiag{{
			Name:           "n1",
			Ready:          false,
			MemoryPressure: true,
			Allocatable:    schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30},
			Usage:          schedcore.Resources{CPUMilli: 3900, MemBytes: 7<<30 + 512<<20},
			UsageKnown:     true,
		}},
		Pods: []capacity.PodDiag{
			{
				Namespace: "prod", Name: "crash", NodeName: "n1", Phase: "Running",
				Restarts: 12, WaitingReason: "CrashLoopBackOff", LastTerminatedReason: "OOMKilled",
				CreatedAt: now.Add(-20 * time.Minute), StartTime: now.Add(-20 * time.Minute),
			},
			{
				Namespace: "prod", Name: "image", NodeName: "n1", Phase: "Pending",
				WaitingReason: "ImagePullBackOff", CreatedAt: now.Add(-20 * time.Minute),
			},
			{
				Namespace: "batch", Name: "pending", Phase: "Pending",
				CreatedAt: now.Add(-10 * time.Minute),
			},
			{
				Namespace: "prod", Name: "notready", NodeName: "n1", Phase: "Running",
				CreatedAt: now.Add(-20 * time.Minute), StartTime: now.Add(-10 * time.Minute),
			},
		},
		Tenants: []TenantInput{{
			ID:        "team-a",
			Quota:     schedcore.Resources{CPUMilli: 1000, MemBytes: 1 << 30},
			Requested: schedcore.Resources{CPUMilli: 950, MemBytes: 950 << 20},
		}},
	}

	findings := Detect(in)
	byType := make(map[IncidentType]Severity, len(findings))
	for _, f := range findings {
		byType[f.Type] = f.Severity
	}

	want := map[IncidentType]Severity{
		IncidentNodeNotReady:     SeverityCritical,
		IncidentNodePressure:     SeverityWarn,
		IncidentNodeHighUsage:    SeverityWarn,
		IncidentMetricsStale:     SeverityWarn,
		IncidentCrashLoop:        SeverityCritical,
		IncidentOOMKilled:        SeverityCritical,
		IncidentImagePullBackOff: SeverityWarn,
		IncidentPodPendingLong:   SeverityWarn,
		IncidentPodReadinessFail: SeverityWarn,
		IncidentQuotaNear:        SeverityWarn,
	}
	for typ, sev := range want {
		if got, ok := byType[typ]; !ok {
			t.Errorf("缺少事件 %s", typ)
		} else if got != sev {
			t.Errorf("%s 严重度期望 %s，实际 %s", typ, sev, got)
		}
	}
}

func TestDetectMetricsStaleSkippedBeforeFirstSample(t *testing.T) {
	findings := Detect(RuleInput{Now: time.Now()})
	for _, f := range findings {
		if f.Type == IncidentMetricsStale {
			t.Fatal("首轮无样本不应直接产生指标过期事件")
		}
	}
}

func TestDetectQuotaCritical(t *testing.T) {
	findings := Detect(RuleInput{
		Now: time.Now(),
		Tenants: []TenantInput{{
			ID:        "team-b",
			Quota:     schedcore.Resources{CPUMilli: 1000, MemBytes: 1 << 30},
			Requested: schedcore.Resources{CPUMilli: 1000, MemBytes: 2 << 30},
		}},
	})
	if len(findings) != 1 || findings[0].Severity != SeverityCritical {
		t.Fatalf("配额用尽应为 CRITICAL，实际 %+v", findings)
	}
}

func TestDetectDoesNotKeepRecoveredPodsOpen(t *testing.T) {
	now := time.Now()
	findings := Detect(RuleInput{
		Now: now,
		Pods: []capacity.PodDiag{{
			Namespace: "prod", Name: "web", Phase: "Running",
			Restarts: 8, Ready: true,
			LastTerminatedReason: "OOMKilled",
			LastTerminatedAt:     now.Add(-2 * time.Hour),
		}},
	})
	for _, f := range findings {
		if f.Type == IncidentCrashLoop || f.Type == IncidentOOMKilled {
			t.Fatalf("已恢复的 Pod 不应继续产生重启/OOM 事件: %+v", f)
		}
	}
}
