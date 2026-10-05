// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/services/catalog/internal/config"
	"go-video/services/catalog/internal/repository"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 catalog 服务的运行时上下文。
type ServiceContext struct {
	Config     config.Config
	Repository *repository.Repository

	// Rights 是 rights 服务的只读客户端（版权窗口校验）。未配置时为 nil。
	Rights repository.RightsClient

	// Asset 是 asset 服务的只读客户端（媒资就绪校验）。未配置时为 nil。
	Asset repository.AssetClient
}

// NewServiceContext 构造 ServiceContext。
// 下游 RPC 客户端按配置构造：RightsRPC/AssetRPC 未提供 Target 也没有 etcd Hosts 时
// 保持 nil，由 logic 层的 guard 按“默认严格”策略拒绝上架/建集
// （见 internal/logic/guard.go 与 README 的校验失败语义）。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)
	ctx := &ServiceContext{
		Config:     c,
		Repository: repository.New(rds, conn),
	}
	if rpcConfigured(c.RightsRPC) {
		ctx.Rights = repository.NewRightsClient(c.RightsRPC)
	}
	if rpcConfigured(c.AssetRPC) {
		ctx.Asset = repository.NewAssetClient(c.AssetRPC)
	}
	return ctx
}

// rpcConfigured 判断 RpcClientConf 是否给出可用下游端点（直连 Target 或 etcd 服务发现）。
func rpcConfigured(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0
}
