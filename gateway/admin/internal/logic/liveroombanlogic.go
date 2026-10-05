// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveRoomBanLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 禁播：进入 BANNED 并终止场次（临时禁播需 duration_seconds）
func NewLiveRoomBanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomBanLogic {
	return &LiveRoomBanLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomBan 聚合 live-room BanRoom。
// 禁播是运营处置：operator_mid、request_id 与权限点（live:ban/create）三道门槛都在出网关前
// 过一遍；ban_type 与 duration_seconds 的组合（临时必填时长、永久必须 end_at=0）以及
// reason 是否必填属 live-room 判定（banroomlogic.go 的 banEndAt/checkReason），
// 网关不做秒数换算也不补默认时长——换算口径一旦两边各写一半，
// 「禁播 1 小时」和「禁播 3600 秒」就可能不是同一件事。
func (l *LiveRoomBanLogic) LiveRoomBan(req *types.ParamLiveRoomBan) (resp *types.LiveRoomBanResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveRoomBan", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ban_type", req.BanType); err != nil {
		return nil, err
	}
	if err := liveNonNeg("duration_seconds", req.DurationSeconds); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.BanRoom(l.ctx, &liveroomrpc.BanRoomReq{
		RoomId:          req.RoomId,
		BanType:         liveroomrpc.BanType(req.BanType),
		DurationSeconds: req.DurationSeconds,
		Reason:          req.Reason,
		OperatorMid:     req.OperatorMid,
		RequestId:       req.RequestId,
		TraceId:         req.TraceId,
	})
	if err != nil {
		// reason 正文不进日志，只留主键与主体（AGENTS.md §4）。
		l.Errorf("gateway/admin/liveRoomBan: room_id=%d ban_type=%d duration_seconds=%d operator_mid=%d request_id=%s err=%v",
			req.RoomId, req.BanType, req.DurationSeconds, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomBanResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomBanData{
			BanId:               reply.GetBanId(),
			State:               int32(reply.GetState()),
			TerminatedSessionId: reply.GetTerminatedSessionId(),
			EndAt:               reply.GetEndAt(),
			Replayed:            reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
