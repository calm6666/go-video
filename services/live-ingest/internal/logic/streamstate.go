// 本文件是 logic 包的手写状态机引擎（一次迁移 = 状态位 + 断流区间 + 事件 + Outbox），
// 不是 goctl 生成产物。
//
// 为什么只有一个引擎：README「同事务约束」要求 live_stream.state 迁移、
// live_stream_event、live_ingest_outbox 与断流区间的开/关必须原子提交。
// ReportStreamState、CloseStream、ReportStreamHealth、RevokeStreamKey 级联
// 四个入口都走这里，任何一处漏写 Outbox 就会让 live-room 的投影永久落后，
// 所以「不许各自抄一份」是本文件存在的唯一理由。
package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
)

// eventProducer 是信封的 producer 字段（README「live.state.v1 事件契约」）。
const eventProducer = "live-ingest"

// errNoTransaction 用于「引擎被误用在事务外」。这里不用哨兵：
// 它是编程错误，调用方无法通过重试恢复，也不该被映射成业务状态码。
var errNoTransaction = errors.New("live-ingest: state transition requires a transaction session")

// stateEventPayload 是 live.state.v1 的业务负载，字段与 live-room ReportStreamStateReq
// 一一对应（README 的 payload 表），再加上 anchor_mid：
// inbox 是本 topic 的另一个消费者，它要把断流/停播通知发给主播本人，
// 而主播 ID 只在 live_stream 行里（live-room 才持有房间→主播绑定）。
// 事件里带上它是「生产者提供路由所需的事实」，不是替消费方做业务判断；
// 注意不含任何密钥材料，也不含客户端 IP。
type stateEventPayload struct {
	StreamID           string `json:"stream_id"`
	RoomID             int64  `json:"room_id"`
	AnchorMid          int64  `json:"anchor_mid"`
	SessionID          int64  `json:"session_id"`
	StreamState        int32  `json:"stream_state"`
	StreamSeq          int64  `json:"stream_seq"`
	OccurredAt         int64  `json:"occurred_at"`
	InterruptedSeconds int64  `json:"interrupted_seconds,omitempty"`
	Reason             string `json:"reason,omitempty"`
	TraceID            string `json:"trace_id,omitempty"`
}

// transitionInput 是一次「带事件的迁移」的入参。
//
// ReportID 非空时成为事件的 uniq_report_id 幂等锚点；内部触发（健康越界、级联停流）
// 传空串，靠 (stream_id, seq) 唯一索引防重号。
type transitionInput struct {
	streamID   string
	to         int32
	at         int64  // 0 表示用当前时间
	expectSeq  int64  // 0 表示不校验；非 0 时与当前 seq 不符即 ErrSeqConflict
	nodeID     string // 空串表示沿用流上的当前节点
	reportID   string
	source     string // model.EventSource*
	reason     string
	stopReason int32
	traceID    string
}

// transitionResult 回显迁移结果。applied=false 只发生在「同态 no-op」，
// 那是幂等成功而不是失败：不占 seq、不写事件、不发信封。
type transitionResult struct {
	applied         bool
	streamID        string
	roomID          int64
	sessionID       int64
	from            int32
	to              int32
	seq             int64
	eventID         string
	nodeID          string
	interruptionID  int64
	interruptedSecs int64
	message         string
}

