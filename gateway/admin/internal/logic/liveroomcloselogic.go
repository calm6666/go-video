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

type LiveRoomCloseLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营下架/关闭房间（admin=true，强制终止进行中场次并留审计）
func NewLiveRoomCloseLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveRoomCloseLogic {
	return &LiveRoomCloseLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveRoomClose 聚合 live-room CloseRoom。
// admin 固定 true：本路由能被调到说明 AdminPermission 已放行，网关据此让 live-room 走
// SourceRPCAdmin 分支并跳过「必须生效房主」判定；把 admin 位开放给请求体等于让表单自己声明
// 「我是运营」，那才是真正危险的口子。
// 终态房间由 live-room 按幂等重放回 replayed=true，网关不把它改写成错误；
// 状态机是否允许关闭、场次如何终止全部在服务侧（AGENTS.md §8）。
func (l *LiveRoomCloseLogic) LiveRoomClose(req *types.ParamLiveRoomClose) (resp *types.LiveRoomCloseResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveRoomClose", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.CloseRoom(l.ctx, &liveroomrpc.CloseRoomReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		Admin:       true, // 固定：运营侧关闭，不接受客户端声明
		Reason:      req.Reason,
		RequestId:   req.RequestId,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveRoomClose: room_id=%d operator_mid=%d request_id=%s err=%v",
			req.RoomId, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomCloseResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomCloseData{
			State:               int32(reply.GetState()),
			TerminatedSessionId: reply.GetTerminatedSessionId(),
			Replayed:            reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
