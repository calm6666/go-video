// Code scaffolded by goctl. Safe to edit.

package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/event-collector/internal/svc"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryPendingDeliveryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryPendingDeliveryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryPendingDeliveryLogic {
	return &RetryPendingDeliveryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 推进到期未发送事件（cron 兜底任务与运维入口）。
//
// 幂等分两层（AGENTS.md §5）：
//   - 轮次层：idempotency_key 必填，同一轮重复提交直接回放首次统计（Redis），
//     cron 重试不会因为「又跑了一遍」把同一批事件再投一次；
//   - 行层：ec_pending_delivery 的租约（lease_owner + lease_until）才是真围栏 ——
//     Redis 不可用时两个实例可能同时进入，此时同一行只会被一个持有者推进，
//     另一个拿 applied=false 自觉退出（runPendingRound 里按 fenced 计数并记日志）。
//
// fail closed：dispatcher 未启用时在「抢租约之前」就返回 model.ErrDispatcherMissing。
// 顺序很重要 —— 先抢后判会让完好待投的行被领走却又发不出去，几轮之后被 attempts
// 上限推进死信，等于自己制造事故（AGENTS.md §9 禁止吞错）。
func (l *RetryPendingDeliveryLogic) RetryPendingDelivery(in *rpc.RetryPendingDeliveryReq) (*rpc.RetryPendingDeliveryReply, error) {
	key := strings.TrimSpace(in.GetIdempotencyKey())
	if key == "" || len(key) > maxIdemKeyBytes {
		return nil, model.ErrIdempotencyKeyRequired
	}
	operator := strings.TrimSpace(in.GetOperator())
	if operator != "" && !validOperatorName(operator) {
		return nil, fmt.Errorf("%w: operator 不能超过 %d 字节且不含空白", model.ErrOperatorRequired,
			maxOperatorBytes)
	}
	topic := strings.TrimSpace(in.GetTopic())
	if topic != "" && !validTopicFilter(topic) {
		return nil, model.ErrInvalidPage
	}
	limit, err := l.resolveLimit(in.GetLimit())
	if err != nil {
		return nil, err
	}
	now := in.GetNow()
	if now <= 0 {
		now = timeNow()
	}
	sender, err := dispatcherSender(l.svcCtx)
	if err != nil {
		return nil, err
	}

	return withRoundDedup(l.ctx, l.svcCtx, rpcRetryPendingDelivery, key, l.Logger,
		func() *rpc.RetryPendingDeliveryReply { return &rpc.RetryPendingDeliveryReply{} },
		func() (*rpc.RetryPendingDeliveryReply, error) {
			out, rerr := runPendingRound(l.ctx, l.svcCtx, sender, topic, now, limit, operator)
			if rerr != nil {
				return nil, rerr
			}
			if out.fenced > 0 {
				// 有行被别的实例接管：说明并发推进正在发生，必须看得见而不是被合并进 sent。
				l.Errorf("event-collector/logic: 本轮 %d 行租约被其他实例接管 topic=%s worker=%s",
					out.fenced, fitColumn(topic, maxTopicBytes), fitColumn(workerID(operator, now), maxLeaseOwnerBytes))
			}
			l.Infof("event-collector/logic: 投递推进轮次完成 topic=%s scanned=%d sent=%d retrying=%d dead=%d "+
				"operator=%s idempotency_key=%s", fitColumn(topic, maxTopicBytes), out.scanned, out.sent,
				out.retrying, out.dead, fitColumn(operator, maxOperatorBytes), fitColumn(key, maxIdemKeyBytes))
			return &rpc.RetryPendingDeliveryReply{
				Scanned:   out.scanned,
				Sent:      out.sent,
				Retrying:  out.retrying,
				Dead:      out.dead,
				NextRunAt: out.nextRunAt,
			}, nil
		})
}

// resolveLimit 收敛单轮扫描条数：0 用 Dispatch.BatchSize，超过 Collector.BatchLimit 直接拒。
//
// 不静默截断成上限：调用方以为「我limit=5000 全扫了」而实际只扫 200，
// 会让它对积压速度的判断完全失真。
func (l *RetryPendingDeliveryLogic) resolveLimit(req int32) (int32, error) {
	max := l.svcCtx.Config.Collector.BatchLimit
	limit := req
	if limit == 0 {
		limit = l.svcCtx.Config.Dispatch.BatchSize
	}
	if limit <= 0 {
		return 0, model.ErrInvalidPage
	}
	if limit > max {
		return 0, fmt.Errorf("%w: limit=%d 超过服务端上限 %d", model.ErrBatchLimitTooLarge, limit, max)
	}
	return limit, nil
}
