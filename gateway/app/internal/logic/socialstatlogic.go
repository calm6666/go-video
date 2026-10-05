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

type SocialStatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询关注数与粉丝数
func NewSocialStatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SocialStatLogic {
	return &SocialStatLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询关注数与粉丝数：聚合 social-graph Stat RPC（关系计数快照）。
func (l *SocialStatLogic) SocialStat(req *types.ParamSocialStat) (resp *types.SocialStatResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.Stat(l.ctx, &socialgraphrpc.MidReq{
		Mid:    req.Mid,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/socialStat: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.SocialStatResponse{
		Code:    0,
		Message: "ok",
		Data:    types.SocialStatData{Stat: toSocialStat(reply)},
		TTL:     0,
	}, nil
}
