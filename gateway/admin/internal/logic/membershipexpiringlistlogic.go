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

type MembershipExpiringListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 到期区间扫描（只读：核对 cron 将终结谁，后台不代为置过期）
func NewMembershipExpiringListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MembershipExpiringListLogic {
	return &MembershipExpiringListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MembershipExpiringList 转发 membership ListExpiringMemberships（到期区间扫描）。
//
// 本路由只读：后台靠它核对 cron 下一批要终结谁。ExpireMembership 刻意不开放，
// 因为「只有 expire_at 确实早于服务端 now 才生效」这条判定归服务和 cron，
// 后台点它等于手工改会员状态、让台账里的 operator 说谎（§1、§8）。
//
// 窗口口径照服务：to_expire_at 必须为正（它没有「不设上界」的语义）、
// from_expire_at 的 0 由服务按 1 处理以覆盖历史行、limit 的 0 表示用批处理上限，
// 超限由服务裁剪而不是报错。next_expire_at_cursor=0 表示本区间已扫完，原样转达。
func (l *MembershipExpiringListLogic) MembershipExpiringList(req *types.ParamMembershipExpiringList) (resp *types.MembershipExpiringListResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errMembershipServiceNotConfigured
	}
	if req == nil {
		return nil, errMembershipRequestMissing
	}
	if err := membershipExpireWindow(req.FromExpireAt, req.ToExpireAt, req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.ListExpiringMemberships(l.ctx, &membershiprpc.ListExpiringMembershipsReq{
		FromExpireAt:  req.FromExpireAt,
		ToExpireAt:    req.ToExpireAt,
		AutoRenewOnly: req.AutoRenewOnly,
		Limit:         req.Limit,
	})
	if err != nil {
		l.Errorf("gateway/admin/membershipExpiringList: from_expire_at=%d to_expire_at=%d auto_renew_only=%t limit=%d err=%v",
			req.FromExpireAt, req.ToExpireAt, req.AutoRenewOnly, req.Limit, err)
		return nil, err
	}
	return &types.MembershipExpiringListResponse{
		Code:    0,
		Message: "ok",
		Data: types.MembershipExpiringListData{
			List:               membershipMembersToAPI(reply.GetMemberships()),
			NextExpireAtCursor: reply.GetNextExpireAtCursor(),
		},
		TTL: 0,
	}, nil
}
