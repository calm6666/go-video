// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MembershipPlanStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 套餐上下架（只允许合法迁移；改价必须走新草稿，reason 必填）
func NewMembershipPlanStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipPlanStateLogic {
	return &MembershipPlanStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipPlanState 转发 membership SetPlanState（上下架）。
//
// 状态机只有 DRAFT→ON_SALE、ON_SALE→OFF_SALE、OFF_SALE→ON_SALE 三条边，判定完全在
// membership（§8「写接口只能推进合法状态」）：网关连「同状态重复提交」都不拦，因为
// 服务把幂等重放排在状态机校验之前，先拦一次就会把合法重试误判成非法迁移。
//
// 网关只挡三样东西：主体（会话渲染的 operator）、幂等键、以及服务侧无条件硬失败的必填位
// （plan_id>0、target_state 非 UNSPECIFIED、reason 非空）。reason 在服务侧无条件必填
// （上下架立刻影响终端可见性），所以网关可以提前给出可定位字段的错误消息；
// expected_version 只在 0 以下拒——CAS 是否命中由服务判，重放路径本身不需要它。
//
// 返回的套餐行是服务改完后回读的那一行（含新 state 与新 version），网关不推算结果状态。
func (l *MembershipPlanStateLogic) MembershipPlanState(req *types.ParamMembershipPlanState) (resp *types.MembershipPlanStateResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	operator, err := membershipOperator(l.ctx, "membershipPlanState", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := membershipPositive("plan_id", req.PlanId); err != nil {
		return nil, err
	}
	if err := membershipEnum("target_state", req.TargetState); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	plan, err := l.svcCtx.Membership.SetPlanState(l.ctx, &membershiprpc.SetPlanStateReq{
		PlanId:          req.PlanId,
		TargetState:     membershiprpc.PlanSaleState(req.TargetState),
		ExpectedVersion: req.ExpectedVersion,
		Operator:        operator,
		RequestId:       req.IdempotencyKey,
		Reason:          req.Reason,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipPlanState: plan_id=%d target_state=%d expected_version=%d operator=%s trace_id=%s err=%v",
			req.PlanId, req.TargetState, req.ExpectedVersion, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/membershipPlanState: plan_id=%d plan_code=%s state=%d version=%d operator=%s",
		req.PlanId, plan.GetPlanCode(), plan.GetState(), plan.GetVersion(), operator)
	return &types.MembershipPlanStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipPlanStateData{
			Plan: membershipPlanToAPI(plan),
		},
		TTL: 0,
	}, nil
}
