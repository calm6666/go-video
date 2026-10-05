// 本文件是 logic 包的手写扩展（CDN 回调的入参归一、域名白名单匹配与事件名到
// 建议状态的映射），不是 goctl 生成产物。
//
// 放这里的判断标准：不碰 SQL、不碰密钥值本身、可被单测穷举。
// 签名比对与留证写入在 VerifyCdnCallback（要读 Vault 引用、要落库），不在此处。
//
// 隐私纪律：本文件的每个函数都只接受/返回「域名、事件名、流标识、摘要」，
// 没有一个会返回签名原文或厂商密钥；日志侧也只允许使用它们的判定结论。
package logic

import (
	"fmt"
	"strings"

	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
)

// 回调相关列宽（与 deploy/migrations/live-ingest/000003_create_live_stream_event_tables.sql 对齐）。
const (
	// maxCallbackDomainBytes 对应 live_cdn_callback.domain VARCHAR(191)。
	maxCallbackDomainBytes = 191
	// maxCallbackEventBytes 对应 live_cdn_callback.event_type VARCHAR(32)。
	maxCallbackEventBytes = 32
	// maxCallbackNonceBytes 对应 live_cdn_callback.nonce VARCHAR(64)。
	maxCallbackNonceBytes = 64
	// sha256HexLen 是 SHA-256 hex 摘要长度（signature_hash / client_ip_hash / raw_params_digest）。
	sha256HexLen = 64
	// maxCallbackIPBytes 是来源 IP 的入参长度上限（只用于算哈希，绝不入库明文）。
	maxCallbackIPBytes = 64
)

// checkCallbackNonce 校验回调 nonce：它是防重放的唯一锚点，缺失就等于
// 「这条回调无法被判定是否重放」，必须拒绝而不是当普通请求处理。
func checkCallbackNonce(nonce string) (string, error) {
	trimmed := strings.TrimSpace(nonce)
	if trimmed == "" {
		return "", fmt.Errorf("%w: 回调缺少 nonce", model.ErrIdempotencyKeyRequired)
	}
	if len(trimmed) > maxCallbackNonceBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: nonce 不是合法随机串", model.ErrIdempotencyKeyRequired)
	}
	return trimmed, nil
}

// checkCallbackDomain 归一回调域名：小写比较（DNS 大小写不敏感），
// 但入库保留原始 trim 后的小写值，避免同一域名以两种写法各留一份证据。
func checkCallbackDomain(domain string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(domain))
	if trimmed == "" {
		return "", fmt.Errorf("%w: 回调缺少域名", model.ErrCallbackDomainUnbound)
	}
	if len(trimmed) > maxCallbackDomainBytes || strings.ContainsAny(trimmed, " /\t\n@:") {
		return "", fmt.Errorf("%w: 回调域名不是合法主机名", model.ErrCallbackDomainUnbound)
	}
	return trimmed, nil
}

// checkCallbackEventType 归一厂商事件名：判定 `suggest_state` 与签名都依赖它，
// 空值或超长（>32，落库必失败）直接拒绝。
func checkCallbackEventType(eventType string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(eventType))
	if trimmed == "" {
		return "", fmt.Errorf("%w: 回调缺少 event_type", model.ErrCallbackSignatureMismatch)
	}
	if len(trimmed) > maxCallbackEventBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: event_type 超长或含空白", model.ErrCallbackSignatureMismatch)
	}
	return trimmed, nil
}

// isHexDigest 判断是否为 SHA-256 hex 摘要（长度 64、只含十六进制字符）。
// raw_params_digest 由调用方直接传入，本服务不解析其内容，但也不能让任意长文本入库。
func isHexDigest(s string) bool {
	if len(s) != sha256HexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// checkCallbackClientIP 校验来源 IP 的入参长度（只为算 SHA-256 留证，明文一次都不入库）。
func checkCallbackClientIP(ip string) (string, error) {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return "", nil
	}
	if len(trimmed) > maxCallbackIPBytes || strings.ContainsAny(trimmed, " /\t\n") {
		return "", fmt.Errorf("%w: client_ip 长度异常", model.ErrCallbackSignatureMismatch)
	}
	return trimmed, nil
}

// callbackDomainBound 判断回调域名是否在 Cdn.PublishDomains 白名单内。
//
// 白名单为空时返回 false：没有配置就不接受任何回调，而不是「谁都能打」。
// 比较用小写全等（不做后缀匹配）：按后缀匹配会让 attacker 注册
// live.example.com.evil.tld 这种域名直接通过。
func callbackDomainBound(svcCtx *svc.ServiceContext, domain string) bool {
	if svcCtx == nil {
		return false
	}
	for _, d := range svcCtx.Config.Cdn.PublishDomains {
		if strings.ToLower(strings.TrimSpace(d)) == domain {
			return true
		}
	}
	return false
}

// suggestStateFromCallbackEvent 把厂商事件名映射为「建议的流状态」。
//
// 只产出建议：本服务从不在回调路径里就地改状态（AGENTS.md §8），
// 推进必须由入口带 report_id 调 ReportStreamState，走合法迁移矩阵。
// 返回 0 表示不认识这个事件名——此时调用方拿不到建议，而不是被塞一个 PUBLISHING。
//
// 判定顺序有意的：unpublish / publish_done 里都含 "publish"，
// 先判终止类事件，否则「下播」会被判成「开播」，把已停的流又点起来。
func suggestStateFromCallbackEvent(eventType string) int32 {
	name := strings.ToLower(strings.TrimSpace(eventType))
	if name == "" {
		return 0
	}
	switch {
	// 终止类：先判，避免被下面的 "publish" 前缀误命中。
	case containsAny(name, "publish_done", "unpublish", "publish_end", "stream_end", "end_stream", "offline", "stop"):
		return model.StreamStateStopped
	// 中断类：厂商侧的「断流/暂停」只表示要进宽限期，不表示会话结束。
	case containsAny(name, "interrupt", "pause", "break", "disconnect", "freeze"):
		return model.StreamStateInterrupted
	// 开播类。
	case containsAny(name, "publish", "push", "stream_start", "start", "online", "resume"):
		return model.StreamStatePublishing
	default:
		return 0
	}
}

// containsAny 判断 s 是否含 needles 中任意一个（事件名匹配用，不区分大小写由调用方保证）。
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// callbackResultName 给日志用的可读判定名（数字不进日志，避免聚合基数爆炸）。
func callbackResultName(result int32) string {
	switch result {
	case model.CallbackResultPassed:
		return "passed"
	case model.CallbackResultBadSignature:
		return "bad_signature"
	case model.CallbackResultTimestampSkew:
		return "timestamp_skew"
	case model.CallbackResultReplayed:
		return "replayed"
	case model.CallbackResultDomainUnbound:
		return "domain_unbound"
	case model.CallbackResultStreamNotFound:
		return "stream_not_found"
	default:
		return "pending"
	}
}
