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

type UpdateRoomInfoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateRoomInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateRoomInfoLogic {
	return &UpdateRoomInfoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 修改标题/封面/分区：终态房间不可改；改动后重新送审
//
// 口径与其它写接口不同处：本方法是「未传即不改」（空串/0 表示保持原值），
// UpdateRoomSetting 才是整段覆盖。差量为空时不写库也不送审——
// 空改动也去建一条 moderation 任务会污染审核队列。
func (l *UpdateRoomInfoLogic) UpdateRoomInfo(in *rpc.UpdateRoomInfoReq) (*rpc.UpdateRoomInfoReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkMid(in.GetOperatorMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
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
	bound, role, err := l.svcCtx.Anchors.IsEnabled(l.ctx, room.RoomID, in.GetOperatorMid())
	if err != nil {
		return nil, err
	}
	if !bound || !canEditRoomInfo(role) {
		return nil, model.ErrAnchorForbidden
	}

	newTitle, err := changedTitle(in.GetTitle(), room.Title, l.svcCtx.Config.LiveRoom.TitleMaxLength)
	if err != nil {
		return nil, err
	}
	newCover, err := changedRef(in.GetCover(), room.Cover, checkCover)
	if err != nil {
		return nil, err
	}
	newAreaID := int64(0)
	if in.GetAreaId() > 0 && in.GetAreaId() != room.AreaID {
		usable, err := l.svcCtx.Areas.IsUsable(l.ctx, in.GetAreaId())
		if err != nil {
			return nil, err
		}
		if !usable {
			return nil, fmt.Errorf("%w: area_id=%d", model.ErrAreaDisabled, in.GetAreaId())
		}
		newAreaID = in.GetAreaId()
	}
	changingArea := newAreaID > 0
	if newTitle == "" && newCover == "" && !changingArea {
		// 无差量：原样回投影，moderation_task_id=0 表示本次没有产生新送审。
		return &rpc.UpdateRoomInfoReply{Room: roomInfo(room)}, nil
	}

	allowStates := roomInfoEditableStates(changingArea)
	if !stateIn(allowStates, room.State) {
		return nil, fmt.Errorf("%w: state=%d, changing_area=%t",
			model.ErrRoomStateNotEditable, room.State, changingArea)
	}
	if l.svcCtx.Moderation == nil {
		return nil, model.ErrModerationNotConfigured
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcUpdateRoomInfo, reqID, model.IdempotencyKindRequest,
		room.RoomID, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcUpdateRoomInfo, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.UpdateRoomInfoReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	// 先送审再改行：反过来会留下「资料已改但没人审」的房间，
	// 而这里失败时本地一行未写，调用方可以安全重试。
	taskID, err := submitProfileReview(l.ctx, l.svcCtx, room.RoomID, in.GetOperatorMid(), "live_room_profile_updated")
	if err != nil {
		return nil, err
	}
	target := verifyTargetAfterResubmit(room.VerifyState)
	ok, err := l.svcCtx.Rooms.UpdateProfile(l.ctx, room.RoomID, newTitle, newCover, newAreaID,
		target, taskID, allowStates)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, model.ErrConcurrentUpdate
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)
	if _, err := l.svcCtx.StateLogs.Insert(l.ctx, &model.LiveRoomStateLog{
		RoomID: room.RoomID, StateType: model.LogTypeVerifyState,
		FromState: room.VerifyState, ToState: target,
		OperatorMid: in.GetOperatorMid(), Source: model.SourceRPCClient,
		RequestID: reqID, TraceID: traceID, Reason: "profile_resubmitted",
	}); err != nil {
		// 资料变更与送审任务 ID 已落在 live_room 行上，日志失败不推翻既成事实，
		// 但必须留下错误证据（审计缺行由 cron 对账补齐）。
		l.Errorf("liveroom: room %d 资料变更审计日志写入失败: %v", room.RoomID, err)
	}

	fresh, err := l.svcCtx.Rooms.FindOne(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrRoomNotFound
	}
	reply := &rpc.UpdateRoomInfoReply{Room: roomInfo(fresh), ModerationTaskId: taskID}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// changedTitle 只在「非空且与现值不同」时返回新值，否则返回空串表示不修改。
func changedTitle(in, cur string, maxRunes int32) (string, error) {
	if strings.TrimSpace(in) == "" {
		return "", nil
	}
	t, err := checkTitle(in, maxRunes)
	if err != nil {
		return "", err
	}
	if t == cur {
		return "", nil
	}
	return t, nil
}

// changedRef 同 changedTitle，用于封面这类引用字段（校验口径由 check 决定）。
func changedRef(in, cur string, check func(string) (string, error)) (string, error) {
	if strings.TrimSpace(in) == "" {
		return "", nil
	}
	v, err := check(in)
	if err != nil {
		return "", err
	}
	if v == cur {
		return "", nil
	}
	return v, nil
}

// stateIn 判定状态是否落在允许集合内（logic 侧唯一需要的位置查询，model 不导出集合）。
func stateIn(states []int32, state int32) bool {
	for _, s := range states {
		if s == state {
			return true
		}
	}
	return false
}
