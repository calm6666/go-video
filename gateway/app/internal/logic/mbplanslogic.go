// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MbPlansLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 会员套餐列表（终端可见档位；含下架档需 all=true，供续费页）
func NewMbPlansLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbPlansLogic {
	return &MbPlansLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbPlans 透传平台与档位过滤。all 翻译成 on_sale_only 的反义（续费页要能显示已下架旧档），
// DRAFT 档是否泄漏由 membership 判定，网关不做二次过滤。
// 价格是服务端的套餐原值，网关不换算单位、不算折扣（PromPriceMinor 原样透出）。
func (l *MbPlansLogic) MbPlans(req *types.ParamMbPlans) (resp *types.MbPlansResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	reply, err := l.svcCtx.Membership.ListPlans(l.ctx, &membershiprpc.ListPlansReq{
		Platform:   membershiprpc.PlanPlatform(req.Platform),
		VipType:    membershiprpc.VipType(req.VipType),
		OnSaleOnly: !req.All,
	})
	if err != nil {
		l.Errorf("gateway/app/mbPlans: platform=%d vip_type=%d all=%t err=%v", req.Platform, req.VipType, req.All, err)
		return nil, err
	}
	return &types.MbPlansResponse{
		Code:    0,
		Message: "ok",
		Data: types.MbPlansData{
			Plans: mbPlansToAPI(reply.GetPlans()),
		},
		TTL: 60,
	}, nil
}
