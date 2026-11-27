package aiops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
)

type fakeStore struct {
	items  map[string]Incident
	diags  map[string][]Diagnosis
	nextID int
}

func newFakeStore() *fakeStore {
	return &fakeStore{items: map[string]Incident{}, diags: map[string][]Diagnosis{}}
}

func (s *fakeStore) ListIncidents(_ context.Context, f Filter) ([]Incident, error) {
	out := make([]Incident, 0)
	for _, inc := range s.items {
		if f.Status != "" && inc.Status != f.Status {
			continue
		}
		out = append(out, inc)
	}
	return out, nil
}

func (s *fakeStore) GetIncident(_ context.Context, id string) (Detail, bool, error) {
	inc, ok := s.items[id]
	if !ok {
		return Detail{}, false, nil
	}
	detail := Detail{Incident: inc, RecoveryActions: []RecoveryAction{}}
	if list := s.diags[id]; len(list) > 0 {
		latest := list[len(list)-1]
		detail.Diagnosis = &latest
	}
	return detail, true, nil
}

func (s *fakeStore) CreateIncident(_ context.Context, inc Incident, diag Diagnosis) (Incident, error) {
	s.nextID++
	inc.ID = fmt.Sprintf("inc-%d", s.nextID)
	s.items[inc.ID] = inc
	diag.IncidentID = inc.ID
	diag.ID = fmt.Sprintf("diag-%d", s.nextID)
	s.diags[inc.ID] = append(s.diags[inc.ID], diag)
	return inc, nil
}

func (s *fakeStore) EscalateIncident(_ context.Context, id string, sev Severity) error {
	inc, ok := s.items[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	inc.Severity = sev
	s.items[id] = inc
	return nil
}

func (s *fakeStore) ResolveIncident(_ context.Context, id string, at time.Time) error {
	inc, ok := s.items[id]
	if !ok {
		return fmt.Errorf("not found")
	}
	inc.Status = "RESOLVED"
	inc.ResolvedAt = &at
	s.items[id] = inc
	return nil
}

func (s *fakeStore) AppendDiagnosis(_ context.Context, incidentID string, diag Diagnosis) (Diagnosis, error) {
	diag.IncidentID = incidentID
	s.diags[incidentID] = append(s.diags[incidentID], diag)
	return diag, nil
}

func TestDetectorCreatesDeduplicatesAndResolves(t *testing.T) {
	store := newFakeStore()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	diag := capacity.Diagnostics{
		TakenAt:   now,
		MetricsAt: now,
		Nodes:     []capacity.NodeDiag{{Name: "n1", Ready: false}},
	}
	source := SourceFuncs{
		DiagnosticsFn: func(context.Context) (capacity.Diagnostics, error) { return diag, nil },
	}
	det := NewDetector(store, source, time.Minute, nil)
	det.now = func() time.Time { return now }

	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("首次扫描: %v", err)
	}
	if len(store.items) != 1 {
		t.Fatalf("期望创建 1 个事件，实际 %d", len(store.items))
	}

	// 同一条件重复扫描不应产生重复事件。
	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("重复扫描: %v", err)
	}
	if len(store.items) != 1 {
		t.Fatalf("不应重复创建事件，实际 %d", len(store.items))
	}

	// 条件消失：第一次保留，第二次自动恢复。
	diag.Nodes[0].Ready = true
	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("恢复扫描 1: %v", err)
	}
	for _, inc := range store.items {
		if inc.Status != "OPEN" {
			t.Fatal("连续 1 次未命中不应立即恢复")
		}
	}
	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("恢复扫描 2: %v", err)
	}
	for _, inc := range store.items {
		if inc.Status != "RESOLVED" || inc.ResolvedAt == nil {
			t.Fatalf("连续 2 次未命中应自动恢复: %+v", inc)
		}
	}
}

func TestDetectorEscalatesSeverity(t *testing.T) {
	store := newFakeStore()
	now := time.Now()
	diag := capacity.Diagnostics{
		MetricsAt: now,
		Pods: []capacity.PodDiag{{
			Namespace: "prod", Name: "web", Phase: "Running", Restarts: 4,
			WaitingReason: "CrashLoopBackOff", CreatedAt: now.Add(-time.Hour),
		}},
	}
	source := SourceFuncs{
		DiagnosticsFn: func(context.Context) (capacity.Diagnostics, error) { return diag, nil },
	}
	det := NewDetector(store, source, time.Minute, nil)
	det.now = func() time.Time { return now }

	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("首次扫描: %v", err)
	}
	diag.Pods[0].Restarts = 12
	if err := det.Scan(context.Background()); err != nil {
		t.Fatalf("升级扫描: %v", err)
	}
	for _, inc := range store.items {
		if inc.Type != IncidentCrashLoop {
			continue
		}
		if inc.Severity != SeverityCritical {
			t.Fatalf("重启次数增加后应升级为 CRITICAL，实际 %s", inc.Severity)
		}
	}
}
