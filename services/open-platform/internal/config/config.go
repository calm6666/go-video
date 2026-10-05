// Code scaffolded by goctl. Safe to edit.

package config

import (
	"errors"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 open-platform 服务的配置结构。
//
// 本服务只提供 gRPC（AGENTS.md §3/§4）：对外开放的 HTTP 入口、签名编排与响应信封
// 都在 gateway，本服务负责授权判定、凭证生命周期、配额与回调投递。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载 token 校验短缓存、nonce 防重放集合与配额扣减的热计数回落层。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_open_platform 库的 MySQL DSN。
	DataSource string

	// OpenPlatform 是开放平台领域参数（凭证 TTL、配额、Webhook 退避等）。
	OpenPlatform OpenPlatformConf

	// Security 是密钥材料参数：只从 Secret/Vault 注入，示例配置必须留空。
	Security SecurityConf
}

// SecurityConf 凭证哈希与签名参数。
//
// 三个字段都是「服务端秘密」，禁止入库、禁止进仓库、禁止出现在日志与事件里。
// 未注入时 svc 不阻断启动，但所有验签/哈希路径都会返回
// model.ErrSecretVerificationUnavailable，绝不退化成「无 pepper 比较」或直接放行。
type SecurityConf struct {
	// CredentialPepper 是 client_secret / 授权码 / token 哈希的 HMAC-SHA256 pepper：
	// hash = HMAC(pepper, salt || 明文)。库泄露时攻击者也无法离线还原或伪造。
	CredentialPepper string `json:",optional"`
	// WebhookMasterPepper 是回调签名密钥的派生根：
	// signKey = HMAC(webhookMasterPepper, app_id || key_version)，
	// 因此 op_webhook_endpoint 只存版本号、不存任何密钥材料。
	WebhookMasterPepper string `json:",optional"`
	// KeyVersion 当前签名/哈希密钥版本，写入响应与投递签名串，支持双版本并行验签。
	KeyVersion int32 `json:",default=1"`
	// SignatureSkewSeconds 允许的请求时间戳偏差（秒），超出返回 model.ErrSignatureExpired。
	SignatureSkewSeconds int64 `json:",default=300"`
	// NonceTTLSeconds nonce 在 Redis 中的存活秒数，必须 >= 2 * SignatureSkewSeconds，
	// 否则窗口内可重放（Validate 会拦住这种配错）。
	NonceTTLSeconds int64 `json:",default=600"`
}

// OpenPlatformConf 开放平台业务参数，全部来自 etc yaml 或配置中心。
type OpenPlatformConf struct {
	// AuthCodeTTLSeconds 授权码有效期（秒）。短期是防截获的核心手段，默认 60s。
	AuthCodeTTLSeconds int64 `json:",default=60"`
	// AccessTokenTTLSeconds access token 有效期（秒）。
	// 撤销依赖 op_grant.revoked_at 位点 + 逐条标记双保险，因此本值可以不必很短。
	AccessTokenTTLSeconds int64 `json:",default=3600"`
	// RefreshTokenTTLSeconds refresh token 有效期（秒），默认 30 天。
	RefreshTokenTTLSeconds int64 `json:",default=2592000"`
	// SecretValidDays 新签发 client_secret 的有效期（天），0 表示长期有效（靠轮换/吊销）。
	SecretValidDays int `json:",default=0"`
	// RotationGraceSeconds 轮换时旧密钥的默认宽限秒数；请求显式传 grace 时以请求为准。
	RotationGraceSeconds int64 `json:",default=3600"`
	// PageSize 请求未指定 ps 时的默认每页条数。
	PageSize int32 `json:",default=20"`
	// MaxPageSize 服务端允许的每页上限，超过返回 model.ErrPsTooLarge。
	MaxPageSize int32 `json:",default=50"`
	// MaxRedirectURIs 单应用回调白名单条数上限（防把白名单当开放重定向池）。
	MaxRedirectURIs int32 `json:",default=5"`
	// MaxRedirectURIBytes 单条回调地址字节上限。
	MaxRedirectURIBytes int `json:",default=512"`
	// MaxWebhookEndpointsPerApp 单应用回调端点数上限。
	MaxWebhookEndpointsPerApp int32 `json:",default=20"`
	// WebhookMaxAttempts 单次投递最大尝试次数，超过置为死信等待人工重放。
	WebhookMaxAttempts int32 `json:",default=6"`
	// WebhookRetryBaseSeconds 退避基数：第 n 次失败后等待 base * 2^(n-1) 秒
	// （model.NextRetryAt 实现，上限 WebhookRetryMaxSeconds）。
	WebhookRetryBaseSeconds int64 `json:",default=30"`
	// WebhookRetryMaxSeconds 退避上限（秒），默认 1 小时。
	WebhookRetryMaxSeconds int64 `json:",default=3600"`
	// WebhookLeaseSeconds 投递 worker 租约秒数：崩溃后任务最多被占用这么久即可被回收。
	WebhookLeaseSeconds int64 `json:",default=60"`
	// WebhookHTTPTimeoutMS 单次回调请求超时（毫秒），避免慢端点拖垮投递协程池。
	WebhookHTTPTimeoutMS int64 `json:",default=5000"`
	// WebhookPayloadMaxBytes 事件正文上限，超过直接拒绝入队（ErrPayloadTooBig）。
	WebhookPayloadMaxBytes int `json:",default=32768"`
	// WebhookPayloadRetentionDays 投递任务正文保留天数：到期清空 payload 列，
	// 只保留摘要与投递结论（隐私最小化；结论仍在，重放需上游重新生产事件）。
	WebhookPayloadRetentionDays int `json:",default=30"`
	// TokenRetentionDays 已失效 token 哈希的保留天数，到期物理清理（审计走 grant/log）。
	TokenRetentionDays int `json:",default=90"`
	// AuthCodeMaxPerUserPerHour 单用户每小时可签发的授权码上限（防批量刷码）。
	AuthCodeMaxPerUserPerHour int32 `json:",default=30"`
	// QuotaRecomputeLookbackSeconds 重算默认回溯窗口（秒），cron 按此区间对齐重算。
	QuotaRecomputeLookbackSeconds int64 `json:",default=7200"`
	// IntrospectCacheSeconds token 校验结果在 Redis 中的缓存秒数。
	// 上限被撤销生效时间约束：必须 <= AccessTokenTTLSeconds，且撤销位点比对仍走 DB，
	// 因此缓存只会延后「拒绝」的传播，不会延后「允许」。0 表示关闭缓存。
	// 与其余 TTL 字段（AuthCode/AccessToken/RefreshToken/RotationGrace/Quota 等）
	// 统一为 int64：_seconds 语义在本服务一律用 int64 承载，避免与 int64 TTL 比较时
	// 出现跨类型运算，也保证后续参与时间戳/时长算术时不需要到处转换。
	IntrospectCacheSeconds int64 `json:",default=5"`
	// PostQps 进程级写接口 QPS（复用 common/ratelimit 令牌桶，保护 MySQL）。
	PostQps int32 `json:",default=400"`
	// PostBurst 进程级令牌桶突发容量。
	PostBurst int32 `json:",default=100"`
}

