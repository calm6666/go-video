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

type CreateLiveRoomLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 主播面：创建直播间（request_id 幂等，资料送审由服务发起）
func NewCreateLiveRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLiveRoomLogic {
	return &CreateLiveRoomLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CreateLiveRoom 主播资格（creator UpAttr）与房间数上限由 live-room 判定，
// 资料送审也由该服务发起，网关不预判「这个人能不能建房间」。
// request_id 缺失会让重试产生第二个房间，因此按幂等键必填在此拒绝。
func (l *CreateLiveRoomLogic) CreateLiveRoom(req *types.ParamLiveRoomCreate) (resp *types.LiveRoomCreateResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	if strings.TrimSpace(req.RequestId) == "" {
		return nil, errors.New("request_id 必填：缺少幂等键的重试会创建出第二个房间")
	}
	reply, err := l.svcCtx.LiveRoom.CreateRoom(l.ctx, &liveroomrpc.CreateRoomReq{
		Mid:        req.Mid,
		Title:      req.Title,
		Cover:      req.Cover,
		AreaId:     req.AreaId,
		Platform:   liveroomrpc.Platform(req.Platform),
		AppVersion: req.AppVersion,
		// 未填配置时传 nil，由服务按默认配置落库，网关不代替服务定义开播默认值。
		Setting:   liveCreateSetting(req),
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/createLiveRoom: mid=%d area_id=%d request_id=%q err=%v",
			req.Mid, req.AreaId, req.RequestId, err)
		return nil, err
	}
	return &types.LiveRoomCreateResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveRoomCreateData{
			RoomId:           reply.GetRoomId(),
			State:            int32(reply.GetState()),
			VerifyState:      int32(reply.GetVerifyState()),
			Replayed:         reply.GetReplayed(),
			ModerationTaskId: reply.GetModerationTaskId(),
		},
		TTL: 0,
	}, nil
}

// liveCreateSetting 客户端一个配置字段都没填时返回 nil（走服务端默认），
// 只要填了任意一项就按整段提交——RoomSetting 是消息而非逐字段 optional，
// 半填半不填无法在 protobuf 里表达，因此不伪造中间态。
func liveCreateSetting(req *types.ParamLiveRoomCreate) *liveroomrpc.RoomSetting {
	if !req.DanmakuEnabled && !req.ReplyEnabled && !req.RecordEnabled && !req.LinkmicEnabled &&
		req.LiveType == 0 && req.MinClientVersionCode == 0 {
		return nil
	}
	return liveRoomSettingForRPC(req.DanmakuEnabled, req.ReplyEnabled, req.RecordEnabled,
		req.LinkmicEnabled, req.LiveType, req.MinClientVersionCode)
}
