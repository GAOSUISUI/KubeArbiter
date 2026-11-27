package aiops

import (
	"fmt"
	"time"

	"github.com/kubearbiter/kubearbiter/internal/decision/capacity"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

const (
	// MetricsStaleAfter 与采集周期 15s、指标过期阈值 60s 对齐，留出一轮重试余量。
	MetricsStaleAfter = 90 * time.Second
	// PendingWarnAfter 是未调度 Pod 触发告警的最短年龄。
	PendingWarnAfter = 5 * time.Minute
	// ReadinessFailAfter 是 Running 但未就绪的最短持续时间。
	ReadinessFailAfter = 3 * time.Minute
	// HighUsageRatio 是节点 CPU/内存 EWMA 占 allocatable 的告警阈值。
	HighUsageRatio = 0.9
	// QuotaWarnRatio / QuotaCriticalRatio 是租户配额使用率的两档阈值。
	QuotaWarnRatio     = 0.9
	QuotaCriticalRatio = 1.0
	// CrashLoopCriticalRestarts 是 CrashLoop 升级为 CRITICAL 的重启次数。
	CrashLoopCriticalRestarts = 10
	// OOMRecentWindow 是 OOM 事件的有效期；lastState 会长期保留，超过窗口且已就绪则视为历史。
	OOMRecentWindow = 30 * time.Minute
)

// TenantInput 是检测与健康分需要的租户视图。
type TenantInput struct {
	ID        string
	Name      string
	Quota     schedcore.Resources
	Requested schedcore.Resources
	Share     float64 // 主导份额，0 表示未知
	FairShare float64 // 目标公平份额，0 表示未配置
}

// RuleInput 是一次检测所需的全部只读数据。
type RuleInput struct {
	Now       time.Time
	Nodes     []capacity.NodeDiag
	Pods      []capacity.PodDiag
	Tenants   []TenantInput
	MetricsAt time.Time
}

// Finding 是一条待落库的检测结果。
type Finding struct {
	Type       IncidentType
	Namespace  string
	Target     string
	Severity   Severity
	Facts      []Fact
	Inferences []Inference
}

// Key 是事件去重键。
func (f Finding) Key() string {
	return string(f.Type) + "|" + f.Namespace + "|" + f.Target
}

// Detect 把观察结果转成事件候选，纯函数，便于表驱动测试。
func Detect(in RuleInput) []Finding {
	now := in.Now
	out := make([]Finding, 0)

	if !in.MetricsAt.IsZero() {
		if age := now.Sub(in.MetricsAt); age > MetricsStaleAfter {
			out = append(out, Finding{
				Type:      IncidentMetricsStale,
				Namespace: NamespaceCluster,
				Target:    TargetCluster,
				Severity:  SeverityWarn,
				Facts: []Fact{newFact(now, "cluster", "节点指标已过期", map[string]any{
					"metrics_at":  in.MetricsAt,
					"age_seconds": int64(age.Seconds()),
				})},
				Inferences: []Inference{{
					RootCause:       "kubelet 或 nodes/proxy 不可达，指标采集中断",
					Confidence:      "HIGH",
					Evidence:        []string{fmt.Sprintf("最新指标样本距今 %d 秒", int64(age.Seconds()))},
					CounterEvidence: []string{"采集器刚启动时会短暂无样本"},
				}},
			})
		}
	}

	for _, node := range in.Nodes {
		if !node.Ready {
			out = append(out, Finding{
				Type:      IncidentNodeNotReady,
				Namespace: NamespaceNode,
				Target:    node.Name,
				Severity:  SeverityCritical,
				Facts: []Fact{newFact(now, "node/"+node.Name, "节点 Ready 条件为 False", map[string]any{
					"unschedulable": node.Unschedulable,
				})},
				Inferences: []Inference{{
					RootCause:       "节点 kubelet 未上报或网络中断",
					Confidence:      "HIGH",
					Evidence:        []string{"NodeReady=False"},
					CounterEvidence: []string{"节点可能处于刚启动或重启窗口"},
				}},
			})
		}

		pressures := make([]string, 0, 3)
		if node.MemoryPressure {
			pressures = append(pressures, "MemoryPressure")
		}
		if node.DiskPressure {
			pressures = append(pressures, "DiskPressure")
		}
		if node.PIDPressure {
			pressures = append(pressures, "PIDPressure")
		}
		if len(pressures) > 0 {
			out = append(out, Finding{
				Type:      IncidentNodePressure,
				Namespace: NamespaceNode,
				Target:    node.Name,
				Severity:  SeverityWarn,
				Facts: []Fact{newFact(now, "node/"+node.Name, "节点存在资源压力", map[string]any{
					"pressures": pressures,
				})},
				Inferences: []Inference{{
					RootCause:       "节点资源耗尽，kubelet 已进入压力驱逐模式",
					Confidence:      "MEDIUM",
					Evidence:        pressures,
					CounterEvidence: []string{"短时突发流量也可能触发压力条件"},
				}},
			})
		}

		if node.UsageKnown {
			cpuRatio := ratio(node.Usage.CPUMilli, node.Allocatable.CPUMilli)
			memRatio := ratio(node.Usage.MemBytes, node.Allocatable.MemBytes)
			if cpuRatio >= HighUsageRatio || memRatio >= HighUsageRatio {
				out = append(out, Finding{
					Type:      IncidentNodeHighUsage,
					Namespace: NamespaceNode,
					Target:    node.Name,
					Severity:  SeverityWarn,
					Facts: []Fact{newFact(now, "node/"+node.Name, "节点实际用量持续偏高", map[string]any{
						"cpu_ratio": round3(cpuRatio),
						"mem_ratio": round3(memRatio),
						"cpu_milli": node.Usage.CPUMilli,
						"mem_bytes": node.Usage.MemBytes,
						"alloc_cpu": node.Allocatable.CPUMilli,
						"alloc_mem": node.Allocatable.MemBytes,
					})},
					Inferences: []Inference{{
						RootCause:       "节点负载接近容量上限，继续放置会放大争抢",
						Confidence:      "MEDIUM",
						Evidence:        []string{fmt.Sprintf("CPU %.1f%%，内存 %.1f%%", cpuRatio*100, memRatio*100)},
						CounterEvidence: []string{"EWMA 平滑可能低估瞬时峰值"},
					}},
				})
			}
		}
	}

	for _, pod := range in.Pods {
		object := "pod/" + pod.Namespace + "/" + pod.Name
		base := map[string]any{
			"phase":     string(pod.Phase),
			"node":      pod.NodeName,
			"restarts":  pod.Restarts,
			"waiting":   pod.WaitingReason,
			"last_term": pod.LastTerminatedReason,
		}

		switch pod.WaitingReason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
			out = append(out, Finding{
				Type:      IncidentImagePullBackOff,
				Namespace: pod.Namespace,
				Target:    pod.Name,
				Severity:  SeverityWarn,
				Facts:     []Fact{newFact(now, object, "镜像拉取失败", base)},
				Inferences: []Inference{{
					RootCause:       "镜像名错误、仓库不可达或拉取凭证失效",
					Confidence:      "HIGH",
					Evidence:        []string{"container waiting reason = " + pod.WaitingReason},
					CounterEvidence: []string{"仓库限流也可能短时表现为拉取失败"},
				}},
			})
		}

		if pod.LastTerminatedReason == "OOMKilled" &&
			(!pod.Ready || (!pod.LastTerminatedAt.IsZero() && now.Sub(pod.LastTerminatedAt) <= OOMRecentWindow)) {
			out = append(out, Finding{
				Type:      IncidentOOMKilled,
				Namespace: pod.Namespace,
				Target:    pod.Name,
				Severity:  SeverityCritical,
				Facts:     []Fact{newFact(now, object, "容器因 OOM 被杀", base)},
				Inferences: []Inference{{
					RootCause:       "容器内存 request/limit 过低或存在内存泄漏",
					Confidence:      "HIGH",
					Evidence:        []string{"lastState.terminated.reason = OOMKilled"},
					CounterEvidence: []string{"节点整体内存压力也可能触发内核 OOM"},
				}},
			})
		}

		// 重启次数是累计值，已恢复的 Pod 不应仅凭历史重启次数持续告警。
		if pod.WaitingReason == "CrashLoopBackOff" || (pod.Restarts >= 3 && !pod.Ready) {
			severity := SeverityWarn
			if pod.Restarts >= CrashLoopCriticalRestarts {
				severity = SeverityCritical
			}
			out = append(out, Finding{
				Type:      IncidentCrashLoop,
				Namespace: pod.Namespace,
				Target:    pod.Name,
				Severity:  severity,
				Facts:     []Fact{newFact(now, object, "容器反复重启", base)},
				Inferences: []Inference{{
					RootCause:       "进程启动即退出，常见于配置错误、依赖不可达或启动命令异常",
					Confidence:      "HIGH",
					Evidence:        []string{fmt.Sprintf("restartCount=%d", pod.Restarts)},
					CounterEvidence: []string{"滚动发布期间的正常重启也可能被计入"},
				}},
			})
		}

		if pod.Phase == "Pending" && pod.NodeName == "" && !pod.CreatedAt.IsZero() {
			if age := now.Sub(pod.CreatedAt); age > PendingWarnAfter {
				out = append(out, Finding{
					Type:      IncidentPodPendingLong,
					Namespace: pod.Namespace,
					Target:    pod.Name,
					Severity:  SeverityWarn,
					Facts:     []Fact{newFact(now, object, "Pod 长时间未被调度", base)},
					Inferences: []Inference{{
						RootCause:       "资源不足、亲和性/污点无法满足或配额已满",
						Confidence:      "MEDIUM",
						Evidence:        []string{fmt.Sprintf("Pending 已持续 %d 秒", int64(age.Seconds()))},
						CounterEvidence: []string{"集群自动扩容尚未完成"},
					}},
				})
			}
		}

		if pod.Phase == "Running" && !pod.Ready {
			since := pod.StartTime
			if since.IsZero() {
				since = pod.CreatedAt
			}
			if !since.IsZero() {
				if age := now.Sub(since); age > ReadinessFailAfter {
					out = append(out, Finding{
						Type:      IncidentPodReadinessFail,
						Namespace: pod.Namespace,
						Target:    pod.Name,
						Severity:  SeverityWarn,
						Facts:     []Fact{newFact(now, object, "Pod 运行中但未就绪", base)},
						Inferences: []Inference{{
							RootCause:       "就绪探针失败，依赖未就绪或应用启动过慢",
							Confidence:      "MEDIUM",
							Evidence:        []string{fmt.Sprintf("Ready=False 已持续 %d 秒", int64(age.Seconds()))},
							CounterEvidence: []string{"探针阈值配置过严"},
						}},
					})
				}
			}
		}
	}

	for _, tenant := range in.Tenants {
		cpuRatio := ratio(tenant.Requested.CPUMilli, tenant.Quota.CPUMilli)
		memRatio := ratio(tenant.Requested.MemBytes, tenant.Quota.MemBytes)
		used := cpuRatio
		if memRatio > used {
			used = memRatio
		}
		if used >= QuotaWarnRatio {
			severity := SeverityWarn
			if used >= QuotaCriticalRatio {
				severity = SeverityCritical
			}
			out = append(out, Finding{
				Type:      IncidentQuotaNear,
				Namespace: NamespaceTenant,
				Target:    tenant.ID,
				Severity:  severity,
				Facts: []Fact{newFact(now, "tenant/"+tenant.ID, "租户配额接近或已用尽", map[string]any{
					"cpu_ratio": round3(cpuRatio),
					"mem_ratio": round3(memRatio),
					"requested": tenant.Requested,
					"quota":     tenant.Quota,
				})},
				Inferences: []Inference{{
					RootCause:       "租户请求量接近硬配额，新工作负载可能被过滤",
					Confidence:      "HIGH",
					Evidence:        []string{fmt.Sprintf("配额使用率 %.1f%%", used*100)},
					CounterEvidence: []string{"配额可能刚被人工调低"},
				}},
			})
		}
	}

	return out
}

func newFact(at time.Time, object, summary string, values map[string]any) Fact {
	return Fact{Stage: "OBSERVED", At: at, Object: object, Summary: summary, Values: values}
}

func ratio(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total)
}

func round3(v float64) float64 {
	return float64(int64(v*1000+0.5)) / 1000
}
