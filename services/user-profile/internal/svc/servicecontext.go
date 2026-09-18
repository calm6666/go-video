// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"go-video/services/user-profile/internal/config"
	"go-video/services/user-profile/internal/repository"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 user-profile 服务的运行时上下文，
// 承载跨请求共享的 repository 依赖。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.Redis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, c)

	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}
