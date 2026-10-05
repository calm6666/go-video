// Code scaffolded by goctl. Safe to edit.

package config

import (
	"errors"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 private-message 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 与响应信封由 gateway 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载未读计数加速、发送频控窗口与门禁判定短缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_private_message 库的 MySQL DSN。
	DataSource string

	// SocialGraphRPC 提供黑名单/关注关系真值。
	// 反骚扰门禁不能默认放行：未配置时涉及关系的判定返回
	// model.ErrSocialGraphNotConfigured，而不是「允许发送」（见 svc 注释）。
	SocialGraphRPC zrpc.RpcClientConf `json:",optional"`

	// RiskControlRPC 提供名单与频控判定真值。
	// 未配置时返回 model.ErrRiskControlNotConfigured，不伪造「已风控通过」。
	RiskControlRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC 是 moderation-orchestrator 的 zrpc client 配置。
	// 未配置时 MachineReviewEnabled=true 的发送路径返回 model.ErrModerationNotConfigured。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// PrivateMessage 是私信领域参数。
	PrivateMessage PrivateMessageConf

	// Cipher 是正文加密与内容指纹参数（密钥材料只从 Secret/Vault 注入，示例配置必须留空）。
	Cipher CipherConf
}

// PrivateMessageConf 私信业务参数，全部来自 etc yaml 或配置中心，不写死在代码里。
type PrivateMessageConf struct {
	// PageSize 请求未指定 ps 时的默认每页条数。
	PageSize int32 `json:",default=20"`
	// MaxPageSize 服务端允许的每页上限，超过返回 model.ErrPsTooLarge。
	MaxPageSize int32 `json:",default=50"`
	// MaxTextLength 文本正文 rune 数上限（按 rune 计，避免多字节绕过）。
	MaxTextLength int32 `json:",default=2000"`
	// PreviewRunes 会话列表摘要的截断长度；摘要必须脱敏，事件与日志只允许带摘要。
	PreviewRunes int32 `json:",default=30"`
	// DefaultAllowFrom 用户未显式设置偏好时的接收范围（1 所有人、2 仅关注、3 仅互关、4 关闭）。
	// 站点级默认取「仅关注」，比「所有人」更保守（陌生人骚扰门禁的兜底）。
	DefaultAllowFrom int32 `json:",default=2,options=1|2|3|4"`
	// RejectStrangerByDefault 未设置偏好的用户是否拒收非互关陌生人首条消息。
	RejectStrangerByDefault bool `json:",default=true"`
	// KeywordFilterEnabled 是否启用发送侧关键词过滤（关闭仅限压测/回放环境）。
	KeywordFilterEnabled bool `json:",default=true"`
	// MachineReviewEnabled 是否强制机审门禁：true 时命中规则的私信落库为待审核，
	// 只有 ApplyModerationVerdict 通过后才对接收方可见（AGENTS.md §8）。
	MachineReviewEnabled bool `json:",default=true"`
	// MaxPerUserPerMinute 单用户每分钟发送上限（本地限流，真值判定仍以 risk-control 为准）。
	MaxPerUserPerMinute int32 `json:",default=20"`
	// MaxStrangerConversationsPerDay 单用户每日可新建的陌生人会话上限（防批量骚扰）。
	MaxStrangerConversationsPerDay int32 `json:",default=10"`
	// WithdrawWindowSeconds 发送者自助撤回时间窗（秒），超过返回 model.ErrWithdrawWindowClosed。
	// 审核/运营撤回不受本窗口限制，但仍必须写 pm_withdraw_log 留证。
	WithdrawWindowSeconds int64 `json:",default=120"`
	// MessageRetentionDays 正文密文留存天数：到期由 PurgeExpiredMessages 清空
	// content_cipher 并置 content_purged=1，保留行以维持 seq 连续与撤回/审核审计链。
	// 取 0 表示不清理（只允许开发环境使用，生产必须 >0，见 Validate）。
	MessageRetentionDays int `json:",default=180"`
	// PurgeBatchSize 单次清理的最大行数，超过 model.ErrBatchLimitTooLarge。
	PurgeBatchSize int32 `json:",default=500"`
	// PostQps 进程级写接口 QPS（复用 common/ratelimit 令牌桶，保护 MySQL）。
	PostQps int32 `json:",default=400"`
	// PostBurst 进程级令牌桶突发容量。
	PostBurst int32 `json:",default=100"`
}

// CipherConf 私信内容加密参数。
//
// 明文正文永不入库：写入前用 AES-GCM 加密（nonce||ciphertext），
// 读取只在「调用者是会话成员 + 状态可见」两道门禁后才解密。
// 两个密钥字段都只从 Secret/Vault 注入，示例配置必须留空；
// 留空时加密路径返回 model.ErrCipherKeyMissing，绝不退化成明文存储。
type CipherConf struct {
	// KeyVersion 当前 AES 数据密钥版本，写入 pm_message.key_version；
	// 轮换后旧版本密钥仍需可解密历史消息，由部署侧的密钥环管理。
	KeyVersion int32 `json:",default=1"`
	// DataKeyBase64 是 AES-256-GCM 数据密钥（base64，32 字节）。禁止提交真实值。
	DataKeyBase64 string `json:",optional"`
	// HashPepper 是 pm_message.content_hash 的 HMAC-SHA256 pepper，
	// 用于风控查重与重复骚扰识别；不可反推原文，也不入库。禁止提交真实值。
	HashPepper string `json:",optional"`
}

// Validate 启动期自检：把「配置看起来能用但语义危险」的情况在启动时暴露，
// 而不是等到线上跑到某条私信才失败。返回 error 时 svc.NewServiceContext 直接终止启动。
//
// 这里刻意不 import model：config 只依赖 go-zero，保持「配置层 → 领域层」单向依赖，
// 错误文案也自解释，便于运维直接照 error 修配置。
func (c Config) Validate() error {
	if c.PrivateMessage.MessageRetentionDays <= 0 {
		return errors.New("PrivateMessage.MessageRetentionDays 必须 > 0：" +
			"私信正文密文必须有留存上限，0 表示永不清理，只允许在一次性调试实例上手工绕过")
	}
	if c.PrivateMessage.DefaultAllowFrom < allowFromAnyone || c.PrivateMessage.DefaultAllowFrom > allowFromNone {
		return errors.New("PrivateMessage.DefaultAllowFrom 必须落在 1..4（1 所有人、2 仅关注、3 仅互关、4 关闭私信）")
	}
	if c.PrivateMessage.WithdrawWindowSeconds <= 0 {
		return errors.New("PrivateMessage.WithdrawWindowSeconds 必须 > 0，否则发送者自助撤回永远被拒")
	}
	if c.PrivateMessage.MaxPageSize < c.PrivateMessage.PageSize {
		return errors.New("PrivateMessage.MaxPageSize 不能小于 PageSize")
	}
	if c.Cipher.KeyVersion <= 0 {
		return errors.New("Cipher.KeyVersion 必须 > 0（密文行要记录加密密钥版本，否则无法轮换）")
	}
	return nil
}

// CipherConfigured 判断是否已注入密钥材料；未注入时 svc 不阻断启动，
// 但所有加解密路径都会返回 model.ErrCipherKeyMissing（不静默降级成明文）。
func (c Config) CipherConfigured() bool {
	return c.Cipher.DataKeyBase64 != "" && c.Cipher.HashPepper != ""
}

// 接收范围取值（与 model.AllowFrom* 一致，config 侧只做区间校验，不导入 model）。
const (
	allowFromAnyone = 1
	allowFromNone   = 4
)
