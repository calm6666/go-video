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

type DelBlackLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消拉黑（幂等）
func NewDelBlackLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelBlackLogic {
	return &DelBlackLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DelBlack 取消拉黑：聚合 social-graph DelBlack RPC。
// 幂等（重复取消不报错、不减计数）由 social-graph 保证；取消拉黑不会自动恢复原关注关系。
func (l *DelBlackLogic) DelBlack(req *types.ParamAddBlack) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	if _, err = l.svcCtx.SocialGraph.DelBlack(l.ctx, &socialgraphrpc.BlackReq{
		Mid:      req.Mid,
		BlackMid: req.BlackMid,
		RealIp:   req.IP,
	}); err != nil {
		l.Errorf("gateway/app/delBlack: mid=%d black_mid=%d err=%v", req.Mid, req.BlackMid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
