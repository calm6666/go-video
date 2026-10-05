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

type DelFolderLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除收藏夹（软删）
func NewDelFolderLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DelFolderLogic {
	return &DelFolderLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DelFolder 删除收藏夹（软删）：聚合 engagement DelFolder RPC。
// 归属校验（只能删自己的夹）与夹内收藏记录的清理由 engagement 负责，网关不判定 fid 归属。
func (l *DelFolderLogic) DelFolder(req *types.ParamDelFolder) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Engagement == nil {
		return nil, errors.New("engagement service not configured")
	}
	if _, err = l.svcCtx.Engagement.DelFolder(l.ctx, &engagementrpc.DelFolderReq{
		Tp:  req.Tp,
		Mid: req.Mid,
		Fid: req.Fid,
	}); err != nil {
		l.Errorf("gateway/app/delFolder: tp=%d mid=%d fid=%d err=%v", req.Tp, req.Mid, req.Fid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
