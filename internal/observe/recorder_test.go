package observe

import (
	"testing"
	"time"
)

func TestRecorderBucketsByMinute(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return base }
	rec.current.start = base

	rec.observeAt(base.Add(10*time.Second), 200)
	rec.observeAt(base.Add(20*time.Second), 500)
	rec.observeAt(base.Add(70*time.Second), 200)

	rec.mu.Lock()
	rec.rollLocked(base.Add(2 * time.Minute))
	items := rec.completed
	rec.completed = nil
	rec.mu.Unlock()

	if len(items) != 2 {
		t.Fatalf("期望 2 个分钟桶，实际 %d", len(items))
	}
	if items[0].Start != base || items[0].Total != 2 || items[0].Errors != 1 {
		t.Fatalf("第一桶统计错误: %+v", items[0])
	}
	if items[1].Total != 1 || items[1].Errors != 0 {
		t.Fatalf("第二桶统计错误: %+v", items[1])
	}
}

func TestRecorderFillsIdleMinutes(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	rec.current.start = base

	// 跳过 3 分钟：0/1/2 分钟为零值桶，第 3 分钟包含请求。
	rec.observeAt(base.Add(3*time.Minute+5*time.Second), 200)
	rec.mu.Lock()
	rec.rollLocked(base.Add(4 * time.Minute))
	items := rec.completed
	rec.completed = nil
	rec.mu.Unlock()

	if len(items) != 4 {
		t.Fatalf("期望 4 个分钟桶，实际 %d", len(items))
	}
	for i, bucket := range items {
		if bucket.Start != base.Add(time.Duration(i)*time.Minute) {
			t.Fatalf("第 %d 个桶时间错误: %+v", i, bucket)
		}
		if i < 3 && (bucket.Total != 0 || bucket.Errors != 0) {
			t.Fatalf("空分钟应为 0 值桶: %+v", bucket)
		}
	}
	if items[3].Total != 1 {
		t.Fatalf("第 3 分钟应记录 1 个请求: %+v", items[3])
	}
}

func TestRecorderOnlyCounts5xxAsErrors(t *testing.T) {
	rec := NewRecorder()
	base := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	rec.current.start = base
	rec.observeAt(base, 404)
	rec.observeAt(base, 503)

	rec.mu.Lock()
	rec.rollLocked(base.Add(time.Minute))
	items := rec.completed
	rec.completed = nil
	rec.mu.Unlock()

	if len(items) != 1 || items[0].Total != 2 || items[0].Errors != 1 {
		t.Fatalf("4xx 不应计为错误: %+v", items)
	}
}
