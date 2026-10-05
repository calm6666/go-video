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

type DelSpecialLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消特别关注（幂等）
func NewDelSpecialLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelSpecialLogic {
	return &DelSpecialLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DelSpecial 取消特别关注：聚合 social-graph DelSpecial RPC。
// 幂等由 social-graph 保证；取消特别关注不影响底层关注关系，网关不改写 attr。
func (l *DelSpecialLogic) DelSpecial(req *types.ParamAddSpecial) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.DelSpecial(l.ctx, &socialgraphrpc.SpecialReq{
		Mid:        req.Mid,
		SpecialMid: req.SpecialMid,
		RealIp:     req.IP,
	}); err != nil {
		l.Errorf("gateway/app/delSpecial: mid=%d special_mid=%d err=%v", req.Mid, req.SpecialMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
