// Package worker 消费 tasks 表：领取 QUEUED / RETRYING 任务，先 dry-run 再执行，
// 并把 RUNNING → SUCCEEDED / FAILED / RETRYING 的跃迁、日志与审计写回。
// 执行器只负责集群写操作；本包负责状态机、租约、重试与审计。
package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/actuator"
	"github.com/kubearbiter/kubearbiter/internal/observe"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

// 存储层错误，供调用方区分记录消失与并发冲突。
var (
	ErrNotFound = errors.New("task not found")
	ErrConflict = errors.New("task status changed")
)

// defaultTimeout 与 db/schema.sql 的 tasks.timeout_seconds 默认值一致。
const defaultTimeout = 300 * time.Second

// codeTaskTimeout 用于执行超时；任务表只要求 error_code 为整数，与 HTTP 契约解耦。
const codeTaskTimeout = 50001

// Task 是 worker 视角的任务行。比 API 的 Task DTO 多出 timeout / created_at，
// 因为执行与抢单需要它们，而 HTTP 响应不暴露。
type Task struct {
	ID         string
	Type       taskengine.Type
	Status     taskengine.Status
	Params     map[string]any
	RetryCount int
	MaxRetries int
	Timeout    time.Duration
	Error      *string
	ErrorCode  *int
	CreatedBy  string
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	// notBefore 只在内存实现中使用，等价于 PostgreSQL 的 updated_at 作为"可再次领取时间"。
	notBefore time.Time
}

// Log 对应 task_logs。
type Log struct {
	TS      time.Time
	Level   string
	Message string
}

// Audit 对应 audit_logs（只追加）。
type Audit struct {
	Actor      string
	Action     string
	ObjectType string
	ObjectID   string
	Before     map[string]any
	After      map[string]any
	TS         time.Time
}

// Store 是 worker 对任务存储的最小依赖：抢单、回收、状态回写、日志与审计。
// UpdateTask 用 expected 做乐观并发控制，nextVisibleAt 非零时写为"可再次领取时间"
// （PostgreSQL 实现落在 updated_at，内存实现落在 notBefore），用于指数退避。
type Store interface {
	Claim(ctx context.Context, now time.Time) (Task, bool, error)
	Reap(ctx context.Context, now time.Time, grace time.Duration) ([]Task, error)
	UpdateTask(ctx context.Context, t Task, expected taskengine.Status, nextVisibleAt time.Time) error
	AppendTaskLog(ctx context.Context, taskID string, entry Log) error
	AppendAuditLog(ctx context.Context, entry Audit) error
}

// Config 由 cmd/arbiter-worker 的环境变量填充。
type Config struct {
	ID              string
	Concurrency     int
	PollInterval    time.Duration
	ReapInterval    time.Duration
	LeaseGrace      time.Duration
	Retry           taskengine.RetryPolicy
	FinalizeTimeout time.Duration
	// Metrics 为 nil 时不采集指标，不影响任务执行。
	Metrics *observe.Metrics
}

func (c Config) normalized() Config {
	if c.ID == "" {
		c.ID = "arbiter-worker"
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = 30 * time.Second
	}
	if c.Retry.BaseDelay <= 0 {
		c.Retry.BaseDelay = taskengine.DefaultRetryPolicy.BaseDelay
	}
	if c.Retry.MaxDelay <= 0 {
		c.Retry.MaxDelay = taskengine.DefaultRetryPolicy.MaxDelay
	}
	if c.FinalizeTimeout <= 0 {
		c.FinalizeTimeout = 5 * time.Second
	}
	return c
}

// Worker 是任务执行循环。可安全地用多个 goroutine 调用 Run，
// 也可以单个实例开多个并发 worker（本实现采用后者）。
type Worker struct {
	cfg      Config
	store    Store
	executor actuator.Executor
}

func New(cfg Config, store Store, executor actuator.Executor) *Worker {
	if executor == nil {
		executor = actuator.UnavailableExecutor{}
	}
	return &Worker{cfg: cfg.normalized(), store: store, executor: executor}
}

// Run 启动并发执行循环与租约回收循环，阻塞到 ctx 取消。
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reapLoop(ctx)
	}()
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runLoop(ctx)
		}()
	}
	<-ctx.Done()
	wg.Wait()
}

func (w *Worker) runLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		task, ok, err := w.store.Claim(ctx, time.Now().UTC())
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("worker %s 领取任务失败: %v", w.cfg.ID, err)
			w.cfg.Metrics.TaskClaimFailed()
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		if !ok {
			w.sleep(ctx, w.cfg.PollInterval)
			continue
		}
		w.execute(ctx, task)
	}
}

