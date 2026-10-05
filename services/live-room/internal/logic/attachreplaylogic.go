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

type AttachReplayLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAttachReplayLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AttachReplayLogic {
	return &AttachReplayLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 关联回放：只写 record/asset/aid 引用与回放状态，不落媒资数据
//
// 单条 UPDATE 同时要求「属于该房间 + 已终态 + CanReplayTransition(from,to)」，
// 未命中后回查区分到底是哪一条不成立——事件入口回错原因，消费方就会做错重试决策。
// 置 AVAILABLE 之前必须已由 live-media 确认媒资可播，本服务不自行判定，
// 但要求引用齐备：一个 AVAILABLE 却没有任何媒资引用的场次是不可解释的数据。
func (l *AttachReplayLogic) AttachReplay(in *rpc.AttachReplayReq) (*rpc.AttachReplayReply, error) {
	if in == nil {
		return nil, model.ErrInvalidSessionID
	}
	if err := checkSessionID(in.GetSessionId()); err != nil {
		return nil, err
	}
	if err := checkRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	if err := checkRequestID(in.GetRequestId()); err != nil {
		return nil, err
	}
	target := int32(in.GetReplayState())
	if !allowReplayTarget(target) {
		return nil, fmt.Errorf("%w: replay_state=%d", model.ErrReplayStateInvalid, target)
	}
	if in.GetRecordId() < 0 || in.GetRecordAssetId() < 0 || in.GetRecordAid() < 0 {
		return nil, model.ErrSettingInvalid
	}
	if target == model.ReplayStateAvailable && in.GetRecordAssetId() <= 0 {
		return nil, fmt.Errorf("%w: AVAILABLE 需要 record_asset_id 引用", model.ErrReplayStateInvalid)
	}
	session, err := l.svcCtx.Sessions.FindOne(l.ctx, in.GetSessionId())
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, model.ErrSessionNotFound
	}
	if session.RoomID != in.GetRoomId() {
		return nil, fmt.Errorf("%w: session %d 属于房间 %d", model.ErrSessionRoomMismatch,
			session.SessionID, session.RoomID)
	}
	if !model.SessionStateIsTerminal(session.State) {
		return nil, fmt.Errorf("%w: session %d state=%d", model.ErrSessionNotTerminal,
			session.SessionID, session.State)
	}
	reqID := strings.TrimSpace(in.GetRequestId())
	traceID := sanitizeTraceID(in.GetTraceId())

	first, err := claimDedup(l.ctx, l.svcCtx, rpcAttachReplay, reqID, model.IdempotencyKindRequest,
		session.RoomID, session.SessionID, traceID)
	if err != nil {
		return nil, err
	}
	if !first {
		rec, err := dedupRecord(l.ctx, l.svcCtx, rpcAttachReplay, reqID)
		if err != nil {
			return nil, err
		}
		reply := &rpc.AttachReplayReply{}
		if err := unmarshalResult(rec.ResultJSON, reply); err != nil {
			return nil, err
		}
		reply.Replayed = true
		return reply, nil
	}

	if session.ReplayState == target && replayRefsEqual(session, in) {
		// 同状态同引用：幂等重放语义，不重复写日志。
		reply := &rpc.AttachReplayReply{
			SessionId:   session.SessionID,
			ReplayState: rpc.ReplayState(session.ReplayState),
		}
		saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
		return reply, nil
	}

	ok, err := l.svcCtx.Sessions.AttachReplay(l.ctx, session.SessionID, session.RoomID,
		session.ReplayState, target, in.GetRecordId(), in.GetRecordAssetId(), in.GetRecordAid())
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, l.replayMissCause(session.SessionID, target)
	}
	if _, err := l.svcCtx.StateLogs.Insert(l.ctx, &model.LiveRoomStateLog{
		RoomID: session.RoomID, SessionID: session.SessionID, StateType: model.LogTypeReplayState,
		FromState: session.ReplayState, ToState: target,
		Source: model.SourceStreamEvent, RequestID: reqID, TraceID: traceID,
	}); err != nil {
		l.Errorf("liveroom: session %d 回放状态审计日志写入失败: %v", session.SessionID, err)
	}
	invalidateRoomCache(l.ctx, l.svcCtx, session.RoomID)

	reply := &rpc.AttachReplayReply{
		SessionId:   session.SessionID,
		ReplayState: rpc.ReplayState(target),
	}
	saveDedupResult(l.ctx, l.svcCtx, reqID, reply, l.Logger)
	return reply, nil
}

// replayMissCause 回查一次真实行，把「条件未命中」翻译成具体原因。
func (l *AttachReplayLogic) replayMissCause(sessionID int64, target int32) error {
	fresh, err := l.svcCtx.Sessions.FindOne(l.ctx, sessionID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return model.ErrSessionNotFound
	}
	if !model.SessionStateIsTerminal(fresh.State) {
		return fmt.Errorf("%w: state=%d", model.ErrSessionNotTerminal, fresh.State)
	}
	return fmt.Errorf("%w: %d->%d", model.ErrInvalidReplayTransition, fresh.ReplayState, target)
}

// replayRefsEqual 判定三个引用列是否与现值完全一致（幂等重放的判据）。
func replayRefsEqual(session *model.LiveSession, in *rpc.AttachReplayReq) bool {
	return session.RecordID == in.GetRecordId() &&
		session.RecordAssetID == in.GetRecordAssetId() &&
		session.RecordAid == in.GetRecordAid()
}
