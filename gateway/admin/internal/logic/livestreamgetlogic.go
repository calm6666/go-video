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

type LiveStreamGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单流状态：按 stream_id 或房间的当前非终态流
func NewLiveStreamGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveStreamGetLogic {
	return &LiveStreamGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveStreamGet 聚合 live-ingest GetStreamState。
//
// 只读面，不挂 AdminPermission（与 live-room 的 get 同一口径）。GetStreamStateReq 没有
// operator/admin 位——按 proto 它是纯状态查询，主体约束由调用方的房间/流标识限定，
// 因此这里没有可声明的读取主体，门槛落在「stream_id 与 room_id 至少给一个」。
//
// found 显式回传「服务有没有给出这一段」：Stream 是指针消息，全零值既可能是「没有这条流」
// 也可能被误读成「有一条 stream_id 为空的状态 1 的流」。
func (l *LiveStreamGetLogic) LiveStreamGet(req *types.ParamLiveStreamGet) (resp *types.LiveStreamStateResponse, err error) {
	if l.svcCtx.LiveIngest == nil {
		return nil, errLiveIngestNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveStreamSubjectGate(req.StreamId, req.RoomId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveIngest.GetStreamState(l.ctx, &liveingestrpc.GetStreamStateReq{
		StreamId: req.StreamId,
		RoomId:   req.RoomId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveStreamGet: stream_id=%s room_id=%d err=%v", req.StreamId, req.RoomId, err)
		return nil, err
	}
	return &types.LiveStreamStateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveStreamStateData{
			Stream: liveStreamToAPI(reply.GetStream()),
			Found:  reply.GetFound(),
		},
		TTL: 0,
	}, nil
}
