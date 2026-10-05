package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/common/eventenvelope"
	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// notification.request.v1 的 payload 契约。
//
// docs/api-and-events.md §5 规定该事件由各领域服务生产、notification 消费；
// 共享事件 schema 的目标位置是顶层 api/events/（该目录尚未创建，见 README 缺口），
// 因此本文件是本 payload 的权威定义，字段与 rpc 契约一一对应。
// 隐私：payload 不得包含明文手机号/邮箱/证件号；接收人只写 mid 或受控 target_ref。

// EventTypeNotificationRequest 是本服务订阅的事件类型（topic 由 eventenvelope.Topic 派生）。
const EventTypeNotificationRequest = "notification.request"

// SchemaVersionV1 当前支持的 schema 版本。
const SchemaVersionV1 = 1

// RequestPayload 是 notification.request.v1 的 payload。
type RequestPayload struct {
	Channel        string             `json:"channel"`         // push|sms|email
	TemplateCode   string             `json:"template_code"`   // 模板码
	TemplateParams map[string]string  `json:"template_params"` // 模板变量（禁止明文敏感值）
	Recipients     []PayloadRecipient `json:"recipients"`      // 接收人
	BizKey         string             `json:"biz_key"`         // 请求级业务幂等键（必填）
	IdempotencyKey string             `json:"idempotency_key"` // 可选：调用方幂等键
	Priority       string             `json:"priority"`        // low|normal|high，或数字 1/2/3
	ExpireAt       string             `json:"expire_at"`       // RFC3339 或 Unix 秒，空表示不过期
	Lang           string             `json:"lang"`            // 请求级默认语言（接收人未指定时使用）
}

// PayloadRecipient 事件里的接收人。
type PayloadRecipient struct {
	Mid       int64  `json:"mid"`
	TargetRef string `json:"target_ref"`
	Lang      string `json:"lang"`
	DeviceID  string `json:"device_id"`
}

// ParseRequestEnvelope 解析并校验事件信封 + payload，返回可直接调用 SendNotification 的请求。
// 任何格式错误都返回错误，由 consumer 决定重投或死信，绝不静默丢弃。
func ParseRequestEnvelope(raw []byte) (*eventenvelope.Envelope, *rpc.SendNotificationReq, error) {
	var env eventenvelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("notification/policy: decode event envelope: %w", err)
	}
	if env.EventType != EventTypeNotificationRequest {
		return &env, nil, fmt.Errorf("notification/policy: unexpected event_type %q", env.EventType)
	}
	if env.SchemaVersion != SchemaVersionV1 {
		return &env, nil, fmt.Errorf("notification/policy: unsupported schema_version %d", env.SchemaVersion)
	}
	var p RequestPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return &env, nil, fmt.Errorf("notification/policy: decode payload: %w", err)
	}
	req, err := p.ToSendRequest(env.TraceID, env.EventID)
	if err != nil {
		return &env, nil, err
	}
	return &env, req, nil
}

// ToSendRequest 把 payload 转成 RPC 请求；eventID 写入投递任务的来源事件。
func (p *RequestPayload) ToSendRequest(traceID, eventID string) (*rpc.SendNotificationReq, error) {
	if p == nil {
		return nil, fmt.Errorf("notification/policy: %w", ErrEmptyPayload)
	}
	channel := model.ChannelCodeOf(p.Channel)
	if !model.IsValidChannel(channel) {
		return nil, fmt.Errorf("notification/policy: invalid channel %q in payload", p.Channel)
	}
	if strings.TrimSpace(p.TemplateCode) == "" {
		return nil, fmt.Errorf("notification/policy: template_code is required in payload")
	}
	if len(p.Recipients) == 0 {
		return nil, fmt.Errorf("notification/policy: %w", ErrEmptyPayload)
	}
	priority, err := parsePriority(p.Priority)
	if err != nil {
		return nil, err
	}
	expireAt, err := parseExpireAt(p.ExpireAt)
	if err != nil {
		return nil, err
	}
	defaultLang, err := normalizeLang(p.Lang)
	if err != nil {
		return nil, err
	}
	req := &rpc.SendNotificationReq{
		Channel:         rpc.Channel(channel),
		TemplateCode:    strings.TrimSpace(p.TemplateCode),
		TemplateParams:  p.TemplateParams,
		BizKey:          strings.TrimSpace(p.BizKey),
		IdempotencyKey:  strings.TrimSpace(p.IdempotencyKey),
		Priority:        priority,
		ExpireAt:        expireAt,
		TraceId:         traceID,
		Recipients:      make([]*rpc.Recipient, 0, len(p.Recipients)),
		DefaultLanguage: defaultLang,
	}
	if req.BizKey == "" && eventID != "" {
		// 生产方忘记带 biz_key 时退化为按 event_id 幂等，保证重复投递不会产生第二次发送。
		req.BizKey = "event:" + eventID
	}
	for _, r := range p.Recipients {
		lang, err := normalizeLang(r.Lang)
		if err != nil {
			return nil, err
		}
		if r.Mid < 0 {
			return nil, fmt.Errorf("notification/policy: recipient mid must be >= 0")
		}
		req.Recipients = append(req.Recipients, &rpc.Recipient{
			Mid:       r.Mid,
			TargetRef: strings.TrimSpace(r.TargetRef),
			Language:  lang,
			DeviceId:  strings.TrimSpace(r.DeviceID),
		})
	}
	return req, nil
}

