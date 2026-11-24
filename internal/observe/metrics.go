// Package observe 装配进程自身的可观测能力：Prometheus 指标与暴露端点。
// 后续的 slog 与 trace_id 也归入本包（见 docs/00-freeze.md 的仓库布局）。
package observe

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "kubearbiter"

// Metrics 是 worker 自身指标。所有方法对 nil 接收者安全：
// 未启用指标时调用方不需要判空，也不会因为指标系统故障影响任务执行。
type Metrics struct {
	tasks       *prometheus.CounterVec
	duration    *prometheus.HistogramVec
	inflight    prometheus.Gauge
	reaped      *prometheus.CounterVec
	claimErrors prometheus.Counter
}

// NewMetrics 在给定 registry 上注册 worker 指标；reg 为 nil 时返回 nil。
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		tasks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "worker",
			Name:      "tasks_total",
			Help:      "worker 完成的任务数，按任务类型与最终结果统计",
		}, []string{"type", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "worker",
			Name:      "task_duration_seconds",
			Help:      "单次任务执行耗时，按任务类型与最终结果统计",
			Buckets:   prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"type", "status"}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "worker",
			Name:      "inflight_tasks",
			Help:      "当前正在执行的任务数",
		}),
		reaped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "worker",
			Name:      "reaped_tasks_total",
			Help:      "因租约过期被回收的任务数，按回收后的状态统计",
		}, []string{"status"}),
		claimErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "worker",
			Name:      "claim_errors_total",
			Help:      "领取任务失败的次数（数据库错误等）",
		}),
	}
	reg.MustRegister(m.tasks, m.duration, m.inflight, m.reaped, m.claimErrors)
	return m
}

// TaskStarted 标记一次任务执行开始。
func (m *Metrics) TaskStarted() {
	if m == nil {
		return
	}
	m.inflight.Inc()
}

// TaskFinished 标记一次任务执行结束并记录耗时；status 使用任务状态值。
func (m *Metrics) TaskFinished(taskType, status string, elapsed time.Duration) {
	if m == nil {
		return
	}
	m.inflight.Dec()
	m.tasks.WithLabelValues(taskType, status).Inc()
	m.duration.WithLabelValues(taskType, status).Observe(elapsed.Seconds())
}

// TaskClaimFailed 记录一次领取失败。
func (m *Metrics) TaskClaimFailed() {
	if m == nil {
		return
	}
	m.claimErrors.Inc()
}

// TaskReaped 记录一次租约回收，status 为回收后的任务状态。
func (m *Metrics) TaskReaped(status string) {
	if m == nil {
		return
	}
	m.reaped.WithLabelValues(status).Inc()
}

// NewRegistry 创建独立 registry，避免默认全局 registry 被第三方包污染。
func NewRegistry() *prometheus.Registry {
	return prometheus.NewRegistry()
}

// Handler 返回 /metrics 的 HTTP handler。
func Handler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
