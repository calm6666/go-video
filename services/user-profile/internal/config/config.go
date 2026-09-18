// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 user-profile 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4）：本服务不提供 HTTP，
// HTTP 入口由 gateway 聚合。配置承载 gRPC 服务、MySQL、Redis 缓存、
// 实名证件 RSA 密钥、Outbox 发布与下游 account RPC 客户端。
type Config struct {
	zrpc.RpcServerConf

	// Redis 缓存连接（基础资料/经验/节操/实名信息缓存、实名验证码、经验奖励位图）。
	Redis redis.RedisConf

	// MySQL 主库连接 DSN（user-profile 服务自有的 14 张表）。
	DataSource string

	// Realname 实名证件加解密与证件照配置。
	Realname RealnameConf

	// Outbox 领域事件 Outbox 发布配置。
	Outbox OutboxConf

	// AccountRPC 是 account 服务的 zrpc client 配置。
	// 资料更新事件（user.profile_updated）通过 account 的 DelCache RPC
	// 失效 account 侧 Info/Card/Profile 缓存（替代参考仓库 databus 的
	// MemberService-AccountNotify 主题，遵循"跨服务只用同步 RPC/事件"约束）。
	AccountRPC zrpc.RpcClientConf
}

// RealnameConf 实名证件加解密与证件照配置。
// 证书号入库前用公钥加密、出库后用私钥解密，密钥必须注入 Secret/Vault，
// 生产环境禁止硬编码。
type RealnameConf struct {
	// PublicKey RSA 公钥（PEM 格式），用于加密证件号。
	PublicKey string `json:",optional"`
	// PrivateKey RSA 私钥（PEM 格式），用于解密证件号。
	PrivateKey string `json:",optional"`
	// IMGURLTemplate 证件照 CDN URL 模板（含 %s 占位 token），
	// 例如 https://cdn.example.com/idenfiles/%s.txt。
	// 留空时实名详情的手持照字段直接返回 token 路径。
	IMGURLTemplate string `json:",optional"`
}

// OutboxConf Outbox 发布器配置。
type OutboxConf struct {
	// PollIntervalSeconds 轮询间隔（秒），默认 2。
	PollIntervalSeconds int64 `json:",default=2"`
	// BatchSize 每轮最多发布的事件数，默认 100。
	BatchSize int `json:",default=100"`
	// MaxAttempts 单条事件最大投递次数，超过后标记为失败进入人工处理，默认 10。
	MaxAttempts int `json:",default=10"`
}
