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
)

type ReportStreamStateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewReportStreamStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReportStreamStateLogic {
	return &ReportStreamStateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 推流状态事件入口（live.state.v1 消费者或 live-ingest 直调）：event_id 去重 + seq 乱序守卫
//
// 这里是本服务唯一由外部事件驱动的状态写入口，三条硬约束：
//  1. event_id 命中 live_room_idempotency（kind=IdempotencyKindEvent）即视为重复投递，
//     只回读当前投影，绝不第二次改状态；
//  2. seq 必须严格大于 live_session.last_stream_seq（model.AdvanceStreamSeqTx 是唯一落库点），
//     未命中后回查区分「seq 陈旧」与「状态已被并发推进」，两者给不同 result；
//  3. 场次与房间的投影在同一事务里写；非法迁移与不匹配一律不产生任何写入。
//
// 返回值刻意不用 gRPC error 表达「事件被丢弃」：丢弃是消费链路的正常结果，
// 用 error 会让消费者无限重投；但入参本身非法（缺 event_id、未知 stream_state）仍是 error。
func (l *ReportStreamStateLogic) ReportStreamState(in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
	if in == nil {
		return nil, model.ErrEventIDRequired
	}
	if err := checkEventID(in.GetEventId()); err != nil {
		return nil, err
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if !model.ValidStreamState(in.GetStreamState()) {
		return nil, fmt.Errorf("%w: stream_state=%d", model.ErrStreamStateInvalid, in.GetStreamState())
	}
	if in.GetStreamSeq() <= 0 {
		return nil, fmt.Errorf("%w: seq=%d", model.ErrStreamSeqStale, in.GetStreamSeq())
	}
	if in.GetOccurredAt() < 0 || in.GetInterruptedSeconds() < 0 {
		return nil, fmt.Errorf("%w: occurred_at/interrupted_seconds 不得为负", model.ErrStreamStateInvalid)
	}
	streamID, err := checkRef("stream_id", in.GetStreamId())
	if err != nil {
		return nil, err
	}
	eventID := strings.TrimSpace(in.GetEventId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcReportStreamState, eventID, model.IdempotencyKindEvent,
		in.GetRoomId(), in.GetSessionId(), traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		return l.duplicate(in, eventID)
	}

	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	if room == nil {
		return nil, model.ErrRoomNotFound
	}

	var session *model.LiveSession
	if in.GetSessionId() > 0 {
		session, err = l.svcCtx.Sessions.FindOne(l.ctx, in.GetSessionId())
		if err != nil {
			return nil, err
		}
		if session == nil || session.RoomID != room.RoomID {
			// 场次不存在或不属于该房间：不改任何状态，交回消费方判定（不猜、不自愈）。
			return l.reply(model.StreamResultMismatch, room, nil, "场次与房间不匹配或不存在"), nil
		}
	} else {
		session, err = l.svcCtx.Sessions.FindActiveByRoom(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return l.reply(model.StreamResultMismatch, room, nil, "房间没有进行中场次"), nil
		}
	}

	// 流引用一致性：事件里的 stream_id 必须与场次登记的（若已登记）一致，
	// 否则说明 ingest 把另一条流的事件投到了这个场次上。
	if streamID != "" && session.StreamID != "" && streamID != session.StreamID {
		return l.reply(model.StreamResultMismatch, room, session, "stream_id 与场次登记值不一致"), nil
	}

	grace := l.svcCtx.Config.LiveRoom.StreamInterruptGraceSeconds
	plan, err := streamEventPlan(in.GetStreamState(), session.State, in.GetInterruptedSeconds(), grace)
	if err != nil {
		return nil, err
	}
	if plan.noChange {
		return l.reply(plan.result, room, session, plan.message), nil
	}

	// 房间侧迁移的合法性必须在写场次之前判定：任何一步不合法都不该留下半个写入。
	needRoomMove := plan.roomTo != 0 && room.State != plan.roomTo
	if needRoomMove && !model.CanRoomTransition(room.State, plan.roomTo) {
		return l.reply(model.StreamResultIllegalTransition, room, session,
			fmt.Sprintf("房间状态 %d 不接受该流事件（%s）", room.State, plan.message)), nil
	}

	// seq 守卫未命中时用它记录「本次要不要回滚房间写入」，事务外再回查区分原因。
	var seqRejected bool
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		if plan.bumpOnly || plan.sessionTo == session.State {
			ok, err := l.svcCtx.Sessions.BumpStreamSeqTx(ctx, tx, session.SessionID, in.GetStreamSeq())
			if err != nil {
				return err
			}
			if !ok {
				seqRejected = true
				return model.ErrStreamSeqStale
			}
			return nil
		}
		ok, err := l.svcCtx.Sessions.AdvanceStreamSeqTx(ctx, tx, session.SessionID, in.GetStreamSeq(),
			session.State, plan.sessionTo, plan.endReason, in.GetOccurredAt())
		if err != nil {
			return err
		}
		if !ok {
			seqRejected = true
			return model.ErrConcurrentUpdate
		}
		if needRoomMove {
			patch := model.RoomPatch{}
			if plan.roomTo == model.RoomStateLiving {
				patch.ActiveSessionID = i64Ptr(session.SessionID)
				patch.ActiveStreamID = strPtr(session.StreamID)
			} else {
				patch = clearActiveSessionPatch()
			}
			ok, err = l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, room.State, plan.roomTo,
				room.StateVersion, patch)
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, SessionID: session.SessionID, StateType: model.LogTypeRoomState,
				FromState: room.State, ToState: plan.roomTo, Source: model.SourceStreamEvent,
				EventID: eventID, Reason: truncateRunes(plan.message, maxReasonRunes), TraceID: traceID,
			}); err != nil {
				return err
			}
		}
		if !plan.bumpOnly {
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, SessionID: session.SessionID, StateType: model.LogTypeSessionState,
				FromState: session.State, ToState: plan.sessionTo, Source: model.SourceStreamEvent,
				EventID: eventID, Reason: truncateRunes(plan.message, maxReasonRunes), TraceID: traceID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if seqRejected {
		// 事务已回滚：回查区分「seq 陈旧」与「状态已被并发推进」，绝不把乱序事件当成功。
		fresh, ferr := l.svcCtx.Sessions.FindOne(l.ctx, session.SessionID)
		if ferr != nil {
			return nil, ferr
		}
		result, msg := staleStreamResult(fresh, in.GetStreamSeq())
		return l.reply(result, room, fresh, msg), nil
	}
	if err != nil {
		return nil, err
	}

	// 首次到达的事件可以补登 stream_id 引用（只在原值为空时写，重连不漂流）。
	if streamID != "" && session.StreamID == "" {
		if _, err := l.svcCtx.Sessions.SetStreamID(l.ctx, session.SessionID, streamID); err != nil {
			l.Errorf("liveroom: session %d 回填 stream_id 失败: %v", session.SessionID, err)
		}
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	freshRoom, err := l.svcCtx.Rooms.FindOne(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	freshSession, err := l.svcCtx.Sessions.FindOne(l.ctx, session.SessionID)
	if err != nil {
		return nil, err
	}
	return l.reply(plan.result, freshRoom, freshSession, plan.message), nil
}

// reply 组装事件处理结果：状态一律回读自当前投影，不回填「期望值」。
func (l *ReportStreamStateLogic) reply(result int32, room *model.LiveRoom, session *model.LiveSession, msg string) *rpc.ReportStreamStateReply {
	out := &rpc.ReportStreamStateReply{Result: result, Message: msg}
	if room != nil {
		out.RoomState = rpc.RoomState(room.State)
	}
	if session != nil {
		out.SessionId = session.SessionID
		out.SessionState = rpc.SessionState(session.State)
		return out
	}
	if room != nil && room.ActiveSessionID > 0 {
		out.SessionId = room.ActiveSessionID
	}
	return out
}

// duplicate 处理 event_id 重投：只读当前投影回 result=2，不产生任何写入。
func (l *ReportStreamStateLogic) duplicate(in *rpc.ReportStreamStateReq, eventID string) (*rpc.ReportStreamStateReply, error) {
	rec, err := dedupRecord(l.ctx, l.svcCtx, rpcReportStreamState, eventID)
	if err != nil {
		return nil, err
	}
	room, err := l.svcCtx.Rooms.FindOne(l.ctx, in.GetRoomId())
	if err != nil {
		return nil, err
	}
	var session *model.LiveSession
	if in.GetSessionId() > 0 {
		session, err = l.svcCtx.Sessions.FindOne(l.ctx, in.GetSessionId())
		if err != nil {
			return nil, err
		}
	} else if room != nil && room.ActiveSessionID > 0 {
		session, err = l.svcCtx.Sessions.FindOne(l.ctx, room.ActiveSessionID)
		if err != nil {
			return nil, err
		}
	}
	// rec 一定存在（dedupRecord 已保证）；这里保留 result_json 里的首次结果口径，
	// 但状态字段以当前投影为准，消费方据此判断是否还要重投。
	if rec.ResultJSON != "" {
		out := &rpc.ReportStreamStateReply{}
		if err := unmarshalResult(rec.ResultJSON, out); err == nil {
			fresh := l.reply(model.StreamResultDuplicate, room, session, out.Message+"（重复投递）")
			return fresh, nil
		}
	}
	return l.reply(model.StreamResultDuplicate, room, session, "同一 event_id 重复投递，未产生新写入"), nil
}
