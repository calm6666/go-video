// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package svc

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	auditrpc "go-video/services/audit/rpc"
	"go-video/services/ops-config/internal/config"
	"go-video/services/ops-config/model"
)

// CacheKV 是 logic 读写运行时投影所需的最小缓存面。
//
// 为什么不直接把字段类型写成 *redis.Redis：
//   - logic 的单测必须在**没有 Redis、没有 MySQL** 的条件下跑（AGENTS.md §9），
//     接口是这个包的唯一注入点，否则只能给每个用例起一个真 Redis；
//   - 这一层是「可整域重建的只读投影」，实现只有 redisCache 一个；
//   - 未配置 Redis 时 newRedisCache 仍然返回一个非 nil 实现（所有读都算 miss、
//     所有写都只记日志），这样 logic 不必到处判 nil，也不会把缓存故障放大成请求失败。
type CacheKV interface {
	// Get 读一个键；未命中返回 ("", nil)。真出错返回非 nil error（调用方按 miss 处理并记日志）。
	Get(ctx context.Context, key string) (string, error)
	// Setex 写一个带 TTL 的键；seconds<=0 视为「不缓存」，直接返回 nil。
	Setex(ctx context.Context, key, value string, seconds int) error
	// Del 删除若干键，返回**实际删除**的条数（RefreshCache 的 affected 回的就是它）。
	Del(ctx context.Context, keys ...string) (int64, error)
}

type redisCache struct {
	rds *redis.Redis
}

func newRedisCache(rds *redis.Redis) CacheKV { return &redisCache{rds: rds} }

func (c *redisCache) Get(ctx context.Context, key string) (string, error) {
	if c == nil || c.rds == nil {
		return "", nil
	}
	v, err := c.rds.GetCtx(ctx, key)
	if err != nil {
		// go-zero 把「键不存在」也当错误抛出；缓存 miss 不是故障，
		// 交给调用方按 miss 处理，避免一次未命中在日志里长成一条 Error。
		return "", err
	}
	return v, nil
}

func (c *redisCache) Setex(ctx context.Context, key, value string, seconds int) error {
	if c == nil || c.rds == nil || seconds <= 0 {
		return nil
	}
	return c.rds.SetexCtx(ctx, key, value, seconds)
}

func (c *redisCache) Del(ctx context.Context, keys ...string) (int64, error) {
	if c == nil || c.rds == nil || len(keys) == 0 {
		return 0, nil
	}
	// go-zero 的 Del 返回 int（删除条数），这里统一到 int64 供 affected 直接使用。
	n, err := c.rds.DelCtx(ctx, keys...)
	return int64(n), err
}

// Models 聚合本服务自有 8 张表的 model。
//
// 聚合成一个结构体的理由不是「少写几行」，而是**跨表事务必须由持有同一个
// sqlx.SqlConn 的 model 参与**：PublishConfig 要在一个事务里
// 「插版本行 → 条件 UPDATE 推 latest_version/epoch」，两张表走不同连接就会出现
// 一个已提交、一个回滚的半发布状态（AGENTS.md §5、§8）。
// 逻辑轮若把事务编排下沉到 internal/repository，这个结构体就是 repository 的依赖集。
type Models struct {
	// ConfigItem ops_config_item：配置项身份 + 当前正式版本指针 + 缓存代次。
	ConfigItem model.ConfigItemModel
	// ConfigVersion ops_config_version：不可变版本快照（发布与回滚的历史事实源）。
	ConfigVersion model.ConfigVersionModel
	// RolloutRule ops_rollout_rule：灰度规则（放量决策的证据，收口靠 state 不靠删除）。
	RolloutRule model.RolloutRuleModel
	// Topic ops_topic：专题主记录（只引用 catalog 的分区/标签 ID）。
	Topic model.TopicModel
	// TopicItem ops_topic_item：专题条目（只引用内容主键）。
	TopicItem model.TopicItemModel
	// Slot ops_recommend_slot：坑位定义。
	Slot model.RecommendSlotModel
	// SlotItem ops_recommend_slot_item：坑位条目与排期。
	SlotItem model.SlotItemModel
	// ClientSwitch ops_client_switch：按端的能力开关。
	ClientSwitch model.ClientSwitchModel
}

// ServiceContext 是 ops-config 服务的运行时上下文。
type ServiceContext struct {
	Config config.Config
	// Conn 本服务自有库连接（库名 go_video_ops_config）。
	// 暴露给 logic 是为了发起跨表事务；严禁用它连别人的库（AGENTS.md §5）。
	Conn sqlx.SqlConn
	// Cache 运行时读缓存。它是**可整域重建的只读投影**，不是事实源：
	// 缓存丢失只会让第一次解析回源打库，不会改变任何判定结果。
	Cache CacheKV
	// Models 8 张自有表的 model 集合。
	Models Models
	// Audit 唯一下游：本服务写自身配置动作的存证（见 rpc/opsconfig.proto 文件头）。
	// nil 表示本环境未配 AuditRPC：写接口照常推进配置状态机，但 audit_entry_id 回 0
	// 并打 Error 日志，由补偿任务重投 —— 绝不因为下游不可用就伪造一个 entry_id。
	Audit auditrpc.AuditClient
}

// NewServiceContext 构造 ServiceContext。
//
// 启动期策略与仓库内其它服务一致：Redis 走 MustNewRedis（本地未起 Redis 会明确失败，
// 而不是「看起来起来了、所有读请求都在回源」），MySQL 客户端惰性建连。
// 注意这里不做「库连不上就不启动」的预检：库不可用属于运行期故障，
// 由健康检查与 logic 的超时处理暴露，启动预检会把一次抖动放大成整服务重启。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)
	rds := redis.MustNewRedis(c.CacheRedis)
	return &ServiceContext{
		Config: c,
		Conn:   conn,
		Cache:  newRedisCache(rds),
		Models: Models{
			ConfigItem:    model.NewConfigItemModel(conn),
			ConfigVersion: model.NewConfigVersionModel(conn),
			RolloutRule:   model.NewRolloutRuleModel(conn),
			Topic:         model.NewTopicModel(conn),
			TopicItem:     model.NewTopicItemModel(conn),
			Slot:          model.NewRecommendSlotModel(conn),
			SlotItem:      model.NewSlotItemModel(conn),
			ClientSwitch:  model.NewClientSwitchModel(conn),
		},
		Audit: newAuditClient(c.AuditRPC),
	}
}

// newAuditClient 只在显式配置了下游时才建客户端（与 cron 的下游装配方式一致）：
// 本地无 etcd 的环境里 MustNewClient 会让服务起不来，而审计缺失只应是
// 「audit_entry_id 留 0 + Error 日志」这种可见缺口，不应阻塞配置发布。
func newAuditClient(c zrpc.RpcClientConf) auditrpc.AuditClient {
	if c.Target == "" && len(c.Etcd.Hosts) == 0 && len(c.Endpoints) == 0 {
		logx.Infof("ops-config/svc: AuditRPC 未配置，写接口的 audit_entry_id 将回 0 并打 Error 日志，由补偿任务重投")
		return nil
	}
	return auditrpc.NewAuditClient(zrpc.MustNewClient(c).Conn())
}
