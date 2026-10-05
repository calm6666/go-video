// Package provider 定义 notification 的外部通道投递适配器。
//
// 设计约束（AGENTS.md §2 禁止新增依赖、docs/service-catalog.md）：
//   - 仓库内不引入任何厂商 Push/SMS/Email SDK；通道差异全部由
//     Endpoint/Method/Headers/BodyTemplate/签名方式 等配置表达，
//     实现基于标准库 net/http + encoding/json。
//   - HTTP 客户端以接口注入，单元测试可用 fake 替换，不发起真实网络请求。
//   - 密钥只从环境变量名（*Ref）解析后注入，绝不写入配置文件或日志。
//   - 通道未配置时必须返回显式错误，禁止伪造“发送成功”。
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 通道名（与 model.ChannelName 一致，provider 不依赖 model 包）。
const (
	// ChannelPush 应用推送。
	ChannelPush = "push"
	// ChannelSMS 短信。
	ChannelSMS = "sms"
	// ChannelEmail 邮件。
	ChannelEmail = "email"
)

// 签名方式。
const (
	// SignNone 不签名。
	SignNone = "none"
	// SignBearer Authorization: Bearer <key>。
	SignBearer = "bearer"
	// SignAPIKey 自定义头携带 API Key。
	SignAPIKey = "apikey"
	// SignHMAC X-Timestamp + X-Signature = hex(HMAC-SHA256(secret, timestamp+"\n"+body))。
	SignHMAC = "hmac-sha256"
)

// 显式错误：logic/consumer 依据这些错误决定重试或终态，不得吞掉。
var (
	// ErrProviderNotConfigured 对应通道没有可用适配器（未启用或端点为空）。
	// 调用方必须把任务标记为 retry / dead_letter，绝不能视为发送成功。
	ErrProviderNotConfigured = errors.New("notification/provider: channel provider not configured")
	// ErrUnsupportedChannel 通道不在 push/sms/email 之内（本项目不支持小程序通道）。
	ErrUnsupportedChannel = errors.New("notification/provider: unsupported channel")
	// ErrInvalidConfig 适配器配置不合法（缺端点、签名密钥为空、模板占位符未知等）。
	ErrInvalidConfig = errors.New("notification/provider: invalid provider config")
	// ErrRejected 供应商明确拒绝请求（HTTP 非期望状态或业务码不匹配）。
	ErrRejected = errors.New("notification/provider: provider rejected request")
	// ErrContactNotWired 收件联系方式未接入：account.v1 契约没有“按 mid 取手机号/邮箱”的方法，
	// 本服务不落明文号码，因此只能显式失败（详见 README 缺口章节）。
	ErrContactNotWired = errors.New("notification/provider: contact source not wired (account.v1 has no contact lookup rpc)")
	// ErrKafkaRuntimeNotBuilt 当前构建产物未包含 Kafka 接线。
	ErrKafkaRuntimeNotBuilt = errors.New("notification/consumer: kafka runtime not built, rebuild with -tags notification_kafka")
)

// SendRequest 一次投递请求。只携带受控标识，不含明文手机号/邮箱。
type SendRequest struct {
	DeliveryID     string            // 投递任务 ID
	Channel        string            // 通道：push|sms|email
	TargetRef      string            // 受控投递标识（设备 token 引用/供应商收件人引用/哈希）
	Title          string            // 渲染后的标题
	Body           string            // 渲染后的正文
	Lang           string            // 语言
	TemplateCode   string            // 模板码
	IdempotencyKey string            // 供应商侧幂等键（等于行级 biz_key）
	TraceID        string            // 链路 ID
	Extra          map[string]string // 适配器可用的附加字段（如 device_id）
}

// Validate 拒绝缺少通道/幂等键的请求，避免向供应商发出不可回溯的投递。
func (r *SendRequest) Validate() error {
	if r == nil {
		return errors.New("notification/provider: nil send request")
	}
	switch r.Channel {
	case ChannelPush, ChannelSMS, ChannelEmail:
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedChannel, r.Channel)
	}
	if strings.TrimSpace(r.TargetRef) == "" {
		return fmt.Errorf("%w: target_ref is required (plaintext phone/email must not enter this service)", ErrInvalidConfig)
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		return fmt.Errorf("%w: idempotency key is required", ErrInvalidConfig)
	}
	return nil
}

// SendResult 供应商受理结果。
type SendResult struct {
	Provider      string // 适配器名
	ProviderMsgID string // 供应商回执消息 ID
	Status        int    // HTTP 状态码
	Accepted      bool   // 是否已受理
}

// Provider 单个通道的投递适配器。
type Provider interface {
	// Name 适配器名，落库到 notification_delivery.provider。
	Name() string
	// Channel 适配器负责的通道。
	Channel() string
	// Send 投递一次；返回 error 时调用方按错误类型决定退避重试或终态。
	Send(ctx context.Context, req *SendRequest) (*SendResult, error)
}

// Registry 按通道索引的适配器注册表。
type Registry struct {
	byChannel map[string]Provider
}

// NewRegistry 构造空注册表。
func NewRegistry() *Registry {
	return &Registry{byChannel: make(map[string]Provider, 3)}
}

// Register 注册一个通道适配器；同一通道重复注册返回错误（一个通道一个条目）。
func (r *Registry) Register(p Provider) error {
	if p == nil {
		return errors.New("notification/provider: nil provider")
	}
	ch := strings.ToLower(strings.TrimSpace(p.Channel()))
	switch ch {
	case ChannelPush, ChannelSMS, ChannelEmail:
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedChannel, ch)
	}
	if _, ok := r.byChannel[ch]; ok {
		return fmt.Errorf("notification/provider: channel %q already registered", ch)
	}
	r.byChannel[ch] = p
	return nil
}

// Get 按通道取适配器；未配置返回 ErrProviderNotConfigured。
func (r *Registry) Get(channel string) (Provider, error) {
	ch := strings.ToLower(strings.TrimSpace(channel))
	if ch == "" {
		return nil, ErrUnsupportedChannel
	}
	p, ok := r.byChannel[ch]
	if !ok || p == nil {
		return nil, fmt.Errorf("%w: channel=%s", ErrProviderNotConfigured, ch)
	}
	return p, nil
}

// Channels 返回已配置的通道名（升序），供健康检查与运维排查使用。
func (r *Registry) Channels() []string {
	out := make([]string, 0, len(r.byChannel))
	for ch := range r.byChannel {
		out = append(out, ch)
	}
	sort.Strings(out)
	return out
}
