package model

import (
	"errors"
	"strings"
)

// open-platform 域哨兵错误。
// 这些错误会被 gateway 映射成响应信封 code，因此文案必须可安全外发：
// 绝不包含 secret、token、授权码、哈希、salt 或 SQL 片段。
var (
	// ErrNotImplemented 契约轮占位：本轮只落契约、model 与迁移，生成的 logic 骨架
	// 一律返回该错误，禁止伪造成功（AGENTS.md §9）。逻辑轮逐个方法删除。
	ErrNotImplemented = errors.New("open-platform: not implemented in contract round")

	// ErrInvalidAppID app_id 非法。
	ErrInvalidAppID = errors.New("open-platform: invalid app_id")
	// ErrAppKeyRequired app_key 缺失。
	ErrAppKeyRequired = errors.New("open-platform: app_key required")
	// ErrAppNotFound 应用不存在。
	ErrAppNotFound = errors.New("open-platform: application not found")
	// ErrDuplicateAppName 同名应用已存在（同一 owner 下应用名唯一）。
	ErrDuplicateAppName = errors.New("open-platform: duplicate application name for this owner")
	// ErrClientTokenRequired 注册幂等键缺失。
	ErrClientTokenRequired = errors.New("open-platform: client_token required (<=64 chars)")
	// ErrIdempotencyKeyRequired 写接口缺少幂等键。
	ErrIdempotencyKeyRequired = errors.New("open-platform: idempotency_key required (<=128 chars)")
	// ErrRequestIDRequired 网关授权检查缺少 request_id（配额幂等锚点）。
	ErrRequestIDRequired = errors.New("open-platform: request_id required")
	// ErrOperatorRequired 运营身份缺失。
	ErrOperatorRequired = errors.New("open-platform: operator_mid required")
	// ErrOwnerRequired 调用者不是应用归属者且不是运营。
	ErrOwnerRequired = errors.New("open-platform: application owner or operator required")
	// ErrInvalidRedirectURI 回调地址非法（必须 https、无 fragment、非内网地址）。
	ErrInvalidRedirectURI = errors.New("open-platform: invalid redirect_uri")
	// ErrTooManyRedirectURIs 回调白名单超出上限。
	ErrTooManyRedirectURIs = errors.New("open-platform: too many redirect_uri")
	// ErrTooManyScopes 一次请求携带的 scope 条数超出服务端上限（拒绝无界批量授予/申请）。
	ErrTooManyScopes = errors.New("open-platform: too many scopes")
	// ErrInvalidWebhookURL 回调端点地址非法（https only，禁止内网/本机地址，防 SSRF）。
	ErrInvalidWebhookURL = errors.New("open-platform: invalid webhook url")
	// ErrConcurrentUpdate 乐观锁失败（version 不匹配），调用方应重读后重试。
	ErrConcurrentUpdate = errors.New("open-platform: concurrent state update")
	// ErrInvalidStateTransition 应用状态机非法迁移。
	ErrInvalidStateTransition = errors.New("open-platform: invalid state transition")
	// ErrApplicationNotActive 应用处于待审/停用/驳回/下线状态。
	ErrApplicationNotActive = errors.New("open-platform: application is not active")
	// ErrSecretNotConfigured 应用没有生效密钥（签名调用不可用）。
	ErrSecretNotConfigured = errors.New("open-platform: client secret not configured")
	// ErrSecretVerificationUnavailable 缺少 HMAC pepper（Secret/Vault 未注入），
	// 不允许退化成“无 pepper 比较”或直接放行。
	ErrSecretVerificationUnavailable = errors.New("open-platform: secret verification key missing")
	// ErrScopeUnknown scope 不在目录中。
	ErrScopeUnknown = errors.New("open-platform: unknown scope")
	// ErrScopeDisabled scope 已停用，不可授予。
	ErrScopeDisabled = errors.New("open-platform: scope disabled")
	// ErrScopeNotGranted 请求的 scope 超出应用获批范围（最小权限边界）。
	ErrScopeNotGranted = errors.New("open-platform: scope not granted for this application")
	// ErrScopeWriteRequiresConsent 写 scope 必须有用户显式同意记录。
	ErrScopeWriteRequiresConsent = errors.New("open-platform: write scope requires explicit user consent")
	// ErrConsentRequired 用户未确认授权（consent_given=false）。
	ErrConsentRequired = errors.New("open-platform: user consent required")
	// ErrInvalidGrantType 授权方式不支持（本期只开放 authorization_code / refresh_token）。
	ErrInvalidGrantType = errors.New("open-platform: unsupported grant_type")
	// ErrAuthCodeInvalid 授权码无效或不属于该应用。
	ErrAuthCodeInvalid = errors.New("open-platform: invalid authorization code")
	// ErrAuthCodeExpired 授权码已过期。
	ErrAuthCodeExpired = errors.New("open-platform: authorization code expired")
	// ErrAuthCodeUsed 授权码已被消费（重放）。
	ErrAuthCodeUsed = errors.New("open-platform: authorization code already used")
	// ErrRedirectURIMismatch 换码时的 redirect_uri 与签发时不一致。
	ErrRedirectURIMismatch = errors.New("open-platform: redirect_uri mismatch")
	// ErrTokenInvalid token 无效。
	ErrTokenInvalid = errors.New("open-platform: invalid token")
	// ErrTokenExpired token 过期。
	ErrTokenExpired = errors.New("open-platform: token expired")
	// ErrTokenRevoked token 被撤销。
	ErrTokenRevoked = errors.New("open-platform: token revoked")
	// ErrGrantRevoked 授权关系已撤销（撤销位点晚于 token 签发时间）。
	ErrGrantRevoked = errors.New("open-platform: grant revoked")
	// ErrRefreshReused refresh token 重放：调用方必须撤销整条 grant（保守失效）。
	ErrRefreshReused = errors.New("open-platform: refresh token reuse detected")
	// ErrScopeNarrowingDenied 刷新时请求扩大 scope（只允许收窄）。
	ErrScopeNarrowingDenied = errors.New("open-platform: refresh may only narrow scopes")
	// ErrSignatureInvalid 应用签名校验失败。
	ErrSignatureInvalid = errors.New("open-platform: invalid signature")
	// ErrSignatureExpired 签名时间戳超出允许偏差。
	ErrSignatureExpired = errors.New("open-platform: signature timestamp out of window")
	// ErrNonceReused 随机串重放（nonce 在窗口内只能使用一次）。
	ErrNonceReused = errors.New("open-platform: nonce replayed")
	// ErrQuotaPolicyNotFound 未找到匹配的配额规则。
	ErrQuotaPolicyNotFound = errors.New("open-platform: quota policy not found")
	// ErrQuotaExceeded 超出配额（携带 retry_after 语义）。
	ErrQuotaExceeded = errors.New("open-platform: quota exceeded")
	// ErrQuotaLimitInvalid limit 非法（<=0 表示禁用该接口）。
	ErrQuotaLimitInvalid = errors.New("open-platform: invalid quota limit")
	// ErrWindowInvalid 时间窗非法。
	ErrWindowInvalid = errors.New("open-platform: invalid quota window")
	// ErrInvalidCursor 游标无法解析。
	ErrInvalidCursor = errors.New("open-platform: invalid cursor")
	// ErrInvalidPage 分页参数非法。
	ErrInvalidPage = errors.New("open-platform: invalid page size")
	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("open-platform: ps exceeds server limit")
	// ErrWebhookNotFound 回调端点不存在或不属于该应用。
	ErrWebhookNotFound = errors.New("open-platform: webhook endpoint not found")
	// ErrWebhookUnverified 端点未通过验证，不投递。
	ErrWebhookUnverified = errors.New("open-platform: webhook endpoint not verified")
	// ErrWebhookAlreadyRegistered 同一 (应用, 事件, 地址) 已注册。
	ErrWebhookAlreadyRegistered = errors.New("open-platform: webhook endpoint already registered")
	// ErrDeliveryNotFound 投递记录不存在。
	ErrDeliveryNotFound = errors.New("open-platform: webhook delivery not found")
	// ErrDeliveryNotRetryable 投递状态不允许重放（只有 dead/ignored 可人工重放）。
	ErrDeliveryNotRetryable = errors.New("open-platform: webhook delivery not retryable")
	// ErrEventIDRequired Webhook 入队缺少幂等键 event_id。
	ErrEventIDRequired = errors.New("open-platform: event_id required")
	// ErrInvalidEventType 事件类型非法（不存在商业化事件位）。
	ErrInvalidEventType = errors.New("open-platform: invalid webhook event type")
	// ErrPayloadTooBig 事件 payload 超过上限。
	ErrPayloadTooBig = errors.New("open-platform: webhook payload too large")
	// ErrForbiddenScopeCategory scope/api_code 命中未开放类目（支付/广告/会员等），
	// 服务端直接拒绝注册与授予，保证 §1 商业化红线不被绕过。
	ErrForbiddenScopeCategory = errors.New("open-platform: capability category not open")
	// ErrRateLimited 进程级写令牌桶无余量（保护 MySQL），调用方应按 retry-after 退避重试。
	ErrRateLimited = errors.New("open-platform: write rate limited, retry later")
	// ErrSignatureModeUnavailable 签名模式（app_key + client_secret 验签）当前不可用：
	// client_secret 只存 HMAC(pepper, salt||明文) 单向哈希，而冻结契约要求
	// signature = HMAC(明文 secret, canonical)——服务端无法从哈希还原明文密钥去重算 HMAC，
	// 冻结的 DDL 里也没有可存放可解密密钥或公钥的列。
	// 因此本路径在参数阶段即拒绝（不消耗 nonce、不扣配额、不写流水），
	// 绝不退化成「拿不到密钥就当签名有效」。详见 README「已知缺口」。
	ErrSignatureModeUnavailable = errors.New("open-platform: client-secret signature mode unavailable")
	// ErrPayloadUnavailable Webhook 投递正文已按保留期清空，无法人工重放
	// （隐私最小化的必然代价：重放需上游按同一 event_id 重新生产事件）。
	ErrPayloadUnavailable = errors.New("open-platform: webhook payload already purged")
)

