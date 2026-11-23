package placement

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
)

func TestHashIsStable(t *testing.T) {
	snap := capacity.Snapshot{
		TakenAt: time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC),
		Nodes:   []capacity.NodeView{nodeView("ka-w1", true, 4000, 8<<30, 3, "z1")},
	}

	h1, err := Hash(snap)
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	h2, err := Hash(snap)
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("同一快照应得到同一哈希: %s != %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("sha256 十六进制应为 64 字符，实际 %d", len(h1))
	}

	snap.Nodes[0].PodCount = 4
	h3, err := Hash(snap)
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	if h3 == h1 {
		t.Fatal("快照内容变化后哈希应变化")
	}
}

func TestHashIgnoresTakenAt(t *testing.T) {
	nodes := []capacity.NodeView{nodeView("ka-w1", true, 4000, 8<<30, 3, "z1")}
	h1, err := Hash(capacity.Snapshot{TakenAt: time.Date(2026, 11, 23, 4, 0, 0, 0, time.UTC), Nodes: nodes})
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	// 同一份集群状态在另一个时刻取数，应得到同一个哈希，否则快照无法去重。
	h2, err := Hash(capacity.Snapshot{TakenAt: time.Date(2026, 11, 23, 5, 30, 0, 0, time.UTC), Nodes: nodes})
	if err != nil {
		t.Fatalf("计算哈希失败: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("TakenAt 不应参与哈希: %s != %s", h1, h2)
	}
}
