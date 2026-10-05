// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"os"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/audit/internal/config"
	"go-video/services/audit/internal/repository"
)

// ServiceContext 是 audit 服务的运行时上下文。
//
// 与其它服务的差别：这里没有下游 RPC 客户端。审计是被动写入方，
// 鉴权由 gateway/admin + operation 完成，本服务只保证
// 「写入必带归因、查询必受约束、读了什么必须留痕」。
type ServiceContext struct {
	Config config.Config
	// Repository 聚合自有表 model、幂等缓存与哈希盐。
	Repository *repository.Repository
}

// NewServiceContext 构造 ServiceContext。
//
// 启动期不做「连得上才算健康」之外的隐式降级：Redis 走 go-zero 的 MustNewRedis
// （非 NonBlock 时会做连通性检查），MySQL 客户端惰性建连。
// 哈希盐缺失不在这里 panic —— 只读接口与导出查询不受影响，
// 写入接口会在第一次 Append 时返回明确错误，部署缺陷立刻暴露而不拖垮进程。
func NewServiceContext(c config.Config) *ServiceContext {
	rds := redis.MustNewRedis(c.CacheRedis)
	conn := sqlx.NewMysql(c.DataSource)

	repo := repository.New(rds, conn, repository.Options{
		IpHashSalt:        loadSalt(c.Security.IpHashSaltRef),
		MaxRangeDays:      c.Query.MaxRangeDays,
		MaxPageSize:       c.Query.MaxPageSize,
		MaxBatchSize:      c.Write.MaxBatchSize,
		ChainRetry:        c.Write.ChainRetry,
		MaxReasonLen:      c.Write.MaxReasonLen,
		MaxUserAgentLen:   c.Security.MaxUserAgentLen,
		MaxVerifyEntries:  c.Verify.MaxEntriesPerCall,
		ExportBatchRows:   c.Export.BatchRows,
		ExportMaxRows:     c.Export.MaxRowsPerTask,
		ObjectTTLSeconds:  c.Export.ObjectTTLSeconds,
		PresignTTLSeconds: c.Export.PresignTTLSeconds,
		ArchiveMaxEntries: c.Archive.MaxEntriesPerBatch,
		VerifyBeforePurge: c.Archive.VerifyBeforePurge,
		Storage: repository.StorageConf{
			Enabled:      c.Storage.Enabled,
			Endpoint:     c.Storage.Endpoint,
			Region:       c.Storage.Region,
			Bucket:       c.Storage.Bucket,
			AccessKeyRef: c.Storage.AccessKeyRef,
			SecretKeyRef: c.Storage.SecretKeyRef,
			UseSSL:       c.Storage.UseSSL,
			PathStyle:    c.Storage.PathStyle,
		},
	})

	return &ServiceContext{
		Config:     c,
		Repository: repo,
	}
}

// loadSalt 读取哈希盐环境变量。
// 只取值、不报错：缺失时 repository 在写入路径返回 model.ErrHashSaltMissing，
// 保证「宁可拒写也不落弱哈希」这条防线不会因为启动顺序而被绕过。
func loadSalt(ref string) []byte {
	if ref == "" {
		logx.Error("audit: Security.IpHashSaltRef is empty, AppendAudit will fail with ErrHashSaltMissing")
		return nil
	}
	val := os.Getenv(ref)
	if val == "" {
		logx.Errorf("audit: Security.IpHashSaltRef=%s not found in environment, AppendAudit will fail with ErrHashSaltMissing", ref)
		return nil
	}
	return []byte(val)
}
