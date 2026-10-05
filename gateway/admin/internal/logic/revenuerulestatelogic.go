// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevenueRuleStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 规则状态迁移（DRAFT→ACTIVE→ARCHIVED；ACTIVE 不可原地改价）
func NewRevenueRuleStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueRuleStateLogic {
	return &RevenueRuleStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueRuleState 转发 creator-revenue SetRevenueRuleState（本域后果最重的一步）。
//
// 让一版规则 ACTIVE 会立刻改变**所有作者**的下期应计金额，所以它是独立权限点
// revenue:rule/publish，与改草稿（revenue:rule/update）不合并。
//
// 网关挡的三类（其余一律不接管，§5 规则状态机只属于 creator-revenue）：
//  1. 主体：operator 只能由会话渲染；无会话即 fail-closed，一次 RPC 都不发。
//  2. 幂等：idempotency_key → request_id 原值。服务会派生 `<request_id>#a<rule_id>`
//     作为自动归档旧 ACTIVE 规则的子键（父键超长会毁掉重放判定，所以服务侧
//     requireScopedRequestID 会拒过长的键）—— 网关不拼、不改写、也不预先算长度。
//     命中重放时服务回首次结论（规则行），本契约同样**没有 duplicated 位**（缺口已上报）。
//  3. 不可能形状：rule_id<=0（服务 ErrRuleTargetRequired）、target_state 未填（0 不是
//     一次迁移）、reason/idempotency_key 缺空、expected_version 为负。
//
// 刻意**不下判断**的：
//   - 「这一刀是不是合法迁移」（DRAFT→ACTIVE、ACTIVE→ARCHIVED、DRAFT→ARCHIVED）需要知道
//     **当前行**，服务用 checkRuleTransition 判并回 ErrRuleStateTransition；网关不查当前
//     状态也不预先放行/拦截（预先拦截会挡住合法迁移，预先放行只是白跑一次）；
//   - expected_version=0 原样下传，由服务按「状态切换必须带 CAS 版本」拒（ErrVersionConflict
//     且消息里带当前版本）。网关**不去**先读一次规则再替它补版本号 —— 那会把乐观锁
//     变成「保证不冲突」，正好丢掉它要防的那类并发覆盖；
//   - 「ACTIVE 规则不可原地改价」是服务侧 UpsertRevenueRule 的判定，本路由也不代为改价。
func (l *RevenueRuleStateLogic) RevenueRuleState(req *types.ParamRevenueRuleState) (resp *types.RevenueRuleStateResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	operator, err := revenueOperator(l.ctx, "revenueRuleState", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := revenuePositive("rule_id", req.RuleId); err != nil {
		return nil, err
	}
	if err := revenueRuleTargetState(req.TargetState); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	rule, err := l.svcCtx.CreatorRevenue.SetRevenueRuleState(l.ctx, &creatorrevenuerpc.SetRevenueRuleStateReq{
		RuleId:          req.RuleId,
		TargetState:     creatorrevenuerpc.RuleState(req.TargetState),
		ExpectedVersion: req.ExpectedVersion,
		Operator:        operator,
		RequestId:       req.IdempotencyKey,
		Reason:          req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueRuleState: rule_id=%d target_state=%d expected_version=%d operator=%s trace_id=%s err=%v",
			req.RuleId, req.TargetState, req.ExpectedVersion, operator, req.TraceId, err)
		return nil, err
	}
	// 生效后的 state/version 是本路由唯一要转达的结论：后台据此判断「现在生效的是哪版」。
	l.Infof("gateway/admin/revenueRuleState: rule_code=%q state=%d version=%d operator=%s",
		rule.GetRuleCode(), rule.GetState(), rule.GetVersion(), operator)
	return &types.RevenueRuleStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueRuleStateData{
			Rule: revenueRuleToAPI(rule),
		},
		TTL: 0,
	}, nil
}
