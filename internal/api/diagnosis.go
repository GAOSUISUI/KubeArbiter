package api

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/aiops"
)

type diagnosisRequest struct {
	Target     string `json:"target"`
	Namespace  string `json:"namespace"`
	IncidentID string `json:"incident_id"`
}

func runDiagnosis(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if d.Incidents == nil || d.IncidentWriter == nil {
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
			return runDiagnosisOnce(c, d, raw)
		})
	}
}

func runDiagnosisOnce(c *gin.Context, d Deps, raw []byte) (int, []byte, *apiError) {
	var req diagnosisRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, nil, errInvalidArgument(ErrorDetail{Field: "body", Reason: err.Error()})
	}

	var inc aiops.Incident
	if req.IncidentID != "" {
		detail, ok, err := d.Incidents.GetIncident(c.Request.Context(), req.IncidentID)
		if err != nil {
			return 0, nil, errUnavailable()
		}
		if !ok {
			return 0, nil, errNotFound()
		}
		inc = detail.Incident
	} else {
		if req.Target == "" || req.Namespace == "" {
			return 0, nil, errInvalidArgument(
				ErrorDetail{Field: "target", Reason: "target 与 namespace 不能为空"},
			)
		}
		items, err := d.Incidents.ListIncidents(c.Request.Context(), aiops.Filter{
			Status:    "OPEN",
			Namespace: req.Namespace,
			Target:    req.Target,
		})
		if err != nil {
			return 0, nil, errUnavailable()
		}
		if len(items) == 0 {
			return 0, nil, errNotFound()
		}
		inc = items[0]
	}
	if inc.Status != "OPEN" {
		return 0, nil, errConflict("INCIDENT_RESOLVED", ErrorDetail{
			Field: "incident_id", Reason: "已恢复的事件不再接受诊断",
		})
	}

	diag, err := d.IncidentWriter.AppendDiagnosis(c.Request.Context(), inc.ID, buildDiagnosis(c.Request.Context(), d, inc))
	if err != nil {
		return 0, nil, errUnavailable()
	}
	body, apiErr := successBody(c, diag)
	if apiErr != nil {
		return 0, nil, apiErr
	}
	return http.StatusOK, body, nil
}

// buildDiagnosis 用当前观察结果重建诊断；条件已消失时返回低保真事实与推断，不伪造复现。
func buildDiagnosis(ctx context.Context, d Deps, inc aiops.Incident) aiops.Diagnosis {
	now := time.Now().UTC()
	diag := aiops.Diagnosis{
		Facts:      []aiops.Fact{},
		Inferences: []aiops.Inference{},
		CreatedAt:  now,
	}

	if d.Observer != nil {
		if view, err := d.Observer(ctx); err == nil {
			findings := aiops.Detect(aiops.RuleInput{
				Now:       now,
				Nodes:     view.Nodes,
				Pods:      view.Pods,
				Tenants:   TenantInputs(ctx, d.TenantCatalog, view),
				MetricsAt: view.MetricsAt,
			})
			key := string(inc.Type) + "|" + inc.Namespace + "|" + inc.Target
			for _, f := range findings {
				if f.Key() == key {
					diag.Facts = f.Facts
					diag.Inferences = f.Inferences
					break
				}
			}
		}
	}

	if len(diag.Facts) == 0 {
		diag.Facts = []aiops.Fact{{
			Stage:   "OBSERVED",
			At:      now,
			Object:  inc.Namespace + "/" + inc.Target,
			Summary: "当前观察窗口内未复现该事件的条件",
			Values:  map[string]any{"incident_type": string(inc.Type)},
		}}
		diag.Inferences = []aiops.Inference{{
			RootCause:       "事件条件已消失或数据暂不可用",
			Confidence:      "LOW",
			Evidence:        []string{"本轮检测未产生同类事件"},
			CounterEvidence: []string{"检测周期为 30 秒，短时故障可能未被捕获"},
		}}
	}
	return diag
}
