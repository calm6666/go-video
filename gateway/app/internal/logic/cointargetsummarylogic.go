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

type CoinTargetSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单内容投币汇总（详情页）
func NewCoinTargetSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CoinTargetSummaryLogic {
	return &CoinTargetSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CoinTargetSummary 读单条内容收到的硬币汇总（按未取消的投币记录聚合）。
// aid<=0 在网关拒绝：下游会把它当不存在的稿件聚合成全 0，伪装成真实结论。
// 汇总里刻意不含 like_count——点赞归 engagement，网关各自取，不在此伪造。
// 计数随时在变（详情页拉一次算一次），TTL 0。
func (l *CoinTargetSummaryLogic) CoinTargetSummary(req *types.ParamCoinTarget) (resp *types.CoinTargetSummaryResponse, err error) {
	if l.svcCtx.Coin == nil {
		return nil, errors.New("coin service not configured")
	}
	if req.Aid <= 0 {
		return nil, errors.New("aid is required")
	}
	reply, err := l.svcCtx.Coin.GetTargetSummary(l.ctx, &coinrpc.GetTargetSummaryReq{
		Aid: req.Aid,
	})
	if err != nil {
		l.Errorf("gateway/app/coinTargetSummary: aid=%d err=%v", req.Aid, err)
		return nil, err
	}
	return &types.CoinTargetSummaryResponse{
		Code:    0,
		Message: "ok",
		Data:    coinTargetToAPI(reply.GetSummary()),
		TTL:     0,
	}, nil
}
