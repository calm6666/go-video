package logic

import (
	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"
)

// 本文件集中放置 model/policy ↔ rpc 的映射。
// 集中一处是为了让「proto 枚举值」与「库表存储值」的对齐关系可被一眼审计，
// 避免出现两份不同的枚举翻译表（rpc 与 model 的编号约定见 model/types.go 注释）。

// metricToProto 指标名 -> proto 枚举；未识别返回 UNSPECIFIED（不伪造为某个已实现指标）。
func metricToProto(metric string) rpc.Metric {
	switch metric {
	case model.MetricActionCount:
		return rpc.Metric_METRIC_ACTION_COUNT
	case model.MetricDeviceActionCount:
		return rpc.Metric_METRIC_DEVICE_ACTION_COUNT
	case model.MetricIpActionCount:
		return rpc.Metric_METRIC_IP_ACTION_COUNT
	case model.MetricDeviceRiskScore:
		return rpc.Metric_METRIC_DEVICE_RISK_SCORE
	case model.MetricDeviceMidCount:
		return rpc.Metric_METRIC_DEVICE_MID_COUNT
	default:
		return rpc.Metric_METRIC_UNSPECIFIED
	}
}

// metricFromProto proto 枚举 -> 指标名；未实现指标返回空串，
// 由 model.RiskRule.Validate 拒绝写入（避免配出永远「不可观测」的规则）。
func metricFromProto(m rpc.Metric) string {
	switch m {
	case rpc.Metric_METRIC_ACTION_COUNT:
		return model.MetricActionCount
	case rpc.Metric_METRIC_DEVICE_ACTION_COUNT:
		return model.MetricDeviceActionCount
	case rpc.Metric_METRIC_IP_ACTION_COUNT:
		return model.MetricIpActionCount
	case rpc.Metric_METRIC_DEVICE_RISK_SCORE:
		return model.MetricDeviceRiskScore
	case rpc.Metric_METRIC_DEVICE_MID_COUNT:
		return model.MetricDeviceMidCount
	default:
		return ""
	}
}

// ruleToProto 规则转换。
func ruleToProto(r *model.RiskRule) *rpc.Rule {
	if r == nil {
		return nil
	}
	return &rpc.Rule{
		RuleId:        r.RuleID,
		Name:          r.Name,
		ActionType:    rpc.GuardedAction(r.ActionType),
		Metric:        metricToProto(r.Metric),
		Op:            rpc.CompareOp(r.Op),
		Threshold:     r.Threshold,
		WindowSeconds: r.WindowSeconds,
		Decision:      rpc.Decision(r.Decision),
		Priority:      r.Priority,
		State:         r.State,
		Version:       r.Version,
		Operator:      r.Operator,
		Ctime:         r.Ctime,
		Mtime:         r.Mtime,
	}
}

