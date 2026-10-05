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

type ListDanmakuUserBlocksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 本人弹幕屏蔽列表
func NewListDanmakuUserBlocksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDanmakuUserBlocksLogic {
	return &ListDanmakuUserBlocksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListDanmakuUserBlocksLogic) ListDanmakuUserBlocks(req *types.ParamDanmakuUserBlocks) (resp *types.DanmakuUserBlocksResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	reply, err := l.svcCtx.Danmaku.ListUserBlocks(l.ctx, &danmakurpc.ListUserBlocksReq{
		Mid:  req.Mid,
		Type: danmakurpc.UserBlockType(req.Type),
		Pn:   req.Pn,
		Ps:   req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/app/listDanmakuUserBlocks: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.DanmakuUserBlocksResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuUserBlocksData{
			Blocks: danmakuUserBlocksToAPI(reply.GetBlocks()),
			Total:  reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
