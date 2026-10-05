// Package repository 是 live-ingest 的数据访问与外部依赖入口。
//
// 它把本服务的 MySQL model 集合、CacheRedis 和外部媒体入口适配器收敛成一个结构，
// logic 层只依赖这里，不直接拼 SQL、不直接持有 *redis.Redis（AGENTS.md §4：
// handler 管参数与鉴权上下文、logic 管用例、repository 管持久化）。
//
// 本服务的 Redis 键空间（全部 live-ingest 自有前缀 lingest:，其他服务禁止读写）：
//
//	lingest:auth:<key_hash_short>   接入鉴权结果短缓存（不含明文密钥与哈希原值）
//	lingest:nonce:<nonce>           CDN 回调防重放标记（TTL = Cdn 配置窗口）
//	lingest:node:score:<region>     节点打分快照（分配时减少 DB 扫描）
//	lingest:stream:<stream_id>      流状态读缓存（终态立即失效）
package repository

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/live-ingest/internal/config"
	"go-video/services/live-ingest/model"
)

// Repository 聚合本服务自有表的 model 与缓存客户端。
type Repository struct {
	conn sqlx.SqlConn
	rds  *redis.Redis

	// StreamKey 推流密钥（哈希/引用/轮转链）。
	StreamKey model.StreamKeyModel
	// IngestNode 接入节点注册与配额。
	IngestNode model.IngestNodeModel
	// NodeAssignment 节点分配记录。
	NodeAssignment model.NodeAssignmentModel
	// Stream 流状态机主体。
	Stream model.StreamModel
	// StreamInterruption 断流与重连记录。
	StreamInterruption model.StreamInterruptionModel
	// StreamHealthReport 健康采样事实。
	StreamHealthReport model.StreamHealthReportModel
	// StreamEvent 状态迁移事件（append-only，live.state.v1 的事实来源）。
	StreamEvent model.StreamEventModel
	// Outbox 事件投递位点（同事务写、异步发布、按 event_id 幂等）。
	Outbox model.EventOutboxModel
	// CdnCallback 回调留证与 nonce 防重放。
	CdnCallback model.CdnCallbackModel

	// Redis 暴露缓存客户端，供鉴权结果与 nonce 防重放使用。
	Redis *redis.Redis

	// MaxListPageSize 分页每页上限（来自配置，logic 夹取时用）。
	MaxListPageSize int32
}

// New 构造 Repository。
// 所有 model 共享同一个 sqlx.SqlConn：跨表事务由 logic 通过 conn.TransactCtx
// 开启，并把 sqlx.Session 逐层传给 model，保证「状态迁移 + 事件 + Outbox」原子提交。
func New(conn sqlx.SqlConn, rds *redis.Redis, c config.Config) *Repository {
	return &Repository{
		conn:               conn,
		rds:                rds,
		StreamKey:          model.NewStreamKeyModel(conn),
		IngestNode:         model.NewIngestNodeModel(conn),
		NodeAssignment:     model.NewNodeAssignmentModel(conn),
		Stream:             model.NewStreamModel(conn),
		StreamInterruption: model.NewStreamInterruptionModel(conn),
		StreamHealthReport: model.NewStreamHealthReportModel(conn),
		StreamEvent:        model.NewStreamEventModel(conn),
		Outbox:             model.NewEventOutboxModel(conn),
		CdnCallback:        model.NewCdnCallbackModel(conn),
		Redis:              rds,
		MaxListPageSize:    c.LiveIngest.MaxListPageSize,
	}
}

// Conn 返回底层连接。logic 用它开启 TransactCtx 事务，再把 sqlx.Session 逐层
// 传给各 model：迁移矩阵要求「改状态 + 写事件 + 写断流记录 + 写 Outbox」必须原子
// （AGENTS.md §5：业务写与 Outbox 同事务）。
func (r *Repository) Conn() sqlx.SqlConn { return r.conn }
