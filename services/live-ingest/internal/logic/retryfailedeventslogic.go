package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type RetryFailedEventsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryFailedEventsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryFailedEventsLogic {
	return &RetryFailedEventsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 把超过重试上限的失败事件重置为待发布（运营补偿）
//
// 三个不可省的点：
//  1. retry_count 与 next_retry_at 必须一起清零（model.ResetFailed 负责），否则发布器
//     按旧重试计数立刻再判一次失败，运营看到的就是「放行后 3 秒又红」；
//  2. reason 必填。ResetFailed 会把 last_error 覆盖成 reason——那是这条投递失败
//     的唯一物证，覆盖它必须留下「谁为什么放行」；
//  3. 没有失败事件时报 ErrNoFailedEvents，而不是静默返回 retried=0：
//     运营点「重试」是为了处理告警，0 条和「告警已经不存在」必须能区分开。
//
// 幂等口径：live_ingest_outbox 没有承载 request_id 的唯一索引（README「幂等承载点」），
// 因此本方法的幂等锚点是「FAILED 状态本身」：重置只作用于当前仍为 FAILED 的行，
// 重放不会把已发布成功的事件改回去。要精确回答「同一个 request_id 是否已执行过」
// 需要给 outbox 增加 request_id 列，见 README「已知缺口」。
func (l *RetryFailedEventsLogic) RetryFailedEvents(in *rpc.RetryFailedEventsReq) (*rpc.RetryFailedEventsReply, error) {
	cfg := l.svcCtx.Config.LiveIngest
	repo := l.svcCtx.Repository
	if repo == nil {
		return nil, errNoRepository
	}
	requestID, err := checkRequestID(in.RequestId)
	if err != nil {
		return nil, err
	}
	// 人工放行事件投递是运营动作：没有归因主体的批量写不受理。
	if err := checkOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	reason, err := checkReason("reason", in.Reason, true)
	if err != nil {
		return nil, err
	}
	eventIDs, err := checkEventIDs(in.EventIds)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(in.Limit, cfg.MaxEventRetryBatch)

	var retried int64
	remaining := int64(0)
	err = repo.Conn().TransactCtx(l.ctx, func(ctx context.Context, tx sqlx.Session) error {
		affected, err := repo.Outbox.ResetFailed(ctx, tx, eventIDs, limit, reason)
		if err != nil {
			return err
		}
		retried = affected
		// 计数放在同一事务里：事务外再 COUNT 会看到别处刚重置的行，
		// 回带的 remaining_failed 就比实际值小，运营会误判「已经处理干净了」。
		cnt, err := repo.Outbox.CountByState(ctx, model.OutboxStateFailed)
		if err != nil {
			return err
		}
		remaining = cnt
		return nil
	})
	if err != nil {
		return nil, err
	}
	if retried == 0 {
		return nil, model.ErrNoFailedEvents
	}
	l.Logger.Infow("retry failed live state events",
		logx.Field("module", "live-ingest"), logx.Field("op", "retry_failed_events"),
		logx.Field("retried", retried), logx.Field("request_id", requestID),
		logx.Field("operator_mid", in.OperatorMid))

	message := "已把失败事件重置为待发布，发布器会按 outbox.id 升序重投"
	if len(eventIDs) > 0 {
		message = fmt.Sprintf("已重置指定的 %d 个事件为待发布", retried)
	}
	return &rpc.RetryFailedEventsReply{
		Retried:         toInt32Total(retried),
		RemainingFailed: toInt32Total(remaining),
		Message:         message,
	}, nil
}

// checkEventIDs 校验并归一事件 ID 列表：条数超上限直接拒绝而不是截断
// （截断会让运营以为「全部放行了」，实际只放行了前 500 个）。
func checkEventIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if int32(len(ids)) > maxStreamIDListForRetry {
		return nil, fmt.Errorf("%w: event_ids %d 条，上限 %d", model.ErrInvalidStreamId,
			len(ids), maxStreamIDListForRetry)
	}
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" || len(id) > maxIdempotencyKeyBytes || strings.ContainsAny(id, " /\t\n") {
			return nil, fmt.Errorf("%w: event_id 不是合法引用", model.ErrInvalidStreamId)
		}
		out = append(out, id)
	}
	return out, nil
}
