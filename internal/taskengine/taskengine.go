// Package taskengine 是任务与工作流引擎的纯逻辑层：
// 只定义类型、状态机与审批/重试规则，不做任何 IO，便于表驱动单测。
package taskengine

import (
	"errors"
	"fmt"
	"time"
)

// Type 对应 openapi.yaml 的 TaskType。
type Type string

const (
	PlacementApply Type = "PLACEMENT_APPLY"
	Preempt        Type = "PREEMPT"
	Scale          Type = "SCALE"
	ReleasePromote Type = "RELEASE_PROMOTE"
	ReleaseAbort   Type = "RELEASE_ABORT"
	Recovery       Type = "RECOVERY"
	ConfigRollback Type = "CONFIG_ROLLBACK"
)

var validTypes = map[Type]bool{
	PlacementApply: true,
	Preempt:        true,
	Scale:          true,
	ReleasePromote: true,
	ReleaseAbort:   true,
	Recovery:       true,
	ConfigRollback: true,
}

func (t Type) Validate() error {
	if !validTypes[t] {
		return fmt.Errorf("未知任务类型 %q", t)
	}
	return nil
}

// Dangerous 表示任务会改变集群状态，默认需要人工审批。
// 本期所有任务类型都会写集群，因此统一返回 true；未来只读任务在此扩展。
func (t Type) Dangerous() bool { return true }

// Status 对应 openapi.yaml 的 TaskStatus。
type Status string

const (
	Pending         Status = "PENDING"
	PendingApproval Status = "PENDING_APPROVAL"
	Queued          Status = "QUEUED"
	Running         Status = "RUNNING"
	Retrying        Status = "RETRYING"
	Succeeded       Status = "SUCCEEDED"
	Failed          Status = "FAILED"
	Cancelled       Status = "CANCELLED"
)

func (s Status) Validate() error {
	switch s {
	case Pending, PendingApproval, Queued, Running, Retrying, Succeeded, Failed, Cancelled:
		return nil
	default:
		return fmt.Errorf("未知任务状态 %q", s)
	}
}

// Terminal 表示任务已结束，不再接受任何状态变更。
func (s Status) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled
}

// Cancellable 与契约保持一致：只有尚未开始执行的三个状态可取消。
func (s Status) Cancellable() bool {
	return s == Pending || s == PendingApproval || s == Queued
}

var transitions = map[Status][]Status{
	Pending:         {Queued, PendingApproval, Cancelled},
	PendingApproval: {Queued, Cancelled},
	Queued:          {Running, Cancelled},
	Running:         {Succeeded, Failed, Retrying},
	Retrying:        {Running, Failed, Cancelled},
	Succeeded:       {},
	Failed:          {},
	Cancelled:       {},
}

// ErrIllegalTransition 由调用方映射为 40902。
var ErrIllegalTransition = errors.New("illegal task transition")

// CheckTransition 校验状态机跃迁，非法时返回 ErrIllegalTransition。
func CheckTransition(from, to Status) error {
	for _, next := range transitions[from] {
		if next == to {
			return nil
		}
	}
	return fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, from, to)
}

// InitialStatus 决定任务创建后的初始状态：只有能证明目标为测试工作负载
// 且调用方显式允许时才能跳过审批直接入队（G4）。
func InitialStatus(testWorkload, allowAutoExecute bool) Status {
	if testWorkload && allowAutoExecute {
		return Queued
	}
	return PendingApproval
}

// CheckApprover 校验审批人：不能为空，且审批人不得与创建人相同（admin 除外）。
func CheckApprover(creator, approver string, isAdmin bool) error {
	if approver == "" {
		return errors.New("审批人不能为空")
	}
	if !isAdmin && creator == approver {
		return errors.New("审批人与创建人不得相同")
	}
	return nil
}

// ErrorClass 区分可重试与不可重试错误。网络抖动、超时属于可重试；
// 参数非法、业务规则拒绝属于不可重试。
type ErrorClass int

const (
	Retryable ErrorClass = iota
	Permanent
)

// RetryPolicy 使用指数退避：BaseDelay << retryCount，封顶 MaxDelay。
type RetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
}

// DefaultRetryPolicy 与 tasks.max_retries 默认值 3 保持一致。
var DefaultRetryPolicy = RetryPolicy{
	MaxRetries: 3,
	BaseDelay:  2 * time.Second,
	MaxDelay:   5 * time.Minute,
}

// Next 返回下一次重试的等待时长；不再重试时 ok=false。
func (p RetryPolicy) Next(retryCount int, class ErrorClass) (time.Duration, bool) {
	if class == Permanent || retryCount >= p.MaxRetries || retryCount < 0 {
		return 0, false
	}
	if retryCount > 20 { // 防止位移溢出，实际不会达到
		retryCount = 20
	}
	delay := p.BaseDelay << retryCount
	if p.MaxDelay > 0 && delay > p.MaxDelay {
		delay = p.MaxDelay
	}
	return delay, true
}
