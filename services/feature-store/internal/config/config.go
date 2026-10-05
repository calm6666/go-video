// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"

	"go-video/services/feature-store/model"
)

// DatabaseName 是本服务独占的 schema 名（AGENTS.md §5：服务只能写自己的数据库）。
// 迁移脚本 deploy/migrations/feature-store/*.sql 与 Validate 的自检都用这一个常量，
// 避免「DSN 指向别人的库」这种要到第一次写入才暴露的错误。
const DatabaseName = "go_video_feature_store"

// Config 是 feature-store 服务的配置结构。
//
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向端的响应聚合与信封由 gateway 负责。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载在线特征值主读缓存（fs:val:...）与 ACTIVE 版本指针缓存（fs:active:...）。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名的 redis.RedisKeyConf 字段（限流用），
	// 同名字段会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	//
	// 这里存的才是「在线读的主存」：feature_value 表只是回源兜底与隐私擦除的持久层，
	// 可从上游重算（见 deploy/migrations/feature-store/000003 文件头的事实定位）。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_feature_store 库的 MySQL DSN。
	// 密钥与真实地址由环境变量/配置中心注入，示例配置只保留结构（AGENTS.md §4）。
	DataSource string

	// Read 是在线读路径参数（GetFeature / BatchGetFeatures / ListEntityFeatures）。
	Read ReadConf

	// Write 是写路径与幂等回执参数（WriteFeatures 及 8 个写方法的 receipt 生命周期）。
	Write WriteConf

	// Backfill 是回填作业参数（worker 认领租约与批大小）。
	Backfill BackfillConf

	// Privacy 是隐私类操作的授权参数（EraseEntityFeatures / ListEntityFeatures）。
	Privacy PrivacyConf
}

// ReadConf 在线读参数。
//
// 上限之所以全部可配而不写死在代码里：条数与响应体上限的正确值取决于
// 调用方（recommend-rank 一次打分要读几十路特征）和 gRPC 消息上限，
// 这两项都会随部署变化，改配置比改契约安全。
type ReadConf struct {
	// MaxBatchResponseBytes 单次批量读的响应体硬上限（字节）。
	// 只能下调，上界是 model.MaxBatchResponseBytes：条数上限挡不住
	// 「50 个 512 维向量」这种组合，条目合法但响应能长到几十 MB。
	MaxBatchResponseBytes int `json:",default=1048576"`
	// CacheJitterRatio 缓存 TTL 的向下抖动比例（0..0.5，见 model.CacheTTL）。
	// 上游按分钟批量写入，一批键的 expire_at 完全相同；缓存 TTL 若也取整值，
	// 就会在同一秒集体失效并让 DB 兜底路径承受惊群。
	CacheJitterRatio float64 `json:",default=0.2"`
	// ActivePointerCacheSeconds ACTIVE 版本指针的缓存秒数。
	// 必须显著短于切换审计到人眼能看到的时长：指针缓存陈旧 = 切了版本但在线还在读旧版本。
	ActivePointerCacheSeconds int64 `json:",default=10"`
	// DBFallbackEnabled false = 缓存 miss 时不回源 feature_value，直接按降级返回。
	// 只在 DB 故障演练与压测时关闭，让「缓存层单独能撑多久」可被观测。
	DBFallbackEnabled bool `json:",default=true"`
}

// WriteConf 写路径与幂等回执参数。
type WriteConf struct {
	// ReceiptLeaseSeconds 执行权租约秒数，上界 model.MaxReceiptLeaseSeconds：
	// 超过它说明调用方把回执当成了「长期占位」，正常请求（含 500 行批量写）都在秒级完成。
	ReceiptLeaseSeconds int64 `json:",default=60"`
	// ReceiptRetentionSeconds 回执保留秒数（cron 据此清理 feature_write_receipt）。
	// 幂等窗口必须大于上游的最大重试跨度：删早了等于作废幂等键，
	// 上游重试会拿到「新执行一遍」的结果而不是回放。
	ReceiptRetentionSeconds int64 `json:",default=604800"`
	// MaxPurgeRowsPerCall 单次 PurgeExpired 的清理行数上限，
	// 上界 model.MaxPurgeRows（契约里的 5000）。
	MaxPurgeRowsPerCall int32 `json:",default=5000"`
}

// BackfillConf 回填作业参数。model 侧对每个值都留了兜底常量，
// 配置缺失时租约不会退化成「永久持有」。
type BackfillConf struct {
	// LeaseSeconds worker 认领作业的租约秒数，AddProgress 心跳用同一个值续期。
	// 心跳若用另一个值，一次心跳就会把长批次作业的租约缩回默认值、跑到一半被接管。
	LeaseSeconds int64 `json:",default=120"`
	// BatchRows 单次推进的主体行数上限。
	// 与 Backfill.BatchRows 对应的是 worker 写 feature_value 的批大小，
	// 不是 RPC 的 500 行上限（那是对外契约的约束，两者不该混用）。
	BatchRows int32 `json:",default=1000"`
	// WorkerEnabled true = 本实例认领并执行回填作业。
	// 默认 false：取数来源适配器（internal/featuresource）还没建，
	// 打开它只会让作业稳定 FAILED 并刷错误日志（见 README「当前阶段」）。
	WorkerEnabled bool `json:",default=false"`
}

