// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 trade-order（商业订单）服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/app、gateway/admin 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 为读侧缓存预留位。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	//
	// 本轮刻意不用它缓存任何订单读结果：订单是资金相关事实，
	// 状态与金额必须回源 MySQL（GetOrder/ListOrders 的响应 ttl 由网关按 0 处理）。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_trade_order 库的 MySQL DSN。
	DataSource string

	// MembershipRPC 是会员域客户端：CreateOrder 取套餐价（唯一可信价格来源）、
	// 履约发放会员、退款回收权益。未配置时不构造客户端，
	// CreateOrder/履约直接返回 model.ErrMembershipNotConfigured，不伪造金额与发放。
	MembershipRPC zrpc.RpcClientConf `json:",optional"`

	// PaymentRPC 是资金域客户端：受理支付、退款到余额、关单。
	// 未配置时不构造客户端，建单与退款审批返回 model.ErrPaymentNotConfigured，
	// 绝不落一张「看起来已支付」的假订单。
	PaymentRPC zrpc.RpcClientConf `json:",optional"`

	// CoinRPC 是硬币域客户端：硬币包履约发放、退款回收。
	// 未配置时不构造客户端，硬币包订单履约返回 model.ErrCoinNotConfigured。
	CoinRPC zrpc.RpcClientConf `json:",optional"`

	// TradeOrder 是订单领域参数，全部来自 etc yaml 或配置中心，不写死在代码里。
	TradeOrder TradeOrderConf
}

// TradeOrderConf 是商业订单领域参数。
type TradeOrderConf struct {
	// MaxQuantityPerOrder 单笔订单份数上限；请求超出按上限裁剪（proto 注释「上限由服务侧配置裁剪」），
	// 裁剪后金额与客户端上报金额不一致会被拒绝，等于把改价企图挡在门外。
	MaxQuantityPerOrder int32 `json:",default=10"`
	// OrderExpireSeconds 未支付订单的关单时间（秒），写入 to_order.expire_at，
	// 同时透传给 payment.CreatePayment.expire_at，两边超时口径一致。
	OrderExpireSeconds int64 `json:",default=1800"`
	// StuckScanMaxLimit ListStuckOrders 单次返回上限（cron 用它做卡单巡检）。
	StuckScanMaxLimit int64 `json:",default=200"`
	// MaxListWindowSeconds 运营面跨用户查询的最大时间窗（90 天）。
	// 无界扫描会锁表，超限一律拒绝而不是裁剪。
	MaxListWindowSeconds int64 `json:",default=7776000"`
	// MaxPageSize 分页 size 上限，超出直接拒绝（裁剪会让调用方误以为「数据就这么多了」）。
	MaxPageSize int64 `json:",default=100"`
	// FulfillMaxAttempts 履约累计尝试上限；达到后 FulfillOrder 拒绝再试，
	// 必须人工介入（避免下游持续故障时被无限重试打出双倍发放）。
	FulfillMaxAttempts int32 `json:",default=5"`
	// MinSecondsBetweenFulfillRetry 两次履约尝试的最小间隔（秒）。
	// 没有这一条，cron 每轮都会把所有 FULFILLING 卡单重放给下游，直接打爆会员/硬币服务。
	MinSecondsBetweenFulfillRetry int64 `json:",default=30"`
	// DefaultCurrency 套餐未声明币种时使用的币种；金额一律最小货币单位（分），不用浮点。
	DefaultCurrency string `json:",default=CNY"`
}
