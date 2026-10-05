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

type UserFoldersLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户收藏夹列表
func NewUserFoldersLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UserFoldersLogic {
	return &UserFoldersLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 用户收藏夹列表：聚合 engagement UserFolders RPC。
// vmid 用于他人主页场景，可见性判断由 engagement 负责。
func (l *UserFoldersLogic) UserFolders(req *types.ParamUserFolders) (resp *types.EngagementFoldersResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	reply, err := l.svcCtx.Engagement.UserFolders(l.ctx, &engagementrpc.UserFoldersReq{
		Tp:       req.Tp,
		Mid:      req.Mid,
		Vmid:     req.Vmid,
		Oid:      req.Oid,
		AllCount: req.AllCount,
		Otype:    req.Otype,
	})
	if err != nil {
		l.Errorf("gateway/app/folders: tp=%d mid=%d vmid=%d oid=%d err=%v",
			req.Tp, req.Mid, req.Vmid, req.Oid, err)
		return nil, err
	}
	return &types.EngagementFoldersResponse{
		Code:    0,
		Message: "ok",
		Data:    types.EngagementFoldersData{Folders: toEngagementFolders(reply.GetFolders())},
		TTL:     0,
	}, nil
}
