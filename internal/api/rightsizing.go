package api

import (
	"math"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/kubearbiter/kubearbiter/internal/decision/metrics"
	"github.com/kubearbiter/kubearbiter/pkg/schedcore"
)

// rightsizing 阈值：双维利用率同时低于下限视为超配，同时高于上限视为低配。
const (
	RightsizingOverRatio  = 0.3
	RightsizingUnderRatio = 0.95
	// RightsizingOverHead / RightsizingUnderHead 是建议值的放大/收缩系数，
	// 超配建议留 30% 余量防止压得太紧，低配建议放大 50%。
	RightsizingOverHead  = 1.3
	RightsizingUnderHead = 1.5
)

// rightsizingClassification 是建议类别；unknown 表示缺少实际用量。
type rightsizingClassification string

const (
	rightsizeOK      rightsizingClassification = "OK"
	rightsizeOver    rightsizingClassification = "OVER_PROVISIONED"
	rightsizeUnder   rightsizingClassification = "UNDER_PROVISIONED"
	rightsizeUnknown rightsizingClassification = "UNKNOWN"
)

type rightsizingResources struct {
	CPUMilli int64 `json:"cpu_milli"`
	MemBytes int64 `json:"mem_bytes"`
}

type rightsizingItem struct {
	Namespace      string                    `json:"namespace"`
	Name           string                    `json:"name"`
	Node           string                    `json:"node"`
	Tenant         string                    `json:"tenant,omitempty"`
	Requests       rightsizingResources      `json:"requests"`
	Usage          *rightsizingResources     `json:"usage"`
	CPURatio       *float64                  `json:"cpu_ratio"`
	MemRatio       *float64                  `json:"mem_ratio"`
	Classification rightsizingClassification `json:"classification"`
	Recommendation *rightsizingResources     `json:"recommendation"`
	RecommendNote  string                    `json:"recommend_note"`
}

type rightsizingResponse struct {
	TakenAt          time.Time         `json:"taken_at"`
	Items            []rightsizingItem `json:"items"`
	OverProvisioned  int               `json:"over_provisioned"`
	UnderProvisioned int               `json:"under_provisioned"`
	Unknown          int               `json:"unknown"`
}

// rightsizing 对比 per-pod request 与实际用量 EWMA，输出调优建议。
// 仅建议：绝不自动修改配额或触发任务。缺用量（UNKNOWN）如实标注。
func rightsizing(d Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		response := rightsizingResponse{
			TakenAt: time.Now().UTC(),
			Items:   []rightsizingItem{},
		}
		if d.Observer == nil {
			writeError(c, errUnavailable())
			return
		}
		diag, err := d.Observer(c.Request.Context())
		if err != nil {
			writeError(c, errUnavailable())
			return
		}

		var usages map[metrics.PodKey]schedcore.Resources
		if d.PodUsage != nil {
			usages = d.PodUsage.Snapshot()
		}

		for _, pod := range diag.Pods {
			if pod.Requests.CPUMilli <= 0 && pod.Requests.MemBytes <= 0 {
				continue
			}
			item := rightsizingItem{
				Namespace: pod.Namespace,
				Name:      pod.Name,
				Node:      pod.NodeName,
				Requests: rightsizingResources{
					CPUMilli: pod.Requests.CPUMilli,
					MemBytes: pod.Requests.MemBytes,
				},
				Classification: rightsizeUnknown,
				RecommendNote:  "缺少实际用量，等采集器积累 EWMA 后再评估",
			}
			if d.TenantOfNamespace != nil {
				if tenant, ok := d.TenantOfNamespace(pod.Namespace); ok {
					item.Tenant = tenant
				}
			}

			usage, ok := usages[metrics.PodKey{Namespace: pod.Namespace, Name: pod.Name}]
			if ok {
				u := rightsizingResources{CPUMilli: usage.CPUMilli, MemBytes: usage.MemBytes}
				item.Usage = &u
				cpuRatio := ratioOf(usage.CPUMilli, pod.Requests.CPUMilli)
				memRatio := ratioOf(usage.MemBytes, pod.Requests.MemBytes)
				item.CPURatio = &cpuRatio
				item.MemRatio = &memRatio

				switch {
				case cpuRatio < RightsizingOverRatio && memRatio < RightsizingOverRatio:
					item.Classification = rightsizeOver
					item.Recommendation = &rightsizingResources{
						CPUMilli: ceilOf(usage.CPUMilli, RightsizingOverHead),
						MemBytes: ceilOf(usage.MemBytes, RightsizingOverHead),
					}
					item.RecommendNote = "两维利用率均低于 30%，建议按用量 1.3 倍收缩 request"
				case cpuRatio > RightsizingUnderRatio && memRatio > RightsizingUnderRatio:
					item.Classification = rightsizeUnder
					item.Recommendation = &rightsizingResources{
						CPUMilli: ceilOf(pod.Requests.CPUMilli, RightsizingUnderHead),
						MemBytes: ceilOf(pod.Requests.MemBytes, RightsizingUnderHead),
					}
					item.RecommendNote = "两维利用率均超过 95%，建议按 request 1.5 倍上调"
				default:
					item.Classification = rightsizeOK
					item.RecommendNote = "利用率在合理区间"
				}
			}

			switch item.Classification {
			case rightsizeOver:
				response.OverProvisioned++
			case rightsizeUnder:
				response.UnderProvisioned++
			case rightsizeUnknown:
				response.Unknown++
			}
			response.Items = append(response.Items, item)
		}

		sort.Slice(response.Items, func(i, j int) bool {
			if response.Items[i].Namespace != response.Items[j].Namespace {
				return response.Items[i].Namespace < response.Items[j].Namespace
			}
			return response.Items[i].Name < response.Items[j].Name
		})
		writeOK(c, response)
	}
}

// ratioOf 是"用量 / request"；request 为 0 时返回 0（无 request 不参与判断）。
func ratioOf(usage, request int64) float64 {
	if request <= 0 {
		return 0
	}
	return float64(usage) / float64(request)
}

func ceilOf(v int64, factor float64) int64 {
	return int64(math.Ceil(float64(v) * factor))
}
