package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

// actorHeader 是接入 JWT 认证前临时的调用方标识；认证落地后由 token subject 取代。
const actorHeader = "X-Actor"

// anonymousActor 表示未携带调用方标识，审计里保留事实而不是编造用户名。
const anonymousActor = "anonymous"

// adminActor 暂以固定名称识别管理员，认证落地后改为 RBAC 判定。
const adminActor = "admin"

func actorOf(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader(actorHeader)); v != "" {
		return v
	}
	return anonymousActor
}

// Task 对应 openapi.yaml 的 Task。
type Task struct {
	ID         string            `json:"id"`
	Type       taskengine.Type   `json:"type"`
	Status     taskengine.Status `json:"status"`
	Params     map[string]any    `json:"params"`
	RetryCount int               `json:"retry_count"`
	MaxRetries int               `json:"max_retries"`
	Error      *string           `json:"error"`
	ErrorCode  *int              `json:"error_code"`
	CreatedBy  string            `json:"created_by"`
	ApprovedBy *string           `json:"approved_by"`
	ApprovedAt *time.Time        `json:"-"`
	CreatedAt  time.Time         `json:"created_at"`
	StartedAt  *time.Time        `json:"started_at"`
	FinishedAt *time.Time        `json:"finished_at"`
}

// TaskLog 对应 openapi.yaml 的 TaskLog。
type TaskLog struct {
	TS      time.Time `json:"ts"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// TaskDetail 对应 openapi.yaml 的 TaskDetail。
type TaskDetail struct {
	Task
	Logs []TaskLog `json:"logs"`
}

type taskPage struct {
	NextCursor *string `json:"next_cursor"`
	Items      []Task  `json:"items"`
}

// TaskFilter 中的字段已由 handler 校验过枚举值。
type TaskFilter struct {
	Status taskengine.Status
	Type   taskengine.Type
}

// TaskReader 读取任务与执行日志。
type TaskReader interface {
	ListTasks(ctx context.Context, f TaskFilter) ([]Task, error)
	GetTask(ctx context.Context, id string) (Task, bool, error)
	ListTaskLogs(ctx context.Context, taskID string) ([]TaskLog, error)
}

// TaskWriter 写任务。UpdateTask 以 expected 做乐观并发控制：
// 当前状态与 expected 不一致时返回 ErrConflict，handler 映射为 40903。
type TaskWriter interface {
	CreateTask(ctx context.Context, t Task) (Task, error)
	UpdateTask(ctx context.Context, t Task, expected taskengine.Status) (Task, error)
	AppendTaskLog(ctx context.Context, taskID string, log TaskLog) error
}

func listTasks(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Tasks == nil {
			writeError(c, errUnavailable())
			return
		}
		limit, err := parseLimit(c.Query("limit"))
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "limit", Reason: err.Error()}))
			return
		}
		filter := TaskFilter{}
		if raw := c.Query("status"); raw != "" {
			filter.Status = taskengine.Status(raw)
			if err := filter.Status.Validate(); err != nil {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "status", Reason: err.Error()}))
				return
			}
		}
		if raw := c.Query("type"); raw != "" {
			filter.Type = taskengine.Type(raw)
			if err := filter.Type.Validate(); err != nil {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "type", Reason: err.Error()}))
				return
			}
		}

		items, err := d.Tasks.ListTasks(c.Request.Context(), filter)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		start := 0
		if raw := c.Query("cursor"); raw != "" {
			id, ok := decodeCursor(raw)
			if !ok {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标格式非法"}))
				return
			}
			idx := -1
			for i := range items {
				if items[i].ID == id {
					idx = i
					break
				}
			}
			if idx < 0 {
				writeError(c, errInvalidArgument(ErrorDetail{Field: "cursor", Reason: "游标已失效"}))
				return
			}
			start = idx + 1
		}

		rest := items[start:]
		page := taskPage{Items: []Task{}}
		if len(rest) > limit {
			page.Items = append(page.Items, rest[:limit]...)
			next := encodeCursor(rest[limit-1].ID)
			page.NextCursor = &next
		} else {
			page.Items = append(page.Items, rest...)
		}
		writeOK(c, page)
	}
}

func getTask(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Tasks == nil {
			writeError(c, errUnavailable())
			return
		}
		ctx := c.Request.Context()
		task, ok, err := d.Tasks.GetTask(ctx, c.Param("id"))
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if !ok {
			writeError(c, errNotFound())
			return
		}
		logs, err := d.Tasks.ListTaskLogs(ctx, task.ID)
		if err != nil {
			writeError(c, errUnavailable())
			return
		}
		if logs == nil {
			logs = []TaskLog{}
		}
		writeOK(c, TaskDetail{Task: task, Logs: logs})
	}
}

func cancelTask(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Tasks == nil || d.TaskWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, nil)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return cancelTaskCore(c, d)
		})
	}
}

func cancelTaskCore(c *gin.Context, d Deps) (int, []byte, *apiError) {
	ctx := c.Request.Context()
	task, ok, err := d.Tasks.GetTask(ctx, c.Param("id"))
	if err != nil {
		return 0, nil, errUnavailable()
	}
	if !ok {
		return 0, nil, errNotFound()
	}
	if !task.Status.Cancellable() {
		return 0, nil, errIllegalTransition("TASK_NOT_CANCELLABLE", ErrorDetail{
			Field: "status", Reason: "只有 PENDING / PENDING_APPROVAL / QUEUED 可取消",
		})
	}
	if err := taskengine.CheckTransition(task.Status, taskengine.Cancelled); err != nil {
		return 0, nil, errIllegalTransition("ILLEGAL_TRANSITION", ErrorDetail{Field: "status", Reason: err.Error()})
	}

	from := task.Status
	now := time.Now().UTC()
	task.Status = taskengine.Cancelled
	task.FinishedAt = &now
	updated, apiErr := persistTaskTransition(c, d, task, from)
	if apiErr != nil {
		return 0, nil, apiErr
	}
	appendTaskLog(c, d, updated.ID, "INFO", "任务已取消")
	recordAudit(c, d, "TASK_CANCEL", "task", updated.ID, map[string]any{"status": from}, taskDoc(updated))
	return taskSuccess(c, updated)
}

func approveTask(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Tasks == nil || d.TaskWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, nil)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return approveTaskCore(c, d)
		})
	}
}

func approveTaskCore(c *gin.Context, d Deps) (int, []byte, *apiError) {
	ctx := c.Request.Context()
	task, ok, err := d.Tasks.GetTask(ctx, c.Param("id"))
	if err != nil {
		return 0, nil, errUnavailable()
	}
	if !ok {
		return 0, nil, errNotFound()
	}
	if task.Status != taskengine.PendingApproval {
		return 0, nil, errIllegalTransition("TASK_NOT_PENDING_APPROVAL", ErrorDetail{
			Field: "status", Reason: "只有 PENDING_APPROVAL 可审批",
		})
	}

	actor := actorOf(c)
	if err := taskengine.CheckApprover(task.CreatedBy, actor, actor == adminActor); err != nil {
		return 0, nil, errForbidden("APPROVER_IS_CREATOR", ErrorDetail{Field: actorHeader, Reason: err.Error()})
	}

	now := time.Now().UTC()
	task.Status = taskengine.Queued
	task.ApprovedBy = &actor
	task.ApprovedAt = &now
	updated, apiErr := persistTaskTransition(c, d, task, taskengine.PendingApproval)
	if apiErr != nil {
		return 0, nil, apiErr
	}
	appendTaskLog(c, d, updated.ID, "INFO", "任务已审批，进入队列")
	recordAudit(c, d, "TASK_APPROVE", "task", updated.ID,
		map[string]any{"status": taskengine.PendingApproval},
		map[string]any{"status": updated.Status, "approved_by": actor})
	return taskSuccess(c, updated)
}

// persistTaskTransition 把校验过的状态写回，并把并发冲突/记录消失映射成契约错误。
func persistTaskTransition(c *gin.Context, d Deps, task Task, expected taskengine.Status) (Task, *apiError) {
	updated, err := d.TaskWriter.UpdateTask(c.Request.Context(), task, expected)
	if err != nil {
		switch {
		case errors.Is(err, ErrConflict):
			return Task{}, errResourceLocked("TASK_STATE_CHANGED", ErrorDetail{
				Field: "status", Reason: "任务状态已被其他请求修改",
			})
		case errors.Is(err, ErrNotFound):
			return Task{}, errNotFound()
		default:
			return Task{}, errUnavailable()
		}
	}
	return updated, nil
}

func taskSuccess(c *gin.Context, task Task) (int, []byte, *apiError) {
	body, apiErr := successBody(c, task)
	if apiErr != nil {
		return 0, nil, apiErr
	}
	return 200, body, nil
}

func appendTaskLog(c *gin.Context, d Deps, taskID, level, message string) {
	log := TaskLog{TS: time.Now().UTC(), Level: level, Message: message}
	// 日志是可观测性附属信息，写失败不回滚已完成的状态跃迁。
	_ = d.TaskWriter.AppendTaskLog(c.Request.Context(), taskID, log)
}

// taskDoc 是审计快照：只记录可读的关键字段，避免把 params 里的敏感值扩散。
func taskDoc(t Task) map[string]any {
	doc := map[string]any{
		"id":         t.ID,
		"type":       t.Type,
		"status":     t.Status,
		"created_by": t.CreatedBy,
	}
	if t.ApprovedBy != nil {
		doc["approved_by"] = *t.ApprovedBy
	}
	return doc
}
