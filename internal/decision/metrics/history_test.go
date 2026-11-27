package metrics

import (
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestStoreSeriesAndRetention(t *testing.T) {
	store := NewStoreWithRetention(2 * time.Minute)
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

	store.Observe("n1", schedcore.Resources{CPUMilli: 100, MemBytes: 1024}, base)
	store.Observe("n1", schedcore.Resources{CPUMilli: 200, MemBytes: 2048}, base.Add(time.Minute))
	store.Observe("n1", schedcore.Resources{CPUMilli: 300, MemBytes: 3072}, base.Add(2*time.Minute))
	// 乱序样本必须被忽略，否则历史与 EWMA 都会不可复现。
	store.Observe("n1", schedcore.Resources{CPUMilli: 999}, base.Add(30*time.Second))

	series := store.Series("n1", time.Time{}, time.Time{})
	if len(series) != 3 {
		t.Fatalf("期望保留 3 条样本，实际 %d", len(series))
	}
	if series[2].Usage.CPUMilli != 300 {
		t.Fatalf("最后一条样本 CPU 期望 300，实际 %d", series[2].Usage.CPUMilli)
	}

	// 超过 2 分钟保留期的历史应被裁剪；至少保留最新一条。
	store.Observe("n1", schedcore.Resources{CPUMilli: 400}, base.Add(5*time.Minute))
	series = store.Series("n1", time.Time{}, time.Time{})
	if len(series) != 1 {
		t.Fatalf("裁剪后期望 1 条样本，实际 %d", len(series))
	}
	if got := series[0].At; !got.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("裁剪后样本时间期望 %v，实际 %v", base.Add(5*time.Minute), got)
	}
}

func TestStoreSeriesWindow(t *testing.T) {
	store := NewStore()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		store.Observe("n1", schedcore.Resources{CPUMilli: int64(i * 100)}, base.Add(time.Duration(i)*time.Minute))
	}
	store.Observe("n2", schedcore.Resources{CPUMilli: 1}, base)

	if nodes := store.Nodes(); len(nodes) != 2 || nodes[0] != "n1" || nodes[1] != "n2" {
		t.Fatalf("节点列表期望 [n1 n2]，实际 %v", nodes)
	}

	from := base.Add(2 * time.Minute)
	to := base.Add(4 * time.Minute)
	series := store.Series("n1", from, to)
	if len(series) != 2 {
		t.Fatalf("窗口内期望 2 条样本，实际 %d", len(series))
	}
	if series[0].Usage.CPUMilli != 200 || series[1].Usage.CPUMilli != 300 {
		t.Fatalf("窗口样本不匹配: %+v", series)
	}
	if got := store.Retention(); got != DefaultRetention {
		t.Fatalf("默认保留时长期望 %v，实际 %v", DefaultRetention, got)
	}
}
