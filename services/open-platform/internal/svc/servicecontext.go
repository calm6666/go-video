// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/common/ratelimit"
	"go-video/services/open-platform/internal/config"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 open-platform 的运行时上下文：MySQL 连接、缓存与各表 model。
//
// 本服务没有下游 RPC 依赖（AGENTS.md §5 数据自治）：account 的会话校验在 gateway 完成，
// 本域只认 app_id / mid / grant_id 等主键；Webhook 投递 worker 与配额重算 cron
// 在逻辑轮接入，届时也通过这里取同一份 model。
type ServiceContext struct {
	Config config.Config

	// DB 是 go_video_open_platform 库连接（scope 授予、授权码换 token 等跨表写走
	// DB.TransactCtx，model 方法一律接受 sqlx.Session）。
	DB sqlx.SqlConn

	// Cache 承载 token 校验短缓存与 nonce 防重放集合；撤销位点比对仍以 MySQL 为真值。
	Cache *redis.Redis

	// Apps 应用主体（状态机 + 乐观锁版本）。
	Apps model.ApplicationModel
	// Secrets client_secret 哈希（明文只签发一次）。
	Secrets model.AppSecretModel
	// Scopes 权限点目录（读/写与风险级别声明）。
	Scopes model.ScopeModel
	// AppScopes 应用-scope 审批关系。
	AppScopes model.AppScopeModel
	// AuthCodes OAuth 授权码（短期、一次性消费）。
	AuthCodes model.AuthCodeModel
	// Grants 授权关系，兼作撤销位点。
	Grants model.GrantModel
	// Tokens access/refresh 哈希与轮换链。
	Tokens model.TokenModel
	// QuotaPolicies 配额规则（应用 × 接口 × 时间窗）。
	QuotaPolicies model.QuotaPolicyModel
	// QuotaUsages 配额用量投影（可由 CallLogs 重算）。
	QuotaUsages model.QuotaUsageModel
	// CallLogs 调用流水（配额与审计的事实来源，request_id 幂等锚点）。
	CallLogs model.ApiCallLogModel
	// WebhookEndpoints 回调端点（只存签名密钥版本，不存密钥材料）。
	WebhookEndpoints model.WebhookEndpointModel
	// WebhookDeliveries 投递任务（退避重试 + 死信）。
	WebhookDeliveries model.WebhookDeliveryModel

	// WriteLimiter 进程级令牌桶（复用 common/ratelimit），保护 MySQL。
	WriteLimiter ratelimit.Limiter
}

// NewServiceContext 构造 ServiceContext。
//
// 配置自检失败直接 Severe 终止启动：凭证 TTL、nonce 窗口与留存期配错都会削弱
// 安全性（例如 nonce 存活期小于时间窗即可重放），宁可不启动也不带病上线。
// 密钥材料缺失只记日志：服务仍可启动，但验签/哈希路径返回
// model.ErrSecretVerificationUnavailable，绝不退化成「无 pepper 比较」。
func NewServiceContext(c config.Config) *ServiceContext {
	if err := c.Validate(); err != nil {
		logx.Severe("open-platform/svc: 配置自检失败: ", err)
	}
	if !c.SecurityConfigured() {
		logx.Errorf("open-platform/svc: Security.CredentialPepper/WebhookMasterPepper 未注入，" +
			"验签、token 哈希与回调签名相关方法将返回 open-platform: secret verification key missing")
	}

	conn := sqlx.NewMysql(c.DataSource)
	qps := int(c.OpenPlatform.PostQps)
	burst := int(c.OpenPlatform.PostBurst)
	if burst < qps {
		burst = qps
	}

	return &ServiceContext{
		Config:            c,
		DB:                conn,
		Cache:             redis.MustNewRedis(c.CacheRedis),
		Apps:              model.NewApplicationModel(conn),
		Secrets:           model.NewAppSecretModel(conn),
		Scopes:            model.NewScopeModel(conn),
		AppScopes:         model.NewAppScopeModel(conn),
		AuthCodes:         model.NewAuthCodeModel(conn),
		Grants:            model.NewGrantModel(conn),
		Tokens:            model.NewTokenModel(conn),
		QuotaPolicies:     model.NewQuotaPolicyModel(conn),
		QuotaUsages:       model.NewQuotaUsageModel(conn),
		CallLogs:          model.NewApiCallLogModel(conn),
		WebhookEndpoints:  model.NewWebhookEndpointModel(conn),
		WebhookDeliveries: model.NewWebhookDeliveryModel(conn),
		WriteLimiter:      ratelimit.NewTokenBucket(qps, burst),
	}
}
