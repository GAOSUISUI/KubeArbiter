package worker

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

// MemoryStore 是 Store 的内存实现，供单测使用。行为与 PostgreSQL 实现对齐：
// 抢单唯一、按 expected 状态做乐观并发、RETRYING 尊重退避时间。
type MemoryStore struct {
	mu     sync.Mutex
	tasks  []Task
	logs   map[string][]Log
	audits []Audit
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{logs: make(map[string][]Log)}
}

// Seed 插入一条任务，补齐 ID / CreatedAt / Timeout，方便测试构造场景。
func (s *MemoryStore) Seed(t Task) Task {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.Timeout <= 0 {
		t.Timeout = defaultTimeout
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, t)
	return t
}

func (s *MemoryStore) Claim(_ context.Context, now time.Time) (Task, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i := range s.tasks {
		if !claimable(s.tasks[i], now) {
			continue
		}
		if idx < 0 || s.tasks[i].CreatedAt.Before(s.tasks[idx].CreatedAt) {
			idx = i
		}
	}
	if idx < 0 {
		return Task{}, false, nil
	}
	t := &s.tasks[idx]
	t.Status = taskengine.Running
	started := now
	t.StartedAt = &started
	t.notBefore = time.Time{}
	return *t, true, nil
}

func claimable(t Task, now time.Time) bool {
	switch t.Status {
	case taskengine.Queued:
		return true
	case taskengine.Retrying:
		return !t.notBefore.After(now)
	default:
		return false
	}
}

func (s *MemoryStore) Reap(_ context.Context, now time.Time, grace time.Duration) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []Task
	for i := range s.tasks {
		t := &s.tasks[i]
		if t.Status != taskengine.Running || t.StartedAt == nil {
			continue
		}
		if now.Before(t.StartedAt.Add(t.timeout() + grace)) {
			continue
		}
		msg := "worker 租约过期，任务被回收"
		code := 50301
		t.RetryCount++
		t.StartedAt = nil
		t.notBefore = time.Time{}
		t.Error, t.ErrorCode = &msg, &code
		if t.RetryCount > t.MaxRetries {
			t.Status = taskengine.Failed
			finished := now
			t.FinishedAt = &finished
		} else {
			t.Status = taskengine.Retrying
		}
		out = append(out, *t)
	}
	return out, nil
}

func (s *MemoryStore) UpdateTask(_ context.Context, t Task, expected taskengine.Status, nextVisibleAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tasks {
		if s.tasks[i].ID != t.ID {
			continue
		}
		if s.tasks[i].Status != expected {
			return ErrConflict
		}
		next := t
		next.notBefore = nextVisibleAt
		s.tasks[i] = next
		return nil
	}
	return ErrNotFound
}

func (s *MemoryStore) AppendTaskLog(_ context.Context, taskID string, entry Log) error {
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs[taskID] = append(s.logs[taskID], entry)
	return nil
}

func (s *MemoryStore) AppendAuditLog(_ context.Context, entry Audit) error {
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, entry)
	return nil
}

// Get 返回任务快照，供测试断言。
func (s *MemoryStore) Get(id string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.tasks {
		if t.ID == id {
			return t, true
		}
	}
	return Task{}, false
}

// Logs 返回某任务的日志副本。
func (s *MemoryStore) Logs(id string) []Log {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Log{}, s.logs[id]...)
}

// Audits 返回全部审计副本。
func (s *MemoryStore) Audits() []Audit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Audit{}, s.audits...)
}
