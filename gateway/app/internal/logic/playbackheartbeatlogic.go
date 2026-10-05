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

type PlaybackHeartbeatLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 上报播放心跳（进度单调不回退，事件走 Outbox）
func NewPlaybackHeartbeatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PlaybackHeartbeatLogic {
	return &PlaybackHeartbeatLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PlaybackHeartbeat 转发到 playback：进度取最大值、事件与 outbox 同事务，
// 播放量/完播率由下游 SPM 链路消费事件计算，网关不做计数（AGENTS.md §7）。
func (l *PlaybackHeartbeatLogic) PlaybackHeartbeat(req *types.ParamPlaybackHeartbeat) (resp *types.PlaybackHeartbeatResponse, err error) {
	if l.svcCtx.Playback == nil {
		return nil, errors.New("playback service not configured")
	}
	reply, err := l.svcCtx.Playback.ReportHeartbeat(l.ctx, &playbackrpc.ReportHeartbeatReq{
		SessionId:   req.SessionId,
		PositionMs:  req.PositionMs,
		DurationMs:  req.DurationMs,
		BufferCount: req.BufferCount,
		AvgBitrate:  req.AvgBitrate,
		LastError:   req.LastError,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/playbackHeartbeat: session=%s err=%v", req.SessionId, err)
		return nil, err
	}
	return &types.PlaybackHeartbeatResponse{
		Code:    0,
		Message: "ok",
		Data: types.PlaybackHeartbeatData{
			EventId:       reply.GetEventId(),
			AcceptedAt:    reply.GetAcceptedAt(),
			MaxPositionMs: reply.GetMaxPositionMs(),
		},
		TTL: 0,
	}, nil
}