// applyStreamTransition 在调用方事务内推进状态并留下完整的事件链。
// 所有失败都让事务回滚：宁可让上报方重试，也不要「状态改了、事件没写」的半截事实。
func applyStreamTransition(
	ctx context.Context,
	svcCtx *svc.ServiceContext,
	tx sqlx.Session,
	in transitionInput,
) (*transitionResult, error) {
	repo := svcCtx.Repository
	if repo == nil || tx == nil {
		return nil, errNoTransaction
	}
	if in.streamID == "" || !model.ValidStreamState(in.to) {
		return nil, model.ErrInvalidStreamState
	}

	s, err := repo.Stream.LockByID(ctx, tx, in.streamID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrStreamNotFound
	}

	at := in.at
	if at <= 0 {
		at = nowUnix()
	}
	nodeID := s.NodeID
	if in.nodeID != "" {
		nodeID = in.nodeID
	}

	// 同态上报：幂等 no-op。回查该 seq 的事件以便调用方仍能回显 event_id。
	if s.State == in.to {
		res := &transitionResult{
			applied: false, streamID: s.StreamID, roomID: s.RoomID, sessionID: s.SessionID,
			from: s.State, to: in.to, seq: s.Seq, nodeID: nodeID,
			message: "stream already in requested state",
		}
		existing, err := repo.StreamEvent.FindByStreamSeq(ctx, s.StreamID, s.Seq)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			res.eventID = existing.EventID
			res.interruptionID = existing.InterruptionID
			res.interruptedSecs = existing.InterruptedSeconds
		}
		return res, nil
	}

	if s.State == model.StreamStateStopped {
		// 终态无出边：重推是新 stream_id，绝不给「把已停的流拉回来」的口子。
		return nil, model.ErrTerminalStream
	}
	if in.expectSeq > 0 && in.expectSeq != s.Seq {
		return nil, fmt.Errorf("%w: reported seq does not match current stream seq", model.ErrSeqConflict)
	}
	if !model.CanTransitionStreamState(s.State, in.to) {
		return nil, fmt.Errorf("%w: %s to %s", model.ErrInvalidStateTransition,
			streamStateName(s.State), streamStateName(in.to))
	}

	newSeq := s.Seq + 1
	ok, err := repo.Stream.ApplyTransition(ctx, tx, s.StreamID, s.State, in.to, newSeq, at, in.stopReason, in.reason)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 行已锁定却仍未命中，说明同事务里被别的写改掉了；回滚重来。
		return nil, model.ErrConcurrentUpdate
	}

	eventID, err := newEventID()
	if err != nil {
		return nil, err
	}

	res := &transitionResult{
		applied: true, streamID: s.StreamID, roomID: s.RoomID, sessionID: s.SessionID,
		from: s.State, to: in.to, seq: newSeq, eventID: eventID, nodeID: nodeID,
	}

	// 断流区间的开与合：进入 INTERRUPTED 开一段，离开 INTERRUPTED 合上它。
	if in.to == model.StreamStateInterrupted {
		open, err := repo.StreamInterruption.FindOpenByStream(ctx, s.StreamID)
		if err != nil {
			return nil, err
		}
		if open == nil {
			episode, err := repo.StreamInterruption.NextEpisodeNo(ctx, tx, s.StreamID)
			if err != nil {
				return nil, err
			}
			id, err := repo.StreamInterruption.Insert(ctx, tx, &model.StreamInterruption{
				StreamID: s.StreamID, RoomID: s.RoomID, EpisodeNo: episode, NodeID: nodeID,
				StartedAt: at, StartEventID: eventID, Reason: in.reason,
			})
			if err != nil {
				return nil, err
			}
			res.interruptionID = id
			counted, err := repo.Stream.OpenInterruption(ctx, tx, s.StreamID)
			if err != nil {
				return nil, err
			}
			if !counted {
				return nil, model.ErrConcurrentUpdate
			}
		} else {
			res.interruptionID = open.InterruptionID
		}
	} else if s.State == model.StreamStateInterrupted {
		open, err := repo.StreamInterruption.FindOpenByStream(ctx, s.StreamID)
		if err != nil {
			return nil, err
		}
		if open == nil {
			res.message = "interruption window already closed"
		} else {
			duration := at - open.StartedAt
			if duration < 0 {
				duration = 0
			}
			endReason := model.InterruptionEndReconnected
			if in.to == model.StreamStateStopped {
				endReason = model.InterruptionEndClosed
				if in.stopReason == model.StopReasonNodeTimeout {
					endReason = model.InterruptionEndTimeout
				}
			}
			closed, err := repo.StreamInterruption.Close(ctx, tx, open.InterruptionID, at, duration,
				int64(endReason), eventID, in.reason)
			if err != nil {
				return nil, err
			}
			if !closed {
				return nil, model.ErrConcurrentUpdate
			}
			accumulated, err := repo.Stream.CloseInterruption(ctx, tx, s.StreamID, duration)
			if err != nil {
				return nil, err
			}
			if !accumulated {
				return nil, model.ErrConcurrentUpdate
			}
			res.interruptionID = open.InterruptionID
			res.interruptedSecs = duration
		}
	}

	// 停流的四件收尾必须与状态迁移同事务：健康归位、节点指针清除、密钥活跃指针、节点租约与配额。
	if in.to == model.StreamStateStopped {
		if err := repo.Stream.MarkStopped(ctx, tx, s.StreamID); err != nil {
			return nil, err
		}
		// 与 ReleaseIngestNode 同一条 CAS 口径清掉流行上的节点指针，否则库里留下
		// 「终态流还住在节点上、配额已归还」两个事实，按节点筛流与运营面板都会算错。
		if s.NodeID != "" {
			cleared, err := repo.Stream.SetNode(ctx, tx, s.StreamID, "", s.NodeID)
			if err != nil {
				return nil, err
			}
			if !cleared {
				return nil, model.ErrConcurrentUpdate
			}
		}

		if s.KeyID > 0 {
			// 指针为空或已指向后继流时返回 false，两者都是正确结果，不算失败。
			if _, err := repo.StreamKey.ReleaseActiveStream(ctx, tx, s.KeyID, s.StreamID); err != nil {
				return nil, err
			}
		}
		assignment, err := repo.NodeAssignment.FindActiveByStream(ctx, tx, s.StreamID)
		if err != nil {
			return nil, err
		}
		if assignment != nil {
			released, err := repo.NodeAssignment.Release(ctx, tx, assignment.AssignmentID,
				model.AssignmentStateReleased, at, in.reason, assignment.NodeID)
			if err != nil {
				return nil, err
			}
			if !released {
				return nil, model.ErrConcurrentUpdate
			}
			quota, err := repo.IngestNode.ReleaseQuota(ctx, tx, assignment.NodeID)
			if err != nil {
				return nil, err
			}
			if !quota {
				// 租约刚被 CAS 释放却无配额可退：节点行被删了。回滚让运维看到，
				// 而不是把节点配额悄悄记少（后续分配会超卖）。
				return nil, fmt.Errorf("%w: node quota already drained", model.ErrNodeNotFound)
			}
			res.nodeID = ""
		}
	}

	// 事件行为什么放最后：InterruptionID 只有断流区间开合后才知道。
	if _, err := repo.StreamEvent.Insert(ctx, tx, &model.StreamEvent{
		EventID: eventID, StreamID: s.StreamID, RoomID: s.RoomID, SessionID: s.SessionID,
		Seq: newSeq, FromState: s.State, ToState: in.to, NodeID: nodeID,
		InterruptionID: res.interruptionID, InterruptedSeconds: res.interruptedSecs,
		StopReason: in.stopReason, ReportID: in.reportID, Source: sourceOr(in.source, model.EventSourceEntry),
		Reason: in.reason, OccurredAt: at, TraceID: in.traceID,
	}); err != nil {
		if model.IsDuplicate(err) {
			// uniq_event_id / uniq_stream_seq / uniq_report_id 任一命中都说明这次上报
			// 已经产生过事件；回滚后由调用方按重放语义回查首次结果。
			return nil, fmt.Errorf("%w: state event unique index conflict", model.ErrConcurrentUpdate)
		}
		return nil, err
	}

	payload, err := buildStateEnvelopePayload(s.StreamID, s.RoomID, s.AnchorMid, s.SessionID, in.to, newSeq, at,
		res.interruptedSecs, in.reason, in.traceID, eventID)
	if err != nil {
		return nil, err
	}
	if err := repo.Outbox.Insert(ctx, tx, &model.EventOutbox{
		EventID: eventID, EventType: model.EventTypeStreamState, SchemaVersion: model.SchemaVersionStreamState,
		AggregateType: model.AggregateTypeStream, AggregateID: s.StreamID, StreamID: s.StreamID,
		RoomID: s.RoomID, Seq: newSeq, Payload: payload, State: model.OutboxStatePending,
		OccurredAt: at,
	}); err != nil {
		return nil, err
	}
	return res, nil
}

