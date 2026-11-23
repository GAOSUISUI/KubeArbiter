package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func webhookFixture(t *testing.T) (http.Handler, *MemoryStore, corev1.Pod) {
	t.Helper()
	store := NewMemoryStore()
	weights, err := schedcore.PresetWeights(schedcore.Balanced)
	if err != nil {
		t.Fatalf("预置权重失败: %v", err)
	}
	store.AddPolicy(Policy{ID: testPolicyID, Name: "balanced", Strategy: schedcore.Balanced, Weights: weights, IsDefault: true})
	store.AddTenant(schedcore.TenantState{ID: "t-a", Weight: 1})

	snap := capacity.Snapshot{
		TakenAt: time.Date(2026, 11, 23, 7, 0, 0, 0, time.UTC),
		Nodes: []capacity.NodeView{
			{Name: "ka-w1", Ready: true, Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30}},
			{Name: "ka-w2", Ready: true, Allocatable: schedcore.Resources{CPUMilli: 4000, MemBytes: 8 << 30}},
		},
	}

	router := NewRouter(Deps{
		Policies:  store,
		Decisions: store,
		Writer:    store,
		Tenants:   store,
		Snapshots: store,
		Snapshotter: func(context.Context) (capacity.Snapshot, error) {
			return snap, nil
		},
		TenantOfNamespace: func(ns string) (string, bool) {
			if ns == "team-a" {
				return "t-a", true
			}
			return "", false
		},
	})

	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "team-a"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app",
			Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			}},
		}}},
	}
	return router, store, pod
}

func admissionBody(t *testing.T, pod corev1.Pod) []byte {
	t.Helper()
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatalf("序列化 Pod 失败: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "admission.k8s.io/v1",
		"kind":       "AdmissionReview",
		"request": map[string]any{
			"uid":       "u-1",
			"name":      pod.Name,
			"namespace": pod.Namespace,
			"operation": "CREATE",
			"object":    json.RawMessage(raw),
		},
	})
	if err != nil {
		t.Fatalf("序列化 AdmissionReview 失败: %v", err)
	}
	return body
}

func TestWebhookInjectsNodeAffinity(t *testing.T) {
	router, store, pod := webhookFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/mutate-pods", bytes.NewReader(admissionBody(t, pod)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("webhook 应恒返回 200，实际 %d", rec.Code)
	}
	var review struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Response   struct {
			Allowed   bool   `json:"allowed"`
			PatchType string `json:"patchType"`
			Patch     string `json:"patch"`
		} `json:"response"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &review); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if review.APIVersion != "admission.k8s.io/v1" || review.Kind != "AdmissionReview" {
		t.Fatalf("必须返回 AdmissionReview 信封，实际 %s/%s", review.APIVersion, review.Kind)
	}
	resp := review.Response
	if !resp.Allowed || resp.PatchType != "JSONPatch" || resp.Patch == "" {
		t.Fatalf("应返回 JSONPatch，实际 allowed=%v type=%s", resp.Allowed, resp.PatchType)
	}

	patchJSON, err := base64.StdEncoding.DecodeString(resp.Patch)
	if err != nil {
		t.Fatalf("补丁不是 base64: %v", err)
	}
	var ops []map[string]any
	if err := json.Unmarshal(patchJSON, &ops); err != nil {
		t.Fatalf("补丁不是 JSON Patch 数组: %v", err)
	}

	var affinityFound bool
	for _, op := range ops {
		path, _ := op["path"].(string)
		if path == "/spec/affinity" {
			affinityFound = true
			raw, _ := json.Marshal(op["value"])
			var aff corev1.Affinity
			if err := json.Unmarshal(raw, &aff); err != nil {
				t.Fatalf("affinity 结构非法: %v", err)
			}
			if aff.NodeAffinity == nil || aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
				t.Fatal("必须注入 required nodeAffinity")
			}
			terms := aff.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
			if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
				t.Fatalf("亲和性结构不符合预期: %+v", terms)
			}
			exprs := terms[0].MatchExpressions[0]
			if exprs.Key != "kubernetes.io/hostname" || len(exprs.Values) != 1 {
				t.Fatalf("必须按主机名限定节点，实际 %+v", exprs)
			}
			if exprs.Values[0] != "ka-w1" && exprs.Values[0] != "ka-w2" {
				t.Fatalf("节点名应来自决策结果，实际 %s", exprs.Values[0])
			}
		}
	}
	if !affinityFound {
		t.Fatalf("补丁里缺少 /spec/affinity，实际 %+v", ops)
	}

	list, _ := store.ListDecisions(context.Background(), DecisionFilter{})
	if len(list) != 1 || list[0].Status != schedcore.StatusOK {
		t.Fatalf("webhook 决策应落库且状态 OK，实际 %+v", list)
	}
}

func TestWebhookSkipsUnmanagedNamespace(t *testing.T) {
	router, store, pod := webhookFixture(t)
	pod.Namespace = "unmanaged"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/mutate-pods", bytes.NewReader(admissionBody(t, pod)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	var review struct {
		Response struct {
			Allowed bool   `json:"allowed"`
			Patch   string `json:"patch"`
		} `json:"response"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &review)
	resp := review.Response
	if !resp.Allowed || resp.Patch != "" {
		t.Fatalf("未托管命名空间应放行且不注入，实际 %+v", resp)
	}
	if list, _ := store.ListDecisions(context.Background(), DecisionFilter{}); len(list) != 0 {
		t.Fatalf("跳过的 Pod 不应产生决策，实际 %d 条", len(list))
	}
}

