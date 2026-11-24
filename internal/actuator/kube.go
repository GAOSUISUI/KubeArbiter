package actuator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/kubearbiter/kubearbiter/internal/taskengine"
)

const hostnameLabel = "kubernetes.io/hostname"

var (
	deploymentGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	statefulSetGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
)

// patchAPI 是执行器需要的最小集群读写面。抽出接口只为单测能验证
// "dry-run 必须先于正式提交"，生产实现固定为 dynamic.Interface。
type patchAPI interface {
	get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error)
	patch(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, patch []byte, dryRun bool) (*unstructured.Unstructured, error)
}

type dynamicAPI struct {
	client dynamic.Interface
}

func (d dynamicAPI) get(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string) (*unstructured.Unstructured, error) {
	return d.client.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
}

func (d dynamicAPI) patch(ctx context.Context, gvr schema.GroupVersionResource, namespace, name string, patch []byte, dryRun bool) (*unstructured.Unstructured, error) {
	opts := metav1.PatchOptions{}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return d.client.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.JSONPatchType, patch, opts)
}

// KubeExecutor 通过写工作负载的 Pod 模板实现放置落地：把决策选中的节点写成
// requiredDuringSchedulingIgnoredDuringExecution 的 hostname 约束。
// 它只负责 Deployment / StatefulSet，与 G5 的写权限白名单一致。
type KubeExecutor struct {
	api patchAPI
}

// NewKubeExecutor 用 dynamic client 构造执行器；client 为 nil 时返回明确不可用的执行器。
func NewKubeExecutor(client dynamic.Interface) Executor {
	if client == nil {
		return UnavailableExecutor{Reason: "集群客户端未初始化"}
	}
	return &KubeExecutor{api: dynamicAPI{client: client}}
}

func (e *KubeExecutor) Execute(ctx context.Context, req Request) (Result, error) {
	switch req.Type {
	case taskengine.PlacementApply:
		return e.applyPlacement(ctx, req.Params)
	default:
		return Result{}, &Error{
			Class:   taskengine.Permanent,
			Code:    CodeBusinessRejected,
			Message: fmt.Sprintf("任务类型 %s 尚无可执行器", req.Type),
		}
	}
}

type workloadRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Node      string `json:"-"`
}

func (e *KubeExecutor) applyPlacement(ctx context.Context, params map[string]any) (Result, error) {
	ref, err := parsePlacementParams(params)
	if err != nil {
		return Result{}, &Error{Class: taskengine.Permanent, Code: CodeBusinessRejected, Message: err.Error(), Err: err}
	}
	gvr, err := gvrForKind(ref.Kind)
	if err != nil {
		return Result{}, &Error{Class: taskengine.Permanent, Code: CodeBusinessRejected, Message: err.Error(), Err: err}
	}

	obj, err := e.api.get(ctx, gvr, ref.Namespace, ref.Name)
	if err != nil {
		return Result{}, classify(err, fmt.Sprintf("读取 %s %s/%s 失败", ref.Kind, ref.Namespace, ref.Name))
	}
	patch, already, err := buildNodeAffinityPatch(obj.Object, ref.Node)
	if err != nil {
		return Result{}, &Error{Class: taskengine.Permanent, Code: CodeBusinessRejected, Message: err.Error(), Err: err}
	}
	if already {
		return Result{
			Message: fmt.Sprintf("%s %s/%s 已约束到节点 %s，无需重复注入", ref.Kind, ref.Namespace, ref.Name, ref.Node),
			Detail:  detail(ref),
		}, nil
	}

	// G2：dry-run 失败必须中止，不得尝试正式提交。
	if _, err := e.api.patch(ctx, gvr, ref.Namespace, ref.Name, patch, true); err != nil {
		return Result{}, classify(err, "dry-run 校验失败，已中止")
	}
	if _, err := e.api.patch(ctx, gvr, ref.Namespace, ref.Name, patch, false); err != nil {
		return Result{}, classify(err, "正式提交失败")
	}
	return Result{
		Message: fmt.Sprintf("%s %s/%s 已注入 nodeAffinity：%s=%s（dry-run 通过）", ref.Kind, ref.Namespace, ref.Name, hostnameLabel, ref.Node),
		Detail:  detail(ref),
	}, nil
}

func detail(ref workloadRef) map[string]any {
	return map[string]any{
		"schema_version": 1,
		"kind":           ref.Kind,
		"namespace":      ref.Namespace,
		"workload":       ref.Name,
		"node":           ref.Node,
		"dry_run":        true,
	}
}

func parsePlacementParams(params map[string]any) (workloadRef, error) {
	var ref workloadRef
	if params == nil {
		return ref, errors.New("任务参数为空")
	}
	raw, ok := params["workload"]
	if !ok || raw == nil {
		return ref, errors.New("任务参数缺少 workload")
	}
	// workload 可能来自 jsonb（map）或内存中的结构体，统一走一次 JSON 往返。
	data, err := json.Marshal(raw)
	if err != nil {
		return ref, fmt.Errorf("解析 workload: %w", err)
	}
	if err := json.Unmarshal(data, &ref); err != nil {
		return ref, fmt.Errorf("解析 workload: %w", err)
	}
	ref.Kind = strings.TrimSpace(ref.Kind)
	if ref.Kind == "" {
		ref.Kind = "Deployment"
	}
	ref.Name = strings.TrimSpace(ref.Name)
	ref.Namespace = strings.TrimSpace(ref.Namespace)
	if ref.Name == "" || ref.Namespace == "" {
		return ref, errors.New("workload 的 name / namespace 不能为空")
	}
	node, _ := params["chosen_node"].(string)
	ref.Node = strings.TrimSpace(node)
	if ref.Node == "" {
		return ref, errors.New("任务参数缺少 chosen_node")
	}
	return ref, nil
}

