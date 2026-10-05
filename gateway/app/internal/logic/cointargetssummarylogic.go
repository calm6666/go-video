// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	coinrpc "go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CoinTargetsSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 批量内容投币汇总（列表页）
func NewCoinTargetsSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTargetsSummaryLogic {
	return &CoinTargetsSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinTargetsSummary 列表页批量取硬币汇总。aids 必须非空且逐个为正：
// 空列表直接拒绝而不是去问下游（下游返回空 summaries 会被端上渲染成「全部为 0」）。
// 返回值只包含下游实际算出的行，网关不补零行、不保证与请求顺序一致（端上按 aid 索引）。
func (l *CoinTargetsSummaryLogic) CoinTargetsSummary(req *types.ParamCoinTargets) (resp *types.CoinTargetsSummaryResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if err := requireAids(req.Aids); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Coin.BatchGetTargetSummary(l.ctx, &coinrpc.BatchGetTargetSummaryReq{
		Aids: req.Aids,
	})
	if err != nil {
		l.Errorf("gateway/app/coinTargetsSummary: aids=%d err=%v", len(req.Aids), err)
		return nil, err
	}
	return &types.CoinTargetsSummaryResponse{
		Code:    0,
		Message: "ok",
		Data: types.CoinTargetsSummaryData{
			Summaries: coinTargetsToAPI(reply.GetSummaries()),
		},
		TTL: 0,
	}, nil
}
