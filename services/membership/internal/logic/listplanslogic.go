package logic

import (
	"context"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListPlansLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListPlansLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListPlansLogic {
	return &ListPlansLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListPlans 终端套餐列表。
//
// 口径（AGENTS.md §6：不写死某个端的 UI 行为，只提供可选性）：
//  1. 只返回 state=ON_SALE 的行，DRAFT/OFF_SALE 一律不泄露到终端面；
//     需要看草稿或已下架必须走 ListPlansAdmin。
//  2. on_sale_only=false 不放宽过滤：该字段留给未来「已购用户续期可见下架套餐」的需求，
//     本轮终端面恒定只出在售，避免运营误勾一个参数就把草稿挂上收银台。
//  3. platform 非 UNSPECIFIED 时按位与过滤（该端不可见的套餐不返回）；
//     vip_type 非 UNSPECIFIED 时按档位过滤。
func (l *ListPlansLogic) ListPlans(in *rpc.ListPlansReq) (*rpc.ListPlansReply, error) {
	var platformBit uint32
	if in.Platform != rpc.PlanPlatform_PLAN_PLATFORM_UNSPECIFIED {
		bit, err := model.PlatformMaskOf(int32(in.Platform))
		if err != nil {
			return nil, err
		}
		platformBit = bit
	}

	var vipType int32
	if in.VipType != rpc.VipType_VIP_TYPE_UNSPECIFIED {
		if err := requireVipType(in.VipType); err != nil {
			return nil, err
		}
		vipType = int32(in.VipType)
	}

	rows, err := l.svcCtx.Plan.ListOnSale(l.ctx, platformBit, vipType)
	if err != nil {
		l.Errorf("membership/ListPlans: read failed platform=%d vip_type=%d err=%v", int32(in.Platform), vipType, err)
		return nil, err
	}
	return &rpc.ListPlansReply{Plans: planListToRPC(rows)}, nil
}
