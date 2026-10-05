// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/membership/internal/config"
	"go-video/services/membership/model"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 membership 服务的运行时上下文，承载跨请求共享的依赖。
//
// 依赖策略：DB 是本服务的硬依赖（sqlx 懒连接，构造不 panic，首查失败即上抛错误，
// logic 绝不把查询失败折叠成「未开通」）；Redis 只是读缓存，缺失或故障一律回落 DB。
type ServiceContext struct {
	Config config.Config

	// DB 是本服务 MySQL 连接；logic 用它把「改会员身份 + 写授予台账」放进同一事务。
	DB sqlx.SqlConn

	// Cache 是会员身份/权益码读缓存；nil 或 TTL=0 时读路径直接回源 DB。
	Cache *redis.Redis

	Plan        model.PlanModel
	PlanLog     model.PlanChangeLogModel
	Entitlement model.EntitlementModel
	Membership  model.MembershipModel
	Grant       model.GrantModel
	Request     model.BizRequestModel
}

// NewServiceContext 构造 ServiceContext。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	var cache *redis.Redis
	if c.Membership.MembershipCacheTTLSeconds > 0 {
		cache = redis.MustNewRedis(c.CacheRedis)
	}

	return &ServiceContext{
		Config:      c,
		DB:          conn,
		Cache:       cache,
		Plan:        model.NewPlanModel(conn),
		PlanLog:     model.NewPlanChangeLogModel(conn),
		Entitlement: model.NewEntitlementModel(conn),
		Membership:  model.NewMembershipModel(conn),
		Grant:       model.NewGrantModel(conn),
		Request:     model.NewBizRequestModel(conn),
	}
}
