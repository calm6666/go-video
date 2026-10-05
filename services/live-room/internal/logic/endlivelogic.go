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

type EndLiveLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEndLiveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EndLiveLogic {
	return &EndLiveLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 下播：LIVING→READY 并把场次置为 ENDED（时长簿记）
//
// end_reason 只允许 ANCHOR_STOP / STREAM_REPLAY：BANNED、ROOM_CLOSED、STREAM_TIMEOUT
// 分别由 BanRoom / CloseRoom / ReportStreamState 写入，让终端自选就等于允许伪造「被禁播」。
// 场次已终态时按幂等重放处理，回真实终态而不是报错，也不改任何状态。
func (l *EndLiveLogic) EndLive(in *rpc.EndLiveReq) (*rpc.EndLiveReply, error) {
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
	reason := int32(in.GetEndReason())
	if reason == model.EndReasonUnspecified {
		reason = model.EndReasonAnchorStop
	}
	if !allowEndReasonForEndLive(reason) {
		return nil, fmt.Errorf("%w: %d", model.ErrEndReasonInvalid, reason)
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcEndLive, reqID, model.IdempotencyKindRequest,
		in.GetRoomId(), in.GetSessionId(), traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcEndLive, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.EndLiveReply{}
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

	var session *model.LiveSession
	if in.GetSessionId() > 0 {
		session, err = l.svcCtx.Sessions.FindOne(l.ctx, in.GetSessionId())
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, model.ErrSessionNotFound
		}
		if session.RoomID != room.RoomID {
			return nil, model.ErrSessionRoomMismatch
		}
	} else {
		session, err = l.svcCtx.Sessions.FindActiveByRoom(l.ctx, room.RoomID)
		if err != nil {
			return nil, err
		}
		if session == nil {
			return nil, model.ErrNoActiveSession
		}
	}

	bound, role, err := l.svcCtx.Anchors.IsEnabled(l.ctx, room.RoomID, in.GetMid())
	if err != nil {
		return nil, err
	}
	if !bound {
		return nil, model.ErrAnchorForbidden
	}
	// 只有开播者本人或房主能结束这一场（房管不可，否则房管可以踢主播下播）。
	if session.Mid != in.GetMid() && role != model.AnchorRoleOwner {
		return nil, model.ErrAnchorForbidden
	}

	// 场次已终态：幂等重放，回真实的终态与房间状态。
	if model.SessionStateIsTerminal(session.State) {
		if room.State == model.RoomStateLiving {
			if _, err := l.svcCtx.Rooms.ClearActiveSession(l.ctx, room.RoomID, session.SessionID); err != nil {
				return nil, err
			}
			invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)
		}
		reply := &rpc.EndLiveReply{
			SessionId:       session.SessionID,
			SessionState:    rpc.SessionState(session.State),
			RoomState:       rpc.RoomState(room.State),
			DurationSeconds: session.DurationSeconds,
			Replayed:        true,
		}
		saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
		return reply, nil
	}

	var ended *model.LiveSession
	err = l.svcCtx.DB.TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		ok, err := l.svcCtx.Sessions.TransitionTx(ctx, tx, session.SessionID, session.State,
			model.SessionStateEnded, reason, 0)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		// 房间只有 LIVING 才需要回 READY；被禁播/停用的房间不因下播而解禁（只有 LiftBan 能改）。
		if room.State == model.RoomStateLiving {
			ok, err = l.svcCtx.Rooms.TransitionTx(ctx, tx, room.RoomID, model.RoomStateLiving,
				model.RoomStateReady, room.StateVersion, clearActiveSessionPatch())
			if err != nil {
				return err
			}
			if !ok {
				return model.ErrConcurrentUpdate
			}
		}
		if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
			RoomID: room.RoomID, SessionID: session.SessionID, StateType: model.LogTypeSessionState,
			FromState: session.State, ToState: model.SessionStateEnded,
			OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
			RequestID: reqID, Reason: endReasonName(reason), TraceID: traceID,
		}); err != nil {
			return err
		}
		if room.State == model.RoomStateLiving {
			if _, err := l.svcCtx.StateLogs.InsertTx(ctx, tx, &model.LiveRoomStateLog{
				RoomID: room.RoomID, SessionID: session.SessionID, StateType: model.LogTypeRoomState,
				FromState: model.RoomStateLiving, ToState: model.RoomStateReady,
				OperatorMid: in.GetMid(), Source: model.SourceRPCClient,
				RequestID: reqID, TraceID: traceID,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// 房间不是 LIVING 时（例如已被禁播），仍然要清掉指向这一场的投影。
	if room.State != model.RoomStateLiving {
		if _, err := l.svcCtx.Rooms.ClearActiveSession(l.ctx, room.RoomID, session.SessionID); err != nil {
			return nil, err
		}
	}
	invalidateRoomCache(l.ctx, l.svcCtx, room.RoomID)

	if ended, err = l.svcCtx.Sessions.FindOne(l.ctx, session.SessionID); err != nil {
		return nil, err
	}
	if ended == nil {
		return nil, model.ErrSessionNotFound
	}
	fresh, err := l.svcCtx.Rooms.FindOne(l.ctx, room.RoomID)
	if err != nil {
		return nil, err
	}
	roomState := room.State
	if fresh != nil {
		roomState = fresh.State
	}

	// 回放收尾由 live-media 之后回调 AttachReplay，本方法不得伪造回放可用性。
	reply := &rpc.EndLiveReply{
		SessionId:       ended.SessionID,
		SessionState:    rpc.SessionState(ended.State),
		RoomState:       rpc.RoomState(roomState),
		DurationSeconds: ended.DurationSeconds,
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// endReasonName 把终止原因转成审计里可读的稳定词（不写自由文本）。
func endReasonName(reason int32) string {
	switch reason {
	case model.EndReasonAnchorStop:
		return "anchor_stop"
	case model.EndReasonBanned:
		return "banned"
	case model.EndReasonRoomClosed:
		return "room_closed"
	case model.EndReasonStreamTimeout:
		return "stream_timeout"
	case model.EndReasonStreamReplay:
		return "stream_replay"
	default:
		return "unspecified"
	}
}