// PrivacyConf 隐私类操作参数。
type PrivacyConf struct {
	// OperatorPrefixes 允许触发 EraseEntityFeatures 的 operator 前缀白名单。
	// 空白名单 = 谁都不能擦除（model.OperatorInPrefixes 是 fail closed 的），
	// 因此 Validate 把它当成启动期错误：隐私擦除不可用是合规缺陷，不是可接受的稳态。
	// 名单来自配置而不是写死在 model：跑隐私工单的服务名在各环境不同，
	// 写死就等于要么误拒要么改代码。
	OperatorPrefixes []string
	// ExportMaxPrivacyLevel 主体自助导出（ListEntityFeatures）可见的最高隐私级别。
	// 默认 4（含用户画像）：主体核对自己的特征就该看到全部，
	// 收紧它是对「自助核对」这条合规路径的削弱，只用于专项调查窗口。
	ExportMaxPrivacyLevel int32 `json:",default=4"`
}

// Validate 是启动期自检：把「能加载但语义危险」的配置组合在启动时就拦下。
//
// 这些检查为什么不能只靠 conf.Load 的 range 标签：
//   - 跨字段约束（幂等保留期 vs 租约、DSN 库名 vs 本服务 schema）不是单字段范围能表达的；
//   - 与 model 常量的对账（配置只能收紧、不能突破服务端硬上限）必须在运行时比；
//   - 隐私白名单为空是「静默失效」而不是「报错」，最容易在上线后才被发现。
//
// 返回错误即启动失败，不允许带着自相矛盾的特征存储配置上线。
func (c Config) Validate() error {
	if strings.TrimSpace(c.DataSource) == "" {
		return errors.New("feature-store: DataSource is empty")
	}
	// DSN 必须指向本服务自己的 schema：连到别人的库会在第一次写入时才暴露，
	// 那时已经污染了别人的数据（AGENTS.md §5 数据所有权）。
	if !strings.Contains(c.DataSource, DatabaseName) {
		return fmt.Errorf("feature-store: DataSource must point at schema %q", DatabaseName)
	}
	if strings.TrimSpace(c.CacheRedis.Host) == "" {
		// 在线读的主存就是这个缓存；漏配会让所有读稳定走 DB 兜底并被误判为「冷数据」。
		return errors.New("feature-store: CacheRedis.Host is empty")
	}

	if c.Read.MaxBatchResponseBytes <= 0 {
		return fmt.Errorf("feature-store: Read.MaxBatchResponseBytes must be positive, got %d",
			c.Read.MaxBatchResponseBytes)
	}
	if c.Read.MaxBatchResponseBytes > model.MaxBatchResponseBytes {
		return fmt.Errorf("feature-store: Read.MaxBatchResponseBytes %d exceeds server hard limit %d",
			c.Read.MaxBatchResponseBytes, model.MaxBatchResponseBytes)
	}
	if c.Read.CacheJitterRatio < 0 || c.Read.CacheJitterRatio > 0.5 {
		// 上界 0.5 与 model.CacheTTL 一致：再抖就有半数键的可用窗口短于半个 TTL。
		return fmt.Errorf("feature-store: Read.CacheJitterRatio %v out of 0..0.5",
			c.Read.CacheJitterRatio)
	}
	if c.Read.ActivePointerCacheSeconds <= 0 {
		return fmt.Errorf("feature-store: Read.ActivePointerCacheSeconds must be positive, got %d",
			c.Read.ActivePointerCacheSeconds)
	}

	if c.Write.ReceiptLeaseSeconds <= 0 {
		return fmt.Errorf("feature-store: Write.ReceiptLeaseSeconds must be positive, got %d",
			c.Write.ReceiptLeaseSeconds)
	}
	if c.Write.ReceiptLeaseSeconds > model.MaxReceiptLeaseSeconds {
		return fmt.Errorf("feature-store: Write.ReceiptLeaseSeconds %d exceeds %d",
			c.Write.ReceiptLeaseSeconds, model.MaxReceiptLeaseSeconds)
	}
	if c.Write.ReceiptRetentionSeconds <= c.Write.ReceiptLeaseSeconds {
		return fmt.Errorf("feature-store: Write.ReceiptRetentionSeconds(%d) must be greater than "+
			"Write.ReceiptLeaseSeconds(%d)", c.Write.ReceiptRetentionSeconds, c.Write.ReceiptLeaseSeconds)
	}
	if c.Write.MaxPurgeRowsPerCall <= 0 || c.Write.MaxPurgeRowsPerCall > model.MaxPurgeRows {
		return fmt.Errorf("feature-store: Write.MaxPurgeRowsPerCall %d out of 1..%d",
			c.Write.MaxPurgeRowsPerCall, model.MaxPurgeRows)
	}

	if c.Backfill.LeaseSeconds <= 0 {
		return fmt.Errorf("feature-store: Backfill.LeaseSeconds must be positive, got %d",
			c.Backfill.LeaseSeconds)
	}
	if c.Backfill.BatchRows <= 0 || c.Backfill.BatchRows > 10000 {
		return fmt.Errorf("feature-store: Backfill.BatchRows %d out of 1..10000",
			c.Backfill.BatchRows)
	}

	// fail closed：白名单为空时 model.OperatorInPrefixes 会拒绝所有擦除请求。
	// 这在功能上「安全」，但等于隐私工单永远跑不完，必须启动就报出来。
	if len(c.Privacy.OperatorPrefixes) == 0 {
		return errors.New("feature-store: Privacy.OperatorPrefixes is empty; " +
			"EraseEntityFeatures could never be executed (privacy ticket would stall)")
	}
	for _, p := range c.Privacy.OperatorPrefixes {
		if strings.TrimSpace(p) == "" {
			return errors.New("feature-store: Privacy.OperatorPrefixes contains a blank entry")
		}
	}
	if c.Privacy.ExportMaxPrivacyLevel != 0 &&
		!model.ValidPrivacyLevel(c.Privacy.ExportMaxPrivacyLevel) {
		return fmt.Errorf("feature-store: Privacy.ExportMaxPrivacyLevel %d is not a declared level",
			c.Privacy.ExportMaxPrivacyLevel)
	}
	return nil
}
