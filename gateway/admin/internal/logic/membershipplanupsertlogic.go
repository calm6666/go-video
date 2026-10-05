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

type MembershipPlanUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/修改套餐草稿（无 state 位，改完不会自动生效）
func NewMembershipPlanUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipPlanUpsertLogic {
	return &MembershipPlanUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipPlanUpsert 转发 membership UpsertPlan（新建/修改套餐草稿）。
//
// 网关做四件事，其余全部交回服务（§5 数据所有权：套餐目录归 membership）：
//  1. 身份：operator 由会话渲染成 gateway/admin:<admin_id>，表单自报的 operator 只做
//     日志线索；幂等键原样透传到 request_id（改一个字符等于换一次执行权）；
//  2. 形状门槛：只挡幂等键/编码/名称/币种的「空」（.api 写明币种必须显式带，不因
//     「默认 CNY」而省略）、「0 = *_UNSPECIFIED」的档位位与负的 CAS 位；价格是否为负、
//     时长区间、币种是否支持、平台组合是否合法、DRAFT 之外能否改规格、expected_version
//     的 CAS 语义全部由服务判定（.api 已写明「负数由服务拒」）；
//  3. 金额原样透传 *_minor，不做单位换算、不比较促销价与原价（§6 不用浮点）；
//  4. 投影服务回读的套餐行（version/mtime 以库为准，网关不本地 +1 造一个假版本）。
//
// 表单没有 state 位是刻意的：草稿改完不会自动可售，上下架走 /plan/state 的独立权限点。
// reason 是可选追溯位（2026-09-22 补进 .api）：非空时原样下传，落 mb_plan_change_log.reason；
// 留空则由服务回落成规格摘要。网关**不**代生成摘要、也不 trim——「运营到底写没写理由」
// 是台账里要能分辨的事实。reason 不进服务的幂等指纹（补理由不该被判成换规格）。
func (l *MembershipPlanUpsertLogic) MembershipPlanUpsert(req *types.ParamMembershipPlanUpsert) (resp *types.MembershipPlanUpsertResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	operator, err := membershipOperator(l.ctx, "membershipPlanUpsert", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("plan_code", req.PlanCode); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("name", req.Name); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("currency", req.Currency); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := membershipEnum("vip_type", req.VipType); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("plan_id", req.PlanId); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	plan, err := l.svcCtx.Membership.UpsertPlan(l.ctx, &membershiprpc.UpsertPlanReq{
		PlanId:             req.PlanId,
		PlanCode:           req.PlanCode,
		Name:               req.Name,
		Description:        req.Description,
		VipType:            membershiprpc.VipType(req.VipType),
		DurationDays:       req.DurationDays,
		UnitCount:          req.UnitCount,
		PriceMinor:         req.PriceMinor,
		PromPriceMinor:     req.PromPriceMinor,
		Currency:           req.Currency,
		Platforms:          membershipPlatformsFromAPI(req.Platforms),
		AutoRenewSupported: req.AutoRenewSupported,
		ExpectedVersion:    req.ExpectedVersion,
		Operator:           operator,
		RequestId:          req.IdempotencyKey,
		Reason:             req.Reason,
	})
	if err != nil {
		// trace_id 只进日志（UpsertPlanReq 没有该字段可下传）。
		l.Errorf("gateway/admin/membershipPlanUpsert: plan_id=%d plan_code=%s expected_version=%d operator=%s trace_id=%s err=%v",
			req.PlanId, req.PlanCode, req.ExpectedVersion, operator, req.TraceId, err)
		return nil, err
	}
	l.Infof("gateway/admin/membershipPlanUpsert: plan_id=%d plan_code=%s state=%d version=%d operator=%s",
		plan.GetPlanId(), plan.GetPlanCode(), plan.GetState(), plan.GetVersion(), operator)
	return &types.MembershipPlanUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipPlanUpsertData{
			Plan: membershipPlanToAPI(plan),
		},
		TTL: 0,
	}, nil
}
