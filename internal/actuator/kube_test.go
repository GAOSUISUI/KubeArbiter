package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

type recordedPatch struct {
	gvr    schema.GroupVersionResource
	dryRun bool
	body   []byte
}

type stubAPI struct {
	obj     *unstructured.Unstructured
	getErr  error
	gets    int
	patches []recordedPatch
}

func (s *stubAPI) get(_ context.Context, gvr schema.GroupVersionResource, _, _ string) (*unstructured.Unstructured, error) {
	s.gets++
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.obj, nil
}

func (s *stubAPI) patch(_ context.Context, gvr schema.GroupVersionResource, _, _ string, body []byte, dryRun bool) (*unstructured.Unstructured, error) {
	s.patches = append(s.patches, recordedPatch{gvr: gvr, dryRun: dryRun, body: body})
	return s.obj, nil
}

func podObj(spec map[string]any) map[string]any {
	return map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": spec}},
	}
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

func decodePatch(t *testing.T, raw []byte) patchOp {
	t.Helper()
	var ops []patchOp
	if err := json.Unmarshal(raw, &ops); err != nil {
		t.Fatalf("解析 JSON Patch: %v", err)
	}
	if len(ops) != 1 {
		t.Fatalf("期望单条 patch, got %d", len(ops))
	}
	return ops[0]
}

func requiredWithTerms(terms []any) map[string]any {
	return map[string]any{
		"affinity": map[string]any{
			"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{"nodeSelectorTerms": terms},
			},
		},
	}
}

func TestBuildNodeAffinityPatch(t *testing.T) {
	otherTerm := map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": "topology.kubernetes.io/zone", "operator": "In", "values": []any{"z1"}},
		},
	}
	sameTerm := map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": hostnameLabel, "operator": "In", "values": []any{"node-a"}},
		},
	}

	cases := []struct {
		name    string
		spec    map[string]any
		already bool
		wantErr bool
		path    string
	}{
		{name: "无 affinity", spec: map[string]any{}, path: "/spec/template/spec/affinity"},
		{
			name: "已有其他亲和",
			spec: map[string]any{"affinity": map[string]any{"podAntiAffinity": map[string]any{}}},
			path: "/spec/template/spec/affinity/nodeAffinity",
		},
		{
			name: "已有 preferred nodeAffinity",
			spec: map[string]any{"affinity": map[string]any{"nodeAffinity": map[string]any{}}},
			path: "/spec/template/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution",
		},
		{
			name: "已有 required 但无 terms",
			spec: map[string]any{"affinity": map[string]any{"nodeAffinity": map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{},
			}}},
			path: "/spec/template/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution/nodeSelectorTerms",
		},
		{
			name:    "已有其他节点约束",
			spec:    requiredWithTerms([]any{otherTerm}),
			wantErr: true,
		},
		{
			name:    "已约束到同一节点",
			spec:    requiredWithTerms([]any{sameTerm}),
			already: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, already, err := buildNodeAffinityPatch(podObj(c.spec), "node-a")
			if c.wantErr {
				if err == nil {
					t.Fatal("应拒绝覆盖已有约束")
				}
				return
			}
			if err != nil {
				t.Fatalf("生成 patch 失败: %v", err)
			}
			if already != c.already {
				t.Fatalf("already = %v, want %v", already, c.already)
			}
			if already {
				if raw != nil {
					t.Errorf("已存在约束时不应生成 patch: %s", raw)
				}
				return
			}
			if op := decodePatch(t, raw); op.Op != "add" || op.Path != c.path {
				t.Errorf("patch = %+v, want add %s", op, c.path)
			}
		})
	}
}

