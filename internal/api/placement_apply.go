package api

import (
	"encoding/json"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/kubearbiter/kubearbiter/internal/decision/placement"
	"github.com/kubearbiter/kubearbiter/internal/taskengine"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// placementApplyRequest 对应 openapi.yaml 的 PlacementApplyRequest。
type placementApplyRequest struct {
	DecisionID       string `json:"decision_id"`
	AllowAutoExecute bool   `json:"allow_auto_execute"`
}

// applyPlacement 把一条已落库的放置决策转成受控任务。
// 本期只做到"创建任务 + 审批入队"：真正写集群由 arbiter-worker 的 dry-run 流程完成。
func applyPlacement(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Tasks == nil || d.TaskWriter == nil {
			writeError(c, errUnavailable())
			return
		}
		raw, err := c.GetRawData()
		if err != nil {
			writeError(c, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()}))
			return
		}
		key, apiErr := idempotencyKey(c)
		if apiErr != nil {
			writeError(c, apiErr)
			return
		}
		scope, hash := requestFingerprint(c, raw)
		runIdempotent(c, d.Idempotency, key, scope, hash, func() (int, []byte, *apiError) {
			return applyPlacementCore(c, d, raw)
		})
	}
}

func applyPlacementCore(c *gin.Context, d Deps, raw []byte) (int, []byte, *apiError) {
	ctx := c.Request.Context()
	var req placementApplyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()})
	}
	if _, err := uuid.Parse(req.DecisionID); err != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "decision_id", Reason: "必须是 UUID"})
	}
	if req.AllowAutoExecute {
		// 决策记录里没有工作负载标签，无法证明 arbiter.io/test=true，按 G4 拒绝跳过审批。
		return 0, nil, errBusinessRejected("AUTO_EXECUTE_NOT_ALLOWED", ErrorDetail{
			Field: "allow_auto_execute", Reason: "无法证明目标为测试工作负载，必须走人工审批",
		})
	}

	decision, ok, err := d.Decisions.GetDecision(ctx, req.DecisionID)
	if err != nil {
		return 0, nil, errUnavailable()
	}
	if !ok {
		return 0, nil, errBusinessRejected("DECISION_NOT_FOUND", ErrorDetail{
			Field: "decision_id", Reason: "决策不存在，请先重新模拟",
		})
	}
	if decision.Status != schedcore.StatusOK || decision.ChosenNode == nil {
		return 0, nil, errBusinessRejected("DECISION_NOT_APPLICABLE", ErrorDetail{
			Field: "decision_id", Reason: "决策没有可落地的候选节点",
		})
	}
	// P3：gang 决策可能分布到多个节点；当前执行器只支持单节点 nodeAffinity 注入，
	// 多节点 assignments 一律业务拒绝（批量注入留待后续阶段）。
	if len(decision.Assignments) > 1 {
		return 0, nil, errBusinessRejected("GANG_NOT_EXECUTABLE", ErrorDetail{
			Field: "decision_id", Reason: "gang 决策分布到多个节点，当前执行器仅支持单节点注入，暂不能落地执行",
		})
	}

	// 快照已变化时决策不可信，必须重新模拟（契约 42201 决策已过期）。
	if d.Snapshotter == nil {
		return 0, nil, errUnavailable()
	}
	snap, err := d.Snapshotter(ctx)
	if err != nil {
		return 0, nil, errUnavailable()
	}
	currentHash, err := placement.Hash(snap)
	if err != nil {
		return 0, nil, errInternal()
	}
	if currentHash != decision.SnapshotHash {
		return 0, nil, errBusinessRejected("DECISION_STALE", ErrorDetail{
			Field: "decision_id", Reason: "集群快照已变化，请重新模拟后再落地",
		})
	}

	task := Task{
		Type:   taskengine.PlacementApply,
		Status: taskengine.InitialStatus(false, false),
		Params: map[string]any{
			"schema_version": jsonSchemaVersion,
			"decision_id":    decision.ID,
			"workload":       decision.Workload,
			"chosen_node":    *decision.ChosenNode,
			"snapshot_hash":  decision.SnapshotHash,
			"policy_id":      decision.PolicyID,
		},
		MaxRetries: taskengine.DefaultRetryPolicy.MaxRetries,
		CreatedBy:  actorOf(c),
	}
	created, err := d.TaskWriter.CreateTask(ctx, task)
	if err != nil {
		return 0, nil, errUnavailable()
	}
	appendTaskLog(c, d, created.ID, "INFO", "任务已创建，等待审批")
	recordAudit(c, d, "TASK_CREATE", "task", created.ID, nil, taskDoc(created))
	return taskSuccess(c, created)
}