// ErrEmptyPayload payload 为空或没有接收人。
var ErrEmptyPayload = fmt.Errorf("empty notification payload")

// parsePriority 支持字符串与数字两种写法，未知值显式报错。
func parsePriority(v string) (rpc.Priority, error) {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return rpc.Priority_PRIORITY_NORMAL, nil
	}
	switch s {
	case "low":
		return rpc.Priority_PRIORITY_LOW, nil
	case "normal":
		return rpc.Priority_PRIORITY_NORMAL, nil
	case "high":
		return rpc.Priority_PRIORITY_HIGH, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		switch rpc.Priority(n) {
		case rpc.Priority_PRIORITY_LOW, rpc.Priority_PRIORITY_NORMAL, rpc.Priority_PRIORITY_HIGH:
			return rpc.Priority(n), nil
		}
	}
	return rpc.Priority_PRIORITY_UNSPECIFIED, fmt.Errorf("notification/policy: invalid priority %q", v)
}

// parseExpireAt 支持 RFC3339 与 Unix 秒两种写法。
func parseExpireAt(v string) (int64, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("notification/policy: expire_at must be >= 0")
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("notification/policy: invalid expire_at %q (RFC3339 or Unix seconds)", v)
}

// normalizeLang 把语言写法归一成 rpc.Language 枚举。
func normalizeLang(v string) (rpc.Language, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return rpc.Language_LANGUAGE_UNSPECIFIED, nil
	}
	lang, ok := LangCodeToEnum(s)
	if !ok {
		return rpc.Language_LANGUAGE_UNSPECIFIED, fmt.Errorf("notification/policy: unsupported lang %q", v)
	}
	return lang, nil
}

// LangCodeToEnum 把 "zh-CN"/"zh-TW"/"en" 转成枚举。
func LangCodeToEnum(v string) (rpc.Language, bool) {
	switch v {
	case model.LangZhCN:
		return rpc.Language_LANGUAGE_ZH_CN, true
	case model.LangZhTW:
		return rpc.Language_LANGUAGE_ZH_TW, true
	case model.LangEn:
		return rpc.Language_LANGUAGE_EN, true
	default:
		return rpc.Language_LANGUAGE_UNSPECIFIED, false
	}
}

// LangCodeOfEnum 把枚举转成语言编码；未指定返回空串。
func LangCodeOfEnum(l rpc.Language) string {
	switch l {
	case rpc.Language_LANGUAGE_ZH_CN:
		return model.LangZhCN
	case rpc.Language_LANGUAGE_ZH_TW:
		return model.LangZhTW
	case rpc.Language_LANGUAGE_EN:
		return model.LangEn
	default:
		return ""
	}
}

// ChannelOfEnum 校验通道枚举并返回字符串名。
func ChannelOfEnum(c rpc.Channel) (string, error) {
	name := model.ChannelName(int32(c))
	if name == "" {
		return "", fmt.Errorf("notification/policy: %w channel=%d", model.ErrInvalidChannel, int32(c))
	}
	return name, nil
}

// 事件来源传递：consumer 处理事件时把 event_id 放进 context，
// SendNotification 落库时写入 notification_delivery.source_event_id，
// 这样 RPC 契约不需要暴露内部的事件溯源字段。
type eventIDKeyType struct{}

var eventIDKey = eventIDKeyType{}

// WithEventID 在 context 中携带来源事件 ID。
func WithEventID(ctx context.Context, eventID string) context.Context {
	if eventID == "" {
		return ctx
	}
	return context.WithValue(ctx, eventIDKey, eventID)
}

// EventIDFromCtx 读取来源事件 ID（无则空串）。
func EventIDFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(eventIDKey).(string); ok {
		return v
	}
	return ""
}
