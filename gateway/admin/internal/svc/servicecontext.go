// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package svc

import (
	"go-video/gateway/admin/internal/config"
	accountrpc "go-video/services/account/rpc"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 gateway/admin 的运行时上下文，承载下游 RPC 客户端。
type ServiceContext struct {
	Config config.Config
	// Account account 服务 RPC 客户端。
	Account accountrpc.AccountClient
	// UserProfile user-profile 服务 RPC 客户端。
	UserProfile userprofilerc.UserProfileClient
}

// NewServiceContext 构造 ServiceContext。
// 对应 RPC 配置留空时客户端为 nil，logic 会返回明确的服务不可用错误。
func NewServiceContext(c config.Config) *ServiceContext {
	ctx := &ServiceContext{Config: c}
	if c.AccountRPC.Target != "" || len(c.AccountRPC.Etcd.Hosts) > 0 {
		ctx.Account = accountrpc.NewAccountClient(zrpc.MustNewClient(c.AccountRPC).Conn())
	}
	if c.UserProfileRPC.Target != "" || len(c.UserProfileRPC.Etcd.Hosts) > 0 {
		ctx.UserProfile = userprofilerc.NewUserProfileClient(zrpc.MustNewClient(c.UserProfileRPC).Conn())
	}
	return ctx
}
