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

type RevenueEnrollmentStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 暂停/恢复参与（违规暂停期间不结算；加入与退出归创作者本人，不开后台口）
func NewRevenueEnrollmentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueEnrollmentStateLogic {
	return &RevenueEnrollmentStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueEnrollmentState 转发 creator-revenue SetEnrollmentState（运营侧暂停/恢复收益资格）。
//
// 这一位直接决定「这一期要不要给他出单」，所以是独立权限点 revenue:enrollment/update。
//
// 网关挡的三类（其余归服务，§5 参与关系只属于 creator-revenue）：
//  1. 主体：operator 只能由会话渲染成 gateway/admin:<admin_id>，无会话 fail-closed。
//     服务侧还有一条与此对齐的硬规则：operator=="user"（自助身份）被 ErrForbidden 拒——
//     暂停/恢复是运营处置动作，不接受自助身份；网关永远给得出 gateway/admin:<id>，
//     所以这条拒绝在本域实际落在「没有主体」上，正因如此不能放开。
//  2. 幂等：idempotency_key → request_id 原值（服务用 request_id 台账判重）。
//     duplicated=true 是**成功结论**并回首次状态行；不自己造号、冲突不重打。
//  3. 不可能形状：mid<=0（normalizeMid / ErrInvalidMid，分成没有游客作者号）、
//     target_state 不在 {ENROLLED=1, SUSPENDED=3}（revenueEnrollmentTargetState：
//     LEFT=2 是合法枚举但「退出计划」是创作者本人动作，从后台塞进来等于代签退出，
//     争议时不能当证据）、reason/idempotency_key 缺空。
//
// 刻意**不下判断**的：
//   - 「当前态配不配」由服务判（对 LEFT 或未参加作者做暂停 → ErrEnrollmentStateTransition
//     / ErrEnrollmentNotFound / ErrNotEnrolled），网关不预先查参与行也不代为放宽；
//   - reason 会被服务截断进 cr_enrollment.remark 并写进变更台账，正文不进网关日志。
//
// 响应逐位转达：state / agreed_rule_version / operator / remark 是「谁在什么时候把他怎么了」
// 的证据链，网关不改写、不美化（尤其不把 SUSPENDED 显示成 ENROLLED）。
func (l *RevenueEnrollmentStateLogic) RevenueEnrollmentState(req *types.ParamRevenueEnrollmentState) (resp *types.RevenueEnrollmentStateResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	operator, err := revenueOperator(l.ctx, "revenueEnrollmentState", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := revenuePositive("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := revenueEnrollmentTargetState(req.TargetState); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.SetEnrollmentState(l.ctx, &creatorrevenuerpc.SetEnrollmentStateReq{
		Mid:         req.Mid,
		TargetState: creatorrevenuerpc.EnrollmentState(req.TargetState),
		Operator:    operator,
		RequestId:   req.IdempotencyKey,
		Reason:      req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueEnrollmentState: mid=%d target_state=%d operator=%s trace_id=%s err=%v",
			req.Mid, req.TargetState, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/revenueEnrollmentState: mid=%d target_state=%d duplicated=%t state=%d operator=%s",
		req.Mid, req.TargetState, reply.GetDuplicated(), reply.GetEnrollment().GetState(), operator)
	return &types.RevenueEnrollmentStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueEnrollmentStateData{
			Duplicated: reply.GetDuplicated(),
			Enrollment: revenueEnrollmentToAPI(reply.GetEnrollment()),
		},
		TTL: 0,
	}, nil
}