func gvrForKind(kind string) (schema.GroupVersionResource, error) {
	switch kind {
	case "Deployment", "deployment", "deployments":
		return deploymentGVR, nil
	case "StatefulSet", "statefulset", "statefulsets":
		return statefulSetGVR, nil
	default:
		return schema.GroupVersionResource{}, fmt.Errorf("不支持的工作负载类型 %q（仅支持 Deployment / StatefulSet）", kind)
	}
}

// buildNodeAffinityPatch 生成"把 Pod 模板约束到 node"的 JSON Patch。
// 已存在相同 hostname 约束时返回 already=true，保证重复执行是幂等的。
func buildNodeAffinityPatch(obj map[string]any, node string) (patch []byte, already bool, err error) {
	if _, found, err := unstructured.NestedMap(obj, "spec", "template", "spec"); err != nil {
		return nil, false, fmt.Errorf("读取 Pod 模板: %w", err)
	} else if !found {
		return nil, false, errors.New("工作负载缺少 Pod 模板，无法注入 nodeAffinity")
	}

	const requiredPath = "/spec/template/spec/affinity/nodeAffinity/requiredDuringSchedulingIgnoredDuringExecution"
	terms, hasTerms, err := unstructured.NestedSlice(obj,
		"spec", "template", "spec", "affinity", "nodeAffinity",
		"requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	if err != nil {
		return nil, false, fmt.Errorf("读取 nodeSelectorTerms: %w", err)
	}
	if hasTerms {
		for _, item := range terms {
			tm, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if termRequiresNode(tm, node) {
				return nil, true, nil
			}
		}
		// nodeSelectorTerms 之间是 OR 关系；追加一条只会放宽调度范围。
		// 已有其他约束时不得覆盖（G2/G4），交给人工处理。
		return nil, false, errors.New("工作负载已有其他必须满足的节点约束，拒绝覆盖用户配置")
	}

	if _, has := nestedHas(obj, "spec", "template", "spec", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution"); has {
		return encodePatch("add", requiredPath+"/nodeSelectorTerms", []any{nodeAffinityTerm(node)})
	}
	if _, has := nestedHas(obj, "spec", "template", "spec", "affinity", "nodeAffinity"); has {
		return encodePatch("add", requiredPath, requiredTerms(node))
	}
	if _, has := nestedHas(obj, "spec", "template", "spec", "affinity"); has {
		return encodePatch("add", "/spec/template/spec/affinity/nodeAffinity", map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": requiredTerms(node),
		})
	}
	return encodePatch("add", "/spec/template/spec/affinity", map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": requiredTerms(node),
		},
	})
}

func nodeAffinityTerm(node string) map[string]any {
	return map[string]any{
		"matchExpressions": []any{
			map[string]any{"key": hostnameLabel, "operator": "In", "values": []any{node}},
		},
	}
}

func requiredTerms(node string) map[string]any {
	return map[string]any{"nodeSelectorTerms": []any{nodeAffinityTerm(node)}}
}

// nestedHas 判断路径上每一级是否都存在且为对象，用于决定 JSON Patch 的插入点。
func nestedHas(obj map[string]any, fields ...string) (map[string]any, bool) {
	cur := obj
	for _, f := range fields {
		next, ok := cur[f].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func termRequiresNode(term map[string]any, node string) bool {
	if requiresNode(term["matchExpressions"], node) {
		return true
	}
	return requiresNode(term["matchFields"], node)
}

func requiresNode(raw any, node string) bool {
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		expr, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := expr["key"].(string)
		if key != hostnameLabel && key != "metadata.name" {
			continue
		}
		op, _ := expr["operator"].(string)
		if op != "In" {
			continue
		}
		values, _ := expr["values"].([]any)
		for _, v := range values {
			if s, _ := v.(string); s == node {
				return true
			}
		}
	}
	return false
}

func encodePatch(op, path string, value any) ([]byte, bool, error) {
	body, err := json.Marshal([]map[string]any{{"op": op, "path": path, "value": value}})
	if err != nil {
		return nil, false, fmt.Errorf("序列化 JSON Patch: %w", err)
	}
	return body, false, nil
}

// classify 把集群错误映射成任务错误分类：权限/参数/对象不存在属于永久失败，
// 超时、冲突、服务端临时不可用属于可重试。
func classify(err error, prefix string) *Error {
	if err == nil {
		return nil
	}
	msg := prefix + ": " + err.Error()
	out := &Error{Class: taskengine.Retryable, Code: CodeUnavailable, Message: msg, Err: err}
	switch {
	case apierrors.IsNotFound(err), apierrors.IsForbidden(err), apierrors.IsUnauthorized(err),
		apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsMethodNotSupported(err):
		out.Class = taskengine.Permanent
		out.Code = CodeBusinessRejected
	case apierrors.IsConflict(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err),
		apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err), apierrors.IsInternalError(err),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		out.Class = taskengine.Retryable
	}
	return out
}
