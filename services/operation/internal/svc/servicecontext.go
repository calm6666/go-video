// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"os"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/operation/internal/config"
	"go-video/services/operation/internal/repository"
)

// ServiceContext 是 operation 服务的运行时上下文。
type ServiceContext struct {
	Config config.Config
	// Repository 聚合自有表 model、缓存与下游 RPC 客户端。
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
// 下游 RPC 配置留空时对应客户端为 nil，任务步骤会以「下游不可用」失败，
// 服务本身仍可启动（与 gateway/admin 的 optional client 约定一致）。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, buildDownstream(c), repository.SessionConf{
		TokenTTL: c.AdminSession.TokenTTL,
		Issuer:   c.AdminSession.Issuer,
		Secret:   loadTokenSecret(c.AdminSession.TokenSecretRef),
	}, repository.Options{
		PermissionTTL: c.Cache.PermissionTTL,
		ConfigTTL:     c.Cache.ConfigTTL,
		MenuTTL:       c.Cache.MenuTTL,
		RunSteps:      c.Cache.RunSteps,
		MaxFail:       c.Login.MaxFail,
		LockMinutes:   c.Login.LockMinutes,
	})

	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}

// buildDownstream 按配置构造下游客户端。
// Target 与 Etcd.Hosts 都为空的客户端不构造，避免启动即连接失败。
func buildDownstream(c config.Config) *repository.Downstream {
	ds := &repository.Downstream{}
	if enabled(c.VideoRPC) {
		ds.Video = repository.NewVideoGateway(c.VideoRPC)
	}
	if enabled(c.CatalogRPC) {
		ds.Catalog = repository.NewCatalogGateway(c.CatalogRPC)
	}
	if enabled(c.RightsRPC) {
		ds.Rights = repository.NewRightsGateway(c.RightsRPC)
	}
	if enabled(c.ModerationRPC) {
		ds.Moderation = repository.NewModerationGateway(c.ModerationRPC)
	}
	if enabled(c.AccountRPC) {
		ds.Account = repository.NewAccountGateway(c.AccountRPC)
	}
	return ds
}

// enabled 判断 zrpc client 配置是否可用。
func enabled(c zrpc.RpcClientConf) bool {
	return c.Target != "" || len(c.Etcd.Hosts) > 0
}

// loadTokenSecret 读取 TokenSecretRef 指向的环境变量。
// 只做“取值”，不在此处报错：密钥缺失时 repository.SessionConf 会在签发会话时
// 返回 model.ErrTokenSecretMissing（拒绝签发无摘要的弱 token），而服务仍可启动，
// 这样健康检查与只读接口不受影响，部署缺陷在 AdminLogin 上立刻暴露。
func loadTokenSecret(ref string) []byte {
	if ref == "" {
		logx.Error("operation: AdminSession.TokenSecretRef is empty, AdminLogin will fail with ErrTokenSecretMissing")
		return nil
	}
	val := os.Getenv(ref)
	if val == "" {
		logx.Errorf("operation: AdminSession.TokenSecretRef=%s not found in environment, AdminLogin will fail with ErrTokenSecretMissing", ref)
		return nil
	}
	return []byte(val)
}