// Validate 启动期自检：把「配置能加载但语义危险」的组合在启动时暴露。
// svc.NewServiceContext 调用它，返回 error 即终止启动。
func (c Config) Validate() error {
	op := c.OpenPlatform
	if op.AuthCodeTTLSeconds <= 0 || op.AuthCodeTTLSeconds > 600 {
		return errors.New("OpenPlatform.AuthCodeTTLSeconds 必须在 (0,600] 秒：授权码必须是短期凭证")
	}
	if op.AccessTokenTTLSeconds <= 0 {
		return errors.New("OpenPlatform.AccessTokenTTLSeconds 必须 > 0")
	}
	if op.RefreshTokenTTLSeconds <= op.AccessTokenTTLSeconds {
		return errors.New("OpenPlatform.RefreshTokenTTLSeconds 必须大于 AccessTokenTTLSeconds，否则 refresh 无意义")
	}
	if op.IntrospectCacheSeconds > op.AccessTokenTTLSeconds {
		return errors.New("OpenPlatform.IntrospectCacheSeconds 不能大于 AccessTokenTTLSeconds（缓存只延后拒绝，不得延后过期）")
	}
	if op.MaxPageSize < op.PageSize {
		return errors.New("OpenPlatform.MaxPageSize 不能小于 PageSize")
	}
	if op.WebhookMaxAttempts <= 0 || op.WebhookRetryBaseSeconds <= 0 || op.WebhookRetryMaxSeconds < op.WebhookRetryBaseSeconds {
		return errors.New("OpenPlatform 的 Webhook 重试参数不合法：MaxAttempts>0 且 RetryMaxSeconds>=RetryBaseSeconds>0")
	}
	if op.WebhookLeaseSeconds <= 0 {
		return errors.New("OpenPlatform.WebhookLeaseSeconds 必须 > 0，否则投递中状态无法回收")
	}
	if op.WebhookPayloadRetentionDays <= 0 || op.TokenRetentionDays <= 0 {
		return errors.New("凭证与事件正文必须有保留上限（WebhookPayloadRetentionDays/TokenRetentionDays 必须 > 0）")
	}
	if c.Security.KeyVersion <= 0 {
		return errors.New("Security.KeyVersion 必须 > 0（哈希与签名都要可追溯密钥版本）")
	}
	if c.Security.SignatureSkewSeconds <= 0 {
		return errors.New("Security.SignatureSkewSeconds 必须 > 0")
	}
	if c.Security.NonceTTLSeconds < 2*c.Security.SignatureSkewSeconds {
		return errors.New("Security.NonceTTLSeconds 必须 >= 2*SignatureSkewSeconds，否则时间窗内可重放")
	}
	return nil
}

// SecurityConfigured 判断密钥材料是否已注入；未注入时 svc 不阻断启动，
// 但验签与哈希路径一律返回 model.ErrSecretVerificationUnavailable。
func (c Config) SecurityConfigured() bool {
	return c.Security.CredentialPepper != "" && c.Security.WebhookMasterPepper != ""
}
