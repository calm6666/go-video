// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：
// risk-control RPC → 运营后台投影 + 枚举入参归一 + 分页口径对齐。
//
// 枚举校验依赖 protoc 生成的 *_name 名字表：下游新增枚举值时网关无需跟改，
// 但越界值一定被拒绝——网关绝不把未知枚举透传给风控引擎，
// 否则会得到一条无法解释的裁决（AGENTS.md §9）。
// 网关不判定风险：命中与否、降级与否只由 risk-control 决定。

package logic

import (
	"errors"
	"fmt"

	"go-video/common/validation"
	"go-video/gateway/admin/internal/types"
	riskcontrolrpc "go-video/services/risk-control/rpc"
)

// riskMaxPageSize 与 risk-control 三个列表接口的分页上限一致（logic.maxPageSize = 50）。
const riskMaxPageSize = 50

// normalizeRiskPage 直接复用 risk-control 使用的 common/validation.NormalizePage，
// 保证网关与服务端的分页归一逻辑同源，不会出现两边口径漂移。
func normalizeRiskPage(pn, ps int32) (int32, int32) {
	page := validation.NormalizePage(int(pn), int(ps), riskMaxPageSize)
	return int32(page.Page), int32(page.PageSize)
}

// riskGuardedAction 校验受保护动作取值。
// allowUnspecified=true 用于过滤条件与「全域」语义（处罚 scope、规则 action_type）；
// CheckAction/ReportAction 传 false，ACTION_UNSPECIFIED 在它们那里是非法入参。
func riskGuardedAction(v int32, allowUnspecified bool) (riskcontrolrpc.GuardedAction, error) {
	if _, ok := riskcontrolrpc.GuardedAction_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid action %d", v)
	}
	if v == 0 && !allowUnspecified {
		return 0, errors.New("gateway/admin: action must be 1..7")
	}
	return riskcontrolrpc.GuardedAction(v), nil
}

// riskMetric 校验规则指标；METRIC_UNSPECIFIED 不是可评估指标。
func riskMetric(v int32) (riskcontrolrpc.Metric, error) {
	if _, ok := riskcontrolrpc.Metric_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid metric %d", v)
	}
	if v == 0 {
		return 0, errors.New("gateway/admin: metric must be 1..5")
	}
	return riskcontrolrpc.Metric(v), nil
}

// riskMetricFilter 校验规则列表的指标过滤条件：
// METRIC_UNSPECIFIED(0) 在 ListRules 那里是「不过滤」，越界值仍然拒绝，
// 与 risk-control 判定（metric != UNSPECIFIED && 未知 -> ErrInvalidTarget）一致。
func riskMetricFilter(v int32) (riskcontrolrpc.Metric, error) {
	if v == 0 {
		return riskcontrolrpc.Metric_METRIC_UNSPECIFIED, nil
	}
	return riskMetric(v)
}

// riskStateFilter 校验规则/名单列表的 state 三态过滤条件：-1 不过滤、0 停用、1 生效。
// 这两个 RPC 的 state 是裸 int32（不是枚举），网关先挡一次，
// 免得把 2、7 之类的值透传成一条无解释的 ErrInvalidTarget。
func riskStateFilter(field string, v int32) error {
	if v != -1 && v != 0 && v != 1 {
		return fmt.Errorf("gateway/admin: %s must be -1(不过滤)/0(停用)/1(生效)", field)
	}
	return nil
}

// riskCompareOp 校验阈值比较方向；OP_UNSPECIFIED 非法。
func riskCompareOp(v int32) (riskcontrolrpc.CompareOp, error) {
	if _, ok := riskcontrolrpc.CompareOp_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid op %d", v)
	}
	if v == 0 {
		return 0, errors.New("gateway/admin: op must be 1..5")
	}
	return riskcontrolrpc.CompareOp(v), nil
}

