// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	socialgraphrpc "go-video/services/social-graph/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddBlackLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 拉黑用户（已关注时自动取关）
func NewAddBlackLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddBlackLogic {
	return &AddBlackLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AddBlack 拉黑：聚合 social-graph AddBlack RPC。
// 拉黑自动取关、关系可见性与计数回写全部由 social-graph 负责，网关只透传 real_ip 供审计。
func (l *AddBlackLogic) AddBlack(req *types.ParamAddBlack) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.AddBlack(l.ctx, &socialgraphrpc.BlackReq{
		Mid:      req.Mid,
		BlackMid: req.BlackMid,
		RealIp:   req.IP,
	}); err != nil {
		l.Errorf("gateway/app/addBlack: mid=%d black_mid=%d err=%v", req.Mid, req.BlackMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
