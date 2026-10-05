package policy

import "go-video/services/danmaku/model"

// legalTransitions 是弹幕状态机的合法迁移表。
// key: from 状态；value: 允许到达的目标状态集合。取值见 model.State* 常量。
//
// 依据 AGENTS.md §8：审核结论只能推进合法状态，删除是终态，
// 已删除弹幕不得回到任何可见状态（避免误恢复被删内容）。
//
// 允许 Normal → Rejected：机审先放行、人审后置驳回是真实流程，
// 驳回必须能把已下发的弹幕直接拉回屏蔽池；这类迁移只会收紧可见性，
// 不存在「放宽可见性绕过机审」的风险（放宽只有 Rejected/Folded → Normal 的复核放行）。
var legalTransitions = map[int32][]int32{
	model.StatePending:  {model.StateNormal, model.StateFolded, model.StateRejected, model.StateDeleted},
	model.StateNormal:   {model.StatePending, model.StateFolded, model.StateRejected, model.StateDeleted},
	model.StateFolded:   {model.StatePending, model.StateNormal, model.StateRejected, model.StateDeleted},
	model.StateRejected: {model.StatePending, model.StateNormal, model.StateDeleted},
	model.StateDeleted:  {},
}

// CanTransition 校验 from → to 是否为合法迁移。
// 同状态自迁移视为非法，由调用方按“无需迁移”幂等处理。
func CanTransition(from, to int32) bool {
	targets, ok := legalTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// States 返回状态机全部状态，便于文档与测试遍历。
func States() []int32 {
	return []int32{
		model.StateNormal,
		model.StatePending,
		model.StateFolded,
		model.StateDeleted,
		model.StateRejected,
	}
}

// 审核结论取值，与 rpc.ModerationVerdict 和 moderation.v1.Verdict 一致。
const (
	// VerdictPass 通过。
	VerdictPass int32 = 1
	// VerdictReview 转人审。
	VerdictReview int32 = 2
	// VerdictReject 拒绝。
	VerdictReject int32 = 3
)

// TargetForVerdict 把审核结论映射为弹幕目标状态与弹幕池。
// 返回 ok=false 表示结论取值非法（含 VERDICT_UNSPECIFIED）。
//
// 映射规则：
//   - PASS    → 正常 + 普通池（唯一能让弹幕对所有人可见的路径）
//   - REVIEW  → 待审核 + 审核池（继续只对自身可见）
//   - REJECT  → 驳回 + 屏蔽池（不进入任何下发包）
func TargetForVerdict(verdict int32) (toState, toPool int32, ok bool) {
	switch verdict {
	case VerdictPass:
		return model.StateNormal, model.PoolNormal, true
	case VerdictReview:
		return model.StatePending, model.PoolReview, true
	case VerdictReject:
		return model.StateRejected, model.PoolBlock, true
	default:
		return 0, 0, false
	}
}

// InitialState 返回发送侧落库的初始状态与弹幕池。
// blocked=true 表示命中屏蔽词：直接进屏蔽池并折叠，等待人审；
// 未命中时若机审开关关闭则直接正常池（仅限开发/回放环境，见 README 缺口）。
func InitialState(blocked, machineReview bool) (state, pool int32) {
	if blocked {
		return model.StateFolded, model.PoolBlock
	}
	if !machineReview {
		return model.StateNormal, model.PoolNormal
	}
	return model.StatePending, model.PoolReview
}
