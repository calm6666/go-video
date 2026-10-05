package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type CloseStreamLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCloseStreamLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CloseStreamLogic {
	return &CloseStreamLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 强制停流（主播下播、运营/风控切断；IDLE/PUBLISHING/INTERRUPTED → STOPPED）
//
// 停流的三件收尾（状态迁移、密钥活跃指针、节点租约与配额）由 applyStreamTransition
// 在同一事务里做完，因此不存在「流停了但密钥还占着活跃位」的孤儿状态。
// 已是 STOPPED 时按幂等成功回放首次事件的 seq/event_id：终态本身就是重放判定锚点，
// request_id 同时写入事件的 report_id，防止同一个幂等键停掉两条不同的流。
func (l *CloseStreamLogic) CloseStream(in *rpc.CloseStreamReq) (*rpc.CloseStreamReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	streamID, err := checkStreamID(in.StreamId)
	if err != nil {
		return nil, err
	}
	requestID, err := checkRequestID(in.RequestId)
	if err != nil {
		return nil, err
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	reason, err := checkReason("reason", in.Reason, false)
	if err != nil {
		return nil, err
	}

	s, err := repo.Stream.FindOne(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, model.ErrStreamNotFound
	}
	// admin=true 由内部控制面断言（网关未接线，见 README 缺口）；非运营只能关自己的流。
	if !in.Admin && in.OperatorMid != s.AnchorMid {
		return nil, model.ErrOperatorRequired
	}

	stopReason := stopReasonFromRPC(in.StopReason)
	if stopReason == 0 {
		// 未指明原因时按「谁在关」归一，而不是留 UNSPECIFIED 进事件。
		if in.OperatorMid == s.AnchorMid {
			stopReason = model.StopReasonAnchorStop
		} else {
			stopReason = model.StopReasonAdmin
		}
	}

	var res *transitionResult
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		var err error
		res, err = applyStreamTransition(ctx, l.svcCtx, tx, transitionInput{
			streamID:   streamID,
			to:         model.StreamStateStopped,
			at:         nowUnix(),
			reportID:   requestID,
			source:     model.EventSourceAdmin,
			reason:     reason,
			stopReason: stopReason,
			traceID:    sanitizeTraceID(in.TraceId),
		})
		return err
	})
	if err != nil {
		return nil, err
	}

	fresh, err := repo.Stream.FindOne(l.ctx, streamID)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, model.ErrStreamNotFound
	}
	message := "已停流并释放密钥活跃指针与节点配额"
	if !res.applied {
		message = "流此前已停止；本次未产生新事件"
	}
	return &rpc.CloseStreamReply{
		State:                   rpcStreamState(fresh.State),
		Seq:                     res.seq,
		EventId:                 res.eventID,
		InterruptedTotalSeconds: fresh.InterruptedTotalSecs,
		Replayed:                !res.applied,
		Applied:                 res.applied,
		Message:                 message,
	}, nil
}
