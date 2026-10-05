package logic

import (
	"context"
	"fmt"

	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpsertRuleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpsertRuleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpsertRuleLogic {
	return &UpsertRuleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// maxRuleNameLen 与 risk_rule.name 的列宽（VARCHAR(128)，同时是 uniq_name 的组成列，
// 见 deploy/migrations/risk-control/000001_create_risk_control_decision_tables.sql:47,61）对齐。
// 规则名不能像 platform/app_version 那样交给 sanitizeShortString 裁断：MySQL 的 VARCHAR 长度
// 按**字符**计而 Go 的 len 按字节计，一条中文长名裁到 128 字节会切断多字节序列（落库成非法 UTF-8），
// 更糟的是两条只在第 43 个汉字之后才不同的规则会折叠成同一个 name，
// 第二条于是以「ErrRuleNameDuplicated: <调用方从没发过的名字>」失败 ——
// 规则名是决策日志里的审计身份，只能拒绝不能改写（与 upsertlistentrylogic.go 的 target_value 同口径）。
const maxRuleNameLen = 128

// 新增/更新风控规则（admin 专用，AGENTS.md §5：risk_rule 只有本服务可写）。
//
// 版本语义：任一影响评估结果的字段（动作/指标/比较符/阈值/窗口/裁决/优先级）变化即 version+1，
// 仅启停或换操作人不产生新版本。由此得到两个结论：
//   - 运营重试同一份 payload 不会刷版本（幂等），因此 idempotency_key 本期只用于日志关联；
//   - risk_check_log 里的 rule_id@version 足以还原「当时用的是哪一版规则」。
//
// state 语义：proto3 无法区分「未传」与「传 0」，新建规则 state=0 即创建为停用，
// 由运营显式置 1 生效 —— 宁可不生效，也不静默上线一条没人复核的规则。
func (l *UpsertRuleLogic) UpsertRule(in *rpc.UpsertRuleReq) (*rpc.UpsertRuleReply, error) {
	action, err := ruleActionFromProto(in.GetActionType())
	if err != nil {
		return nil, err
	}
	// 名字照旧规范化（去首尾空白与控制符），但把裁断换成拒绝：见 maxRuleNameLen 注释。
	name := sanitizeShortString(in.GetName(), 0)
	if len(name) > maxRuleNameLen {
		return nil, fmt.Errorf("%w: name too long, max %d bytes", model.ErrInvalidRule, maxRuleNameLen)
	}
	rule := &model.RiskRule{
		RuleID:        in.GetRuleId(),
		Name:          name,
		ActionType:    action,
		Metric:        metricFromProto(in.GetMetric()),
		Op:            int32(in.GetOp()),
		Threshold:     in.GetThreshold(),
		WindowSeconds: in.GetWindowSeconds(),
		Decision:      int32(in.GetDecision()),
		Priority:      in.GetPriority(),
		State:         in.GetState(),
		Operator:      in.GetOperator(),
	}
	// 规则名不可变更由 repository 强制（更新时以库内值覆盖），这里只做入参校验。
	saved, created, err := l.svcCtx.Repository.UpsertRule(l.ctx, rule)
	if err != nil {
		l.Errorf("risk-control/UpsertRule: rejected rule_id=%d name=%s operator=%d err=%v",
			in.GetRuleId(), rule.Name, in.GetOperator(), err)
		return nil, err
	}
	l.Infof("risk-control/UpsertRule: rule=%d version=%d created=%t operator=%d idempotency_key=%s",
		saved.RuleID, saved.Version, created, saved.Operator,
		model.TruncateHash(sanitizeShortString(in.GetIdempotencyKey(), 64), 16))
	return &rpc.UpsertRuleReply{Rule: ruleToProto(saved), Created: created}, nil
}