func rulesToProto(rows []*model.RiskRule) []*rpc.Rule {
	out := make([]*rpc.Rule, 0, len(rows))
	for _, r := range rows {
		if p := ruleToProto(r); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// punishmentToProto 运营视角的完整处罚记录。
func punishmentToProto(p *model.RiskPunishment) *rpc.Punishment {
	if p == nil {
		return nil
	}
	return &rpc.Punishment{
		PunishmentId:   p.PunishmentID,
		Mid:            p.Mid,
		Scope:          rpc.GuardedAction(p.Scope),
		Decision:       rpc.Decision(p.Decision),
		Reason:         p.Reason,
		ReasonCode:     p.ReasonCode,
		Operator:       p.Operator,
		StartAt:        p.StartAt,
		EndAt:          p.EndAt,
		State:          rpc.PunishmentState(p.State),
		IdempotencyKey: p.IdempotencyKey,
		LiftOperator:   p.LiftOperator,
		Ctime:          p.Ctime,
		Mtime:          p.Mtime,
	}
}

func punishmentsToProto(rows []*model.RiskPunishment) []*rpc.Punishment {
	out := make([]*rpc.Punishment, 0, len(rows))
	for _, p := range rows {
		if q := punishmentToProto(p); q != nil {
			out = append(out, q)
		}
	}
	return out
}

// snapshotFromView 生效处罚摘要：只暴露端上需要渲染的字段，
// reason（运营内部说明）与 operator 不在摘要里，避免下发出敏感审计信息。
func snapshotFromView(v *policy.PunishmentView, now int64) *rpc.PunishmentSnapshot {
	if v == nil {
		return nil
	}
	return &rpc.PunishmentSnapshot{
		PunishmentId:     v.ID,
		Scope:            rpc.GuardedAction(v.Scope),
		Decision:         rpc.Decision(v.Decision),
		Permanent:        v.Permanent(),
		EndAt:            v.EndAt,
		RemainingSeconds: v.RemainingSeconds(now),
		ReasonCode:       v.ReasonCode,
	}
}

// ruleHitToProto 命中明细。
func ruleHitToProto(h policy.Hit) *rpc.RuleHit {
	return &rpc.RuleHit{
		RuleId:        h.RuleID,
		Version:       h.Version,
		Name:          h.Name,
		Metric:        metricToProto(h.Metric),
		Op:            rpc.CompareOp(h.Op),
		Threshold:     h.Threshold,
		Observed:      h.Observed,
		WindowSeconds: h.WindowSeconds,
		Decision:      rpc.Decision(h.Decision),
		Priority:      h.Priority,
	}
}

// decisionToReply 把可解释裁决映射为 CheckAction 响应。
func decisionToReply(res *policy.Result, now int64) *rpc.CheckActionReply {
	if res == nil {
		return &rpc.CheckActionReply{Decision: rpc.Decision_DECISION_UNSPECIFIED}
	}
	hits := make([]*rpc.RuleHit, 0, len(res.Hits))
	for _, h := range res.Hits {
		hits = append(hits, ruleHitToProto(h))
	}
	return &rpc.CheckActionReply{
		RequestId:           res.RequestID,
		Decision:            rpc.Decision(res.Decision),
		Score:               res.Score,
		HitRuleIds:          res.HitRuleIDs,
		RuleHits:            hits,
		Punishment:          snapshotFromView(res.Punishment, now),
		ActionCode:          res.ActionCode,
		ChallengeTtlSeconds: res.ChallengeTTLSeconds,
		Basis:               res.Basis,
		SkippedRuleIds:      res.SkippedRuleIDs,
		Evaluated:           res.Evaluated,
		Degraded:            res.Degraded,
	}
}

// deviceToProto 设备画像（只回 device_hash，不回设备号原文）。
func deviceToProto(d *model.RiskDeviceProfile) *rpc.DeviceProfile {
	if d == nil {
		return nil
	}
	return &rpc.DeviceProfile{
		DeviceHash:      d.DeviceHash,
		Labels:          d.LabelList(),
		RiskScore:       d.RiskScore,
		FirstSeen:       d.FirstSeen,
		LastSeen:        d.LastSeen,
		RelatedMidCount: d.RelatedMidCount,
		Ctime:           d.Ctime,
		Mtime:           d.Mtime,
	}
}

// listEntryToProto 名单条目。
func listEntryToProto(e *model.RiskList) *rpc.ListEntry {
	if e == nil {
		return nil
	}
	return &rpc.ListEntry{
		Id:          e.ID,
		ListType:    rpc.ListType(e.ListType),
		TargetType:  rpc.TargetType(e.TargetType),
		TargetValue: e.TargetValue,
		Reason:      e.Reason,
		Operator:    e.Operator,
		ExpireAt:    e.ExpireAt,
		State:       e.State,
		Ctime:       e.Ctime,
		Mtime:       e.Mtime,
	}
}

func listEntriesToProto(rows []*model.RiskList) []*rpc.ListEntry {
	out := make([]*rpc.ListEntry, 0, len(rows))
	for _, e := range rows {
		if p := listEntryToProto(e); p != nil {
			out = append(out, p)
		}
	}
	return out
}