func TestExecutePlacementRunsDryRunFirst(t *testing.T) {
	api := &stubAPI{obj: &unstructured.Unstructured{Object: podObj(map[string]any{})}}
	ex := &KubeExecutor{api: api}

	res, err := ex.Execute(context.Background(), Request{
		TaskID: "t1",
		Type:   taskengine.PlacementApply,
		Params: map[string]any{
			"workload":    map[string]any{"name": "demo", "namespace": "default"},
			"chosen_node": "node-a",
		},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if api.gets != 1 {
		t.Errorf("get 次数 = %d, want 1", api.gets)
	}
	if len(api.patches) != 2 {
		t.Fatalf("patch 次数 = %d, want 2", len(api.patches))
	}
	if !api.patches[0].dryRun || api.patches[1].dryRun {
		t.Errorf("必须先 dry-run 再正式提交: %+v", api.patches)
	}
	if api.patches[0].gvr != deploymentGVR {
		t.Errorf("默认 kind 应使用 deployments, got %+v", api.patches[0].gvr)
	}

	op := decodePatch(t, api.patches[0].body)
	value, _ := json.Marshal(op.Value)
	if !strings.Contains(string(value), hostnameLabel) || !strings.Contains(string(value), "node-a") {
		t.Errorf("patch 值未包含节点约束: %s", value)
	}
	if !strings.Contains(res.Message, "dry-run 通过") {
		t.Errorf("结果消息应说明 dry-run: %q", res.Message)
	}
}

func TestExecutePlacementStatefulSetAndIdempotent(t *testing.T) {
	sameTerm := map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": hostnameLabel, "operator": "In", "values": []any{"node-a"}},
		},
	}
	api := &stubAPI{obj: &unstructured.Unstructured{Object: podObj(requiredWithTerms([]any{sameTerm}))}}
	ex := &KubeExecutor{api: api}

	res, err := ex.Execute(context.Background(), Request{
		Type: taskengine.PlacementApply,
		Params: map[string]any{
			"workload":    map[string]any{"kind": "StatefulSet", "name": "db", "namespace": "default"},
			"chosen_node": "node-a",
		},
	})
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if len(api.patches) != 0 {
		t.Errorf("相同约束不应重复提交, got %d 次 patch", len(api.patches))
	}
	if !strings.Contains(res.Message, "无需重复注入") {
		t.Errorf("消息异常: %q", res.Message)
	}
}

func TestExecutePlacementRejectsInvalidParams(t *testing.T) {
	api := &stubAPI{obj: &unstructured.Unstructured{Object: podObj(map[string]any{})}}
	ex := &KubeExecutor{api: api}

	_, err := ex.Execute(context.Background(), Request{
		Type:   taskengine.PlacementApply,
		Params: map[string]any{"workload": map[string]any{"name": "demo", "namespace": "default"}},
	})
	if err == nil {
		t.Fatal("缺少 chosen_node 应失败")
	}
	ae := AsError(err)
	if ae.Class != taskengine.Permanent || ae.Code != CodeBusinessRejected {
		t.Errorf("错误分类 = %+v, want permanent/42201", ae)
	}
	if api.gets != 0 || len(api.patches) != 0 {
		t.Errorf("参数非法时不应访问集群: gets=%d patches=%d", api.gets, len(api.patches))
	}
}

func TestExecuteClassification(t *testing.T) {
	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "demo")
	api := &stubAPI{getErr: notFound}
	ex := &KubeExecutor{api: api}

	_, err := ex.Execute(context.Background(), Request{
		Type: taskengine.PlacementApply,
		Params: map[string]any{
			"workload":    map[string]any{"name": "demo", "namespace": "default"},
			"chosen_node": "node-a",
		},
	})
	ae := AsError(err)
	if ae.Class != taskengine.Permanent || ae.Code != CodeBusinessRejected {
		t.Errorf("NotFound 应为永久失败, got %+v", ae)
	}

	unknown := AsError(errors.New("network blip"))
	if unknown.Class != taskengine.Retryable || unknown.Code != CodeUnavailable {
		t.Errorf("未分类错误应按可重试处理, got %+v", unknown)
	}
}

func TestNewKubeExecutorWithoutClient(t *testing.T) {
	ex := NewKubeExecutor(nil)
	_, err := ex.Execute(context.Background(), Request{Type: taskengine.PlacementApply})
	ae := AsError(err)
	if ae.Class != taskengine.Retryable || ae.Code != CodeUnavailable {
		t.Errorf("无集群客户端应返回不可用, got %+v", ae)
	}
}
