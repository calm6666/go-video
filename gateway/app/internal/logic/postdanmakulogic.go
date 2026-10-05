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

type PostDanmakuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发送弹幕（幂等，落库后待审核）
func NewPostDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PostDanmakuLogic {
	return &PostDanmakuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PostDanmaku 只负责参数转发与幂等键透传：屏蔽词命中、限流与机审提交全在 danmaku 服务，
// 审核结论回来之前弹幕只对发送者可见（AGENTS.md §8）。
func (l *PostDanmakuLogic) PostDanmaku(req *types.ParamDanmakuPost) (resp *types.DanmakuPostResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	idem := req.IdempotencyKey
	if idem == "" {
		idem = req.ClientMsgId
	}
	if idem == "" {
		return nil, errors.New("gateway/app: idempotency_key or client_msg_id is required")
	}
	reply, err := l.svcCtx.Danmaku.PostDanmaku(l.ctx, &danmakurpc.PostDanmakuReq{
		Oid:            req.Oid,
		Aid:            req.Aid,
		Mid:            req.Mid,
		ProgressMs:     req.ProgressMs,
		Mode:           danmakurpc.DanmakuMode(req.Mode),
		Fontsize:       req.Fontsize,
		Color:          req.Color,
		Content:        req.Content,
		IdempotencyKey: idem,
		ClientMsgId:    req.ClientMsgId,
	})
	if err != nil {
		l.Errorf("gateway/app/postDanmaku: oid=%d mid=%d err=%v", req.Oid, req.Mid, err)
		return nil, err
	}
	return &types.DanmakuPostResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuPostData{
			Dmid:             reply.GetDmid(),
			State:            reply.GetState(),
			Pool:             reply.GetPool(),
			SegNo:            reply.GetSegNo(),
			Ctime:            reply.GetCtime(),
			Replayed:         reply.GetReplayed(),
			ModerationTaskId: reply.GetModerationTaskId(),
		},
		TTL: 0,
	}, nil
}
