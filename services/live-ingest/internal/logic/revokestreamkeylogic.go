package logic

import (
	"context"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RevokeStreamKeyLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRevokeStreamKeyLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevokeStreamKeyLogic {
	return &RevokeStreamKeyLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 吊销密钥：立即失效，可按需级联停止进行中的流
//
// 重放判定来自「密钥终态」而不是 request_id：REVOKED 不可逆，所以同一 request_id
// 重试第二次一定命中「已吊销」分支。级联停流可以涉及多条流，而 report_id 是唯一索引，
// 把吊销键塞进 report_id 会让第二条流停不掉（README「幂等承载点」）。
func (l *RevokeStreamKeyLogic) RevokeStreamKey(in *rpc.RevokeStreamKeyReq) (*rpc.RevokeStreamKeyReply, error) {
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	if in.KeyId <= 0 {
		return nil, model.ErrInvalidKeyId
	}
	if _, err := checkRequestID(in.RequestId); err != nil {
		return nil, err
	}
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	reason, err := checkReason("reason", in.Reason, true)
	if err != nil {
		return nil, err
	}

	k, err := repo.StreamKey.FindOne(l.ctx, in.KeyId)
	if err != nil {
		return nil, err
	}
	if k == nil {
		return nil, model.ErrStreamKeyNotFound
	}
	// admin=true 由内部控制面断言（网关未接线，见 README 缺口）；非运营只能吊销自己的密钥。
	if !in.Admin && in.OperatorMid != k.AnchorMid {
		return nil, model.ErrOperatorRequired
	}

	stopped := make([]string, 0, max(1, int(k.MaxStreams)))
	var alreadyRevoked bool
	var cascadeLimit int32 = revokeCascadeLimit(k.MaxStreams)

	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		locked, err := repo.StreamKey.LockByID(ctx, tx, in.KeyId)
		if err != nil {
			return err
		}
		if locked == nil {
			return model.ErrStreamKeyNotFound
		}
		if locked.State == model.KeyStateRevoked {
			alreadyRevoked = true
			return nil
		}
		// 先抢吊销位再停流：中途失败会整体回滚，不会出现「流停了但密钥还能用」。
		ok, err := repo.StreamKey.TransitionState(ctx, tx, in.KeyId, model.KeyStateRevoked, reason,
			model.KeyStateActive, model.KeyStateRotating, model.KeyStateExpired)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrConcurrentUpdate
		}
		if !in.StopStream {
			return nil
		}
		streams, err := repo.Stream.ListActiveByKey(ctx, in.KeyId, cascadeLimit)
		if err != nil {
			return err
		}
		for _, s := range streams {
			res, err := applyStreamTransition(ctx, l.svcCtx, tx, transitionInput{
				streamID:   s.StreamID,
				to:         model.StreamStateStopped,
				at:         nowUnix(),
				source:     model.EventSourceAdmin,
				reason:     reason,
				stopReason: model.StopReasonRevoked,
				traceID:    sanitizeTraceID(in.TraceId),
			})
			if err != nil {
				return err
			}
			if res.applied || res.to == model.StreamStateStopped {
				stopped = append(stopped, s.StreamID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	message := "密钥已吊销，后续接入鉴权一律拒绝"
	if alreadyRevoked {
		message = "密钥此前已吊销；吊销不可逆，本次未产生新变更"
	} else if !in.StopStream {
		message = "密钥已吊销；进行中的流未停止，重连将被拒绝"
	}
	return &rpc.RevokeStreamKeyReply{
		State:            rpc.StreamKeyState_STREAM_KEY_STATE_REVOKED,
		StoppedStreamIds: stopped,
		Replayed:         alreadyRevoked,
		Message:          message,
	}, nil
}

// revokeCascadeLimit 级联停流的扫描上限：取密钥自身的并发流配额，
// 至少 1 条，避免 max_streams 为 0 的历史数据导致「一次都停不掉」。
func revokeCascadeLimit(maxStreams int32) int32 {
	if maxStreams <= 0 {
		return 1
	}
	if maxStreams > keyCascadeMax {
		return keyCascadeMax
	}
	return maxStreams
}
