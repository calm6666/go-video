// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 membership 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/app、gateway/admin 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载会员身份与权益码目录的读缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	// 缓存只用于减少读放大，任何缓存故障都必须回落 DB（见 MembershipConf 注释）。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_membership 库的 MySQL DSN（本服务唯一可写 schema）。
	DataSource string

	// Membership 是会员域参数，全部来自 etc yaml 或配置中心，不写死在代码里。
	Membership MembershipConf
}

// MembershipConf 是会员/套餐/权益的领域边界参数。
//
// 资金语义（AGENTS.md §1 2026-09-22 修订）：本服务不持有金额以外的资金状态，
// 价格只做展示与下单前置校验；真实扣款走 payment 沙箱台账，订单状态走 trade-order，
// 因此这里没有任何支付渠道密钥类配置项，也不得新增。
type MembershipConf struct {
	// MaxGrantDeltaDays 单次授予/收回的时长上限（天）。
	// 上限而非白名单：年卡+活动叠加等合法场景可达数年，但不能让调用方一次写入
	// 离谱时长（负数按绝对值同口径限制），越界一律 InvalidArgument 而不是静默裁剪。
	MaxGrantDeltaDays int32 `json:",default=3660"`
	// ExpireScanMaxLimit ListExpiringMemberships 单批上限，超限由服务侧裁剪。
	ExpireScanMaxLimit int32 `json:",default=500"`
	// MaxPageSize admin 面分页 size 上限，超限裁剪并在 reply 回显实际 page/size。
	MaxPageSize int32 `json:",default=100"`
	// DefaultPageSize 分页参数缺省值（page<=0 时回落到 1，size<=0 时回落到本值）。
	DefaultPageSize int32 `json:",default=20"`
	// DefaultCurrency 套餐未显式给出币种时使用的币种代码；价格一律以最小货币单位（分）计。
	DefaultCurrency string `json:",default=CNY"`
	// MembershipCacheTTLSeconds 会员身份/权益码读缓存秒数，0 表示关闭缓存。
	// 约束：缓存只允许加速「已落库的结论」，读取失败或 miss 必须回源 DB；
	// 绝不允许把「缓存不可用」折叠成 granted=false 的未开通结论。
	MembershipCacheTTLSeconds int `json:",default=10"`
}
