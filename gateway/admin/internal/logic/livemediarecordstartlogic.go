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

type LiveMediaRecordStartLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 登记录制任务（PENDING，request_id 幂等）
func NewLiveMediaRecordStartLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRecordStartLogic {
	return &LiveMediaRecordStartLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRecordStart 聚合 live-media StartLiveRecord。
//
// 只登记意图：服务侧落 PENDING，真正拉起录制的是 Worker，网关不在此假定录制已开始。
// start_at/end_at 是**期望**区间（0 分别是「立即」「随场次结束」），倒置的窗口在下游只会变成
// 一行永远不满足的记录，因此网关先拒；segment_seconds/timeout_seconds 传 0 用服务默认，
// 分片时长的合法取值与切片上限由 live-media 判定，这里不复算。
// output_bucket/output_prefix 必须成对（只给一半等于让切片写到一个不完整的路径上）。
// source_task_id=0 是「录原画源」的合法哨兵，不是缺参。
// 本方法没有 operator 位：谁登记的只能从网关日志回溯（缺口见 admin.api 与 README）。
func (l *LiveMediaRecordStartLogic) LiveMediaRecordStart(req *types.ParamLiveMediaRecordStart) (resp *types.LiveMediaRecordStartResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaRecordStart"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveMediaRefPair("output_bucket", "output_prefix", req.OutputBucket, req.OutputPrefix); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"live_session_id", req.SessionId},
		{"source_task_id", req.SourceTaskId},
		{"start_at", req.StartAt},
		{"end_at", req.EndAt},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := liveMediaWindow("start_at", "end_at", req.StartAt, req.EndAt); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("segment_seconds", req.SegmentSeconds); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("timeout_seconds", req.TimeoutSeconds); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.StartLiveRecord(l.ctx, &livemediarpc.StartLiveRecordReq{
		RoomId:         req.RoomId,
		LiveSessionId:  req.SessionId,
		SourceTaskId:   req.SourceTaskId,
		StartAt:        req.StartAt,
		EndAt:          req.EndAt,
		SegmentSeconds: req.SegmentSeconds,
		TimeoutSeconds: req.TimeoutSeconds,
		OutputBucket:   req.OutputBucket,
		OutputPrefix:   req.OutputPrefix,
		RequestId:      req.RequestId,
		TraceId:        req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRecordStart: room_id=%d live_session_id=%d source_task_id=%d request_id=%s err=%v",
			req.RoomId, req.SessionId, req.SourceTaskId, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaRecordStartResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaRecordTaskToAPI(info),
		TTL:     0,
	}, nil
}
