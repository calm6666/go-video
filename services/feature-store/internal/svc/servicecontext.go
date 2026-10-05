// 本文件是手写的依赖装配（goctl 只生成骨架），logic 层从这里取 model 与缓存。
//
// 装配原则（AGENTS.md §3/§5）：
//   - 只拥有 go_video_feature_store 库的 6 张 fs 表；任何跨域读取都不在此发生
//     （本服务不依赖其它 go-video 服务的 RPC，见 README §7，因此没有下游 client）；
//   - MySQL 与 CacheRedis 都是硬依赖：幂等回执、指针 CAS 必须落库，
//     在线读的主存是缓存，连不上就启动失败，不带着半残依赖对外服务；
//   - CacheRedis 为 nil（Host 为空）时读路径**不会**伪造成功：
//     helper 会把该次读标成 SourceAvailable=false，按 SOURCE_UNAVAILABLE 降级，
//     写路径则退化为「只删不写」并记日志。

package svc

import (
	"fmt"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/feature-store/internal/config"
	"go-video/services/feature-store/model"
)

// ServiceContext 是 feature-store 的运行时上下文。
type ServiceContext struct {
	Config config.Config

	// DB 本服务独占的 go_video_feature_store 连接；需要事务（指针 CAS + 审计同生共死）时用。
	DB sqlx.SqlConn

	// Cache 在线特征值主读缓存（fs:val:*）与 ACTIVE 指针缓存（fs:active:*）。
	Cache *redis.Redis

	// --- 本服务 6 张表的数据访问 ---

	// Definitions feature_definition：特征版本口径的事实源。
	Definitions model.FeatureDefinitionModel
	// ActiveVersions feature_active_version：同一 key 只有一个生效版本的 DB 层保证。
	ActiveVersions model.ActiveVersionModel
	// Values feature_value：在线值的 MySQL 投影（回源、重放、隐私擦除）。
	Values model.FeatureValueModel
	// Switches feature_version_switch：追加式变更审计。
	Switches model.VersionSwitchModel
	// Backfills feature_backfill_job：回填台账与断点。
	Backfills model.BackfillJobModel
	// Receipts feature_write_receipt：写类操作的执行权与幂等回放快照。
	Receipts model.WriteReceiptModel
}

// NewServiceContext 构造上下文。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	ctx := &ServiceContext{
		Config:         c,
		DB:             conn,
		Cache:          newCache(c),
		Definitions:    model.NewFeatureDefinitionModel(conn),
		ActiveVersions: model.NewActiveVersionModel(conn),
		Values:         model.NewFeatureValueModel(conn),
		Switches:       model.NewVersionSwitchModel(conn),
		Backfills:      model.NewBackfillJobModel(conn),
		Receipts:       model.NewWriteReceiptModel(conn),
	}
	for _, note := range ctx.Notes() {
		logx.Infof("feature-store/svc: %s", note)
	}
	return ctx
}

// newCache 构造业务缓存；Host 为空时返回 nil（配置校验本会拒绝，
// 但 nil 检查仍留在调用点：单测与灰度环境都可能不带缓存启动）。
func newCache(c config.Config) *redis.Redis {
	if c.CacheRedis.Host == "" {
		return nil
	}
	return redis.MustNewRedis(c.CacheRedis)
}

// Notes 输出装配期诊断，让运维一眼看到「哪些能力在这个环境必然降级」。
func (s *ServiceContext) Notes() []string {
	notes := make([]string, 0, 3)
	if s.Cache == nil {
		notes = append(notes, "CacheRedis 未配置：在线读没有主存层，所有请求将以 "+
			"SOURCE_UNAVAILABLE 降级而不是被当成冷数据（绝不静默回退成「只读 DB 也算正常」）")
	}
	if !s.Config.Backfill.WorkerEnabled {
		notes = append(notes, "Backfill.WorkerEnabled=false：回填作业只落台账，取数来源适配器 "+
			"(internal/featuresource) 未创建，PENDING 作业不会被本实例认领（README §10 已知缺口）")
	}
	if len(s.Config.Privacy.OperatorPrefixes) == 0 {
		notes = append(notes, "Privacy.OperatorPrefixes 为空：EraseEntityFeatures 一律拒绝（fail closed），"+
			"这是合规缺陷，Config.Validate 会在启动时拦住")
	}
	return notes
}

// ReceiptLeaseSeconds 供各写方法取回执租约，越界值夹回 model 上界。
func (s *ServiceContext) ReceiptLeaseSeconds() int64 {
	v := s.Config.Write.ReceiptLeaseSeconds
	if v <= 0 {
		v = model.MaxReceiptLeaseSeconds / 5
	}
	if v > model.MaxReceiptLeaseSeconds {
		v = model.MaxReceiptLeaseSeconds
	}
	return v
}

// MaxResponseBytes 单次批量读的响应体上限（配置只能比 model 硬上限更严）。
func (s *ServiceContext) MaxResponseBytes() int {
	v := s.Config.Read.MaxBatchResponseBytes
	if v <= 0 || v > model.MaxBatchResponseBytes {
		v = model.MaxBatchResponseBytes
	}
	return v
}

// PurgeLimit 单次清理/擦除的行数上限。
func (s *ServiceContext) PurgeLimit() int32 {
	v := s.Config.Write.MaxPurgeRowsPerCall
	if v <= 0 || v > model.MaxPurgeRows {
		return model.MaxPurgeRows
	}
	return v
}

// String 便于日志里确认装配结果（不含任何密钥与主体标识）。
func (s *ServiceContext) String() string {
	return fmt.Sprintf("feature-store{schema=%s cache=%v worker=%v}",
		config.DatabaseName, s.Cache != nil, s.Config.Backfill.WorkerEnabled)
}
