// Package actuator 负责把已审批的任务落到集群：所有写操作先 dry-run（G2），
// 失败即中止。本包只做集群写操作，不写业务表；任务状态、日志与审计由 worker 负责。
package actuator

import (
	"context"
	"errors"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

// 与 api/errors.go 的契约错误码保持一致。actuator 不依赖 HTTP 层，
// 因此在这里重复两个常量；改契约时需同步两边。
const (
	CodeBusinessRejected = 42201
	CodeUnavailable      = 50301
)

// Request 描述一次任务执行的输入。Params 就是 tasks.params 的原样内容。
type Request struct {
	TaskID string
	Type   taskengine.Type
	Params map[string]any
}

// Result 是执行成功后的可读产出，worker 会把它写进 task_logs 与 audit_logs。
type Result struct {
	Message string
	Detail  map[string]any
}

// Error 携带错误分类，worker 据此决定 FAILED 还是 RETRYING。
type Error struct {
	Class   taskengine.ErrorClass
	Code    int
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil && e.Message == "" {
		return e.Err.Error()
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

// AsError 把任意错误归一成 *Error；未分类错误按"依赖不可用、可重试"处理，
// 避免因为漏分类把本可恢复的抖动直接判成永久失败。
func AsError(err error) *Error {
	var ae *Error
	if errors.As(err, &ae) {
		return ae
	}
	msg := "执行失败"
	if err != nil {
		msg = err.Error()
	}
	return &Error{Class: taskengine.Retryable, Code: CodeUnavailable, Message: msg, Err: err}
}

// Executor 按任务类型执行集群写操作。实现必须是幂等的：同一任务被重复领取时，
// 不得产生重复或冲突的写操作。
type Executor interface {
	Execute(ctx context.Context, req Request) (Result, error)
}

// UnavailableExecutor 用于当前进程没有可用集群客户端的情况。
// 它明确返回错误而不是假成功，让任务按重试策略走到 FAILED 并留下证据。
type UnavailableExecutor struct {
	Reason string
}

func (u UnavailableExecutor) Execute(context.Context, Request) (Result, error) {
	reason := u.Reason
	if reason == "" {
		reason = "集群客户端未配置"
	}
	return Result{}, &Error{Class: taskengine.Retryable, Code: CodeUnavailable, Message: reason}
}
