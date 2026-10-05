// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 danmaku 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载段缓存、段计数、屏蔽词库缓存与防刷屏分钟窗口计数。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_danmaku 库的 MySQL DSN。
	DataSource string

	// ModerationRPC 是 moderation-orchestrator 的 zrpc client 配置。
	// 未配置时不构造客户端：Danmaku.MachineReviewEnabled 为 true 的情况下
	// PostDanmaku 直接返回 model.ErrModerationNotConfigured，不伪造“已送审”。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// Danmaku 是弹幕领域参数。
	Danmaku DanmakuConf
}

// DanmakuConf 是弹幕业务参数，全部来自 etc yaml 或配置中心，不写死在代码里。
type DanmakuConf struct {
	// SegmentSeconds 时间分段宽度（秒），决定 seg_no 与段缓存粒度。
	SegmentSeconds int32 `json:",default=6"`
	// MaxContentLength 弹幕正文字符数上限（按 rune 计）。
	MaxContentLength int32 `json:",default=100"`
	// MaxPerUserPerMinute 单用户每分钟发送上限，0 表示不限制该维度。
	MaxPerUserPerMinute int32 `json:",default=20"`
	// MaxPerOidPerMinute 单内容每分钟弹幕上限，0 表示不限制该维度。
	MaxPerOidPerMinute int32 `json:",default=6000"`
	// PostQps 进程级发送 QPS（复用 common/ratelimit 令牌桶，保护 MySQL）。
	PostQps int32 `json:",default=800"`
	// PostBurst 进程级令牌桶突发容量。
	PostBurst int32 `json:",default=200"`
	// SensitiveWordCheckEnabled 是否在发送侧做屏蔽词过滤。
	// 关闭时弹幕只走机审门禁，不再本地折叠，仅限压测/回放环境。
	SensitiveWordCheckEnabled bool `json:",default=true"`
	// MachineReviewEnabled 是否强制机审门禁：true 时新弹幕落库为待审核，
	// 只有 ApplyModerationResult 通过后才进入普通池（AGENTS.md §8）。
	MachineReviewEnabled bool `json:",default=true"`
	// SegmentCacheTTLSeconds 段列表缓存秒数，0 表示关闭段缓存。
	SegmentCacheTTLSeconds int `json:",default=15"`
	// SegmentCountTTLSeconds 段计数缓存秒数，0 表示关闭。
	SegmentCountTTLSeconds int `json:",default=60"`
	// BlockWordCacheTTLSeconds 屏蔽词库缓存秒数，0 表示每次回源 DB。
	BlockWordCacheTTLSeconds int `json:",default=300"`
	// UserBlockCacheTTLSeconds 用户屏蔽列表缓存秒数，0 表示每次回源 DB。
	UserBlockCacheTTLSeconds int `json:",default=120"`
	// BlockWordCacheMaxWords 词库缓存条数上限，超出则不写缓存避免大 value。
	BlockWordCacheMaxWords int `json:",default=2000"`
	// MaxSegWindow 单次 ListDanmaku 允许拉取的最大分段数。
	MaxSegWindow int `json:",default=60"`
	// MaxListLimit 单次 ListDanmaku 允许返回的最大条数。
	MaxListLimit int `json:",default=3000"`
}
