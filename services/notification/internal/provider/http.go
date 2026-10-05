package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// HTTPClient 是最小的 HTTP 客户端接口，便于单测注入 fake（禁止真实外呼）。
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Options 通用 HTTP 投递适配器的构造参数。
// 密钥字段（APIKeyValue/SecretValue）由调用方从环境变量解析后传入，配置里只留变量名。
type Options struct {
	Name           string            // 适配器名
	Channel        string            // 通道
	Endpoint       string            // 投递地址
	Method         string            // HTTP 方法，默认 POST
	Timeout        time.Duration     // 单次请求超时
	MaxRetries     int               // 仅针对传输层错误的进程内即时重试次数
	RetryDelay     time.Duration     // 即时重试间隔
	Headers        map[string]string // 固定请求头
	BodyTemplate   string            // JSON 请求体模板
	SignMode       string            // none|bearer|apikey|hmac-sha256
	APIKeyValue    string            // 已解析的 API Key
	SecretValue    string            // 已解析的签名密钥
	APIKeyHeader   string            // apikey 模式的头名，默认 X-Api-Key
	SuccessField   string            // 响应业务码字段（点号路径），空表示只看 HTTP 状态
	SuccessValue   string            // 响应业务码期望值
	MessageIDField string            // 供应商回执 ID 字段（点号路径）
	NowFunc        func() time.Time  // 时间注入点（签名时间戳），nil 时使用 time.Now
}

// 默认请求体模板：占位符必须整体被双引号包裹，替换时按 JSON 字符串编码，
// 因此模板参数无法注入额外字段或改变请求结构。
const defaultBodyTemplate = `{"to":"{{target}}","title":"{{title}}","body":"{{body}}","key":"{{idempotency_key}}","delivery_id":"{{delivery_id}}","trace_id":"{{trace_id}}"}`

// 允许出现在模板里的占位符。
const (
	phTarget          = "target"
	phTitle           = "title"
	phBody            = "body"
	phIdempotencyKey  = "idempotency_key"
	phDeliveryID      = "delivery_id"
	phTraceID         = "trace_id"
	phDeviceID        = "device_id"
	phTemplateCode    = "template_code"
	phLang            = "lang"
	maxResponseBytes  = 64 << 10
	defaultTimeoutDur = 3 * time.Second
)

// HTTPProvider 是配置驱动的通用 HTTP 通道适配器。
type HTTPProvider struct {
	opt Options
	// client 注入的 HTTP 客户端；生产为 *http.Client。
	client HTTPClient
	// successStatus 期望的 HTTP 状态码集合；为空表示接受全部 2xx。
	successStatus map[int]bool
}

// NewHTTP 校验配置并构造适配器。
// 端点为空或通道非法时返回 ErrInvalidConfig，调用方应跳过注册，
// 使该通道后续投递显式返回 ErrProviderNotConfigured。
func NewHTTP(opt Options, client HTTPClient) (*HTTPProvider, error) {
	ch := strings.ToLower(strings.TrimSpace(opt.Channel))
	switch ch {
	case ChannelPush, ChannelSMS, ChannelEmail:
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedChannel, ch)
	}
	if strings.TrimSpace(opt.Endpoint) == "" {
		return nil, fmt.Errorf("%w: channel=%s endpoint is empty", ErrInvalidConfig, ch)
	}
	if !strings.HasPrefix(opt.Endpoint, "http://") && !strings.HasPrefix(opt.Endpoint, "https://") {
		return nil, fmt.Errorf("%w: channel=%s endpoint must be http(s)", ErrInvalidConfig, ch)
	}
	if opt.Name == "" {
		opt.Name = "generic-http-" + ch
	}
	if opt.Method == "" {
		opt.Method = http.MethodPost
	}
	if opt.Timeout <= 0 {
		opt.Timeout = defaultTimeoutDur
	}
	if opt.MaxRetries < 0 {
		opt.MaxRetries = 0
	}
	if opt.RetryDelay < 0 {
		opt.RetryDelay = 0
	}
	if opt.BodyTemplate == "" {
		opt.BodyTemplate = defaultBodyTemplate
	}
	if opt.SignMode == "" {
		opt.SignMode = SignNone
	}
	switch opt.SignMode {
	case SignNone:
	case SignBearer, SignAPIKey:
		if opt.APIKeyValue == "" {
			return nil, fmt.Errorf("%w: channel=%s signMode=%s requires api key from env", ErrInvalidConfig, ch, opt.SignMode)
		}
		if opt.SignMode == SignAPIKey && opt.APIKeyHeader == "" {
			opt.APIKeyHeader = "X-Api-Key"
		}
	case SignHMAC:
		if opt.SecretValue == "" {
			return nil, fmt.Errorf("%w: channel=%s signMode=%s requires secret from env", ErrInvalidConfig, ch, opt.SignMode)
		}
	default:
		return nil, fmt.Errorf("%w: channel=%s unknown signMode %q", ErrInvalidConfig, ch, opt.SignMode)
	}
	if opt.NowFunc == nil {
		opt.NowFunc = time.Now
	}
	if client == nil {
		client = &http.Client{Timeout: opt.Timeout}
	}
	return &HTTPProvider{opt: opt, client: client}, nil
}

