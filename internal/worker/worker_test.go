package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/actuator"
	"github.com/kubearbiter/kubearbiter/internal/observe"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

type stubExecutor struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, req actuator.Request) (actuator.Result, error)
}

func (s *stubExecutor) Execute(ctx context.Context, req actuator.Request) (actuator.Result, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.fn != nil {
		return s.fn(ctx, req)
	}
	return actuator.Result{Message: "已注入 nodeAffinity"}, nil
}

func (s *stubExecutor) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func startWorker(t *testing.T, store *MemoryStore, exec actuator.Executor, cfg Config) func() {
	t.Helper()
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = time.Hour // 测试里不需要回收循环
	}
	w := New(cfg, store, exec)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("worker 未在 5s 内退出")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("等待条件超时")
}

func fastConfig() Config {
	return Config{
		ID:           "test-worker",
		Concurrency:  1,
		PollInterval: 5 * time.Millisecond,
		Retry: taskengine.RetryPolicy{
			MaxRetries: 10,
			BaseDelay:  10 * time.Millisecond,
			MaxDelay:   20 * time.Millisecond,
		},
	}
}

func seedQueued(store *MemoryStore, maxRetries int, timeout time.Duration) Task {
	return store.Seed(Task{
		Type:       taskengine.PlacementApply,
		Status:     taskengine.Queued,
		Params:     map[string]any{"schema_version": 1},
		MaxRetries: maxRetries,
		Timeout:    timeout,
		CreatedBy:  "alice",
	})
}

func statusOf(store *MemoryStore, id string) Task {
	t, _ := store.Get(id)
	return t
}

func auditActions(store *MemoryStore) []string {
	out := make([]string, 0)
	for _, a := range store.Audits() {
		out = append(out, a.Action)
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestWorkerSucceeds(t *testing.T) {
	store := NewMemoryStore()
	task := seedQueued(store, 0, time.Second)
	exec := &stubExecutor{}
	startWorker(t, store, exec, fastConfig())

	waitFor(t, 3*time.Second, func() bool {
		return statusOf(store, task.ID).Status == taskengine.Succeeded
	})

	got := statusOf(store, task.ID)
	if got.FinishedAt == nil || got.Error != nil {
		t.Errorf("成功任务状态异常: %+v", got)
	}
	if exec.callCount() != 1 {
		t.Errorf("执行次数 = %d, want 1", exec.callCount())
	}

	logs := store.Logs(task.ID)
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1].Message, "已注入") {
		t.Errorf("任务日志缺少执行结果: %+v", logs)
	}
	actions := auditActions(store)
	if !contains(actions, "TASK_START") || !contains(actions, "TASK_SUCCEED") {
		t.Errorf("审计缺少开始/成功记录: %v", actions)
	}
}

func TestWorkerRetriesThenFails(t *testing.T) {
	store := NewMemoryStore()
	task := seedQueued(store, 1, time.Second)
	exec := &stubExecutor{fn: func(context.Context, actuator.Request) (actuator.Result, error) {
		return actuator.Result{}, &actuator.Error{
			Class: taskengine.Retryable, Code: actuator.CodeUnavailable, Message: "集群暂时不可用",
		}
	}}
	startWorker(t, store, exec, fastConfig())

	waitFor(t, 3*time.Second, func() bool {
		return statusOf(store, task.ID).Status == taskengine.Failed
	})

	got := statusOf(store, task.ID)
	if got.RetryCount != 1 {
		t.Errorf("retry_count = %d, want 1", got.RetryCount)
	}
	if got.FinishedAt == nil || got.Error == nil || !strings.Contains(*got.Error, "集群暂时不可用") {
		t.Errorf("失败任务状态异常: %+v", got)
	}
	if exec.callCount() != 2 {
		t.Errorf("执行次数 = %d, want 2", exec.callCount())
	}
	actions := auditActions(store)
	if !contains(actions, "TASK_RETRY") || !contains(actions, "TASK_FAIL") {
		t.Errorf("审计缺少重试/失败记录: %v", actions)
	}
}

func TestWorkerPermanentFailureDoesNotRetry(t *testing.T) {
	store := NewMemoryStore()
	task := seedQueued(store, 3, time.Second)
	exec := &stubExecutor{fn: func(context.Context, actuator.Request) (actuator.Result, error) {
		return actuator.Result{}, &actuator.Error{
			Class: taskengine.Permanent, Code: actuator.CodeBusinessRejected, Message: "工作负载不存在",
		}
	}}
	startWorker(t, store, exec, fastConfig())

	waitFor(t, 3*time.Second, func() bool {
		return statusOf(store, task.ID).Status == taskengine.Failed
	})

	got := statusOf(store, task.ID)
	if got.RetryCount != 0 {
		t.Errorf("不可重试错误不应消耗重试次数, got %d", got.RetryCount)
	}
	if exec.callCount() != 1 {
		t.Errorf("执行次数 = %d, want 1", exec.callCount())
	}
}

