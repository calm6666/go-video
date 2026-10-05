// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 risk-control 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 滑窗计数器与裁决短缓存。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库 DSN（risk_rule/risk_punishment/risk_list/risk_device_profile/risk_check_log）。
	DataSource string

	// FeatureStoreRPC 是 feature-store 服务的 RPC client 配置。
	// feature-store 侧契约与实现已落地，但本服务尚未接线（ServiceContext 不构造该
	// client，示例配置保持注释）：未配置或调用失败时，设备风险分类特征降级为
	// risk_device_profile.risk_score 与本地滑窗计数，规则引擎把不可观测的指标标记为
	// skipped_rule_ids，不会伪造命中。降级路径详见 services/risk-control/README.md。
	FeatureStoreRPC zrpc.RpcClientConf `json:",optional"`

	// RiskControl 风控业务参数。
	RiskControl RiskControlConf
}

// RiskControlConf 是风控决策引擎与计数器的业务参数。
type RiskControlConf struct {
	// DefaultWindowSeconds 是 ReportAction 未指定窗口时写入的默认统计窗口。
	DefaultWindowSeconds int64 `json:",default=60"`

	// CounterTiers 是 Redis 滑窗计数写入的窗口档位（秒）。
	// 规则 window_seconds 会向上取齐到最近的档位；
	// 超过最大档位的规则视为不可观测（skipped），不会误判命中。
	CounterTiers []int64 `json:",optional"`

	// WindowBuckets 是每个档位内划分的桶数，语义与 common/counter rolling 一致
	// （桶数越多误差越小、Redis key 越多）。
	WindowBuckets int `json:",default=6"`

	// ChallengeTTLSeconds 是 CHALLENGE 裁决的建议有效期，客户端在此时间内可复用校验结果。
	ChallengeTTLSeconds int64 `json:",default=300"`

	// DecisionCacheSeconds 是同一 request_id 裁决复用的时间，保证 CheckAction 幂等。
	DecisionCacheSeconds int64 `json:",default=60"`

	// RuleCacheSeconds 是启用规则集合的 Redis 缓存时间。
	RuleCacheSeconds int64 `json:",default=30"`

	// HighRiskActions 是 DB 故障时按 BLOCK-on-error 处理的动作枚举值。
	// 默认 1 投稿 / 5 登录 / 6 改名 / 7 直播开播（凭证与内容发布类，误放行代价高）。
	// 为空时使用代码内默认值，避免配置缺失导致语义漂移。
	HighRiskActions []int64 `json:",optional"`

	// OnDbFailureDefault 是动作不在 HighRiskActions 中时的降级策略：
	// allow = 低危动作（评论/弹幕/关注）ALLOW-on-error，保证可用性；
	// block = 全量保守拒绝。
	OnDbFailureDefault string `json:",options=block|allow,default=allow"`

	// LocalFallbackPerWindow 是 Redis 不可用时单实例每动作在进程内滑窗允许的兜底动作数，
	// 超过则该动作降级为 BLOCK（防止计数器全盲时被脚本洪水打穿）。<=0 表示关闭。
	LocalFallbackPerWindow int64 `json:",default=200"`

	// MaxHitRulesPerDecision 限制单次裁决返回的命中规则数量（响应体积保护）。
	MaxHitRulesPerDecision int `json:",default=20"`

	// CheckLogRetentionDays 是 risk_check_log 的期望保留天数，仅写入 README 与注释，
	// 实际归档由 services/cron 执行。
	CheckLogRetentionDays int `json:",default=30"`
}
