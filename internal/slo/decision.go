package slo

import "github.com/kubearbiter/kubearbiter/pkg/schedcore"

// Action 是 SLO 治理给出的受控动作，与 openapi.yaml 的 SloAction 枚举一致。
type Action string

const (
	ActionNone             Action = "NONE"
	ActionTicket           Action = "TICKET"
	ActionAlert            Action = "ALERT"
	ActionBlockFullRollout Action = "BLOCK_FULL_ROLLOUT"
	ActionRollback         Action = "ROLLBACK"
)

// 稳定、机器可读的触发原因，前端与审计按此匹配，不随文案调整。
const (
	ReasonDataDegraded = "SLO_DATA_DEGRADED"
	ReasonFastBurn     = "FAST_BURN_14_4"
	ReasonReleaseSpend = "RELEASE_CONSUMED_OVER_HALF_BUDGET"
	ReasonBudgetLow    = "BUDGET_BELOW_20_PERCENT"
	ReasonBurn6h       = "BURN_6H"
	ReasonBurn3d       = "BURN_3D"
)

// Decision 是"继续推进 / 阻断全量 / 回滚"的判定结果。
type Decision struct {
	Action            Action
	PublishingAllowed bool
	Reasons           []string
}

// Decide 根据 SLO 报告与本次发布已消耗的预算比例给出动作。
// 优先级：数据降级 -> 快速燃尽或发布超支回滚 -> 预算不足阻断 -> 告警 -> 工单。
// releaseBudgetSpent 是"本次发布新增失败预算 / 发布开始前剩余预算"，取值 (0,1]。
func Decide(report Report, releaseBudgetSpent float64) Decision {
	d := Decision{Action: ActionNone, PublishingAllowed: true}

	// 数据不完整时不得驱动任何自动动作，也不允许推进全量。
	if report.Budget.Degraded {
		d.Action = ActionBlockFullRollout
		d.PublishingAllowed = false
		d.Reasons = append(d.Reasons, ReasonDataDegraded)
		return d
	}

	var fastBurn, alert, ticket bool
	for _, w := range report.Windows {
		if !w.Firing {
			continue
		}
		switch w.Action {
		case ActionRollback:
			fastBurn = true
		case ActionAlert:
			alert = true
		case ActionTicket:
			ticket = true
		}
	}

	if fastBurn || releaseBudgetSpent > 0.5 {
		d.Action = ActionRollback
		d.PublishingAllowed = false
		if fastBurn {
			d.Reasons = append(d.Reasons, ReasonFastBurn)
		}
		if releaseBudgetSpent > 0.5 {
			d.Reasons = append(d.Reasons, ReasonReleaseSpend)
		}
		return d
	}

	if !schedcore.PublishingAllowed(report.Definition.Target,
		report.Budget.TotalRequests, report.Budget.BadRequests, false) {
		d.Action = ActionBlockFullRollout
		d.PublishingAllowed = false
		d.Reasons = append(d.Reasons, ReasonBudgetLow)
		return d
	}

	switch {
	case alert:
		d.Action = ActionAlert
		d.Reasons = append(d.Reasons, ReasonBurn6h)
	case ticket:
		d.Action = ActionTicket
		d.Reasons = append(d.Reasons, ReasonBurn3d)
	}
	return d
}
