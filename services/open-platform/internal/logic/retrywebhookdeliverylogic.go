package logic

import (
	"context"
	"strings"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RetryWebhookDeliveryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRetryWebhookDeliveryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RetryWebhookDeliveryLogic {
	return &RetryWebhookDeliveryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 死信重放（运营）。
//
// 状态机口径（proto:687 + proto:620-627 + README「Webhook 重试策略」）：
//
//	PENDING/DELIVERING/RETRY_SCHEDULED → 已在自动重试链上，人工重放一律拒绝
//	SUCCESS                            → 终态，绝不复活（会产生第二次对外副作用，README:73）
//	DEAD                               → 只有 ignore_dead=true 才允许重放（proto:624）
//	IGNORED                            → 端点删除/应用停用抑制出来的终态，可重放，
//	                                      但必须先过「端点仍可投递」与「应用仍生效」两道前置条件
//
// 本方法不发 HTTP：发送只属于投递 worker。重放做的事就是把行重置成 PENDING，
// worker 用 Claim 的 CAS 抢占，因此不存在「重放与 worker 双写」；
// attempt 归零后由 model.NextRetryAt(now, WebhookRetryBaseSeconds, WebhookRetryMaxSeconds, attempt)
// 重新走完整退避，最坏再到 WebhookMaxAttempts 次上限入死信（max_attempts 是入队时快照，不改写）。
//
// 错误映射：ErrDeliveryNotFound/ErrWebhookNotFound/errReasonRequired/errReasonTooLong/
// ErrInvalidAppID→InvalidArgument 或 NotFound（按哨兵原有口径）；ErrOperatorRequired→PermissionDenied；
// ErrDeliveryNotRetryable/ErrWebhookUnverified/errWebhookEndpointDisabled/
// ErrApplicationNotActive→FailedPrecondition；ErrPayloadUnavailable→FailedPrecondition；SQL 失败→Internal。
func (l *RetryWebhookDeliveryLogic) RetryWebhookDelivery(in *rpc.RetryWebhookDeliveryReq) (*rpc.RetryWebhookDeliveryReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 参数与身份：重放是运营处置动作，不允许应用侧自助（proto:623「必填 > 0」）；
	//    reason 必填——ResetForReplay 对空 reason 直接回 ErrDeliveryNotRetryable，
	//    这里提前用 errReasonRequired 给出同一语义的 InvalidArgument，让问责链落到具体的人。
	if in.DeliveryId <= 0 {
		return nil, model.ErrDeliveryNotFound
	}
	if err := requireOperator(in.OperatorMid); err != nil {
		return nil, err
	}
	if err := requireReason(in.Reason); err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)

	// 2. 台账真值：不存在与「不该出现在这张表里」同一个 NotFound。
	delivery, err := s.WebhookDeliveries.FindByID(ctx, in.DeliveryId)
	if err != nil {
		return nil, err
	}
	if delivery == nil {
		return nil, model.ErrDeliveryNotFound
	}

	// 3. 状态门禁（先于端点与正文检查：状态不对时连「为什么要重放」都不必查）。
	//    ManuallyReplayable() 只承认 DEAD/IGNORED，与 ResetForReplay 的
	//    WHERE state IN (DEAD, IGNORED) 完全同集合，因此 CAS 永不与这里的判定漂移。
	if !delivery.ManuallyReplayable() {
		logx.WithContext(ctx).Infof("open-platform: 拒绝重放 delivery_id=%d state=%d attempt=%d "+
			"max_attempts=%d（非人工可重放状态）", delivery.DeliveryID, delivery.State,
			delivery.Attempt, delivery.MaxAttempts)
		return nil, model.ErrDeliveryNotRetryable
	}
	// ignore_dead 不是「绕过状态校验」的开关，而是重放死信前的显式确认位（proto:624）：
	// 缺省 false 时对 DEAD 记录拒绝，避免误点一次就把一批已放弃的外呼重新打出去。
	if delivery.State == model.DeliveryStateDead && !in.IgnoreDead {
		return nil, model.ErrDeliveryNotRetryable
	}

	// 4. 应用必须仍然生效：应用被停用/下线时 UpdateApplication 会把它的在途任务抑制成 IGNORED
	//    （updateapplication.revokeAppCredentials），此时端点行本身仍是「已验证且启用」的。
	//    少了这道门，重放就会把事件推给一个因风控而下线的主体。
	app, err := findApp(ctx, s, delivery.AppID)
	if err != nil {
		return nil, err
	}
	if err := requireActiveApp(app); err != nil {
		return nil, err
	}

	// 5. 端点前置条件：向已删除或从未验证的地址再打一枪，等于把本方法变成 SSRF 复放器。
	endpoint, err := s.WebhookEndpoints.FindByID(ctx, delivery.EndpointID)
	if err != nil {
		return nil, err
	}
	if endpoint == nil || endpoint.DeletedAt > 0 || endpoint.AppID != delivery.AppID {
		// 归属不一致（脏数据）与「不存在」同口径回 NotFound，不额外暴露可探测的差异。
		return nil, model.ErrWebhookNotFound
	}
	if endpoint.VerifiedAt == 0 {
		return nil, model.ErrWebhookUnverified
	}
	if endpoint.Enabled != 1 {
		return nil, errWebhookEndpointDisabled
	}

	// 6. 正文可用性：payload 为空说明已过 WebhookPayloadRetentionDays 被清理。
	//    此时绝不能放行——worker 会把签名算在空串上，应用侧必然验签失败，
	//    真实原因（正文已清理、需上游重新生产事件）反而被掩盖。
	if delivery.Payload == "" {
		logx.WithContext(ctx).Errorf("open-platform: 拒绝重放 delivery_id=%d event_id=%s：正文已按保留期清理，"+
			"需上游按同一 event_id 重新生产事件", delivery.DeliveryID, delivery.EventID)
		return nil, model.ErrPayloadUnavailable
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// 7. 重放：唯一键 uniq_event_endpoint 保证「同一事件 + 端点」不会因重放多出第二行，
	//    条件 state IN (DEAD, IGNORED) 保证并发下的第二次调用影响 0 行（replayed=false 但仍成功）。
	//    本 RPC 一次只重放一条，批量重放归 cron 分批，避免重放风暴打成二次事故。
	now := nowUnix()
	applied, err := s.WebhookDeliveries.ResetForReplay(ctx, delivery.DeliveryID, in.OperatorMid, reason, now)
	if err != nil {
		return nil, err
	}

	state := int32(model.DeliveryStatePending)
	nextRetryAt := now
	if !applied {
		// 判定与重置之间状态被别人改过（例如 worker 刚回收、或另一次重放已生效）：
		// 回库里此刻的真值而不是回我们「希望它变成」的值。
		fresh, err := s.WebhookDeliveries.FindByID(ctx, delivery.DeliveryID)
		if err != nil {
			return nil, err
		}
		if fresh == nil {
			return nil, model.ErrDeliveryNotFound
		}
		state, nextRetryAt = fresh.State, fresh.NextRetryAt
	}

	// 8. 审计：人工重放必须可追溯「谁在什么时候以什么理由把哪条死信放回队列」。
	logx.WithContext(ctx).Infof("open-platform: 回调重放 delivery_id=%d app_id=%d endpoint_id=%d host=%s "+
		"event_id=%s event_type=%d from_state=%d attempt_reset=%t to_state=%d next_retry_at=%d "+
		"max_attempts=%d operator_mid=%d reason=%q trace_id=%s",
		delivery.DeliveryID, delivery.AppID, delivery.EndpointID, webhookURLHostForLog(endpoint.URL),
		delivery.EventID, delivery.EventType, delivery.State, applied, state, nextRetryAt,
		delivery.MaxAttempts, in.OperatorMid, reason, in.TraceId)

	return &rpc.RetryWebhookDeliveryReply{
		DeliveryId:  delivery.DeliveryID,
		State:       deliveryStateToRPC(state),
		NextRetryAt: nextRetryAt,
		Replayed:    applied,
	}, nil
}
