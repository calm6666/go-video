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

type RevSettlementsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的结算单列表
func NewRevSettlementsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevSettlementsLogic {
	return &RevSettlementsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevSettlements 只列本人结算单：mid 必填且为正（ListSettlementsReq 的 mid=0 全量语义
// 属运营/cron 出单口径，不在终端路由出现）。period 与 state 是可选过滤位，
// 0（UNSPECIFIED）=不过滤，网关不改写也不给「只看已确认」这类默认值。
// state=CONFIRMED 只代表「金额已冻结、不再重算」，payout_state 恒 NOT_PAYABLE——
// 两位都原样投影，网关绝不把前者渲染成「已到账」（本项目无出金通道）。
// 结算单会被重算作废，TTL 0。
func (l *RevSettlementsLogic) RevSettlements(req *types.ParamRevSettlements) (resp *types.RevSettlementsResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ListSettlements(l.ctx, &creatorrevenuerpc.ListSettlementsReq{
		Period: req.Period,
		Mid:    req.Mid,
		State:  creatorrevenuerpc.SettlementState(req.State),
		Page:   int64(req.Page),
		Size:   int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/revSettlements: mid=%d period=%s state=%d page=%d err=%v", req.Mid, req.Period, req.State, req.Page, err)
		return nil, err
	}
	return &types.RevSettlementsResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevSettlementsData{
			Settlements: revSettlementsToAPI(reply.GetSettlements()),
			Total:       reply.GetTotal(),
			Page:        reply.GetPage(),
			PageSize:    reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
