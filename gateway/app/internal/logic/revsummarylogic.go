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

type RevSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的收益概览（应计金额，非已到账）
func NewRevSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevSummaryLogic {
	return &RevSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevSummary 读本人收益概览：mid 必填且为正，GetRevenueSummaryReq 只有 mid 一个位，
// 跨用户汇总属于运营面，不在终端路由出现。
// 契约把「未参加」表达成 enrollment.state 而不是 found 位（reply 里没有 found），
// 所以未参加不是错误、也不需要在网关造结论：投影成 enrollment_state=0 + 全 0 金额即可。
// 预估金额会随计量更正变动，且本项目无出金通道（payout_available 恒 false），TTL 0。
func (l *RevSummaryLogic) RevSummary(req *types.ParamRevSummary) (resp *types.RevSummaryResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GetRevenueSummary(l.ctx, &creatorrevenuerpc.GetRevenueSummaryReq{
		Mid: req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/revSummary: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.RevSummaryResponse{
		Code:    0,
		Message: "ok",
		Data:    revSummaryToAPI(reply),
		TTL:     0,
	}, nil
}
