// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevSettlementLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 结算单详情（含分项，便于核对）
func NewRevSettlementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevSettlementLogic {
	return &RevSettlementLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevSettlement 读单张结算单及其分项。GetSettlementReq 的 mid 标注「非 0 时校验归属」，
// 本路由的 mid 必填且为正，因此归属校验恒生效：终端拿不到别人的结算单，
// 越权与不存在都按 found=false 回（服务侧刻意不用 FORBIDDEN，避免单号可枚举）。
// found=false 投影成 Code:0 + data.found=false + 零值单 + 空分项列表，不是 HTTP 错误。
// items 是「按来源拆开的应计构成」，金额全部取服务侧计算值，网关不核对加总是否等于 amount_minor
// （核对是 creator-revenue 的职责，网关做了也不会让它更对）。TTL 0：结算单可被作废重算。
func (l *RevSettlementLogic) RevSettlement(req *types.ParamRevSettlement) (resp *types.RevSettlementDetailResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("settlement_no", req.SettlementNo); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GetSettlement(l.ctx, &creatorrevenuerpc.GetSettlementReq{
		SettlementNo: req.SettlementNo,
		Mid:          req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/revSettlement: mid=%d settlement_no=%s err=%v", req.Mid, req.SettlementNo, err)
		return nil, err
	}
	return &types.RevSettlementDetailResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevSettlementDetailData{
			Found:      reply.GetFound(),
			Settlement: revSettlementToAPI(reply.GetSettlement()),
			Items:      revSettlementItemsToAPI(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
