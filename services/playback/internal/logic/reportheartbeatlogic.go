package logic

import (
	"context"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/playback/internal/svc"
	"go-video/services/playback/model"
	"go-video/services/playback/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ReportHeartbeatLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportHeartbeatLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportHeartbeatLogic {
	return &ReportHeartbeatLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ReportHeartbeat 上报播放心跳：幂等更新断点进度，并在同一事务写入
// playback.heartbeat.v1 事件（Outbox），由发布器异步投递给 event-collector/spm。
//
// 幂等：playback_progress 以 session_id 为唯一键 upsert，位置只前进不回退，
// 因此客户端重复上报、乱序上报和 gRPC 重试都不会破坏断点；下游消费者按 event_id 去重。
//
// 迟到的心跳（会话已过期）仍然保留进度——断点属于用户资产，
// 但事件里会带 session_expired 标记，下游可据此过滤质量统计。
func (l *ReportHeartbeatLogic) ReportHeartbeat(in *rpc.ReportHeartbeatReq) (*rpc.ReportHeartbeatReply, error) {
	if in.GetSessionId() == "" {
		return nil, model.ErrMissingSessionID
	}
	if in.GetPositionMs() < 0 || in.GetDurationMs() < 0 || in.GetDurationMs() > model.MaxDurationMS {
		return nil, model.ErrInvalidHeartbeat
	}
	if in.GetDurationMs() > 0 && in.GetPositionMs() > in.GetDurationMs() {
		return nil, model.ErrInvalidHeartbeat
	}
	now := time.Now().Unix()

	s, err := l.svcCtx.Repository.FindSession(l.ctx, in.GetSessionId(), now)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrSessionNotFound
	}
	// 先读既有进度用于事件负载（max_position_ms 与库里保持一致），
	// 真正的落库判定在 repository 的事务里用 GREATEST 完成。
	prev, err := l.svcCtx.Repository.FindProgress(l.ctx, s.SessionId)
	if err != nil {
		return nil, err
	}
	maxPositionMs := in.GetPositionMs()
	if prev != nil && prev.PositionMs > maxPositionMs {
		maxPositionMs = prev.PositionMs
	}

	p := &model.PlaybackProgress{
		SessionId:   s.SessionId,
		ContentType: s.ContentType,
		ContentId:   s.ContentId,
		Vid:         s.Vid,
		Mid:         s.Mid,
		PositionMs:  in.GetPositionMs(),
		DurationMs:  in.GetDurationMs(),
		BufferCount: in.GetBufferCount(),
		AvgBitrate:  in.GetAvgBitrate(),
		LastError:   in.GetLastError(),
		Ctime:       now,
		Mtime:       now,
	}
	raw, err := buildHeartbeatPayload(s, in, maxPositionMs, now)
	if err != nil {
		return nil, err
	}
	env, err := eventenvelope.New(model.Producer, model.EventPlaybackHeartbeat,
		model.AggregateTypeSession, s.SessionId, model.EventSchemaVersion, raw, in.GetTraceId())
	if err != nil {
		return nil, err
	}
	saved, err := l.svcCtx.Repository.ReportHeartbeat(l.ctx, p, env)
	if err != nil {
		l.Errorf("playback/ReportHeartbeat session=%s err=%v", in.GetSessionId(), err)
		return nil, err
	}
	return &rpc.ReportHeartbeatReply{
		EventId:       env.EventID,
		AcceptedAt:    now,
		MaxPositionMs: saved.PositionMs,
	}, nil
}