func TestWebhookFailsOpenWithoutSnapshot(t *testing.T) {
	store := NewMemoryStore()
	router := NewRouter(Deps{Policies: store, Decisions: store, TenantOfNamespace: func(string) (string, bool) {
		return "t-a", true
	}})
	_, _, pod := webhookFixture(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhook/mutate-pods", bytes.NewReader(admissionBody(t, pod)))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	var review struct {
		Response struct {
			Allowed bool   `json:"allowed"`
			Patch   string `json:"patch"`
		} `json:"response"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &review)
	resp := review.Response
	if rec.Code != http.StatusOK || !resp.Allowed {
		t.Fatalf("依赖缺失时应 fail-open，实际 code=%d allowed=%v", rec.Code, resp.Allowed)
	}
}

func TestEscapeJSONPointer(t *testing.T) {
	if got := escapeJSONPointer("a/b~c"); got != "a~1b~0c" {
		t.Fatalf("RFC 6901 转义错误: %s", got)
	}
}

func TestAffinityPatchOpOnExistingAffinity(t *testing.T) {
	preferredOnly := corev1.Pod{Spec: corev1.PodSpec{Affinity: &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 1,
			Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "zone", Operator: corev1.NodeSelectorOpExists,
			}}},
		}}},
	}}}
	patch, err := affinityPatch(&preferredOnly, "ka-w2", "d-1", "t-a")
	if err != nil {
		t.Fatalf("生成补丁失败: %v", err)
	}
	var op string
	for _, item := range patch {
		if item["path"] == "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution" {
			op, _ = item["op"].(string)
		}
	}
	if op != "add" {
		t.Fatalf("没有 required 段时必须用 add 而不是 %q", op)
	}

	withRequired := preferredOnly.DeepCopy()
	withRequired.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
		NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: "zone", Operator: corev1.NodeSelectorOpIn, Values: []string{"a"},
		}}}},
	}
	patch, err = affinityPatch(withRequired, "ka-w2", "d-1", "t-a")
	if err != nil {
		t.Fatalf("生成补丁失败: %v", err)
	}
	op = ""
	for _, item := range patch {
		if item["path"] == "/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution" {
			op, _ = item["op"].(string)
		}
	}
	if op != "replace" {
		t.Fatalf("已有 required 段时应覆盖而非 %q", op)
	}
}
