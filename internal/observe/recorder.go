package observe

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/slo"
)

// FlushInterval 是分钟桶落库的检查周期；它只影响延迟，不影响桶的时间边界。
const FlushInterval = 15 * time.Second

// BucketSink 接收已完成的分钟桶。MemoryStore 与 PGStore 都实现了该接口。
type BucketSink interface {
	AppendSLOBucket(ctx context.Context, service string, bucket slo.Bucket) error
}

type pendingBucket struct {
	start  time.Time
	total  int64
	errors int64
}

// Recorder 统计 /api/v1/ 请求的可用性 SLI：total 计全部请求，errors 计 5xx。
// 4xx 属于调用方错误，不计入服务不可用。
type Recorder struct {
	mu        sync.Mutex
	current   pendingBucket
	completed []slo.Bucket
	now       func() time.Time
}

func NewRecorder() *Recorder {
	return &Recorder{
		current: pendingBucket{start: time.Now().Truncate(time.Minute)},
		now:     time.Now,
	}
}

// Observe 记录一次请求结果；status 为 HTTP 状态码。
func (r *Recorder) Observe(status int) {
	r.observeAt(r.now(), status)
}

func (r *Recorder) observeAt(at time.Time, status int) {
	minute := at.Truncate(time.Minute)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rollLocked(minute)
	r.current.total++
	if status >= 500 {
		r.current.errors++
	}
}

// rollLocked 把 current 推进到 minute，中间跨过的分钟以零值桶补齐，
// 保证 SLO 的 completeness 不会被"没有请求的分钟"拉低。
func (r *Recorder) rollLocked(minute time.Time) {
	if r.current.start.IsZero() {
		r.current.start = minute
		return
	}
	for r.current.start.Before(minute) {
		r.completed = append(r.completed, slo.Bucket{
			Start:  r.current.start,
			Total:  r.current.total,
			Errors: r.current.errors,
		})
		r.current = pendingBucket{start: r.current.start.Add(time.Minute)}
	}
}

// Run 周期性把已完成的分钟桶写入 sink，直到 ctx 取消。
func (r *Recorder) Run(ctx context.Context, sink BucketSink, service string) {
	if sink == nil || service == "" {
		return
	}
	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.flush(ctx, sink, service)
		}
	}
}

func (r *Recorder) flush(ctx context.Context, sink BucketSink, service string) {
	r.mu.Lock()
	r.rollLocked(r.now().Truncate(time.Minute))
	items := r.completed
	r.completed = nil
	r.mu.Unlock()

	for _, bucket := range items {
		if err := sink.AppendSLOBucket(ctx, service, bucket); err != nil {
			log.Printf("写入 SLO 分钟桶失败 %s %s: %v", service, bucket.Start.Format(time.RFC3339), err)
		}
	}
}

// Drain 返回尚未写出的完成桶，仅供测试与优雅停机使用。
func (r *Recorder) Drain() []slo.Bucket {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.completed
	r.completed = nil
	return out
}
