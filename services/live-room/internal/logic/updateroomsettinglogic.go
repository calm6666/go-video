package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateRoomSettingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateRoomSettingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateRoomSettingLogic {
	return &UpdateRoomSettingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 更新直播配置（弹幕/回复/录制/连麦/直播类型）
//
// 整段覆盖语义：本方法不接受 nil setting——「未传」在这里无法与「全部关闭」区分，
// 让一次漏传把房间功能全关成不可用，比拒绝请求危险得多。
// 开关变更只影响后续场次；进行中场次是否续录由 live-media 决定，本服务不下发录制指令。
func (l *UpdateRoomSettingLogic) UpdateRoomSetting(in *rpc.UpdateRoomSettingReq) (*rpc.UpdateRoomSettingReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkOperator(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	if in.GetSetting() == nil {
		return nil, fmt.Errorf("%w: setting 必填（整段覆盖，缺字段无法与显式关闭区分）", model.ErrSettingInvalid)
	}
	row, err := settingRowFromRequest(in.GetRoomId(), in.GetSetting())
	if err != nil {
		return nil, err
	}
	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}
	if model.RoomStateIsTerminal(room.State) {
		return nil, model.ErrRoomFinished
	}
	// 契约里没有 admin 位，配置归属只能按「生效房主」判定（缺口见 README）。
	owner, err := l.svcCtx.Anchors.FindOwner(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	if owner == nil || owner.Mid != in.GetOperatorMid() {
		return nil, fmt.Errorf("%w: operator_mid=%d 不是该房间生效房主", model.ErrAnchorNotOwner, in.GetOperatorMid())
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcUpdateRoomSetting, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcUpdateRoomSetting, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.UpdateRoomSettingReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	if err := l.svcCtx.Settings.Upsert(l.ctx, row); err != nil {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	fresh, err := l.svcCtx.Settings.FindOne(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	reply := &rpc.UpdateRoomSettingReply{Setting: settingInfo(fresh)}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}
