// retry.go 实现退避重投：把 inbox_consumer_offset 中到期的 retry 事件重新跑一遍。
//
// 为什么需要它：go-queue 的 ConsumeHandler 只有返回错误才不提交位点，
// 而 kafka-go 只在读者重启或再均衡时才重取未提交消息。只靠 Kafka 重投，
// 「失败退避」会退化成「等下一次再均衡」，进程长期运行时不会自动收敛。
// 这里以 DB 里的 retry 状态与 next_retry_at 为准主动重投，让失败一定向前推进。
package consumer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/inbox/internal/repository"
	"go-video/services/inbox/model"
)

// SweepOnce 处理一轮到期的重试事件，返回处理条数。
// 单条事件失败只影响它自己：继续下一条，错误汇总在日志与状态表里。
func (p *Processor) SweepOnce(ctx context.Context) (int, error) {
	rows, err := p.store.DueRetryEvents(ctx, p.now().Unix(), p.opts.RetryBatchLimit)
	if err != nil {
		return 0, fmt.Errorf("inbox/consumer: load due retries: %w", err)
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return len(rows), ctx.Err()
		}
		p.retryOne(ctx, row)
	}
	return len(rows), nil
}

// retryOne 重投单条到期事件并推进状态机。
func (p *Processor) retryOne(ctx context.Context, row *model.ConsumerOffset) {
	if row == nil || row.EventID == "" {
		return
	}
	env, err := ParseEnvelope([]byte(row.Payload))
	if err != nil {
		// 暂存原文已损坏或超长按约定没落库：无法再自动处理，留档后判死，交人工。
		reason := fmt.Sprintf("暂存 payload 不可重放: %v", err)
		if _, dlErr := p.store.SaveDeadLetter(ctx, p.deadLetterOf(row, reason)); dlErr != nil {
			logx.WithContext(ctx).Errorf("inbox/consumer: 登记死信失败 event_id=%s err=%v", row.EventID, dlErr)
		}
		if err := p.store.MarkEventDeadLetter(ctx, row.EventID, reason, row.Payload); err != nil {
			logx.WithContext(ctx).Errorf("inbox/consumer: 标记 dead_letter 失败 event_id=%s err=%v", row.EventID, err)
		}
		return
	}

	outcome, current, err := p.Claim(ctx, env, row.Topic, row.Payload)
	switch {
	case errors.Is(err, repository.ErrEventDeferred):
		// 别的实例已抢先领取：本轮跳过，位点与状态都由对方推进。
		return
	case err != nil:
		logx.WithContext(ctx).Errorf("inbox/consumer: 重投领取失败 event_id=%s err=%v", row.EventID, err)
		return
	case outcome == model.ClaimDuplicate:
		return
	}

	attempts := attemptOf(current)
	if err := p.finish(ctx, env, row.Topic, row.Payload, attempts, p.apply(ctx, env)); err != nil {
		logx.WithContext(ctx).Errorf("inbox/consumer: 重投仍失败 event_id=%s attempts=%d err=%v",
			row.EventID, attempts, err)
	}
}

// deadLetterOf 由状态行构造死信留档记录（payload 不可解析时没有信封字段可用）。
func (p *Processor) deadLetterOf(row *model.ConsumerOffset, reason string) *model.DeadLetter {
	return &model.DeadLetter{
		EventID:        row.EventID,
		EventType:      row.EventType,
		Topic:          row.Topic,
		PayloadDigest:  model.DigestPayload([]byte(row.Payload)),
		PayloadPreview: model.RedactPreview([]byte(row.Payload)),
		Reason:         reason,
		ConsumedAt:     p.now().Unix(),
		State:          model.DeadLetterStateOpen,
	}
}

// RunRetrySweeper 周期性推进退避队列，直到 ctx 取消。
// 由 internal/svc 在 Kafka.Enabled 时随服务启动；返回非 nil 只可能是 ctx 取消。
func (p *Processor) RunRetrySweeper(ctx context.Context) error {
	for {
		n, err := p.SweepOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			logx.WithContext(ctx).Errorf("inbox/consumer: 重试清扫失败 err=%v", err)
		}
		wait := p.opts.SweepIdleWait
		if n > 0 {
			wait = time.Second // 还有积压就尽快再来一轮，缩短追平时间
		}
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// sleepCtx 等待 d；返回 false 表示 ctx 已取消（用于优雅退出）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
