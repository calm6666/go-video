// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveingestrpc "go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveStreamInterruptionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 断流与重连记录（含每次中断的起止、重连尝试数与关联事件 ID）
func NewLiveStreamInterruptionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamInterruptionListLogic {
	return &LiveStreamInterruptionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamInterruptionList 聚合 live-ingest ListStreamInterruptions。
//
// 断流记录是 live-ingest 从 live.state.v1 事件序列里派生的证据（episode_no 递增、
// start/end_event_id 可回溯到具体事件），网关不重算时长、也不把 ended_at=0 补成「已恢复」。
//
// ListStreamInterruptionsReq 没有 operator_mid / admin 位——记录本身按 stream 或 room 圈定，
// 因此门槛落在「至少给一个主体」，没有可声明的读取主体。
// total 命中服务侧计数上限时为 -1，这是契约自带的哨兵，原样回传不夹成 0。
func (l *LiveStreamInterruptionListLogic) LiveStreamInterruptionList(req *types.ParamLiveStreamInterruptionList) (resp *types.LiveStreamInterruptionListResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveStreamSubjectGate(req.StreamId, req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("start_time", req.StartTime); err != nil {
		return nil, err
	}
	if err := liveNonNeg("end_time", req.EndTime); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("limit", req.Limit); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.ListStreamInterruptions(l.ctx, &liveingestrpc.ListStreamInterruptionsReq{
		StreamId:  req.StreamId,
		RoomId:    req.RoomId,
		OnlyOpen:  req.OnlyOpen,
		StartTime: req.StartTime,
		EndTime:   req.EndTime,
		Limit:     req.Limit,
		TraceId:   req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamInterruptionList: stream_id=%s room_id=%d only_open=%t err=%v",
			req.StreamId, req.RoomId, req.OnlyOpen, err)
		return nil, err
	}
	return &types.LiveStreamInterruptionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamInterruptionListData{
			List:  liveStreamInterruptionsToAPI(reply.GetInterruptions()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