// riskPunitiveDecision 校验「可作为处罚/规则结论」的裁决：
// 只允许 CHALLENGE(2)/BLOCK(3)/REVIEW(4)——ALLOW 不是处罚，UNSPECIFIED 是漏传。
func riskPunitiveDecision(v int32, field string) (riskcontrolrpc.Decision, error) {
	if _, ok := riskcontrolrpc.Decision_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid %s %d", field, v)
	}
	if v != int32(riskcontrolrpc.Decision_DECISION_CHALLENGE) &&
		v != int32(riskcontrolrpc.Decision_DECISION_BLOCK) &&
		v != int32(riskcontrolrpc.Decision_DECISION_REVIEW) {
		return 0, fmt.Errorf("gateway/admin: %s must be 2/3/4", field)
	}
	return riskcontrolrpc.Decision(v), nil
}

// riskListType 校验名单类型；allowUnspecified=true 用于过滤条件。
func riskListType(v int32, allowUnspecified bool) (riskcontrolrpc.ListType, error) {
	if _, ok := riskcontrolrpc.ListType_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid list_type %d", v)
	}
	if v == 0 && !allowUnspecified {
		return 0, errors.New("gateway/admin: list_type must be 1/2")
	}
	return riskcontrolrpc.ListType(v), nil
}

// riskTargetType 校验名单目标类型；allowUnspecified=true 用于过滤条件。
func riskTargetType(v int32, allowUnspecified bool) (riskcontrolrpc.TargetType, error) {
	if _, ok := riskcontrolrpc.TargetType_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid target_type %d", v)
	}
	if v == 0 && !allowUnspecified {
		return 0, errors.New("gateway/admin: target_type must be 1/2/3")
	}
	return riskcontrolrpc.TargetType(v), nil
}

// riskPunishmentState 校验处罚状态过滤条件（UNSPECIFIED 表示不过滤）。
func riskPunishmentState(v int32) (riskcontrolrpc.PunishmentState, error) {
	if _, ok := riskcontrolrpc.PunishmentState_name[v]; !ok {
		return 0, fmt.Errorf("gateway/admin: invalid state %d", v)
	}
	return riskcontrolrpc.PunishmentState(v), nil
}

// riskRuleToAPI 投影风控规则（含 version，便于解释历史裁决）。
func riskRuleToAPI(r *riskcontrolrpc.Rule) types.RiskRuleItem {
	return types.RiskRuleItem{
		RuleId:        r.GetRuleId(),
		Name:          r.GetName(),
		ActionType:    int32(r.GetActionType()),
		Metric:        int32(r.GetMetric()),
		Op:            int32(r.GetOp()),
		Threshold:     r.GetThreshold(),
		WindowSeconds: r.GetWindowSeconds(),
		Decision:      int32(r.GetDecision()),
		Priority:      r.GetPriority(),
		State:         r.GetState(),
		Version:       r.GetVersion(),
		Operator:      r.GetOperator(),
		Ctime:         r.GetCtime(),
		Mtime:         r.GetMtime(),
	}
}

func riskRulesToAPI(list []*riskcontrolrpc.Rule) []types.RiskRuleItem {
	out := make([]types.RiskRuleItem, 0, len(list))
	for _, r := range list {
		out = append(out, riskRuleToAPI(r))
	}
	return out
}

// riskRuleHitsToAPI 投影规则命中明细：后台排障要看观测值和命中版本，缺一不可解释。
func riskRuleHitsToAPI(list []*riskcontrolrpc.RuleHit) []types.RiskRuleHitItem {
	out := make([]types.RiskRuleHitItem, 0, len(list))
	for _, h := range list {
		out = append(out, types.RiskRuleHitItem{
			RuleId:        h.GetRuleId(),
			Version:       h.GetVersion(),
			Name:          h.GetName(),
			Metric:        int32(h.GetMetric()),
			Op:            int32(h.GetOp()),
			Threshold:     h.GetThreshold(),
			Observed:      h.GetObserved(),
			WindowSeconds: h.GetWindowSeconds(),
			Decision:      int32(h.GetDecision()),
			Priority:      h.GetPriority(),
		})
	}
	return out
}

