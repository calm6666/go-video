package logic

import (
	"context"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetMembershipLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetMembershipLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetMembershipLogic {
	return &GetMembershipLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetMembership 我的会员状态。
//
// 口径：
//  1. vip_type=UNSPECIFIED 返回「当前生效的最高档」；全都过期时退回「曾达到的最高档」，
//     让详情页仍能显示到期时间；指定档位则只看那一档的行。
//  2. found=false 只在「该用户一行会员记录都没有」时给出；有过但已过期是 found=true，
//     是否过期由 server_now 与 expire_at 比较决定（调用方不必自带时钟）。
//  3. granted_entitlements 是只读投影：仅当判定行仍生效时，返回 min_vip_type 达标
//     且 enabled 的权益码；过期身份一个都不给（不得用「曾有过」换今日的能力）。
//  4. DB 读失败一律上抛错误，不能退化成 found=false 的「未开通」。
func (l *GetMembershipLogic) GetMembership(in *rpc.GetMembershipReq) (*rpc.GetMembershipReply, error) {
	if err := requireMid(in.Mid); err != nil {
		return nil, err
	}
	if err := requireVipTypeOrUnspecified(in.VipType); err != nil {
		return nil, err
	}

	now := model.NowUnix()
	rows, err := l.svcCtx.Membership.ListByMid(l.ctx, in.Mid)
	if err != nil {
		l.Errorf("membership/GetMembership: read mid=%d err=%v", in.Mid, err)
		return nil, err
	}

	pick := model.PickMembershipFor(rows, int32(in.VipType), now)
	reply := &rpc.GetMembershipReply{
		Found:     pick != nil,
		ServerNow: now,
	}
	if pick == nil {
		return reply, nil
	}
	reply.Membership = membershipToRPC(pick)

	if pick.IsActive(now) {
		ents, err := l.svcCtx.Entitlement.List(l.ctx, true)
		if err != nil {
			// 权益投影读失败不影响「我的会员」主结论：降级为空列表并记日志，
			// 单项能力判定仍由 CheckEntitlement 负责，绝不因为读不到就返回错误页。
			l.Errorf("membership/GetMembership: entitlement projection failed mid=%d err=%v", in.Mid, err)
		} else {
			granted := make([]*rpc.EntitlementInfo, 0, len(ents))
			for _, e := range ents {
				if e != nil && model.TierSufficient(pick.VipType, e.MinVipType) {
					granted = append(granted, entitlementToRPC(e))
				}
			}
			reply.GrantedEntitlements = granted
		}
	}
	return reply, nil
}
