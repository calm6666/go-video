// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 creator 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4）：本服务不提供 HTTP，
// HTTP 入口由 gateway 聚合。配置承载 gRPC 服务、MySQL、Redis 缓存。
type Config struct {
	zrpc.RpcServerConf

	// Redis 缓存连接（特殊属性/身份/开关/分组列表缓存）。
	Redis redis.RedisConf

	// MySQL 主库连接 DSN（creator 服务自有的 5 张表）。
	DataSource string
}