// Name 实现 Provider。
func (p *HTTPProvider) Name() string { return p.opt.Name }

// Channel 实现 Provider。
func (p *HTTPProvider) Channel() string { return p.opt.Channel }

// renderBody 用 JSON 编码后的值替换模板占位符。
// 未被替换的 {{...}} 视为配置错误：宁可显式失败，也不向供应商发送半成品请求。
func (p *HTTPProvider) renderBody(req *SendRequest) ([]byte, error) {
	values := map[string]string{
		phTarget:         req.TargetRef,
		phTitle:          req.Title,
		phBody:           req.Body,
		phIdempotencyKey: req.IdempotencyKey,
		phDeliveryID:     req.DeliveryID,
		phTraceID:        req.TraceID,
		phTemplateCode:   req.TemplateCode,
		phLang:           req.Lang,
		phDeviceID:       req.Extra["device_id"],
	}
	body := p.opt.BodyTemplate
	for _, name := range []string{phTarget, phTitle, phBody, phIdempotencyKey, phDeliveryID, phTraceID, phTemplateCode, phLang, phDeviceID} {
		encoded, err := json.Marshal(values[name])
		if err != nil {
			return nil, fmt.Errorf("%w: encode placeholder %s: %v", ErrInvalidConfig, name, err)
		}
		body = strings.ReplaceAll(body, `"{{`+name+`}}"`, string(encoded))
	}
	if idx := strings.Index(body, "{{"); idx >= 0 {
		return nil, fmt.Errorf("%w: unknown placeholder in body template near %q", ErrInvalidConfig, body[idx:min(idx+32, len(body))])
	}
	if !json.Valid([]byte(body)) {
		return nil, fmt.Errorf("%w: body template does not produce valid JSON", ErrInvalidConfig)
	}
	return []byte(body), nil
}

// sign 按配置附加签名头。
func (p *HTTPProvider) sign(httpReq *http.Request, body []byte) {
	switch p.opt.SignMode {
	case SignBearer:
		httpReq.Header.Set("Authorization", "Bearer "+p.opt.APIKeyValue)
	case SignAPIKey:
		httpReq.Header.Set(p.opt.APIKeyHeader, p.opt.APIKeyValue)
	case SignHMAC:
		ts := strconv.FormatInt(p.opt.NowFunc().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(p.opt.SecretValue))
		mac.Write([]byte(ts))
		mac.Write([]byte("\n"))
		mac.Write(body)
		httpReq.Header.Set("X-Timestamp", ts)
		httpReq.Header.Set("X-Signature", hex.EncodeToString(mac.Sum(nil)))
	case SignNone:
	default:
	}
}

