// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteDanmakuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除本人弹幕（管理员可删任意，软删保留审计）
func NewDeleteDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteDanmakuLogic {
	return &DeleteDanmakuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteDanmaku 终端入口固定 admin=false：管理员删除走 gateway/admin，
// 越权删除由 danmaku 服务的所有者校验拒绝（AGENTS.md §4/§5）。
func (l *DeleteDanmakuLogic) DeleteDanmaku(req *types.ParamDanmakuDelete) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	if _, err := l.svcCtx.Danmaku.DeleteDanmaku(l.ctx, &danmakurpc.DeleteDanmakuReq{
		Dmid:   req.Dmid,
		Mid:    req.Mid,
		Admin:  false,
		Reason: req.Reason,
	}); err != nil {
		l.Errorf("gateway/app/deleteDanmaku: dmid=%d mid=%d err=%v", req.Dmid, req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
