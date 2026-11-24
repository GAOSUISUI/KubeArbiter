package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
	corev1 "k8s.io/api/core/v1"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/internal/decision/placement"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// 注入到 Pod 上的注解，便于事后从 Pod 反查决策与租户归属。
const (
	AnnotationDecisionID = "arbiter.io/decision-id"
	AnnotationTenant     = "arbiter.io/tenant"
	annotationPlacement  = "arbiter.io/placement"
)

type admissionReview struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Request    *admissionRequest `json:"request"`
}

type admissionRequest struct {
	UID       string          `json:"uid"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Operation string          `json:"operation"`
	Object    json.RawMessage `json:"object"`
}

type admissionResponse struct {
	UID       string   `json:"uid"`
	Allowed   bool     `json:"allowed"`
	PatchType *string  `json:"patchType,omitempty"`
	Patch     []byte   `json:"patch,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// admissionReviewResponse 是回给 apiserver 的信封。必须带上 apiVersion/kind 并把
// 结果放在 response 字段里；只返回裸 AdmissionResponse 会被判为 "webhook response was absent"。
type admissionReviewResponse struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Response   *admissionResponse `json:"response"`
}

func writeReview(c *gin.Context, resp admissionResponse) {
	c.JSON(http.StatusOK, admissionReviewResponse{
		APIVersion: "admission.k8s.io/v1",
		Kind:       "AdmissionReview",
		Response:   &resp,
	})
}

// webhookMutate 处理 Pod CREATE：为托管命名空间里的 Pod 注入 nodeAffinity，
// 把调度范围收敛到决策核选出的节点。任何异常都放行（fail-open），
// 由 MutatingWebhookConfiguration 的 failurePolicy: Ignore 兜底，避免阻塞业务发布。
func webhookMutate(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		var review admissionReview
		if err := c.ShouldBindJSON(&review); err != nil || review.Request == nil {
			writeReview(c, admissionResponse{Allowed: true, Warnings: []string{"无法解析 AdmissionReview，已放行"}})
			return
		}
		req := review.Request
		if req.Operation != "CREATE" {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true})
			return
		}

		var pod corev1.Pod
		if err := json.Unmarshal(req.Object, &pod); err != nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"Pod 反序列化失败，已放行"}})
			return
		}
		// 指定了 nodeName 的 Pod 绕过调度器，注入亲和性没有意义。
		if pod.Spec.NodeName != "" {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true})
			return
		}
		if d.Snapshotter == nil || d.Policies == nil || d.Tenants == nil || d.TenantOfNamespace == nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"决策依赖未就绪，已放行"}})
			return
		}

		ctx := c.Request.Context()
		tenant, _ := d.TenantOfNamespace(pod.Namespace)
		if tenant == "" {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true,
				Warnings: []string{"命名空间未标注 " + capacity.TenantLabelKey + "，跳过仲裁"}})
			return
		}

		policy, err := defaultPolicy(ctx, d)
		if err != nil || policy == nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"没有可用的默认策略，已放行"}})
			return
		}

		workload := WorkloadSpec{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			Replicas:  1,
			Requests:  podRequests(&pod),
		}

		snap, err := d.Snapshotter(ctx)
		if err != nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"读取快照失败，已放行"}})
			return
		}
		hash, err := placement.Hash(snap)
		if err != nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"快照哈希失败，已放行"}})
			return
		}
		if d.Snapshots != nil {
			_ = d.Snapshots.SaveSnapshot(ctx, snap, hash)
		}

		tenants := map[string]schedcore.TenantState{}
		state, ok, err := d.Tenants.GetTenant(ctx, tenant)
		if err != nil || !ok {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true,
				Warnings: []string{"租户 " + tenant + " 未登记，已放行"}})
			return
		}
		tenants[tenant] = state

		fairnessMode := normalizeFairnessMode(policy.FairnessMode)
		clusterCtx := placement.BuildContext(snap, tenants, fairnessMode)
		out, err := schedcore.Place(clusterCtx, toSchedWorkload(simulateRequest{
			Workload: workload,
			TenantID: &tenant,
			PolicyID: policy.ID,
		}), policy.Weights)
		if err != nil || out.Status != schedcore.StatusOK || out.ChosenNode == "" {
			// 无候选节点时不注入，交回默认调度器，避免业务被仲裁器拖死。
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true,
				Warnings: []string{"未选出节点，交回默认调度器"}})
			return
		}

		decision := PlacementDecision{
			Status:       out.Status,
			Workload:     &workload,
			TenantID:     &tenant,
			PolicyID:     policy.ID,
			SnapshotHash: hash,
			ChosenNode:   &out.ChosenNode,
			Scores:       out.Scores,
			Filtered:     out.Filtered,
			Degraded:     degraded(out.Warnings),
			Warnings:     out.Warnings,
			Evidence: map[string]any{
				"schema_version": 1,
				"weights":        policy.Weights,
				"fairness_mode":  fairnessMode,
				"source":         "admission-webhook",
				"metrics_known":  clusterCtx.MetricsKnown,
				"pod":            pod.Namespace + "/" + pod.Name,
			},
		}
		if d.Writer != nil {
			saved, err := d.Writer.CreateDecision(ctx, decision)
			if err == nil {
				decision = saved
			}
		}

		patch, err := affinityPatch(&pod, out.ChosenNode, decision.ID, tenant)
		if err != nil {
			writeReview(c, admissionResponse{UID: req.UID, Allowed: true, Warnings: []string{"生成补丁失败，已放行"}})
			return
		}
		jsonPatch, _ := json.Marshal(patch)
		patchType := "JSONPatch"
		writeReview(c, admissionResponse{
			UID:       req.UID,
			Allowed:   true,
			PatchType: &patchType,
			Patch:     jsonPatch,
		})
	}
}

