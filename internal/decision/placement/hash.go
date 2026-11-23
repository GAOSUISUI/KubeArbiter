package placement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// canonicalSnapshot 是参与哈希的字段子集：剔除 TakenAt 这类每次取数都会变的字段，
// 这样"集群状态没变"就得到同一个哈希，快照去重与审计比对才有意义。
type canonicalSnapshot struct {
	Nodes       []capacity.NodeView
	PendingPods []capacity.PodView
	TenantUsage map[string]schedcore.Resources
}

// Hash 返回快照内容的稳定哈希，用于复现与审计。
// 节点与待调度 Pod 在 View 中已按名字排序，map 由 encoding/json 按键排序，因此结果确定。
func Hash(snap capacity.Snapshot) (string, error) {
	b, err := json.Marshal(canonicalSnapshot{
		Nodes:       snap.Nodes,
		PendingPods: snap.PendingPods,
		TenantUsage: snap.TenantUsage,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
