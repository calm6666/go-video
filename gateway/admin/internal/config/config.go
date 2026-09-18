// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 gateway/admin 的配置结构。
// 管理后台入口聚合（AGENTS.md §3）：HTTP 收口在此，领域数据经下游 RPC 获取，
// 管理业务逻辑归属 services/operation，本网关只做路由与聚合。
type Config struct {
	rest.RestConf

	// AccountRPC 是 account 服务的 zrpc client 配置（缓存失效运营路由）。
	AccountRPC zrpc.RpcClientConf

	// UserProfileRPC 是 user-profile 服务的 zrpc client 配置
	// （节操/经验/属性审核/实名脱敏运营路由）。
	UserProfileRPC zrpc.RpcClientConf
}
