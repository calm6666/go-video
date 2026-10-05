// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 rights 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存连接（用于 CheckPlayable 结果缓存）。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库连接 DSN。
	DataSource string

	// CheckPlayable 缓存 TTL（秒），默认 120；过期窗口被强制失效。
	CheckPlayableCacheTTL int `json:",default=120"`
}
