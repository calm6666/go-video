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

type UpdateLiveRoomInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：修改标题/封面/分区（终态房间不可改，改动后重新送审）
func NewUpdateLiveRoomInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLiveRoomInfoLogic {
	return &UpdateLiveRoomInfoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdateLiveRoomInfo 空串/0 表示不修改（proto 口径）。改动后是否要重新送审、
// 房间是否已进入终态、操作者是否房主或生效联合主播，全部由 live-room 判定并回传
// moderation_task_id；网关不自行判断「改标题算不算需要复审」。
func (l *UpdateLiveRoomInfoLogic) UpdateLiveRoomInfo(req *types.ParamLiveRoomInfoUpdate) (resp *types.LiveRoomInfoUpdateResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：缺少幂等键的重试会重复触发送审")
	}
	reply, err := l.svcCtx.LiveRoom.UpdateRoomInfo(l.ctx, &liveroomrpc.UpdateRoomInfoReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		Title:       req.Title,
		Cover:       req.Cover,
		AreaId:      req.AreaId,
		RequestId:   req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/updateLiveRoomInfo: room_id=%d operator_mid=%d err=%v", req.RoomId, req.OperatorMid, err)
		return nil, err
	}
	return &types.LiveRoomInfoUpdateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomInfoUpdateData{
			Room:             liveRoomToAPI(reply.GetRoom()),
			Replayed:         reply.GetReplayed(),
			ModerationTaskId: reply.GetModerationTaskId(),
		},
		TTL: 0,
	}, nil
}
