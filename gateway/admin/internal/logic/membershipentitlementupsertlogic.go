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

type MembershipEntitlementUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 权益码新增/开关（关掉即全站该能力判否，故与套餐权限点分离）
func NewMembershipEntitlementUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipEntitlementUpsertLogic {
	return &MembershipEntitlementUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipEntitlementUpsert 转发 membership UpsertEntitlement（权益码新增/开关）。
//
// enabled 是全站能力闸：关掉之后所有依赖该码的能力立刻判 ENTITLEMENT_CODE_DISABLED，
// 所以网关**不**给它任何默认值也不「按 name 变化推断」——表单显式给什么就传什么，
// false 是一次真实动作而不是漏填。code 的字符集、长度、能否改名、expected_version 的
// CAS 语义与幂等指纹全部由 membership 判定（§5：权益判定口径归本域）。
//
// 返回的是服务回读后的目录行（含新 version）；重放同一 request_id 会回首次结果，
// 那是成功而不是错误。
func (l *MembershipEntitlementUpsertLogic) MembershipEntitlementUpsert(req *types.ParamMembershipEntitlementUpsert) (resp *types.MembershipEntitlementUpsertResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	operator, err := membershipOperator(l.ctx, "membershipEntitlementUpsert", req.Operator)
	if err != nil {
		return nil, err
	}
	if err := requireNonEmpty("code", req.Code); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("name", req.Name); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := membershipEnum("min_vip_type", req.MinVipType); err != nil {
		return nil, err
	}
	if err := membershipNonNeg("expected_version", req.ExpectedVersion); err != nil {
		return nil, err
	}
	entitlement, err := l.svcCtx.Membership.UpsertEntitlement(l.ctx, &membershiprpc.UpsertEntitlementReq{
		Code:            req.Code,
		Name:            req.Name,
		Description:     req.Description,
		MinVipType:      membershiprpc.VipType(req.MinVipType),
		Enabled:         req.Enabled,
		ExpectedVersion: req.ExpectedVersion,
		Operator:        operator,
		RequestId:       req.IdempotencyKey,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipEntitlementUpsert: code=%s min_vip_type=%d enabled=%t expected_version=%d operator=%s trace_id=%s err=%v",
			req.Code, req.MinVipType, req.Enabled, req.ExpectedVersion, operator, req.TraceId, err)
		return nil, err
	}
	// 能力闸的变化会影响所有端的判定，成功也要留一条「谁在什么时候关掉了哪个码」的网关证据。
	l.Infof("gateway/admin/membershipEntitlementUpsert: code=%s min_vip_type=%d enabled=%t version=%d operator=%s",
		req.Code, req.MinVipType, req.Enabled, entitlement.GetVersion(), operator)
	return &types.MembershipEntitlementUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipEntitlementUpsertData{
			Entitlement: membershipEntitlementToAPI(entitlement),
		},
		TTL: 0,
	}, nil
}