// defaultPolicy 返回标记为默认的策略；没有默认则取第一条，仍为空则交给调用方放行。
func defaultPolicy(ctx context.Context, d Deps) (*Policy, error) {
	policies, err := d.Policies.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range policies {
		if policies[i].IsDefault {
			return &policies[i], nil
		}
	}
	if len(policies) == 0 {
		return nil, nil
	}
	return &policies[0], nil
}

// podRequests 汇总所有容器（含 init 容器取最大值）的资源请求。
func podRequests(pod *corev1.Pod) schedcore.Resources {
	sum := schedcore.Resources{}
	for _, c := range pod.Spec.Containers {
		sum.CPUMilli += c.Resources.Requests.Cpu().MilliValue()
		sum.MemBytes += c.Resources.Requests.Memory().Value()
	}
	return sum
}

// affinityPatch 生成 JSON Patch：注解记录决策，nodeAffinity 固定到选中节点。
func affinityPatch(pod *corev1.Pod, node, decisionID, tenant string) ([]map[string]any, error) {
	annotations := map[string]string{
		AnnotationDecisionID: decisionID,
		AnnotationTenant:     tenant,
		annotationPlacement:  "kubearbiter",
	}
	patch := make([]map[string]any, 0, 3)

	if len(pod.Annotations) == 0 {
		patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations", "value": annotations})
	} else {
		for k, v := range annotations {
			patch = append(patch, map[string]any{"op": "add", "path": "/metadata/annotations/" + escapeJSONPointer(k), "value": v})
		}
	}

	required := &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key:      "kubernetes.io/hostname",
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{node},
			}},
		}},
	}

	switch {
	case pod.Spec.Affinity == nil:
		patch = append(patch, map[string]any{
			"op": "add", "path": "/spec/affinity",
			"value": corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: required}},
		})
	case pod.Spec.Affinity.NodeAffinity == nil:
		patch = append(patch, map[string]any{
			"op": "add", "path": "/spec/affinity/nodeAffinity",
			"value": corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: required},
		})
	default:
		// 已有 nodeAffinity：保留 preferred 段，只覆盖 required 段。
		// 原对象没有 required 段时 JSON Patch 的 replace 会因路径不存在而失败，必须用 add。
		op := "replace"
		if pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
			op = "add"
		}
		patch = append(patch, map[string]any{
			"op": op, "path": "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution",
			"value": required,
		})
	}
	return patch, nil
}

// escapeJSONPointer 按 RFC 6901 转义注解键（/ 需要写成 ~1）。
func escapeJSONPointer(key string) string {
	b := make([]byte, 0, len(key))
	for i := 0; i < len(key); i++ {
		switch key[i] {
		case '~':
			b = append(b, '~', '0')
		case '/':
			b = append(b, '~', '1')
		default:
			b = append(b, key[i])
		}
	}
	return string(b)
}