// riskPunishmentSnapshotToAPI 投影生效处罚摘要（不含运营内部 reason）。
func riskPunishmentSnapshotToAPI(p *riskcontrolrpc.PunishmentSnapshot) types.RiskPunishmentSnapshotItem {
	return types.RiskPunishmentSnapshotItem{
		PunishmentId:     p.GetPunishmentId(),
		Scope:            int32(p.GetScope()),
		Decision:         int32(p.GetDecision()),
		Permanent:        p.GetPermanent(),
		EndAt:            p.GetEndAt(),
		RemainingSeconds: p.GetRemainingSeconds(),
		ReasonCode:       p.GetReasonCode(),
	}
}

// riskPunishmentToAPI 投影处罚全量字段：reason 是运营内部说明，
// 只出现在 /admin 链路，禁止由终端接口下发（riskcontrol.v1.Punishment 注释）。
func riskPunishmentToAPI(p *riskcontrolrpc.Punishment) types.RiskPunishmentItem {
	return types.RiskPunishmentItem{
		PunishmentId:   p.GetPunishmentId(),
		Mid:            p.GetMid(),
		Scope:          int32(p.GetScope()),
		Decision:       int32(p.GetDecision()),
		Reason:         p.GetReason(),
		ReasonCode:     p.GetReasonCode(),
		Operator:       p.GetOperator(),
		StartAt:        p.GetStartAt(),
		EndAt:          p.GetEndAt(),
		State:          int32(p.GetState()),
		IdempotencyKey: p.GetIdempotencyKey(),
		LiftOperator:   p.GetLiftOperator(),
		Ctime:          p.GetCtime(),
		Mtime:          p.GetMtime(),
	}
}

func riskPunishmentsToAPI(list []*riskcontrolrpc.Punishment) []types.RiskPunishmentItem {
	out := make([]types.RiskPunishmentItem, 0, len(list))
	for _, p := range list {
		out = append(out, riskPunishmentToAPI(p))
	}
	return out
}

// riskDeviceProfileToAPI 投影设备画像：只搬运 device_hash，
// 网关拿不到也不打印设备号原文。
func riskDeviceProfileToAPI(d *riskcontrolrpc.DeviceProfile) types.RiskDeviceProfileItem {
	return types.RiskDeviceProfileItem{
		DeviceHash:      d.GetDeviceHash(),
		Labels:          d.GetLabels(),
		RiskScore:       d.GetRiskScore(),
		FirstSeen:       d.GetFirstSeen(),
		LastSeen:        d.GetLastSeen(),
		RelatedMidCount: d.GetRelatedMidCount(),
		Ctime:           d.GetCtime(),
		Mtime:           d.GetMtime(),
	}
}

func riskListEntryToAPI(e *riskcontrolrpc.ListEntry) types.RiskListEntryItem {
	return types.RiskListEntryItem{
		Id:          e.GetId(),
		ListType:    int32(e.GetListType()),
		TargetType:  int32(e.GetTargetType()),
		TargetValue: e.GetTargetValue(),
		Reason:      e.GetReason(),
		Operator:    e.GetOperator(),
		ExpireAt:    e.GetExpireAt(),
		State:       e.GetState(),
		Ctime:       e.GetCtime(),
		Mtime:       e.GetMtime(),
	}
}

func riskListEntriesToAPI(list []*riskcontrolrpc.ListEntry) []types.RiskListEntryItem {
	out := make([]types.RiskListEntryItem, 0, len(list))
	for _, e := range list {
		out = append(out, riskListEntryToAPI(e))
	}
	return out
}

// riskInt64Ids 复制 protobuf 重复字段，避免响应体与消息内部切片共享底层数组。
// nil 输入返回空切片，后台拿到的是 [] 而不是 null。
func riskInt64Ids(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	return append(out, ids...)
}
