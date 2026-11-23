package metrics

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

func TestStoreFirstSampleIsDirect(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	want := schedcore.Resources{CPUMilli: 1200, MemBytes: 3 << 30}
	s.Observe("ka-w1", want, at)

	got, ts, ok := s.NodeUsage("ka-w1")
	if !ok {
		t.Fatal("首个样本应立即可读")
	}
	if got != want {
		t.Fatalf("首个样本应直接作为初值，实际 %+v", got)
	}
	if !ts.Equal(at) {
		t.Fatalf("样本时间 = %v, want %v", ts, at)
	}
	if !s.Newest().Equal(at) {
		t.Fatalf("Newest = %v, want %v", s.Newest(), at)
	}
}

func TestStoreEWMAWeightsByElapsedTime(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s.Observe("ka-w1", schedcore.Resources{CPUMilli: 1000, MemBytes: 1 << 30}, at)

	// 15s 后翻倍的样本应只把 EWMA 抬高一小步（alpha ≈ 4.9%）。
	s.Observe("ka-w1", schedcore.Resources{CPUMilli: 2000, MemBytes: 2 << 30}, at.Add(15*time.Second))

	alpha := 1 - math.Exp(-15/TauSeconds)
	wantCPU := int64(math.Round(1000 + alpha*1000))
	wantMem := int64(math.Round(float64(1<<30) + alpha*float64(1<<30)))

	got, ts, _ := s.NodeUsage("ka-w1")
	if got.CPUMilli != wantCPU || got.MemBytes != wantMem {
		t.Fatalf("EWMA = %+v, want cpu=%d mem=%d", got, wantCPU, wantMem)
	}
	if got.CPUMilli <= 1000 || got.CPUMilli >= 2000 {
		t.Fatalf("EWMA 应介于新旧样本之间，实际 %d", got.CPUMilli)
	}
	if !ts.Equal(at.Add(15 * time.Second)) {
		t.Fatalf("样本时间应前移，实际 %v", ts)
	}
}

func TestStoreIgnoresOutOfOrderSample(t *testing.T) {
	s := NewStore()
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s.Observe("ka-w1", schedcore.Resources{CPUMilli: 1000}, at)
	s.Observe("ka-w1", schedcore.Resources{CPUMilli: 5000}, at.Add(-time.Second))

	got, ts, _ := s.NodeUsage("ka-w1")
	if got.CPUMilli != 1000 {
		t.Fatalf("乱序样本不应生效，实际 %+v", got)
	}
	if !ts.Equal(at) || !s.Newest().Equal(at) {
		t.Fatalf("乱序样本不应推进时间，实际 %v / %v", ts, s.Newest())
	}
}

func TestStoreNewestAcrossNodes(t *testing.T) {
	s := NewStore()
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	s.Observe("ka-w1", schedcore.Resources{CPUMilli: 100}, base)
	s.Observe("ka-w2", schedcore.Resources{CPUMilli: 100}, base.Add(5*time.Second))

	if !s.Newest().Equal(base.Add(5 * time.Second)) {
		t.Fatalf("Newest 应取最新样本，实际 %v", s.Newest())
	}
	if _, _, ok := s.NodeUsage("missing"); ok {
		t.Fatal("未采集的节点应返回 ok=false")
	}
	if !s.Newest().IsZero() && len(s.nodes) != 2 {
		t.Fatalf("应保存两个节点，实际 %d", len(s.nodes))
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := NewStore()
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.Observe("ka-w1", schedcore.Resources{CPUMilli: int64(100 + i*j)}, base.Add(time.Duration(j)*time.Second))
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				s.NodeUsage("ka-w1")
				s.Newest()
			}
		}()
	}
	wg.Wait()
}
