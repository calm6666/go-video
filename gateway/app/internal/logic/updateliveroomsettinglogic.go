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

type UpdateLiveRoomSettingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：更新直播配置（整段覆盖语义）
func NewUpdateLiveRoomSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLiveRoomSettingLogic {
	return &UpdateLiveRoomSettingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// UpdateLiveRoomSetting 整段覆盖：protobuf RoomSetting 没有逐字段 optional，
// 契约明确「字段为 0/false 视为显式关闭」，因此 .api 把这些字段设为必填，
// 端上必须先读后写全量提交。网关不做「只提交改动字段」的合并，避免误开录制或误关弹幕。
// 是否允许连麦/录制的合规判定（含回放归 live-media 产出）仍在服务侧。
func (l *UpdateLiveRoomSettingLogic) UpdateLiveRoomSetting(req *types.ParamLiveRoomSettingUpdate) (resp *types.LiveRoomSettingUpdateResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：配置写入需要幂等键防止重复覆盖")
	}
	reply, err := l.svcCtx.LiveRoom.UpdateRoomSetting(l.ctx, &liveroomrpc.UpdateRoomSettingReq{
		RoomId:      req.RoomId,
		OperatorMid: req.OperatorMid,
		Setting: &liveroomrpc.RoomSetting{
			RoomId:               req.RoomId,
			DanmakuEnabled:       req.DanmakuEnabled,
			ReplyEnabled:         req.ReplyEnabled,
			RecordEnabled:        req.RecordEnabled,
			LinkmicEnabled:       req.LinkmicEnabled,
			LiveType:             req.LiveType,
			MinClientVersionCode: req.MinClientVersionCode,
		},
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/updateLiveRoomSetting: room_id=%d operator_mid=%d err=%v", req.RoomId, req.OperatorMid, err)
		return nil, err
	}
	return &types.LiveRoomSettingUpdateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomSettingUpdateData{
			Setting:  liveSettingToAPI(reply.GetSetting()),
			Replayed: reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
