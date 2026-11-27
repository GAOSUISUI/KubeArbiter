package aiops

import (
	"context"
	"fmt"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
)

// DefaultInterval 是检测周期。与 15s 采集错开，保证每轮都能看到新样本。
const DefaultInterval = 30 * time.Second

// ResolveAfterMisses 是自动恢复所需的连续未命中次数，避免瞬时抖动造成事件反复开关。
const ResolveAfterMisses = 2

// Source 提供检测所需的只读数据。Forecasts 可返回 nil，表示预测不可用。
type Source interface {
	Diagnostics(ctx context.Context) (capacity.Diagnostics, error)
	Tenants(ctx context.Context, diag capacity.Diagnostics) ([]TenantInput, error)
	Forecasts() []NodeForecastSummary
}

// SourceFuncs 让调用方用闭包装配 Source，避免为适配器新增类型。
// ForecastsFn 可为 nil：预测数据不可用时预测规则自然跳过。
type SourceFuncs struct {
	DiagnosticsFn func(ctx context.Context) (capacity.Diagnostics, error)
	TenantsFn     func(ctx context.Context, diag capacity.Diagnostics) ([]TenantInput, error)
	ForecastsFn   func() []NodeForecastSummary
}

func (s SourceFuncs) Diagnostics(ctx context.Context) (capacity.Diagnostics, error) {
	if s.DiagnosticsFn == nil {
		return capacity.Diagnostics{}, fmt.Errorf("未配置诊断数据源")
	}
	return s.DiagnosticsFn(ctx)
}

func (s SourceFuncs) Tenants(ctx context.Context, diag capacity.Diagnostics) ([]TenantInput, error) {
	if s.TenantsFn == nil {
		return nil, nil
	}
	return s.TenantsFn(ctx, diag)
}

func (s SourceFuncs) Forecasts() []NodeForecastSummary {
	if s.ForecastsFn == nil {
		return nil
	}
	return s.ForecastsFn()
}

// Detector 周期扫描集群观察结果，维护事件的生命周期。
type Detector struct {
	store    Store
	source   Source
	interval time.Duration
	now      func() time.Time
	logf     func(format string, args ...any)
	misses   map[string]int
}

func NewDetector(store Store, source Source, interval time.Duration, logf func(string, ...any)) *Detector {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Detector{
		store:    store,
		source:   source,
		interval: interval,
		now:      time.Now,
		logf:     logf,
		misses:   make(map[string]int),
	}
}

// Run 先立即扫描一次，之后按周期执行，直到 ctx 取消。
func (d *Detector) Run(ctx context.Context) {
	d.scanOnce(ctx)
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.scanOnce(ctx)
		}
	}
}

func (d *Detector) scanOnce(ctx context.Context) {
	if err := d.Scan(ctx); err != nil {
		d.logf("故障检测失败: %v", err)
	}
}

// Scan 执行一轮检测：新增、升级、自动恢复。
func (d *Detector) Scan(ctx context.Context) error {
	if d.store == nil || d.source == nil {
		return nil
	}
	diag, err := d.source.Diagnostics(ctx)
	if err != nil {
		return fmt.Errorf("读取诊断视图: %w", err)
	}
	tenants, err := d.source.Tenants(ctx, diag)
	if err != nil {
		// 租户数据不可用不应阻塞节点/Pod 检测。
		d.logf("读取租户配额失败，跳过配额规则: %v", err)
		tenants = nil
	}

	now := d.now()
	findings := Detect(RuleInput{
		Now:       now,
		Nodes:     diag.Nodes,
		Pods:      diag.Pods,
		Tenants:   tenants,
		MetricsAt: diag.MetricsAt,
		Forecasts: d.source.Forecasts(),
	})

	open, err := d.store.ListIncidents(ctx, Filter{Status: "OPEN"})
	if err != nil {
		return fmt.Errorf("读取未解决事件: %w", err)
	}
	openByKey := make(map[string]Incident, len(open))
	for _, inc := range open {
		openByKey[incidentKey(inc)] = inc
	}

	seen := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		key := f.Key()
		seen[key] = struct{}{}
		delete(d.misses, key)

		if inc, ok := openByKey[key]; ok {
			if f.Severity.Rank() > inc.Severity.Rank() {
				if err := d.store.EscalateIncident(ctx, inc.ID, f.Severity); err != nil {
					d.logf("升级事件严重度失败 %s: %v", inc.ID, err)
				} else {
					d.logf("事件升级 %s: %s -> %s", inc.ID, inc.Severity, f.Severity)
				}
			}
			continue
		}

		inc := Incident{
			Target:    f.Target,
			Namespace: f.Namespace,
			Type:      f.Type,
			Severity:  f.Severity,
			Status:    "OPEN",
			FirstSeen: now,
		}
		diagReport := Diagnosis{
			Facts:      f.Facts,
			Inferences: f.Inferences,
			CreatedAt:  now,
		}
		created, err := d.store.CreateIncident(ctx, inc, diagReport)
		if err != nil {
			d.logf("创建事件失败 %s: %v", key, err)
			continue
		}
		d.logf("新事件 %s: %s %s/%s", created.ID, created.Type, created.Namespace, created.Target)
	}

	for key, inc := range openByKey {
		if _, ok := seen[key]; ok {
			continue
		}
		d.misses[key]++
		if d.misses[key] < ResolveAfterMisses {
			continue
		}
		if err := d.store.ResolveIncident(ctx, inc.ID, now); err != nil {
			d.logf("自动恢复事件失败 %s: %v", inc.ID, err)
			continue
		}
		delete(d.misses, key)
		d.logf("事件自动恢复 %s: %s %s/%s", inc.ID, inc.Type, inc.Namespace, inc.Target)
	}
	return nil
}

func incidentKey(inc Incident) string {
	return string(inc.Type) + "|" + inc.Namespace + "|" + inc.Target
}
