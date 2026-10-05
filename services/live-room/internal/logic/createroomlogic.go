package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
)

type CreateRoomLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateRoomLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateRoomLogic {
	return &CreateRoomLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 创建直播间：校验分区有效与房间数上限 → 落 PENDING → 建绑定与配置 → 送资料审核
//
// 落库顺序：房间行 + 房主绑定 + 配置行 + 审计日志同事务（缺一半就是脏房间），
// 送审在事务提交后发生——moderation 是外部系统，不能把它的往返算进本地事务持锁时间。
// 送审失败时 reply 里 verify_state 仍是 NONE、moderation_task_id 仍是 0：
// 这就是「已建房但未送审」的真实形态，不写「已送审」的假状态（AGENTS.md §9）。
func (l *CreateRoomLogic) CreateRoom(in *rpc.CreateRoomReq) (*rpc.CreateRoomReply, error) {
	if in == nil {
		return nil, model.ErrInvalidMid
	}
	if err := checkMid(in.GetMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	title, err := checkTitle(in.GetTitle(), l.svcCtx.Config.LiveRoom.TitleMaxLength)
	if err != nil {
		return nil, err
	}
	cover, err := checkCover(in.GetCover())
	if err != nil {
		return nil, err
	}
	if in.GetAreaId() <= 0 {
		return nil, model.ErrInvalidAreaID
	}
	platform, err := normalizePlatform(in.GetPlatform())
	if err != nil {
		return nil, err
	}
	settingRow, err := settingRowFromRequest(0, in.GetSetting())
	if err != nil {
		return nil, err
	}
	// 分区必须启用才可挂：停用分区下新房间永远进不了发现页，等于建了个死房间。
	usable, err := l.svcCtx.Areas.IsUsable(l.ctx, in.GetAreaId())
	if err != nil {
		return nil, err
	}
	if !usable {
		return nil, fmt.Errorf("%w: area_id=%d", model.ErrAreaDisabled, in.GetAreaId())
	}
	// 送审通道缺失时直接不受理：建一个永远停在「未提交审核」的房间比报错更糟。
	if l.svcCtx.Moderation == nil {
		return nil, model.ErrModerationNotConfigured
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcCreateRoom, reqID, model.IdempotencyKindRequest,
		0, 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcCreateRoom, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.CreateRoomReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	// 单主播房间数上限：终态 FINISHED 也计入（关掉再建不能返还额度）。
	limit := l.svcCtx.Config.LiveRoom.MaxRoomsPerOwner
	if limit > 0 {
		cnt, err := l.svcCtx.Rooms.CountByOwner(l.ctx, in.GetMid(), ownerLimitStates())
		if err != nil {
			return nil, err
		}
		if cnt >= int64(limit) {
			return nil, fmt.Errorf("%w: mid=%d 已有 %d 个房间，上限 %d",
				model.ErrRoomLimitExceeded, in.GetMid(), cnt, limit)
		}
	}

	var roomID int64
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		id, err := l.svcCtx.Rooms.InsertTx(ctx, tx, &model.LiveRoom{
			OwnerMid:     in.GetMid(),
			Title:        title,
			Cover:        cover,
			AreaID:       in.GetAreaId(),
			State:        model.RoomStatePending,
			VerifyState:  model.VerifyStateNone,
			StateVersion: 1,
			Platform:     platform,
			AppVersion:   truncateRunes(in.GetAppVersion(), maxRefRunes),
		})
		if err != nil {
			return err
		}
		roomID = id
		if _, err := l.svcCtx.Anchors.BindTx(ctx, tx, id, in.GetMid(), model.AnchorRoleOwner); err != nil {
			return err
		}
		settingRow.RoomID = id
		if err := l.svcCtx.Settings.UpsertTx(ctx, tx, settingRow); err != nil {
			return err
		}
		_, err = l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID:      id,
			StateType:   model.LogTypeRoomState,
			FromState:   model.RoomStateUnspecified,
			ToState:     model.RoomStatePending,
			OperatorMid: in.GetMid(),
			Source:      model.SourceRPCClient,
			RequestID:   reqID,
			TraceID:     traceID,
			Reason:      "create_room",
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	invalidateRoomCache(l.ctx, l.svcCtx, roomID)

	reply := &rpc.CreateRoomReply{
		RoomId:      roomID,
		State:       rpc.RoomState_ROOM_STATE_PENDING,
		VerifyState: rpc.VerifyState(model.VerifyStateNone),
	}

	taskID, submitErr := submitProfileReview(l.ctx, l.svcCtx, roomID, in.GetMid(), "live_room_created")
	if submitErr != nil {
		// 房间已经建好，不能假装失败：verify_state 停在 NONE 就是「未送审」的事实。
		l.Errorf("liveroom: room %d 送审失败，资料停留在未提交态: %v", roomID, submitErr)
		saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
		return reply, nil
	}
	// 送审成功后一次性回写 verify_state=REVIEWING + 任务 ID；房间仍在 PENDING。
	if _, err := l.svcCtx.Rooms.UpdateProfile(l.ctx, roomID, "", "", 0,
		model.VerifyStateReviewing, taskID, []int32{model.RoomStatePending}); err != nil {
		l.Errorf("liveroom: room %d 回填送审结果失败: %v", roomID, err)
	} else {
		reply.VerifyState = rpc.VerifyState(model.VerifyStateReviewing)
		reply.ModerationTaskId = taskID
		if _, err := l.svcCtx.StateLogs.Insert(l.ctx, &model.LiveRoomStateLog{
			RoomID: roomID, StateType: model.LogTypeVerifyState,
			FromState: model.VerifyStateNone, ToState: model.VerifyStateReviewing,
			OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
			RequestID: reqID, TraceID: traceID, Reason: "profile_submitted",
		}); err != nil {
			l.Errorf("liveroom: room %d 送审审计日志写入失败: %v", roomID, err)
		}
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// submitProfileReview 向 moderation 提交房间资料审核并返回任务 ID。
//
// 契约缺口（已在 README/报告登记）：SubmitReq 以 submission_id 定位对象，
// 而 live-room 的 room_id 只有在本地落库后才存在，因此「先落房、再送审、再回写」
// 是本契约下唯一的可行顺序；CreateRoom 无法在一次调用内原子完成送审。
func submitProfileReview(ctx context.Context, s *svc.ServiceContext, roomID, mid int64, reason string) (int64, error) {
	if s.Moderation == nil {
		return 0, model.ErrModerationNotConfigured
	}
	resp, err := s.Moderation.SubmitForReview(ctx, &moderationrpc.SubmitReq{
		SubmissionId: roomID,
		ContentType:  moderationrpc.ContentType_CONTENT_TYPE_LIVE,
		Mid:          mid,
		UpMid:        mid,
		Business:     s.Config.LiveRoom.ModerationBusiness,
		Reason:       reason,
	})
	if err != nil {
		return 0, fmt.Errorf("%w: moderation submit for room %d: %v", model.ErrDownstreamUnavailable, roomID, err)
	}
	taskID := resp.GetTask().GetTaskId()
	if taskID <= 0 {
		return 0, fmt.Errorf("%w: moderation returned empty task for room %d", model.ErrDownstreamUnavailable, roomID)
	}
	return taskID, nil
}
