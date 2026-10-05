// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"
	"strings"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CloseLiveRoomLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：关闭房间（终态 FINISHED，强制终止进行中场次并保留审计）
func NewCloseLiveRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CloseLiveRoomLogic {
	return &CloseLiveRoomLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CloseRoom 终端入口固定 admin=false：运营关闭房间走 gateway/admin，
// 网关不把客户端可伪造的「我是运营」透传成提权标记（对齐 deleteDanmaku 的 admin=false 口径）。
// operator_mid 是否为房主或生效联合主播由 live-room 判定；房间进入 FINISHED 终态后不复用，
// 历史与审计证据保留（AGENTS.md §8）。
func (l *CloseLiveRoomLogic) CloseLiveRoom(req *types.ParamLiveClose) (resp *types.LiveCloseResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：关闭房间是终态迁移，重试不能二次终止场次")
	}
	reply, err := l.svcCtx.LiveRoom.CloseRoom(l.ctx, &liveroomrpc.CloseRoomReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		Admin:       false,
		Reason:      req.Reason,
		RequestId:   req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/closeLiveRoom: room_id=%d operator_mid=%d err=%v", req.RoomId, req.OperatorMid, err)
		return nil, err
	}
	return &types.LiveCloseResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveCloseData{
			State:               int32(reply.GetState()),
			TerminatedSessionId: reply.GetTerminatedSessionId(),
			Replayed:            reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
