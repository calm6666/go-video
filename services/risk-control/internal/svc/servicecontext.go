// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/risk-control/internal/config"
	"go-video/services/risk-control/internal/policy"
	"go-video/services/risk-control/internal/repository"
	"go-video/services/risk-control/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 risk-control 服务的运行时上下文。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
	// Engine 是风控决策引擎，依赖 Repository 作为事实来源。
	Engine *policy.Engine
}

// NewServiceContext 构造 ServiceContext。
//
// 关于 FeatureStoreRPC：feature-store 的契约与实现已落地，本服务侧接线仍待做——
// 因此本期不建立 client 连接；设备风险类特征降级为
// risk_device_profile.risk_score / related_mid_count + Redis 滑窗计数，
// 无法观测的指标进入 skipped_rule_ids 而不是被当成 0 命中。
// 接线时在此处装配 client（对端 Etcd.Key 为 featurestore.v1.rpc）并扩展 repository.observe，
// 契约与降级路径见 services/risk-control/README.md。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, repository.Config{
		CounterTiers:         c.RiskControl.CounterTiers,
		WindowBuckets:        c.RiskControl.WindowBuckets,
		DefaultWindowSeconds: c.RiskControl.DefaultWindowSeconds,
		DecisionCacheSeconds: c.RiskControl.DecisionCacheSeconds,
		RuleCacheSeconds:     c.RiskControl.RuleCacheSeconds,
		LocalFallbackLimit:   c.RiskControl.LocalFallbackPerWindow,
	})

	engine := policy.NewEngine(repo, policyConfig(c.RiskControl))
	ctx := &ServiceContext{Config: c, Repository: repo, Engine: engine}
	ctx.logStartup()
	return ctx
}

// policyConfig 把服务配置映射为引擎配置（含降级策略）。
func policyConfig(rc config.RiskControlConf) policy.Config {
	degrade := policy.DegradePolicy{
		HighRiskActions:    toInt32s(rc.HighRiskActions),
		HighRiskDecision:   model.DecisionBlock,
		DefaultDecision:    model.DecisionAllow,
		LocalFallbackLimit: rc.LocalFallbackPerWindow,
	}
	if rc.OnDbFailureDefault == "block" {
		degrade.DefaultDecision = model.DecisionBlock
	}
	return policy.Config{
		ChallengeTTLSeconds: rc.ChallengeTTLSeconds,
		MaxHitsPerDecision:  rc.MaxHitRulesPerDecision,
		Degrade:             degrade,
	}
}

func toInt32s(in []int64) []int32 {
	if len(in) == 0 {
		return nil
	}
	out := make([]int32, 0, len(in))
	for _, v := range in {
		out = append(out, int32(v))
	}
	return out
}

// logStartup 打印启动期自检信息，便于验证配置是否按预期生效（AGENTS.md §4 可验证性）。
func (s *ServiceContext) logStartup() {
	logx.Infof("risk-control: window tiers=%v buckets=%d default_window=%ds local_fallback=%t on_db_failure_default=%s high_risk_actions=%v",
		s.Config.RiskControl.CounterTiers, s.Config.RiskControl.WindowBuckets,
		s.Config.RiskControl.DefaultWindowSeconds, s.Repository.LocalFallbackEnabled(),
		s.Config.RiskControl.OnDbFailureDefault, s.Config.RiskControl.HighRiskActions)
}
