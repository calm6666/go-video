package logic

import (
	"context"
	"fmt"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type SetRevenueRuleStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSetRevenueRuleStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SetRevenueRuleStateLogic {
	return &SetRevenueRuleStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 运营面：规则状态切换（DRAFT→ACTIVE→ARCHIVED）
//
// 幂等：cr_rule_change_log.uniq_request_id 兜住重放；同一 request_id 重复调用回首次结果。
// 副作用：置 ACTIVE 会在同一事务里把同来源的旧 ACTIVE 规则自动归档并各记一笔台账，
// 因为「一类收益同一时刻只能有一套生效单价」，两套 ACTIVE 等于让计量自己猜按哪条折算。
func (l *SetRevenueRuleStateLogic) SetRevenueRuleState(in *rpc.SetRevenueRuleStateReq) (*rpc.RevenueRuleInfo, error) {
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	if in.RuleId <= 0 {
		return nil, model.ErrRuleTargetRequired
	}
	pre, err := l.svcCtx.Rules.FindOne(l.ctx, in.RuleId)
	if err != nil {
		return nil, err
	}
	if pre == nil {
		return nil, fmt.Errorf("%w: rule_id=%d", model.ErrRuleNotFound, in.RuleId)
	}

	target := int32(in.TargetState)
	operator, err := requireOperator(in.Operator)
	if err != nil {
		return nil, err
	}
	reason, err := requireReason(in.Reason)
	if err != nil {
		return nil, err
	}
	// 台账子键 = request_id + "#a<rule_id>"，父键长度必须先把空间留出来。
	requestID, err := requireScopedRequestID(in.RequestId, autoArchiveSuffixBytes)
	if err != nil {
		return nil, err
	}
	if in.ExpectedVersion <= 0 {
		return nil, fmt.Errorf("%w: 状态切换必须带 expected_version（当前服务端 version=%d）",
			model.ErrVersionConflict, pre.Version)
	}
	// 先做一次无锁的迁移预判，把明显非法的请求挡在事务外（少烧一次行锁）。
	if err := checkRuleTransition(pre, target); err != nil {
		return nil, err
	}

	err = l.svcCtx.Transact(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		return applyRuleStateChange(ctx, tx, pre, target, in.ExpectedVersion, operator, requestID, reason)
	})
	if err != nil {
		if model.IsDuplicateErr(err) {
			return resolveRuleReplay(l.ctx, l.svcCtx.Rules, l.svcCtx.RuleChanges, pre.RuleCode, requestID)
		}
		l.Errorf("set revenue rule state rule_code=%s target=%d: %v", pre.RuleCode, target, err)
		return nil, err
	}

	cur, err := l.svcCtx.Rules.FindOne(l.ctx, pre.RuleId)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, fmt.Errorf("%w: rule_id=%d 提交后读不到", model.ErrRuleNotFound, pre.RuleId)
	}
	return ruleInfo(cur), nil
}
