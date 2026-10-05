// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type WalletRechargesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的充值单列表
func NewWalletRechargesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletRechargesLogic {
	return &WalletRechargesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletRecharges 只读本人的充值台账：mid 必填且为正，不暴露 ListRechargesReq 的
// mid=0 跨用户语义（那是运营面）。state 枚举位原样透传（0 不过滤），网关不做取值白名单；
// page/page_size 原样透传，由服务取默认并裁剪。台账类读结论 TTL 0。
func (l *WalletRechargesLogic) WalletRecharges(req *types.ParamWalletRecharges) (resp *types.WalletRechargesResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.ListRecharges(l.ctx, &paymentrpc.ListRechargesReq{
		Mid:    req.Mid,
		State:  paymentrpc.RechargeState(req.State),
		FromTs: req.FromTs,
		ToTs:   req.ToTs,
		Page:   int64(req.Page),
		Size:   int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/walletRecharges: mid=%d state=%d page=%d err=%v", req.Mid, req.State, req.Page, err)
		return nil, err
	}
	return &types.WalletRechargesResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletRechargesData{
			Recharges: walletRechargesToAPI(reply.GetRecharges()),
			Total:     reply.GetTotal(),
			Page:      reply.GetPage(),
			PageSize:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