// buildStateEnvelopePayload 组装信封 JSON（即 live_ingest_outbox.payload 全文）。
// event_id 由调用方传入而不是现生成：它同时是 live_stream_event.event_id
// 与断流记录的 start/end event 引用，三处必须同源。
func buildStateEnvelopePayload(
	streamID string,
	roomID, anchorMid, sessionID int64,
	to int32,
	seq, at, interruptedSeconds int64,
	reason, traceID, eventID string,
) (string, error) {
	body, err := json.Marshal(stateEventPayload{
		StreamID: streamID, RoomID: roomID, AnchorMid: anchorMid, SessionID: sessionID,
		StreamState: to, StreamSeq: seq, OccurredAt: at,
		InterruptedSeconds: interruptedSeconds, Reason: reason, TraceID: traceID,
	})
	if err != nil {
		return "", fmt.Errorf("live-ingest: marshal state payload: %w", err)
	}
	env := &eventenvelope.Envelope{
		EventID:       eventID,
		EventType:     model.EventTypeStreamState,
		SchemaVersion: model.SchemaVersionStreamState,
		OccurredAt:    timeutil.FormatRFC3339(time.Unix(at, 0).UTC()),
		Producer:      eventProducer,
		TraceID:       traceID,
		AggregateType: model.AggregateTypeStream,
		AggregateID:   streamID,
		Payload:       body,
	}
	// Envelope.MarshalJSON 内部会 Validate：字段缺失会在这一行暴露，
	// 而不是留下一条永远发不出去的信封。
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("live-ingest: marshal state envelope: %w", err)
	}
	return string(raw), nil
}

func sourceOr(source, fallback string) string {
	if source == "" {
		return fallback
	}
	return source
}

// streamStateName 给错误文案用可读状态名（不带数字，避免日志聚合基数爆炸）。
func streamStateName(v int32) string {
	switch v {
	case model.StreamStateIdle:
		return "idle"
	case model.StreamStatePublishing:
		return "publishing"
	case model.StreamStateInterrupted:
		return "interrupted"
	case model.StreamStateStopped:
		return "stopped"
	default:
		return "unspecified"
	}
}
