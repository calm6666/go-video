// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/moderation-orchestrator/internal/config"
	"go-video/services/moderation-orchestrator/internal/repository"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 moderation-orchestrator 服务的运行时上下文。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
// 当前实现占位：未配置 moderation-worker RPC client；
// SubmitForReview 仅创建 task 并入 MQ 待处理，不直接调用 worker。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)
	repo := repository.New(rds, conn)
	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}
