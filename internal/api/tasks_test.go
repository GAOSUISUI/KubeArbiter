package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/placement"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func newTaskRouter() (*MemoryStore, http.Handler, string) {
	store := NewMemoryStore()
	snap := capacity.Snapshot{TakenAt: time.Now().UTC()}
	hash, err := placement.Hash(snap)
	if err != nil {
		panic(err)
	}
	deps := Deps{
		Policies: store, Decisions: store, Writer: store, Snapshots: store,
		SLO: store, SLOWriter: store, Idempotency: store,
		Tasks: store, TaskWriter: store, AuditLogs: store, AuditWriter: store,
		Snapshotter: func(context.Context) (capacity.Snapshot, error) { return snap, nil },
	}
	return store, NewRouter(deps), hash
}

func seedDecision(t *testing.T, store *MemoryStore, hash string) PlacementDecision {
	t.Helper()
	node := "node-a"
	dec, err := store.CreateDecision(context.Background(), PlacementDecision{
		Status:       schedcore.StatusOK,
		PolicyID:     "policy-1",
		ChosenNode:   &node,
		SnapshotHash: hash,
	})
	if err != nil {
		t.Fatalf("写入决策: %v", err)
	}
	return dec
}

func doJSONAs(t *testing.T, handler http.Handler, method, target string, payload any, actor, key string) *httptest.ResponseRecorder {
	t.Helper()
	var body *bytes.Reader
	if payload == nil {
		body = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求体: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set(actorHeader, actor)
	}
	if key != "" {
		req.Header.Set(idempotencyHeader, key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestApplyPlacementCreatesPendingTaskAndReplays(t *testing.T) {
	store, r, hash := newTaskRouter()
	dec := seedDecision(t, store, hash)
	payload := map[string]any{"decision_id": dec.ID}

	first := doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply", payload, "alice", "apply-1")
	if first.Code != http.StatusOK {
		t.Fatalf("apply status = %d, body=%s", first.Code, first.Body.String())
	}
	task := decodeOK[Task](t, first)
	if task.Status != taskengine.PendingApproval || task.Type != taskengine.PlacementApply {
		t.Errorf("任务状态异常: %+v", task)
	}
	if task.CreatedBy != "alice" || task.MaxRetries != taskengine.DefaultRetryPolicy.MaxRetries {
		t.Errorf("任务元数据异常: %+v", task)
	}
	if task.Params["decision_id"] != dec.ID || task.Params["chosen_node"] != "node-a" {
		t.Errorf("任务参数异常: %+v", task.Params)
	}

	second := doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply", payload, "alice", "apply-1")
	if second.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("同键重放不一致: status=%d body=%s", second.Code, second.Body.String())
	}

	tasks, _ := store.ListTasks(context.Background(), TaskFilter{})
	if len(tasks) != 1 {
		t.Errorf("重放不应重复创建, got %d", len(tasks))
	}
}

func TestApplyPlacementRejectsStaleDecision(t *testing.T) {
	store, r, _ := newTaskRouter()
	dec := seedDecision(t, store, "stale-hash")

	rec := doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": dec.ID}, "alice", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "DECISION_STALE" {
		t.Errorf("message = %s", body.Message)
	}
}

func TestApplyPlacementRejectsUnknownDecision(t *testing.T) {
	_, r, _ := newTaskRouter()
	rec := doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": uuid.NewString()}, "alice", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "DECISION_NOT_FOUND" {
		t.Errorf("message = %s", body.Message)
	}
}

func TestApplyPlacementRejectsAutoExecute(t *testing.T) {
	store, r, hash := newTaskRouter()
	dec := seedDecision(t, store, hash)

	rec := doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": dec.ID, "allow_auto_execute": true}, "alice", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "AUTO_EXECUTE_NOT_ALLOWED" {
		t.Errorf("message = %s", body.Message)
	}
}