// JoinScopes 把 scope 列表规范成入库字符串（去重、升序、逗号分隔）。
// 排序与去重在这里定死，保证 granted_scope 快照可比对、可索引、无顺序噪声。
func JoinScopes(scopes []string) string {
	if len(scopes) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(scopes))
	list := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		list = append(list, s)
	}
	sortStrings(list)
	return strings.Join(list, ",")
}

// SplitScopes 解析入库的 scope 字符串。
// 去空白、丢空元素；不排序不去重（入库串由 JoinScopes 规范化，解析只负责还原）。
// 无可解析内容时返回 nil：与 raw=="" 的分支同口径，调用方只需判 len(...) == 0，
// 不必再区分 nil 与空切片，也不会把 "" 当成一个合法 scope 往下传。
func SplitScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ContainsScope 判断 granted 是否包含 want。
func ContainsScope(granted []string, want string) bool {
	for _, g := range granted {
		if g == want {
			return true
		}
	}
	return false
}

// ScopesSubset 判断 want 是否全部落在 allowed 内（scope 收窄校验）。
func ScopesSubset(want, allowed []string) bool {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		set[a] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

// AlignWindow 把时间戳对齐到窗口起点（配额计数与重算共用同一取齐规则）。
// windowSeconds<=0 时返回 0，由调用方报错，避免默默除零。
func AlignWindow(ts, windowSeconds int64) int64 {
	if windowSeconds <= 0 {
		return 0
	}
	return ts - ts%windowSeconds
}

// sortStrings 升序排序（插入排序即可：scope 数量在十位量级，避免为此引入 sort 包）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
