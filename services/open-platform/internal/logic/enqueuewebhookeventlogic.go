package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type EnqueueWebhookEventLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewEnqueueWebhookEventLogic(ctx context.Context, svcCtx *svc.ServiceContext) *EnqueueWebhookEventLogic {
	return &EnqueueWebhookEventLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 领域事件入队（event_id 幂等）。
//
// 生产侧唯一入口（本服务没有 internal/consumer，README「已知缺口」），
// Kafka at-least-once 的重复投递必须在这里被 uniq_event_endpoint (event_id, endpoint_id) 吃掉。
// 具体校验、正文门禁与端点匹配全部复用 webhookevent.enqueueWebhookEvent——
// 本服务内部撤销授权后的主动通知走的是同一函数，两条入口的安全属性必须完全一致，
// 否则内部路径就成了绕过正文门禁的后门（webhookevent.go 文件头）。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound/ErrInvalidEventType/ErrEventIDRequired/
// errPayloadNotJSON/errPayloadCarriesCredential/errWebhookPayloadCarriesPII/
// ErrPayloadTooBig/errBizRefTooLong/ErrWindowInvalid/errEventTooOld→InvalidArgument；
// ErrSecretVerificationUnavailable→FailedPrecondition；SQL 失败→Internal。
func (l *EnqueueWebhookEventLogic) EnqueueWebhookEvent(in *rpc.EnqueueWebhookEventReq) (*rpc.EnqueueWebhookEventReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 目标范围：app_id==0 是平台级广播（按订阅该事件的全部端点投递，proto:569），
	//    非 0 时应用必须存在——enqueueWebhookEvent 明确不重复读库，这条归属由本方法负责。
	//    负数是脏参数，绝不当成「广播」。
	if in.AppId < 0 {
		return nil, model.ErrInvalidAppID
	}
	if in.AppId > 0 {
		if _, err := findApp(ctx, s, in.AppId); err != nil {
			return nil, err
		}
	}

	// 2. 事件类型：0（UNSPECIFIED）与未知值拒绝，不解释成「全部事件」（AGENTS.md §1 无商业化事件位）。
	eventType := int32(in.EventType)
	if !model.ValidWebhookEventType(eventType) {
		return nil, model.ErrInvalidEventType
	}

	// 3. 幂等键与时间：event_id 必填（服务端绝不补随机值——那等于放弃幂等），
	//    occurred_at 必须 >0（「太旧」的判断留在 enqueueWebhookEvent 里，保持一个口径）。
	eventID, err := idPart(in.EventId, maxEventIDRunes, model.ErrEventIDRequired, model.ErrEventIDRequired)
	if err != nil {
		return nil, err
	}
	if in.OccurredAt <= 0 {
		return nil, model.ErrWindowInvalid
	}

	// 4. 关联用户与业务引用：mid 可为 0（平台级事件无归属用户），但不得为负；
	//    biz_type/biz_id 只做长度与空白门禁——本表没有对应列，它们只进日志（见第 8 步），
	//    超长的自由文本塞进日志本身就是噪声放大，所以拒绝而不是截断。
	if in.Mid < 0 {
		return nil, errWebhookMidInvalid
	}
	bizType, err := optionalLen(in.BizType, maxBizRefRunes, errWebhookBizRefTooLong)
	if err != nil {
		return nil, err
	}
	bizID, err := optionalLen(in.BizId, maxBizRefRunes, errWebhookBizRefTooLong)
	if err != nil {
		return nil, err
	}

	// 5. 正文门禁（与内部通知路径同一实现）：合法 JSON、字节数 <= WebhookPayloadMaxBytes
	//    （超限拒整条而不是截断——截断后的摘要与签名都没有意义）、不含凭证字段。
	normalized, err := normalizeEventPayload(s.Config.OpenPlatform, in.Payload)
	if err != nil {
		return nil, err
	}

	// 6. 个人信息门禁：正文会被 worker 投到公网端点、并在库里留存到保留期为止，
	//    手机号/身份证明文一旦进来就等于被送出了隐私边界，必须在入队前拒掉整条。
	//    命中时按 event_type 记 Error（不带正文片段，避免把被拒绝的 PII 复制进日志）。
	if payloadCarriesPII(normalized) {
		logx.WithContext(ctx).Errorf("open-platform: 回调事件正文疑似携带个人信息，拒绝入队 event_type=%d "+
			"app_id=%d biz_type=%s event_id=%s", eventType, in.AppId, bizType, eventID)
		return nil, errWebhookPayloadCarriesPII
	}

	release, err := writePermit(ctx, s)
	if err != nil {
		return nil, err
	}
	defer release()

	// 7. 入队：每个可投递端点一行 op_webhook_delivery（state=PENDING、attempt=0、
	//    max_attempts=入队时配置快照、next_retry_at=occurred_at）。
	//    「入队与投递台账同事务」在这里是天然成立的：投递台账就是这一行本身，
	//    model 的 Insert 是单条 INSERT ... ON DUPLICATE KEY UPDATE（不接受 session，
	//    本方法也不跨表写），因此不存在「有队列无台账」的中间态。
	//    极端情况下多端点批量中途失败会留下部分行，生产方按同一 event_id 重试即可补齐：
	//    已存在的行撞唯一键回 created=false，不会双份外呼。
	res, err := enqueueWebhookEvent(ctx, s, in.AppId, eventType, eventID, normalized, in.OccurredAt)
	if err != nil {
		return nil, err
	}

	// 8. 观测：matched=0 不是错误（应用没订阅这类事件就是正常态），但它是生产方
	//    「事件被静默丢弃」的唯一线索，因此每次入队都记一条结论日志。
	logx.WithContext(ctx).Infof("open-platform: 回调事件入队 event_id=%s event_type=%d app_id=%d mid=%d "+
		"biz_type=%s biz_id=%s occurred_at=%d matched=%d deliveries=%d deduplicated=%t trace_id=%s",
		eventID, eventType, in.AppId, in.Mid, bizType, bizID, in.OccurredAt, res.matched,
		len(res.deliveryIDs), res.deduplicated, in.TraceId)

	return &rpc.EnqueueWebhookEventReply{
		DeliveryIds:      res.deliveryIDs,
		MatchedEndpoints: res.matched,
		Deduplicated:     res.deduplicated,
	}, nil
}
