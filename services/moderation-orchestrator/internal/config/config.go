// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 moderation-orchestrator 服务的配置结构。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存与计数器连接。
	CacheRedis redis.RedisConf // 不能命名为 Redis：与 zrpc.RpcServerConf 内嵌字段同名会让配置加载失败

	// MySQL 主库连接 DSN。
	DataSource string

	// ModerationWorkerRPC 是 moderation-worker 服务的 RPC client 配置；
	// 当前实现占位：未配置时 SubmitForReview 仅创建 task 并入 MQ 待处理。
	ModerationWorkerRPC zrpc.RpcClientConf `json:",optional"`
}
