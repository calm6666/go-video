// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"go-video/services/account/internal/config"
	"go-video/services/account/internal/repository"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 account 服务的运行时上下文，承载跨请求共享的依赖。
// handler 和 logic 通过 ServiceContext 访问 repository 和下游 RPC client。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
// 当 UserProfileRPC 和 SocialGraphRPC 配置留空时，对应的下游 client 为 nil，
// repository 会降级返回零值字段，不阻塞主流程。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.Redis)
	conn := sqlx.NewMysql(c.DataSource)

	// 下游 RPC client：当对应服务尚未部署时为 nil，repository 降级处理。
	// user-profile 服务已接入：注入 zrpc client 适配器（UserProfileClient）。
	var userProfile repository.UserProfileClient
	if len(c.UserProfileRPC.Etcd.Hosts) > 0 || c.UserProfileRPC.Target != "" {
		userProfile = repository.NewUserProfileClient(c.UserProfileRPC)
	}

	var socialGraph repository.SocialGraphClient
	if len(c.SocialGraphRPC.Etcd.Hosts) > 0 || c.SocialGraphRPC.Target != "" {
		// TODO: 注入 social-graph zrpc client 适配器
	}

	repo := repository.New(rds, conn, c, userProfile, socialGraph)

	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}
