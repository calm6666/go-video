package logic

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"go-video/services/open-platform/internal/config"
	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// Webhook 事件入队的唯一实现。
//
// 两个调用方共用它：EnqueueWebhookEvent RPC（领域事件生产方），以及本服务内部
// 在撤销授权 / 应用下线后主动通知订阅方。共用理由是「正文门禁」与「未验证不投递」
// 这两条安全属性不能只对其中一个入口成立，否则内部路径就成了绕过 payload 扫描的后门。
//
// 签名密钥的口径（README「回调签名」）：
//
//	signKey = HMAC-SHA256(Security.WebhookMasterPepper, app_id || key_version)
//
// 密钥既不入库也不下发，投递记录与任何查询响应都不含它；本文件只在入队前确认
// 派生根存在（没有根就不要制造一批注定验签失败的任务）。

// webhookLookbackSeconds 事件最多可补投的时间跨度：更旧的事件直接拒绝入队。
// 上游重放历史（例如 Kafka 从头消费）会一次性产生成千上万条任务，
// 打爆应用端点属于事故而不是功能，因此这里给出硬上界。
const webhookLookbackSeconds = 86400

// enqueueResult 入队结论。
type enqueueResult struct {
	// deliveryIDs 本次命中的投递任务 ID（含幂等重放回来的既有 ID）。
	deliveryIDs []int64
	// matched 匹配到的可投递端点数（未验证与已删除端点不计入）。
	matched int32
	// deduplicated 至少有一条任务是 (event_id, endpoint_id) 已存在而回来的。
	deduplicated bool
}

// webhookPepper 取回调签名派生根；缺失即 fail closed。
func webhookPepper(s *svc.ServiceContext) (string, error) {
	if s == nil || !s.Config.SecurityConfigured() {
		return "", model.ErrSecretVerificationUnavailable
	}
	return s.Config.Security.WebhookMasterPepper, nil
}

// enqueueWebhookEvent 校验事件并投递入队。
//
// appID=0 表示平台级广播（按订阅该事件的全部端点投递）；此时不做应用状态门禁，
// 因为平台级事件不属于任何单个应用。appID>0 时应用必须存在（调用方已校验，本函数不重复读库）。
func enqueueWebhookEvent(ctx context.Context, s *svc.ServiceContext, appID int64, eventType int32,
	eventID, payload string, occurredAt int64) (*enqueueResult, error) {
	if !model.ValidWebhookEventType(eventType) {
		return nil, model.ErrInvalidEventType
	}
	eid, err := idPart(eventID, maxEventIDRunes, model.ErrEventIDRequired, model.ErrEventIDRequired)
	if err != nil {
		return nil, err
	}
	cfg := s.Config.OpenPlatform
	normalized, err := normalizeEventPayload(cfg, payload)
	if err != nil {
		return nil, err
	}
	now := nowUnix()
	if occurredAt <= 0 {
		return nil, model.ErrWindowInvalid
	}
	if occurredAt < now-webhookLookbackSeconds {
		return nil, errEventTooOld
	}
	if _, err := webhookPepper(s); err != nil {
		return nil, err
	}

	endpoints, err := s.WebhookEndpoints.ListMatching(ctx, appID, eventType)
	if err != nil {
		return nil, err
	}
	res := &enqueueResult{matched: int32(len(endpoints))}
	if len(endpoints) == 0 {
		// 应用没订阅这类事件是正常态，不是错误：返回空 delivery_ids 让生产方自行决定。
		return res, nil
	}
	digest := model.DigestPayload(normalized)
	maxAttempts := cfg.WebhookMaxAttempts
	if maxAttempts <= 0 {
		return nil, model.ErrInvalidStateTransition
	}
	for _, ep := range endpoints {
		if ep == nil || !ep.Deliverable() {
			// ListMatching 已按 verified_at/enabled/deleted_at 过滤；这里再判一次，
			// 保证将来有人改写 SQL 时「未验证永不投递」仍然成立。
			continue
		}
		id, created, err := s.WebhookDeliveries.Insert(ctx, &model.WebhookDelivery{
			AppID:         ep.AppID,
			EndpointID:    ep.EndpointID,
			EventType:     eventType,
			EventID:       eid,
			Payload:       normalized,
			PayloadDigest: digest,
			State:         model.DeliveryStatePending,
			MaxAttempts:   maxAttempts,
			NextRetryAt:   occurredAt,
		})
		if err != nil {
			return nil, err
		}
		if !created {
			res.deduplicated = true
		}
		res.deliveryIDs = append(res.deliveryIDs, id)
	}
	res.matched = int32(len(res.deliveryIDs))
	return res, nil
}

// normalizeEventPayload 事件正文门禁：必须是非空合法 JSON、不超配置上限、
// 且不夹带凭证字段。返回规范化（去除首尾空白）后的正文用于入库与摘要。
func normalizeEventPayload(cfg config.OpenPlatformConf, payload string) (string, error) {
	trimmed := strings.TrimSpace(payload)
	if trimmed == "" {
		return "", errPayloadNotJSON
	}
	if !json.Valid([]byte(trimmed)) {
		return "", errPayloadNotJSON
	}
	max := cfg.WebhookPayloadMaxBytes
	if max <= 0 {
		max = 32768
	}
	if len(trimmed) > max {
		return "", model.ErrPayloadTooBig
	}
	if payloadHasCredentialKey(trimmed) {
		return "", errPayloadCarriesCredential
	}
	return trimmed, nil
}

// notifyGrantRevoked 撤销授权后通知订阅方（尽力而为）。
//
// 返回值只用于日志：这里失败绝不能反向把已提交的撤销事务改成失败——撤销必须成功，
// 通知失败由投递侧的死信与人工重放兜住。event_id 带 grant_id 后缀，重复撤销撞同一唯一键，
// 因此「重放撤销 RPC」不会产生第二条通知。
func notifyGrantRevoked(ctx context.Context, s *svc.ServiceContext, appID, grantID, mid int64,
	reason string) {
	payload := `{"grant_id":` + strconv.FormatInt(grantID, 10) +
		`,"app_id":` + strconv.FormatInt(appID, 10) +
		`,"mid":` + strconv.FormatInt(mid, 10) +
		`,"reason":"` + jsonEscape(clipRunes(reason, maxReasonRunes)) + `"}`
	res, err := enqueueWebhookEvent(ctx, s, appID, model.WebhookEventGrantRevoked,
		"grant-revoked:"+strconv.FormatInt(grantID, 10), payload, nowUnix())
	if err != nil {
		logx.WithContext(ctx).Errorf("open-platform: 撤销通知入队失败 grant_id=%d app_id=%d: %v",
			grantID, appID, err)
		return
	}
	logx.WithContext(ctx).Infof("open-platform: 撤销通知已入队 grant_id=%d matched=%d dedup=%t",
		grantID, res.matched, res.deduplicated)
}

// jsonEscape 把审计文案转义进 JSON 字符串字面量。
// 手写而不是引入 encoding/json 的 Marshal，是为了不把整个投影结构再编码一遍（省一次分配），
// 同时保证控制字符与引号不会破坏正文——正文非法会被 normalizeEventPayload 拒掉。
func jsonEscape(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 8)
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u`)
				const hex = "0123456789abcdef"
				b.WriteByte(hex[(r>>12)&0xf])
				b.WriteByte(hex[(r>>8)&0xf])
				b.WriteByte(hex[(r>>4)&0xf])
				b.WriteByte(hex[r&0xf])
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
