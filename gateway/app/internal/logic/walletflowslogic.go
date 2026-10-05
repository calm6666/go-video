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

type WalletFlowsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的资金流水
func NewWalletFlowsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *WalletFlowsLogic {
	return &WalletFlowsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// WalletFlows 只读本人的资金流水（正入负出）。mid 必填且为正，
// 不开放 ListFlowsReq 的跨用户语义；biz_type 枚举位原样透传（0 不过滤）。
// delta_minor / balance_after_minor 直接取台账值，网关不做求和、不做分→元换算。
// 流水会持续追加，TTL 0。
func (l *WalletFlowsLogic) WalletFlows(req *types.ParamWalletFlows) (resp *types.WalletFlowsResponse, err error) {
	if l.svcCtx.Payment == nil {
		return nil, errors.New("payment service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Payment.ListFlows(l.ctx, &paymentrpc.ListFlowsReq{
		Mid:     req.Mid,
		BizType: paymentrpc.FlowBizType(req.BizType),
		BizNo:   req.BizNo,
		FromTs:  req.FromTs,
		ToTs:    req.ToTs,
		Page:    int64(req.Page),
		Size:    int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/walletFlows: mid=%d biz_type=%d biz_no=%s page=%d err=%v", req.Mid, req.BizType, req.BizNo, req.Page, err)
		return nil, err
	}
	return &types.WalletFlowsResponse{
		Code:    0,
		Message: "ok",
		Data: types.WalletFlowsData{
			Flows:    walletFlowsToAPI(reply.GetFlows()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
