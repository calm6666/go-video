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

type ListBlacksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 本人黑名单列表
func NewListBlacksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListBlacksLogic {
	return &ListBlacksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListBlacks 本人黑名单列表：聚合 social-graph ListBlacks RPC。
// 仅支持查自己的黑名单（mid 即归属者），分页上限（ps 最大 50）由 social-graph 校验。
func (l *ListBlacksLogic) ListBlacks(req *types.ParamListBlacks) (resp *types.SocialBlacksResponse, err error) {
	if l.svcCtx.SocialGraph == nil {
		return nil, errors.New("social-graph service not configured")
	}
	reply, err := l.svcCtx.SocialGraph.ListBlacks(l.ctx, &socialgraphrpc.ListReq{
		Mid:    req.Mid,
		Pn:     req.Pn,
		Ps:     req.Ps,
		RealIp: req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/listBlacks: mid=%d pn=%d ps=%d err=%v", req.Mid, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.SocialBlacksResponse{
		Code:    0,
		Message: "ok",
		Data: types.SocialBlacksData{
			Total: reply.GetTotal(),
			Items: toSocialRelationItems(reply.GetItems()),
		},
		TTL: 0,
	}, nil
}
