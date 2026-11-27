package api

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
	"github.com/kubearbiter/kubearbiter/internal/slo"
)

// ListIncidents 返回按发现时间倒序的事件；过滤条件为空时返回全部。
func (s *MemoryStore) ListIncidents(_ context.Context, f aiops.Filter) ([]aiops.Incident, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]aiops.Incident, 0, len(s.incidents))
	for _, inc := range s.incidents {
		if !matchIncident(inc, f) {
			continue
		}
		out = append(out, inc)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].FirstSeen.Equal(out[j].FirstSeen) {
			return out[i].FirstSeen.After(out[j].FirstSeen)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *MemoryStore) GetIncident(_ context.Context, id string) (aiops.Detail, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, inc := range s.incidents {
		if inc.ID != id {
			continue
		}
		detail := aiops.Detail{Incident: inc, RecoveryActions: []aiops.RecoveryAction{}}
		if list := s.diagnoses[id]; len(list) > 0 {
			latest := list[len(list)-1]
			detail.Diagnosis = &latest
		}
		return detail, true, nil
	}
	return aiops.Detail{}, false, nil
}

func (s *MemoryStore) CreateIncident(_ context.Context, inc aiops.Incident, diag aiops.Diagnosis) (aiops.Incident, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if inc.ID == "" {
		inc.ID = uuid.NewString()
	}
	if inc.Status == "" {
		inc.Status = "OPEN"
	}
	if inc.FirstSeen.IsZero() {
		inc.FirstSeen = time.Now()
	}
	diag.ID = uuid.NewString()
	diag.IncidentID = inc.ID
	if diag.CreatedAt.IsZero() {
		diag.CreatedAt = inc.FirstSeen
	}
	s.incidents = append(s.incidents, inc)
	s.diagnoses[inc.ID] = append(s.diagnoses[inc.ID], diag)
	return inc, nil
}

func (s *MemoryStore) EscalateIncident(_ context.Context, id string, sev aiops.Severity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.incidents {
		if s.incidents[i].ID != id {
			continue
		}
		if sev.Rank() > s.incidents[i].Severity.Rank() {
			s.incidents[i].Severity = sev
		}
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) ResolveIncident(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.incidents {
		if s.incidents[i].ID != id {
			continue
		}
		if s.incidents[i].Status == "OPEN" {
			s.incidents[i].Status = "RESOLVED"
			resolved := at
			s.incidents[i].ResolvedAt = &resolved
		}
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) AppendDiagnosis(_ context.Context, incidentID string, diag aiops.Diagnosis) (aiops.Diagnosis, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for _, inc := range s.incidents {
		if inc.ID == incidentID {
			found = true
			break
		}
	}
	if !found {
		return aiops.Diagnosis{}, ErrNotFound
	}
	diag.ID = uuid.NewString()
	diag.IncidentID = incidentID
	if diag.CreatedAt.IsZero() {
		diag.CreatedAt = time.Now()
	}
	s.diagnoses[incidentID] = append(s.diagnoses[incidentID], diag)
	return diag, nil
}

func matchIncident(inc aiops.Incident, f aiops.Filter) bool {
	if f.Status != "" && inc.Status != f.Status {
		return false
	}
	if f.Type != "" && string(inc.Type) != f.Type {
		return false
	}
	if f.Severity != "" && string(inc.Severity) != f.Severity {
		return false
	}
	if f.Namespace != "" && inc.Namespace != f.Namespace {
		return false
	}
	if f.Target != "" && inc.Target != f.Target {
		return false
	}
	return true
}

// AppendSLOBucket 追加/覆盖一个分钟桶；同一分钟重复写入时以首次为准，避免重复计数。
func (s *MemoryStore) AppendSLOBucket(_ context.Context, service string, bucket slo.Bucket) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.sloBuckets[service] {
		if existing.Start.Equal(bucket.Start) {
			return nil
		}
	}
	s.sloBuckets[service] = append(s.sloBuckets[service], bucket)
	return nil
}