func TestApproveTask(t *testing.T) {
	store, r, hash := newTaskRouter()
	dec := seedDecision(t, store, hash)
	created := decodeOK[Task](t, doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": dec.ID}, "alice", ""))

	rec := doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/approve", nil, "alice", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("创建人自审 status = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "APPROVER_IS_CREATOR" {
		t.Errorf("message = %s", body.Message)
	}

	rec = doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/approve", nil, "admin", "approve-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("审批 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	approved := decodeOK[Task](t, rec)
	if approved.Status != taskengine.Queued || approved.ApprovedBy == nil || *approved.ApprovedBy != "admin" {
		t.Errorf("审批结果异常: %+v", approved)
	}

	// 重复审批属于非法跃迁；同键重放则返回首次审批结果。
	rec = doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/approve", nil, "admin", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复审批 status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "TASK_NOT_PENDING_APPROVAL" {
		t.Errorf("message = %s", body.Message)
	}

	detail := decodeOK[TaskDetail](t, do(r, "/api/v1/tasks/"+created.ID))
	if len(detail.Logs) != 2 {
		t.Errorf("任务日志应包含创建与审批, got %+v", detail.Logs)
	}
}

func TestCancelTask(t *testing.T) {
	store, r, hash := newTaskRouter()
	dec := seedDecision(t, store, hash)
	created := decodeOK[Task](t, doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": dec.ID}, "alice", ""))

	rec := doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil, "bob", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("取消 status = %d, body=%s", rec.Code, rec.Body.String())
	}
	cancelled := decodeOK[Task](t, rec)
	if cancelled.Status != taskengine.Cancelled || cancelled.FinishedAt == nil {
		t.Errorf("取消结果异常: %+v", cancelled)
	}

	rec = doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/cancel", nil, "bob", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复取消 status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeErr(t, rec); body.Message != "TASK_NOT_CANCELLABLE" {
		t.Errorf("message = %s", body.Message)
	}
}

func TestListTasksFiltersAndPagination(t *testing.T) {
	store, r, _ := newTaskRouter()
	ctx := context.Background()
	statuses := []taskengine.Status{taskengine.PendingApproval, taskengine.Queued, taskengine.Queued}
	for i, status := range statuses {
		if _, err := store.CreateTask(ctx, Task{
			Type: taskengine.PlacementApply, Status: status, CreatedBy: "alice",
			CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("创建任务: %v", err)
		}
	}

	page := decodeOK[taskPage](t, do(r, "/api/v1/tasks?status=QUEUED&limit=1"))
	if len(page.Items) != 1 || page.NextCursor == nil {
		t.Fatalf("第一页异常: %+v", page)
	}
	if page.Items[0].Status != taskengine.Queued {
		t.Errorf("过滤失效: %+v", page.Items[0])
	}

	next := decodeOK[taskPage](t, do(r, "/api/v1/tasks?status=QUEUED&limit=1&cursor="+*page.NextCursor))
	if len(next.Items) != 1 || next.Items[0].ID == page.Items[0].ID {
		t.Errorf("第二页异常: %+v", next)
	}
}

func TestGetTaskNotFound(t *testing.T) {
	_, r, _ := newTaskRouter()
	rec := do(r, "/api/v1/tasks/"+uuid.NewString())
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAuditLogsRecordTaskLifecycle(t *testing.T) {
	store, r, hash := newTaskRouter()
	dec := seedDecision(t, store, hash)
	created := decodeOK[Task](t, doJSONAs(t, r, http.MethodPost, "/api/v1/placement/apply",
		map[string]any{"decision_id": dec.ID}, "alice", ""))
	if rec := doJSONAs(t, r, http.MethodPost, "/api/v1/tasks/"+created.ID+"/approve", nil, "admin", ""); rec.Code != http.StatusOK {
		t.Fatalf("审批失败: %d", rec.Code)
	}

	page := decodeOK[auditPage](t, do(r, "/api/v1/audit-logs?object_id="+created.ID))
	if len(page.Items) != 2 {
		t.Fatalf("审计条数 = %d, want 2: %+v", len(page.Items), page.Items)
	}
	if page.Items[0].Action != "TASK_APPROVE" || page.Items[1].Action != "TASK_CREATE" {
		t.Errorf("审计顺序/动作异常: %+v", page.Items)
	}
	if page.Items[0].Actor != "admin" || page.Items[1].Actor != "alice" {
		t.Errorf("审计 actor 异常: %+v", page.Items)
	}
}
