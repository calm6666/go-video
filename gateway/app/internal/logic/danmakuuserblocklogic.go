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

type DanmakuUserBlockLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 屏蔽/解除屏蔽某用户或某关键词的弹幕
func NewDanmakuUserBlockLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DanmakuUserBlockLogic {
	return &DanmakuUserBlockLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DanmakuUserBlock 只影响本人的下发结果（隐私级屏蔽），不影响他人可见弹幕；
// 全局屏蔽词与内容处置属运营能力，走 gateway/admin。
func (l *DanmakuUserBlockLogic) DanmakuUserBlock(req *types.ParamDanmakuUserBlock) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	if _, err := l.svcCtx.Danmaku.UserBlock(l.ctx, &danmakurpc.UserBlockReq{
		Mid:        req.Mid,
		Type:       danmakurpc.UserBlockType(req.Type),
		BlockedMid: req.BlockedMid,
		Keyword:    req.Keyword,
		Unblock:    req.Unblock,
	}); err != nil {
		l.Errorf("gateway/app/danmakuUserBlock: mid=%d type=%d err=%v", req.Mid, req.Type, err)
		return nil, err
	}
	return emptyResponse(), nil
}
