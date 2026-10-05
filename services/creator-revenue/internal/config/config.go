// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 creator-revenue 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 由 gateway/admin 与 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 预留的只读加速位（结算概览等热路径）。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	// 台账真值恒在 MySQL，缓存 miss/故障一律回源 DB，不允许用缓存结果决定是否出单。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_creator_revenue 库的 MySQL DSN。
	DataSource string

	// CreatorRevenue 是分成领域参数。
	CreatorRevenue CreatorRevenueConf
}

// CreatorRevenueConf 是创作者分成的业务参数，全部来自 etc yaml 或配置中心，不写死在代码里。
//
// 这里只放「护栏」而不放「价格」：单价、门槛、封顶的正式取值在 cr_revenue_rule 表里，
// 由运营通过 UpsertRevenueRule 走 DRAFT→ACTIVE 状态机维护；配置项只负责
// 「手滑写成天价时拒写」和「分页/批量不失控」两件事。
type CreatorRevenueConf struct {
	// DefaultCurrency 请求未带 currency 时写入的记账币种（本项目只有 CNY）。
	DefaultCurrency string `json:",default=CNY"`
	// MaxPageSize 列表类接口（规则/名单/计量/结算）单页上限，超出按上限截断。
	MaxPageSize int64 `json:",default=100"`
	// MaxRuleUnitPricePer1000Minor 单价护栏：每 1000 计量单位可支付的最高应计（分）。
	// 超过即拒绝入库——把「¥1 看成 ¥1000」这类手滑写成天价会在结算里放大成资金事故。
	MaxRuleUnitPricePer1000Minor int64 `json:",default=1000000"`
	// MaxMonthlyCapMinor 封顶护栏：单用户单来源单月允许的封顶上限（分），0 表示不限。
	// 规则里的 monthly_cap_minor 不得超过它，否则等于绕过封顶写天价。
	MaxMonthlyCapMinor int64 `json:",default=50000000"`
	// GenerateMaxBatch GenerateSettlement 全量出单的单批上限（mid=0 时生效）。
	// 超出只处理前 N 条并在 reply.truncated 里说明，避免一次事务扫穿整月台账。
	GenerateMaxBatch int64 `json:",default=500"`
	// SummaryRecentPeriods GetRevenueSummary 统计已确认合计时回看的周期数（月）。
	// 0 表示不限窗口（全历史）；默认 12 个月，避免对长尾账户做无界聚合。
	SummaryRecentPeriods int64 `json:",default=12"`
}
