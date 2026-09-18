// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 account 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4）：本服务不提供 HTTP，
// 对外 HTTP 由 gateway 聚合。配置承载 gRPC 服务、Redis 缓存、
// MySQL 主库连接和下游 RPC client 配置。
type Config struct {
	zrpc.RpcServerConf

	// Redis 缓存连接（用于 Info/Card/Profile/Vip 的缓存层）。
	Redis redis.RedisConf

	// MySQL 主库连接 DSN（account 服务自有 account、account_credential 表）。
	DataSource string

	// UserProfileRPC 是 user-profile 服务的 zrpc client 配置。
	// 留空表示 user-profile 服务尚未部署，repository 会降级返回零值字段。
	UserProfileRPC zrpc.RpcClientConf `json:",optional"`

	// SocialGraphRPC 是 social-graph 服务的 zrpc client 配置。
	// 留空表示 social-graph 服务尚未接入，repository 会降级返回零值字段。
	SocialGraphRPC zrpc.RpcClientConf `json:",optional"`

	// PassportRSA 登录密码 RSA 密钥（PEM 格式）。
	// 客户端经网关 /x/passport-login/key 获取公钥加密密码传输，服务端私钥解密；
	// 密钥必须注入 Secret/Vault。两者留空时（仅限开发环境）按明文密码处理。
	PassportRSA PassportRSAConf

	// TokenTTLDays access token 有效期（天），默认 30。
	TokenTTLDays int64 `json:",optional"`
	// RefreshTTLDays refresh token 有效期（天），默认 90。
	RefreshTTLDays int64 `json:",optional"`
}

// PassportRSAConf 登录密码 RSA 密钥配置。
type PassportRSAConf struct {
	// PublicKey RSA 公钥（PEM 格式），网关 /x/passport-login/key 返回给客户端。
	PublicKey string `json:",optional"`
	// PrivateKey RSA 私钥（PEM 格式），服务端解密登录密码。
	PrivateKey string `json:",optional"`
}
