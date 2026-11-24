// Package slo 提供 SLO 定义、错误预算与多窗口燃尽率的纯计算逻辑。
// 这里不做任何 IO：分钟级窗口数据由调用方从预聚合表读入，便于单测与历史回放。
package slo

import (
	"fmt"
	"time"
)

// SLIType 与 openapi.yaml 的 SliType、db/schema.sql 的 CHECK 约束保持一致。
type SLIType string

const (
	SLIAvailability SLIType = "AVAILABILITY"
	SLILatency      SLIType = "LATENCY"
)

func (t SLIType) Validate() error {
	switch t {
	case SLIAvailability, SLILatency:
		return nil
	default:
		return fmt.Errorf("未知的 SLI 类型 %q", t)
	}
}

// Definition 对应 slo_definitions 表的一行。
type Definition struct {
	Service            string
	SLIType            SLIType
	Target             float64
	Window             time.Duration
	LatencyThresholdMS *int
}

func (d Definition) Validate() error {
	if d.Service == "" {
		return fmt.Errorf("service 不能为空")
	}
	if err := d.SLIType.Validate(); err != nil {
		return err
	}
	if d.Target <= 0 || d.Target >= 1 {
		return fmt.Errorf("target 必须在 (0,1) 之间，当前为 %v", d.Target)
	}
	if d.Window <= 0 {
		return fmt.Errorf("window 必须大于 0")
	}
	if d.SLIType == SLILatency && (d.LatencyThresholdMS == nil || *d.LatencyThresholdMS <= 0) {
		return fmt.Errorf("LATENCY 类型必须提供正的 latency_threshold_ms")
	}
	return nil
}

// Bucket 是 1 分钟粒度的预聚合窗口，对应 slo_windows 表一行。
type Bucket struct {
	Start     time.Time
	Total     int64
	Errors    int64
	LatencyOK int64
}

// Bad 返回该桶中不满足 SLI 的请求数：可用性计错误数，延迟计未达标数。
func (b Bucket) Bad(sli SLIType) int64 {
	if sli == SLILatency {
		if b.LatencyOK > b.Total {
			return 0
		}
		return b.Total - b.LatencyOK
	}
	if b.Errors > b.Total {
		return b.Total
	}
	return b.Errors
}

// FormatWindow 把时长渲染成契约里的窗口写法（1h / 5m / 3d / 30d）。
func FormatWindow(d time.Duration) string {
	switch d {
	case 30 * 24 * time.Hour:
		return "30d"
	case 3 * 24 * time.Hour:
		return "3d"
	case 24 * time.Hour:
		return "1d"
	case time.Hour:
		return "1h"
	case 30 * time.Minute:
		return "30m"
	case 5 * time.Minute:
		return "5m"
	}
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// ParseWindow 只接受契约 /slo/{service}/budget?window= 列出的四个取值。
func ParseWindow(raw string) (time.Duration, bool) {
	switch raw {
	case "1h":
		return time.Hour, true
	case "6h":
		return 6 * time.Hour, true
	case "3d":
		return 3 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	default:
		return 0, false
	}
}