func TestWorkerTimeout(t *testing.T) {
	store := NewMemoryStore()
	task := seedQueued(store, 0, 30*time.Millisecond)
	exec := &stubExecutor{fn: func(ctx context.Context, _ actuator.Request) (actuator.Result, error) {
		<-ctx.Done()
		return actuator.Result{}, ctx.Err()
	}}
	startWorker(t, store, exec, fastConfig())

	waitFor(t, 3*time.Second, func() bool {
		return statusOf(store, task.ID).Status == taskengine.Failed
	})

	got := statusOf(store, task.ID)
	if got.Error == nil || !strings.Contains(*got.Error, "超时") {
		t.Errorf("超时任务错误信息异常: %+v", got.Error)
	}
	if got.ErrorCode == nil || *got.ErrorCode != codeTaskTimeout {
		t.Errorf("超时任务 error_code = %v", got.ErrorCode)
	}
}

func TestWorkerShutdownRequeuesRunningTask(t *testing.T) {
	store := NewMemoryStore()
	task := seedQueued(store, 2, 5*time.Second)
	exec := &stubExecutor{fn: func(ctx context.Context, _ actuator.Request) (actuator.Result, error) {
		<-ctx.Done()
		return actuator.Result{}, ctx.Err()
	}}
	stop := startWorker(t, store, exec, fastConfig())

	waitFor(t, 3*time.Second, func() bool {
		return contains(auditActions(store), "TASK_START")
	})
	stop()

	got := statusOf(store, task.ID)
	if got.Status != taskengine.Retrying || got.RetryCount != 1 {
		t.Errorf("退出时应释放为 RETRYING, got %+v", got)
	}
}

func TestClaimIsUnique(t *testing.T) {
	store := NewMemoryStore()
	const total = 20
	for i := 0; i < total; i++ {
		store.Seed(Task{
			Type:      taskengine.PlacementApply,
			Status:    taskengine.Queued,
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		})
	}

	var (
		mu   sync.Mutex
		ids  []string
		wg   sync.WaitGroup
		now  = time.Now().UTC()
		stop = make(chan struct{})
	)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				task, ok, err := store.Claim(context.Background(), now)
				if err != nil {
					t.Errorf("领取失败: %v", err)
					return
				}
				if !ok {
					return
				}
				mu.Lock()
				ids = append(ids, task.ID)
				mu.Unlock()
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		close(stop)
		t.Fatal("并发领取超时")
	}

	if len(ids) != total {
		t.Fatalf("领取数量 = %d, want %d", len(ids), total)
	}
	seen := make(map[string]bool, total)
	for _, id := range ids {
		if seen[id] {
			t.Errorf("任务被重复领取: %s", id)
		}
		seen[id] = true
	}
}

func TestWorkerMetricsRecorded(t *testing.T) {
	store := NewMemoryStore()
	seedQueued(store, 0, time.Second)
	reg := observe.NewRegistry()
	cfg := fastConfig()
	cfg.Metrics = observe.NewMetrics(reg)
	startWorker(t, store, &stubExecutor{}, cfg)

	waitFor(t, 3*time.Second, func() bool {
		rec := httptest.NewRecorder()
		observe.Handler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body := rec.Body.String()
		return strings.Contains(body, `kubearbiter_worker_tasks_total{status="SUCCEEDED",type="PLACEMENT_APPLY"} 1`) &&
			strings.Contains(body, "kubearbiter_worker_inflight_tasks 0")
	})
}

func TestReapExpiredRunningTasks(t *testing.T) {
	store := NewMemoryStore()
	old := time.Now().UTC().Add(-2 * time.Minute)
	retryAble := store.Seed(Task{
		Type: taskengine.PlacementApply, Status: taskengine.Running,
		StartedAt: &old, Timeout: 10 * time.Second, MaxRetries: 1,
	})
	exhausted := store.Seed(Task{
		Type: taskengine.PlacementApply, Status: taskengine.Running,
		StartedAt: &old, Timeout: 10 * time.Second, MaxRetries: 0,
	})

	reaped, err := store.Reap(context.Background(), time.Now().UTC(), 0)
	if err != nil {
		t.Fatalf("回收失败: %v", err)
	}
	if len(reaped) != 2 {
		t.Fatalf("回收数量 = %d, want 2", len(reaped))
	}

	a := statusOf(store, retryAble.ID)
	if a.Status != taskengine.Retrying || a.RetryCount != 1 || a.StartedAt != nil {
		t.Errorf("应回收为 RETRYING: %+v", a)
	}
	b := statusOf(store, exhausted.ID)
	if b.Status != taskengine.Failed || b.FinishedAt == nil {
		t.Errorf("超过重试上限应判失败: %+v", b)
	}
}

func TestReapRecordsAuditAndLog(t *testing.T) {
	store := NewMemoryStore()
	old := time.Now().UTC().Add(-2 * time.Minute)
	task := store.Seed(Task{
		Type: taskengine.PlacementApply, Status: taskengine.Running,
		StartedAt: &old, Timeout: 10 * time.Second, MaxRetries: 1,
	})
	w := New(Config{ID: "reaper"}, store, &stubExecutor{})

	w.reap(context.Background())

	got := statusOf(store, task.ID)
	if got.Status != taskengine.Retrying {
		t.Fatalf("回收后状态 = %s, want RETRYING", got.Status)
	}
	logs := store.Logs(task.ID)
	if len(logs) != 1 || !strings.Contains(logs[0].Message, "租约过期") {
		t.Errorf("回收日志异常: %+v", logs)
	}
	audits := store.Audits()
	if len(audits) != 1 || audits[0].Action != "TASK_RETRY" || audits[0].Actor != "reaper" {
		t.Errorf("回收审计异常: %+v", audits)
	}
}
