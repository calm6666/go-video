// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	engagementrpc "go-video/services/engagement/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddFolderLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建收藏夹
func NewAddFolderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddFolderLogic {
	return &AddFolderLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AddFolder 新建收藏夹：聚合 engagement AddFolder RPC。
// 收藏夹数量上限与名称校验（长度/敏感词）由 engagement 负责，新 fid 只来自服务端回包。
func (l *AddFolderLogic) AddFolder(req *types.ParamAddFolder) (resp *types.EngagementFolderOpResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.AddFolder(l.ctx, &engagementrpc.AddFolderReq{
		Tp:          req.Tp,
		Mid:         req.Mid,
		Name:        req.Name,
		Description: req.Description,
		Cover:       req.Cover,
		Public:      req.Public,
	})
	if err != nil {
		l.Errorf("gateway/app/addFolder: tp=%d mid=%d name=%q err=%v", req.Tp, req.Mid, req.Name, err)
		return nil, err
	}
	// 本路由只回 fid；shares 字段由 share 路由使用，这里保持零值。
	return &types.EngagementFolderOpResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementFolderOpData{Fid: reply.GetFid()},
		TTL:     0,
	}, nil
}
