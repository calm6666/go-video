// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/asset/internal/config"
	"go-video/services/asset/internal/repository"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 asset 服务的运行时上下文。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)
	repo := repository.New(rds, conn)
	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}
