package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

// PGStore 是 Store 的 PostgreSQL 实现。抢单用 FOR UPDATE SKIP LOCKED 保证
// 多个 worker 不会领到同一任务；租约复用 started_at + timeout_seconds，
// 不新增任何列，与冻结的 db/schema.sql 保持一致。
type PGStore struct {
	pool *pgxpool.Pool
}

func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

const workerTaskColumns = `id, type, status, params, retry_count, max_retries, timeout_seconds,
	error, error_code, started_at, finished_at, created_at`

// Claim 领取最早入队且已到可见时间的任务，并原子地置为 RUNNING。
// RETRYING 的可见时间存放在 updated_at（退避到期前不会被领取）。
func (s *PGStore) Claim(ctx context.Context, now time.Time) (Task, bool, error) {
	row := s.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT id FROM tasks
			WHERE status = 'QUEUED'
			   OR (status = 'RETRYING' AND updated_at <= $1)
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE tasks
		SET status = 'RUNNING', started_at = $1, updated_at = $1
		WHERE id IN (SELECT id FROM candidate)
		RETURNING `+workerTaskColumns, now)
	t, err := scanWorkerTask(row)
	if err == pgx.ErrNoRows {
		return Task{}, false, nil
	}
	if err != nil {
		return Task{}, false, fmt.Errorf("领取任务: %w", err)
	}
	return t, true, nil
}

// Reap 回收 started_at + timeout + grace 已过期的 RUNNING 任务。
// 回收算作一次失败尝试：retry_count + 1，超限则直接 FAILED。
func (s *PGStore) Reap(ctx context.Context, now time.Time, grace time.Duration) ([]Task, error) {
	graceSeconds := int64(grace / time.Second)
	if graceSeconds < 0 {
		graceSeconds = 0
	}
	rows, err := s.pool.Query(ctx, `
		UPDATE tasks
		SET retry_count = retry_count + 1,
		    status = CASE WHEN retry_count + 1 > max_retries THEN 'FAILED' ELSE 'RETRYING' END,
		    started_at = NULL,
		    finished_at = CASE WHEN retry_count + 1 > max_retries THEN $2 ELSE NULL END,
		    error = 'worker 租约过期，任务被回收',
		    error_code = 50301,
		    updated_at = $2
		WHERE status = 'RUNNING'
		  AND started_at IS NOT NULL
		  AND started_at + (timeout_seconds + $1) * interval '1 second' < $2
		RETURNING `+workerTaskColumns, graceSeconds, now)
	if err != nil {
		return nil, fmt.Errorf("回收过期任务: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanWorkerTask(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描回收任务: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTask 以 WHERE status = expected 做乐观并发控制；
// nextVisibleAt 非零时写入 updated_at，作为 RETRYING 的退避可见时间。
func (s *PGStore) UpdateTask(ctx context.Context, t Task, expected taskengine.Status, nextVisibleAt time.Time) error {
	if _, err := uuid.Parse(t.ID); err != nil {
		return ErrNotFound
	}
	if nextVisibleAt.IsZero() {
		nextVisibleAt = time.Now().UTC()
	}
	ct, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET status = $2, retry_count = $3, error = $4, error_code = $5,
		    started_at = $6, finished_at = $7, updated_at = $8
		WHERE id = $1 AND status = $9`,
		t.ID, string(t.Status), t.RetryCount, t.Error, t.ErrorCode,
		t.StartedAt, t.FinishedAt, nextVisibleAt, string(expected))
	if err != nil {
		return fmt.Errorf("更新任务: %w", err)
	}
	if ct.RowsAffected() > 0 {
		return nil
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1)`, t.ID).Scan(&exists); err != nil {
		return fmt.Errorf("确认任务存在性: %w", err)
	}
	if !exists {
		return ErrNotFound
	}
	return ErrConflict
}

func (s *PGStore) AppendTaskLog(ctx context.Context, taskID string, entry Log) error {
	if _, err := uuid.Parse(taskID); err != nil {
		return ErrNotFound
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO task_logs (task_id, ts, level, message) VALUES ($1, $2, $3, $4)`,
		taskID, entry.TS, entry.Level, entry.Message)
	if err != nil {
		return fmt.Errorf("写入任务日志: %w", err)
	}
	return nil
}

func (s *PGStore) AppendAuditLog(ctx context.Context, entry Audit) error {
	before, err := marshalNullable(entry.Before)
	if err != nil {
		return err
	}
	after, err := marshalNullable(entry.After)
	if err != nil {
		return err
	}
	if entry.TS.IsZero() {
		entry.TS = time.Now().UTC()
	}
	var objectID any
	if entry.ObjectID != "" {
		objectID = entry.ObjectID
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO audit_logs (actor, action, object_type, object_id, before, after, ts)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		entry.Actor, entry.Action, entry.ObjectType, objectID, before, after, entry.TS)
	if err != nil {
		return fmt.Errorf("写入审计日志: %w", err)
	}
	return nil
}

func marshalNullable(m map[string]any) (any, error) {
	if m == nil {
		return nil, nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("序列化审计快照: %w", err)
	}
	return raw, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanWorkerTask(row scannable) (Task, error) {
	var (
		t              Task
		typeStr        string
		statusStr      string
		rawParams      []byte
		timeoutSeconds int32
	)
	if err := row.Scan(&t.ID, &typeStr, &statusStr, &rawParams, &t.RetryCount, &t.MaxRetries,
		&timeoutSeconds, &t.Error, &t.ErrorCode, &t.StartedAt, &t.FinishedAt, &t.CreatedAt); err != nil {
		return Task{}, err
	}
	t.Type = taskengine.Type(typeStr)
	t.Status = taskengine.Status(statusStr)
	t.Timeout = time.Duration(timeoutSeconds) * time.Second
	if len(rawParams) > 0 {
		if err := json.Unmarshal(rawParams, &t.Params); err != nil {
			return Task{}, fmt.Errorf("解析任务参数: %w", err)
		}
	}
	return t, nil
}