func (w *Worker) sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (w *Worker) execute(ctx context.Context, t Task) {
	started := time.Now()
	outcome := string(taskengine.Failed)
	w.cfg.Metrics.TaskStarted()
	defer func() {
		w.cfg.Metrics.TaskFinished(string(t.Type), outcome, time.Since(started))
	}()

	w.appendLog(ctx, t.ID, "INFO", "开始执行")
	w.audit(ctx, "TASK_START", t, nil, map[string]any{"status": taskengine.Running})

	execCtx, cancel := context.WithTimeout(ctx, t.timeout())
	result, execErr := w.executor.Execute(execCtx, actuator.Request{
		TaskID: t.ID,
		Type:   t.Type,
		Params: t.Params,
	})
	timedOut := errors.Is(execCtx.Err(), context.DeadlineExceeded)
	cancel()

	// 收尾写入不能跟随父 ctx 取消，否则退出时任务会永久停在 RUNNING。
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.FinalizeTimeout)
	defer fcancel()

	if execErr == nil {
		now := time.Now().UTC()
		t.Status = taskengine.Succeeded
		t.FinishedAt = &now
		t.Error, t.ErrorCode = nil, nil
		if err := w.store.UpdateTask(fctx, t, taskengine.Running, time.Time{}); err != nil {
			log.Printf("worker %s 回写任务 %s 成功状态失败: %v", w.cfg.ID, t.ID, err)
			return
		}
		outcome = string(taskengine.Succeeded)
		msg := result.Message
		if msg == "" {
			msg = "执行成功"
		}
		w.appendLog(fctx, t.ID, "INFO", msg)
		w.audit(fctx, "TASK_SUCCEED", t,
			map[string]any{"status": taskengine.Running},
			map[string]any{"status": t.Status, "retry_count": t.RetryCount})
		return
	}

	ae := actuator.AsError(execErr)
	switch {
	case timedOut:
		ae = &actuator.Error{
			Class:   taskengine.Retryable,
			Code:    codeTaskTimeout,
			Message: fmt.Sprintf("执行超时（%s）", t.timeout()),
			Err:     execErr,
		}
	case errors.Is(execErr, context.Canceled) && ctx.Err() != nil:
		ae = &actuator.Error{
			Class:   taskengine.Retryable,
			Code:    actuator.CodeUnavailable,
			Message: "worker 正在退出，任务未完成",
			Err:     execErr,
		}
	}
	outcome = w.fail(fctx, t, ae)
}

// fail 按错误分类决定重试还是终态失败，并返回本次执行的最终结果用于指标。
// retry_count 表示已消耗的重试次数：领取本身不计次，只有一次执行失败才 +1。
func (w *Worker) fail(ctx context.Context, t Task, ae *actuator.Error) string {
	now := time.Now().UTC()
	msg, code := ae.Message, ae.Code
	outcome := string(taskengine.Failed)

	if ae.Class == taskengine.Retryable && t.RetryCount < t.MaxRetries {
		policy := w.cfg.Retry
		policy.MaxRetries = t.MaxRetries // 次数上限由任务行决定
		if delay, ok := policy.Next(t.RetryCount, ae.Class); ok {
			t.Status = taskengine.Retrying
			t.RetryCount++
			t.StartedAt = nil
			t.Error, t.ErrorCode = &msg, &code
			if err := w.store.UpdateTask(ctx, t, taskengine.Running, now.Add(delay)); err != nil {
				log.Printf("worker %s 回写任务 %s 重试状态失败: %v", w.cfg.ID, t.ID, err)
				return outcome
			}
			outcome = string(taskengine.Retrying)
			w.appendLog(ctx, t.ID, "WARN",
				fmt.Sprintf("执行失败，%s 后重试（%d/%d）：%s", delay, t.RetryCount, t.MaxRetries, msg))
			w.audit(ctx, "TASK_RETRY", t,
				map[string]any{"status": taskengine.Running},
				map[string]any{"status": t.Status, "retry_count": t.RetryCount, "error": msg})
			return outcome
		}
	}

	t.Status = taskengine.Failed
	t.FinishedAt = &now
	t.Error, t.ErrorCode = &msg, &code
	if err := w.store.UpdateTask(ctx, t, taskengine.Running, time.Time{}); err != nil {
		log.Printf("worker %s 回写任务 %s 失败状态失败: %v", w.cfg.ID, t.ID, err)
		return outcome
	}
	w.appendLog(ctx, t.ID, "ERROR", "执行失败："+msg)
	w.audit(ctx, "TASK_FAIL", t,
		map[string]any{"status": taskengine.Running},
		map[string]any{"status": t.Status, "error": msg, "error_code": code})
	return outcome
}

func (w *Worker) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.ReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reap(ctx)
		}
	}
}

// reap 回收租约过期的 RUNNING 任务（worker 崩溃或进程被强杀）。
func (w *Worker) reap(ctx context.Context) {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.FinalizeTimeout)
	defer cancel()

	reaped, err := w.store.Reap(fctx, time.Now().UTC(), w.cfg.LeaseGrace)
	if err != nil {
		log.Printf("worker %s 回收过期任务失败: %v", w.cfg.ID, err)
		return
	}
	for _, t := range reaped {
		w.cfg.Metrics.TaskReaped(string(t.Status))
		level, action, msg := "WARN", "TASK_RETRY", "任务租约过期，已重新入队"
		if t.Status == taskengine.Failed {
			level, action, msg = "ERROR", "TASK_FAIL", "任务租约过期且已达最大重试次数，判定失败"
		}
		w.appendLog(fctx, t.ID, level, msg)
		w.audit(fctx, action, t,
			map[string]any{"status": taskengine.Running},
			map[string]any{"status": t.Status, "retry_count": t.RetryCount, "error": msg})
	}
}

func (w *Worker) appendLog(ctx context.Context, taskID, level, message string) {
	entry := Log{TS: time.Now().UTC(), Level: level, Message: message}
	if err := w.store.AppendTaskLog(ctx, taskID, entry); err != nil {
		log.Printf("worker %s 写任务 %s 日志失败: %v", w.cfg.ID, taskID, err)
	}
}

func (w *Worker) audit(ctx context.Context, action string, t Task, before, after map[string]any) {
	entry := Audit{
		Actor:      w.cfg.ID,
		Action:     action,
		ObjectType: "task",
		ObjectID:   t.ID,
		Before:     before,
		After:      after,
		TS:         time.Now().UTC(),
	}
	if err := w.store.AppendAuditLog(ctx, entry); err != nil {
		log.Printf("worker %s 写任务 %s 审计失败: %v", w.cfg.ID, t.ID, err)
	}
}

func (t Task) timeout() time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return defaultTimeout
}