// Send 实现 Provider。
// 传输层错误按 MaxRetries 做进程内即时重试；被供应商拒绝（非期望状态/业务码不符）
// 只返回错误不做即时重试，交由投递调度器按 notification_delivery.next_retry_at 做持久退避。
func (p *HTTPProvider) Send(ctx context.Context, req *SendRequest) (*SendResult, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body, err := p.renderBody(req)
	if err != nil {
		return nil, err
	}

	var lastErr error
	attempts := p.opt.MaxRetries + 1
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(p.opt.RetryDelay):
			}
		}
		res, err := p.doOnce(ctx, body)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryableTransport(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// doOnce 发起一次 HTTP 投递并解析回执。
func (p *HTTPProvider) doOnce(ctx context.Context, body []byte) (*SendResult, error) {
	callCtx, cancel := context.WithTimeout(ctx, p.opt.Timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(callCtx, p.opt.Method, p.opt.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrInvalidConfig, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for k, v := range p.opt.Headers {
		httpReq.Header.Set(k, v)
	}
	p.sign(httpReq, body)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		// 传输层错误：包装成可即时重试的错误（不含密钥，只含错误文案）。
		return nil, transportError{err: fmt.Errorf("notification/provider: http transport failure: %w", err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if readErr != nil {
		return nil, transportError{err: fmt.Errorf("notification/provider: read response: %w", readErr)}
	}
	if !p.statusAccepted(resp.StatusCode) {
		return nil, fmt.Errorf("%w: status=%d body=%s", ErrRejected, resp.StatusCode, sanitizeSnippet(raw))
	}

	result := &SendResult{Provider: p.opt.Name, Status: resp.StatusCode, Accepted: true}
	if p.opt.SuccessField == "" && p.opt.MessageIDField == "" {
		return result, nil
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("%w: response is not a JSON object: %v", ErrRejected, sanitizeSnippet(raw))
	}
	if p.opt.SuccessField != "" {
		got, ok := lookupJSONPath(payload, p.opt.SuccessField)
		if !ok || !strings.EqualFold(got, p.opt.SuccessValue) {
			return nil, fmt.Errorf("%w: field %s=%q want %q", ErrRejected, p.opt.SuccessField, got, p.opt.SuccessValue)
		}
	}
	if p.opt.MessageIDField != "" {
		if msgID, ok := lookupJSONPath(payload, p.opt.MessageIDField); ok {
			result.ProviderMsgID = truncateStr(msgID, 128)
		}
	}
	return result, nil
}

// statusAccepted 判断 HTTP 状态码是否表示受理。
func (p *HTTPProvider) statusAccepted(code int) bool {
	if len(p.successStatus) > 0 {
		return p.successStatus[code]
	}
	return code >= 200 && code < 300
}

// WithSuccessStatus 覆盖受理状态码集合（配置为逗号分隔列表时使用）。
func (p *HTTPProvider) WithSuccessStatus(codes []int) {
	if len(codes) == 0 {
		return
	}
	m := make(map[int]bool, len(codes))
	for _, c := range codes {
		m[c] = true
	}
	p.successStatus = m
}

// transportError 标记“可即时重试”的传输层错误。
type transportError struct{ err error }

func (e transportError) Error() string { return e.err.Error() }
func (e transportError) Unwrap() error { return e.err }

// isRetryableTransport 只对传输层错误做进程内即时重试。
func isRetryableTransport(err error) bool {
	var te transportError
	return errors.As(err, &te)
}

// lookupJSONPath 按点号路径读取 JSON 对象字段，返回字符串形式。
func lookupJSONPath(payload map[string]any, path string) (string, bool) {
	var cur any = payload
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	default:
		bs, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(bs), true
	}
}

// secretFieldRe 用于打码响应片段里可能出现的凭证字段，避免密钥进入 last_error/日志。
var secretFieldRe = regexp.MustCompile(`(?i)("(?:authorization|access_token|token|api[_-]?key|secret|password|signature)"\s*:\s*")[^"]*(")`)

// sanitizeSnippet 截断响应片段并把凭证字段打码，供写 last_error 使用。
func sanitizeSnippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return secretFieldRe.ReplaceAllString(s, "$1***$2")
}

// truncateStr 按字节截断字符串。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
