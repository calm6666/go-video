package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-video/services/live-room/internal/svc"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type StartLiveLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewStartLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *StartLiveLogic {
	return &StartLiveLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开播：READY→LIVING 并新建场次（不接收推流密钥，只登记 stream_id 引用）
//
// 落库顺序刻意是「场次建档 → 房间 CAS → 场次迁移 → 审计日志」全在一个事务里：
// 场次一旦离开事务就是孤儿行，会把该房间后续开播永久卡在「已有非终态场次」上。
// stream_id 只是引用：推流密钥与流真值归 live-ingest，本服务不校验其存在性。
func (l *StartLiveLogic) StartLive(in *rpc.StartLiveReq) (*rpc.StartLiveReply, error) {
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkMid(in.GetMid()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	streamID, err := checkRef("stream_id", in.GetStreamId())
	if err != nil {
		return nil, err
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	// 幂等：同 request_id 重放返回同一 session_id，不再开一场。
	first, err := claimDedup(l.ctx, l.svcCtx, rpcStartLive, reqID, model.IdempotencyKindRequest,
		in.GetRoomId(), 0, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcStartLive, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.StartLiveReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
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
	if room.State == model.RoomStateBanned {
		return nil, model.ErrRoomBanned
	}
	if room.State == model.RoomStateLiving {
		return nil, model.ErrSessionAlreadyActive
	}
	if room.State != model.RoomStateReady {
		return nil, fmt.Errorf("%w: state=%d, 开播前必须先通过 PrepareLive", model.ErrInvalidRoomTransition, room.State)
	}
	if room.VerifyState != model.VerifyStatePassed {
		return nil, model.ErrNotVerified
	}

	bound, _, err := l.svcCtx.Anchors.IsEnabled(l.ctx, room.RoomID, in.GetMid())
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, model.ErrAnchorForbidden
	}

	now := model.NowUnix()
	activeBan, err := l.svcCtx.Bans.FindActiveByRoom(l.ctx, room.RoomID, now)
	if err != nil {
		return nil, err
	}
	if activeBan != nil {
		return nil, model.ErrRoomBanned
	}

	// 同一房间不得并存两个非终态场次。
	actives, err := l.svcCtx.Sessions.ListActiveByRoom(l.ctx, room.RoomID, 2)
	if err != nil {
		return nil, err
	}
	if len(actives) > 1 {
		l.Errorf("liveroom: room %d 出现 %d 条进行中场次，按最早一条判定冲突", room.RoomID, len(actives))
	}
	if len(actives) > 0 {
		return nil, fmt.Errorf("%w: session_id=%d", model.ErrSessionAlreadyActive, actives[0].SessionID)
	}

	session := &model.LiveSession{
		RoomID:         room.RoomID,
		Mid:            in.GetMid(),
		State:          model.SessionStatePending,
		TitleSnapshot:  truncateRunes(room.Title, maxSnapshotTitleRunes),
		AreaIDSnapshot: room.AreaID, // 开播时的分区快照，之后改分区不回写历史场次
		StreamID:       streamID,
		ReplayState:    model.ReplayStateNone,
		TraceID:        traceID,
	}

	var (
		sessionID  int64
		startedAt  int64
		newVersion int32
	)
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		id, err := l.svcCtx.Sessions.InsertTx(ctx, tx, session)
		if err != nil {
			return err
		}
		sessionID = id
		// 房间 CAS：expectVersion 用读到的 state_version，冲突就让调用方重读，绝不覆盖。
		patch := model.RoomPatch{ActiveSessionID: i64Ptr(id), ActiveStreamID: strPtr(streamID)}
		ok, err := l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, model.RoomStateReady,
			model.RoomStateLiving, room.StateVersion, patch)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		// PENDING→LIVING：started_at 由 SQL 侧 GREATEST 落定，断流重连不刷新开播时间。
		ok, err = l.svcCtx.Sessions.TransitionTx(ctx, tx, id, model.SessionStatePending,
			model.SessionStateLiving, model.EndReasonUnspecified, 0)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		startedAt = model.NowUnix()
		newVersion = room.StateVersion + 1
		_, err = l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, SessionID: id, StateType: model.LogTypeRoomState,
			FromState: model.RoomStateReady, ToState: model.RoomStateLiving,
			OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
			RequestID: reqID, TraceID: traceID,
		})
		if err != nil {
			return err
		}
		_, err = l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, SessionID: id, StateType: model.LogTypeSessionState,
			FromState: model.SessionStatePending, ToState: model.SessionStateLiving,
			OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
			RequestID: reqID, TraceID: traceID,
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	// 录制开关只影响是否通知 live-media 起录制。本服务的 ServiceContext 里没有
	// live-media 客户端（契约缺口），因此这里只读开关并记日志，不伪造「已起录制」。
	recordEnabled, err := l.svcCtx.Settings.RecordEnabledFor(l.ctx, room.RoomID)
	switch {
	case errors.Is(err, model.ErrNoSettingRow):
		// 没配过不等于关闭：记日志后交给 live-media 侧的默认策略。
		l.Infof("liveroom: room %d 无配置行，录制开关按默认策略处理", room.RoomID)
	case err != nil:
		l.Errorf("liveroom: room %d 读取录制开关失败: %v", room.RoomID, err)
	case recordEnabled:
		l.Infof("liveroom: room %d session %d 声明要起录制，但本服务无 live-media 客户端，由 live-media 消费事件处理",
			room.RoomID, sessionID)
	}

	reply := &rpc.StartLiveReply{
		SessionId:    sessionID,
		State:        rpc.RoomState_ROOM_STATE_LIVING,
		StartedAt:    startedAt,
		StateVersion: newVersion,
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}
