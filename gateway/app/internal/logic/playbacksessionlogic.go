// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	playbackrpc "go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PlaybackSessionLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询本人播放会话与续播进度
func NewPlaybackSessionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PlaybackSessionLogic {
	return &PlaybackSessionLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PlaybackSession 查询会话详情。请求方 mid 必须与会话归属一致，
// 否则等于是用 session_id 猜测他人的观看记录（网关侧越权保护）。
func (l *PlaybackSessionLogic) PlaybackSession(req *types.ParamPlaybackSession) (resp *types.PlaybackSessionResponse, err error) {
	if l.svcCtx.Playback == nil {
		return nil, errors.New("playback service not configured")
	}
	reply, err := l.svcCtx.Playback.GetSession(l.ctx, &playbackrpc.GetSessionReq{SessionId: req.SessionId})
	if err != nil {
		l.Errorf("gateway/app/playbackSession: session=%s err=%v", req.SessionId, err)
		return nil, err
	}
	sess := reply.GetSession()
	if sess == nil {
		return &types.PlaybackSessionResponse{Code: 0, Message: "ok", TTL: 0}, nil
	}
	if req.Mid != sess.GetMid() {
		return nil, errors.New("gateway/app: session does not belong to this user")
	}
	return &types.PlaybackSessionResponse{
		Code:    0,
		Message: "ok",
		Data: types.PlaybackSessionData{
			Session:        playbackSessionToAPI(sess),
			Progress:       playbackProgressToAPI(reply.GetProgress()),
			LatestProgress: playbackProgressToAPI(reply.GetLatestProgress()),
			Found:          true,
		},
		TTL: 0,
	}, nil
}
