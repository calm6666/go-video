// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/common/ratelimit"
	"go-video/services/spm/internal/config"
	"go-video/services/spm/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 spm 的运行时上下文：go_video_spm 连接、缓存、10 张自有表的 model
// 与进程级限流器。logic 只通过这里取依赖，不在方法内建连接（AGENTS.md §4）。
//
// 这里刻意没有下游 RPC 客户端：spm 的输入面只有 MQ（事件）与 gRPC（口径登记/作业），
// 需要跨服务真值时读的是自己的 spm_content_projection 投影，
// 反过来把排序/推荐结果写回 spm 的通道在本契约里不存在。
type ServiceContext struct {
	Config config.Config

	// DB 是 go_video_spm 库连接。批量写入与「读口径 + 写窗口」的组合由
	// logic 侧 conn.TransactCtx 发起，model 层一律提供 WithSession 以复用同一事务。
	DB sqlx.SqlConn

	// Cache 是热榜分页与口径 ACTIVE 指针的加速层。真值始终在 MySQL，
	// 缓存缺失只回源、不报错，也绝不参与任何口径计算。
	Cache *redis.Redis

	// Events 行为事实（唯一事实源，只由消费者写）。
	Events model.BehaviorEventModel
	// Definitions 指标口径注册表（(metric_key, metric_version) 不可变）。
	Definitions model.MetricDefinitionModel
	// Windows 窗口指标投影（可从事实重算）。
	Windows model.MetricWindowModel
	// Watermarks 窗口闭合水位（GetMetric/BatchGetMetrics/ListHotSubjects 解析
	// window_start=0 与 stale 的依据，可从事实重算）。
	Watermarks model.WindowWatermarkModel
	// Interests 用户兴趣画像投影（可从事实重算）。
	Interests model.UserInterestModel
	// Retention 留存投影（可从事实重算）。
	Retention model.RetentionCohortModel
	// Jobs 聚合作业与租约（重算/回填的编排面）。
	Jobs model.AggregationJobModel
	// Contents 内容只读投影（榜单分区过滤与下架剔除）。
	Contents model.ContentProjectionModel
	// Offsets 事件消费状态机与位点。
	Offsets model.ConsumerOffsetModel
	// DeadLetters 死信留档（只存摘要与脱敏预览）。
	DeadLetters model.DeadLetterModel

	// ReadLimiter 进程级读侧令牌桶，保护 MySQL 投影表。
	ReadLimiter ratelimit.Limiter
	// WriteLimiter 进程级写侧令牌桶：WriteMetricWindow/SubmitAggregationJob 等
	// 计算链路入口的洪峰（离线回填引擎会成批提交）。
	WriteLimiter ratelimit.Limiter
}

// NewServiceContext 构造 ServiceContext。
//
// 配置自检失败直接 Severe 终止启动：spm 的危险配置（行为事实永不清理、
// 迟到容忍比窗口还长、MaxRetentionDay 超过事实留存期）都不会在单次请求里暴露，
// 而是让几周后的留存曲线静默少样本——宁可不启动，也不带着这类配置上线。
func NewServiceContext(c config.Config) *ServiceContext {
	if err := c.Validate(); err != nil {
		logx.Severe("spm/svc: 配置自检失败: ", err)
	}

	conn := sqlx.NewMysql(c.DataSource)
	readQps, writeQps := int(c.Spm.ReadQps), int(c.Spm.WriteQps)
	writeBurst := int(c.Spm.WriteBurst)
	if writeBurst < writeQps {
		writeBurst = writeQps
	}

	return &ServiceContext{
		Config:       c,
		DB:           conn,
		Cache:        redis.MustNewRedis(c.CacheRedis),
		Events:       model.NewBehaviorEventModel(conn),
		Definitions:  model.NewMetricDefinitionModel(conn),
		Windows:      model.NewMetricWindowModel(conn),
		Watermarks:   model.NewWindowWatermarkModel(conn),
		Interests:    model.NewUserInterestModel(conn),
		Retention:    model.NewRetentionCohortModel(conn),
		Jobs:         model.NewAggregationJobModel(conn),
		Contents:     model.NewContentProjectionModel(conn),
		Offsets:      model.NewConsumerOffsetModel(conn),
		DeadLetters:  model.NewDeadLetterModel(conn),
		ReadLimiter:  ratelimit.NewTokenBucket(readQps, readQps),
		WriteLimiter: ratelimit.NewTokenBucket(writeQps, writeBurst),
	}
}
