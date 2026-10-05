// Code scaffolded by goctl. Safe to edit.

package config

import (
	"strings"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 payment（资金域）服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向端的 HTTP 由 gateway 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 预留给读侧列表缓存；资金台账的判定一律走 MySQL，
	// 不从 Redis 读余额/单据，避免脏读造成重复入账或漏入账。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_payment 库的 MySQL DSN（本服务唯一可写的 schema）。
	DataSource string

	// Payment 是资金领域参数：币种、金额上下限、分页与跨用户查询窗口。
	Payment PaymentConf
}

// PaymentConf 是资金业务参数，全部来自 etc yaml 或配置中心，不写死在代码里。
//
// 金额口径：一律 int64 最小货币单位（人民币＝分），禁止浮点。
type PaymentConf struct {
	// DefaultCurrency 默认币种；本服务只支持单一币种台账，
	// 请求带其他币种直接拒绝（ErrUnsupportedCurrency），不做隐式换汇。
	DefaultCurrency string `json:",default=CNY"`
	// MinRechargeMinor 单笔充值下限（分）。<=0 时按 1 处理。
	MinRechargeMinor int64 `json:",default=1"`
	// MaxRechargeMinor 单笔充值上限（分），默认 200000＝2000 元，防手滑。
	MaxRechargeMinor int64 `json:",default=200000"`
	// MaxAdjustMinor 单次运营调整的绝对值上限（分）。充值有上限，
	// 运营调整同样要有上限，否则一次误操作就能把台账打穿。
	MaxAdjustMinor int64 `json:",default=1000000"`
	// MaxPageSize 列表接口单次返回上限。
	MaxPageSize int64 `json:",default=100"`
	// MaxListWindowSeconds 跨用户（mid=0）查询允许的最大时间窗秒数。
	// 超窗或不给窗口一律拒绝，不允许全表扫（资金表行数只增不减）。
	MaxListWindowSeconds int64 `json:",default=2592000"`
	// MaxListOffset 列表接口允许的最大 OFFSET，兜底深翻页造成的扫描放大。
	MaxListOffset int64 `json:",default=10000"`
	// AllowedChannels 受理的渠道名列表（PayChannel 枚举去掉 PAY_CHANNEL_ 前缀）。
	// 本项目唯一合法取值是 SANDBOX；整段缺失时按「只允许 SANDBOX」处理并记告警，
	// 写了无法识别的渠道名同样记告警并忽略——绝不因为配置写错就去猜真实渠道。
	AllowedChannels []string `json:",optional"`
}

// NormalizeCurrency 归一化币种：空串取默认币种，统一大写。
func (c PaymentConf) NormalizeCurrency(currency string) string {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(c.DefaultCurrency))
	}
	if currency == "" {
		currency = "CNY"
	}
	return currency
}

// AllowsChannel 判断渠道名（已去掉 PAY_CHANNEL_ 前缀，大小写不敏感）是否被放行。
// AllowedChannels 为空时按默认口径只放行 SANDBOX。
func (c PaymentConf) AllowsChannel(name string) bool {
	name = strings.ToUpper(strings.TrimSpace(name))
	if name == "" {
		return false
	}
	list := c.AllowedChannels
	if len(list) == 0 {
		list = []string{ChannelSandbox}
	}
	for _, item := range list {
		if strings.ToUpper(strings.TrimSpace(item)) == name {
			return true
		}
	}
	return false
}

// ChannelSandbox 是 PayChannel 中唯一可用渠道的名字片段。
const ChannelSandbox = "SANDBOX"

// KnownChannels 列出可写进 AllowedChannels 的渠道名，供启动期校验配置拼写。
func KnownChannels() []string { return []string{ChannelSandbox} }

// RechargeAmountAllowed 判断充值金额是否落在配置区间内。
func (c PaymentConf) RechargeAmountAllowed(amountMinor int64) bool {
	min := c.MinRechargeMinor
	if min <= 0 {
		min = 1
	}
	max := c.MaxRechargeMinor
	if max <= 0 {
		max = 200000
	}
	return amountMinor >= min && amountMinor <= max
}

// AdjustAmountAllowed 判断运营调整幅度是否在允许的绝对值内。
func (c PaymentConf) AdjustAmountAllowed(deltaMinor int64) bool {
	if deltaMinor == 0 {
		return false
	}
	max := c.MaxAdjustMinor
	if max <= 0 {
		max = 1000000
	}
	abs := deltaMinor
	if abs < 0 {
		abs = -abs
	}
	return abs <= max
}
