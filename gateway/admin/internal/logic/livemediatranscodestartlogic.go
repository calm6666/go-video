// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaTranscodeStartLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记直播转码任务（PENDING，request_id 幂等；不在此拉起 FFmpeg）
func NewLiveMediaTranscodeStartLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeStartLogic {
	return &LiveMediaTranscodeStartLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeStart 聚合 live-media StartLiveTranscode。
//
// 门槛只到形态为止：room_id/template_id 是必填主键，bitrate_level/protocol 的 0 是
// UNSPECIFIED（不知道要开哪一路就不该登记），source_ref 空则 Worker 无从拉流。
// max_attempts/timeout_seconds 传 0 是「用服务默认」的合法哨兵，网关不补默认值也不设上界；
// 模板是否存在、该房间能不能同时开两路同一档位由 live-media 与 transcode 判定。
//
// 本方法在 proto 里**没有 operator 位**：会话身份仍然必须存在（否则不出网关），
// 但「谁登记的」只能从网关日志回溯，缺口逐条记在 admin.api 头部与 README。
// request_id 是幂等键，同键重放返回同一任务，因此运营连点不会多开一路转码。
func (l *LiveMediaTranscodeStartLogic) LiveMediaTranscodeStart(req *types.ParamLiveMediaTranscodeStart) (resp *types.LiveMediaTranscodeStartResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaTranscodeStart"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("template_id", req.TemplateId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("live_session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("anchor_mid", req.AnchorMid); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("bitrate_level", req.BitrateLevel); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("protocol", req.Protocol); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("max_attempts", req.MaxAttempts); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("timeout_seconds", req.TimeoutSeconds); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("source_ref", req.SourceRef); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.StartLiveTranscode(l.ctx, &livemediarpc.StartLiveTranscodeReq{
		RoomId:         req.RoomId,
		LiveSessionId:  req.SessionId,
		TemplateId:     req.TemplateId,
		BitrateLevel:   livemediarpc.BitrateLevel(req.BitrateLevel),
		Protocol:       livemediarpc.StreamProtocol(req.Protocol),
		SourceRef:      req.SourceRef,
		AnchorMid:      req.AnchorMid,
		MaxAttempts:    req.MaxAttempts,
		TimeoutSeconds: req.TimeoutSeconds,
		RequestId:      req.RequestId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeStart: room_id=%d template_id=%d bitrate_level=%d protocol=%d request_id=%s err=%v",
			req.RoomId, req.TemplateId, req.BitrateLevel, req.Protocol, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeStartResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaTranscodeTaskToAPI(info),
		TTL:     0,
	}, nil
}
