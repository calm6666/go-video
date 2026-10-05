package logic

import (
	"context"
	"fmt"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

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

// 上报流状态：按状态机 CAS 推进 + 分配 seq + 同事务写事件与 outbox（report_id 幂等）
//
// 迁移、断流区间开合、live_stream_event 与 live_ingest_outbox 全部由
// applyStreamTransition 在同一事务内完成；本方法不等 MQ，MQ 故障不影响状态推进。
func (l *ReportStreamStateLogic) ReportStreamState(in *rpc.ReportStreamStateReq) (*rpc.ReportStreamStateReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}

	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	reportID, err := checkReportID(in.ReportId)
	if err != nil {
		return nil, err
	}
	// IDLE 由接入鉴权建档产生，不接受上报；上报只允许三个后验状态。
	to := streamStateFromRPC(in.State)
	if to != model.StreamStatePublishing && to != model.StreamStateInterrupted && to != model.StreamStateStopped {
		return nil, model.ErrInvalidStreamState
	}
	reason, err := checkReason("reason", in.Reason, false)
	if err != nil {
		return nil, err
	}
	nodeID := ""
	if in.NodeId != "" {
		if nodeID, err = checkNodeID(in.NodeId); err != nil {
			return nil, err
		}
	}

	// 幂等回放：uniq_report_id 命中即首次结果已定，绝不重复占 seq。
	existing, err := repo.StreamEvent.FindByReportID(l.ctx, reportID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return &rpc.ReportStreamStateReply{
			StreamId: existing.StreamID, State: rpcStreamState(existing.ToState), Seq: existing.Seq,
			EventId: existing.EventID, RoomId: existing.RoomID, SessionId: existing.SessionID,
			Replayed: true, Applied: false, InterruptionId: existing.InterruptionID,
			Message: "相同 report_id 的状态已应用；本次未重复占用 seq",
		}, nil
	}

	now := nowUnix()
	at, err := reportedAt(in.OccurredAt, now, cfg.CallbackSkewSeconds)
	if err != nil {
		return nil, err
	}
	// 预检滞后上报：比上次迁移还早的时间戳直接判乱序，省下一次注定失败的事务。
	current, err := repo.Stream.FindOne(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, model.ErrStreamNotFound
	}
	if current.State != to && current.StateChangedAt > 0 && at < current.StateChangedAt {
		return nil, fmt.Errorf("%w: report timestamp older than last applied transition", model.ErrSeqConflict)
	}

	stopReason := int32(0)
	if to == model.StreamStateStopped {
		// 入口上报的 unpublish 语义即「主播下播」；运营切断走 CloseStream。
		stopReason = model.StopReasonAnchorStop
	}

	var res *transitionResult
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		var err error
		res, err = applyStreamTransition(ctx, l.svcCtx, tx, transitionInput{
			streamID:   streamID,
			to:         to,
			at:         at,
			expectSeq:  in.ExpectSeq,
			nodeID:     nodeID,
			reportID:   reportID,
			source:     model.EventSourceEntry,
			reason:     reason,
			stopReason: stopReason,
			traceID:    sanitizeTraceID(in.TraceId),
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	reply := &rpc.ReportStreamStateReply{
		StreamId: res.streamID, State: rpcStreamState(res.to), Seq: res.seq, EventId: res.eventID,
		RoomId: res.roomID, SessionId: res.sessionID, Applied: res.applied,
		InterruptionId: res.interruptionID,
	}
	switch {
	case res.applied:
		reply.Message = "状态已推进并产生 live.state.v1 事件"
	case res.message != "":
		reply.Message = res.message
	}
	return reply, nil
}
