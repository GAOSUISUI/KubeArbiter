package placement

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
)

// Hash 返回快照的稳定哈希，用于复现与审计。
// Snapshot 内部字段顺序固定，节点与待调度 Pod 已按名字排序，因此同一份快照内容
// （含 TakenAt）总是得到同一哈希；快照内容变化即哈希变化。
func Hash(snap capacity.Snapshot) (string, error) {
	b, err := json.Marshal(snap)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
