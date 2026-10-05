// Package repository 是 video 服务的数据访问层。
// 组合 video_submission、video_version、video_audit_log、video_outbox 4 个 model，
// 为 logic 层提供稿件、版本、审计的统一数据访问入口。
// TransitionState 在事务内同时写 video_submission.state、video_audit_log，
// 以及（改变对外可见性时）一行 content.published.v1 事件（装配见 contentevent.go，
// 投递由 internal/publisher 的发布循环负责）。
// 稿件详情走 Redis 短 TTL 缓存，稿件变更时失效。
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/video/model"
)

// Cache 封装 video 的 Redis 缓存操作。
// 仅缓存稿件详情（Submission 单条记录）；列表查询不缓存（流量大、组合多）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixSubmission = "video:sub:%d" // aid → 稿件详情 JSON

	cacheTTLSubmission = 60 // 稿件详情缓存 60 秒
)

func keySubmission(aid int64) string { return fmt.Sprintf(prefixSubmission, aid) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("video/cache: redis ping failed")
}

// DelSubmission 删除稿件详情缓存（稿件变更时调用）。
func (c *Cache) DelSubmission(ctx context.Context, aid int64) error {
	_, err := c.rds.DelCtx(ctx, keySubmission(aid))
	return err
}

// Cacher 是 Repository 对缓存层的最小依赖面（生产由 *Cache 实现）。
//
// 为什么要有这个接口：Repository.cache 原先是具体类型 *Cache，而 ServiceContext.Repository
// 又是具体类型 *repository.Repository，logic 单测无处塞替身，只能连真 Redis。抽出接口后
// 测试用 NewWithDeps(内存缓存, 假连接, 内存 model) 组装**真实的 Repository**，
// 「状态推进后是否失效详情缓存」「更新是否先落库再失效」这些判定仍整条在被测路径上
// （见 internal/logic/fakes_test.go）。只声明 Repository 实际调用的方法。
type Cacher interface {
	Ping(ctx context.Context) error
	DelSubmission(ctx context.Context, aid int64) error
}

var _ Cacher = (*Cache)(nil)

// Repository 是 video 服务的数据访问入口。
type Repository struct {
	cache   Cacher
	conn    sqlx.SqlConn
	subMd   model.VideoSubmissionModel
	verMd   model.VideoVersionModel
	auditMd model.VideoAuditLogModel
	// outboxMd 承载 content.published.v1 事件行：TransitionState 的第三次写入。
	outboxMd model.VideoOutboxModel
}

// New 构造 Repository（生产路径唯一入口）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn,
		model.NewVideoSubmissionModel(conn),
		model.NewVideoVersionModel(conn),
		model.NewVideoAuditLogModel(conn),
		model.NewVideoOutboxModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
// conn 仍然必需：TransitionState 的「稿件状态 + 审计行 + 事件行」必须走同一个 TransactCtx。
// outboxMd 允许传 nil：不产事件的用例（纯读侧、状态机单测）不必为此造一个假 outbox。
// 但 nil 时 TransitionState 遇到可见性转换会直接报错，不会「状态改了而事件静默丢失」。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn,
	subMd model.VideoSubmissionModel, verMd model.VideoVersionModel,
	auditMd model.VideoAuditLogModel, outboxMd model.VideoOutboxModel) *Repository {
	return &Repository{cache: cache, conn: conn, subMd: subMd, verMd: verMd, auditMd: auditMd, outboxMd: outboxMd}
}

// OutboxModel 暴露 video_outbox 的 model，供 internal/publisher 装配发布循环。
func (r *Repository) OutboxModel() model.VideoOutboxModel { return r.outboxMd }

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 稿件 ---

// CreateSubmission 新建稿件（DRAFT 状态），返回新 aid。
func (r *Repository) CreateSubmission(ctx context.Context, s *model.VideoSubmission) (int64, error) {
	return r.subMd.Insert(ctx, s)
}

// GetSubmission 查询稿件详情（暂不走缓存，列表/详情流量较小时直接 DB；
// 若需要缓存可在此处加 cache-aside）。
func (r *Repository) GetSubmission(ctx context.Context, aid int64) (*model.VideoSubmission, error) {
	return r.subMd.FindOne(ctx, aid)
}

// ListSubmissions 分页查询稿件（mid/typeid 可选过滤）。
func (r *Repository) ListSubmissions(ctx context.Context, mid int64, typeid int32, pn, ps int32) ([]*model.VideoSubmission, int32, error) {
	return r.subMd.List(ctx, mid, typeid, pn, ps)
}

// ListByState 按状态分页查询稿件。
func (r *Repository) ListByState(ctx context.Context, state int32, pn, ps int32) ([]*model.VideoSubmission, int32, error) {
	return r.subMd.ListByState(ctx, state, pn, ps)
}

// UpdateSubmissionFields 更新稿件元信息。
func (r *Repository) UpdateSubmissionFields(ctx context.Context, aid int64, title, desc, cover string, typeid int32, tag string) error {
	if err := r.subMd.UpdateFields(ctx, aid, title, desc, cover, typeid, tag); err != nil {
		return err
	}
	_ = r.cache.DelSubmission(ctx, aid)
	return nil
}

// TransitionState 在事务内推进稿件状态机：
//  1. 校验 from → to 是否合法（由 logic 调用前用 canTransition 判断）；
//  2. 同事务写 video_submission.state、video_audit_log；
//  3. 该转换改变对外可见性时，同事务写一行 content.published.v1 到 video_outbox。
//
// 不合法转换由 logic 层返回 ErrInvalidStateTransition，本方法不重复校验。
// 第 3 步是 AGENTS.md §5「事务内写业务数据和 Outbox 记录」的落点：状态改了而事件没落库，
// 搜索投影和作者通知会永久落后，且没有任何事后可补投的依据；因此事件组装失败必须整笔回滚。
func (r *Repository) TransitionState(ctx context.Context, aid int64, fromState, toState int32, operator, reason string) error {
	return r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		// 二次校验当前状态匹配 fromState，避免并发覆盖。
		cur, err := r.subMd.FindOne(ctx, aid)
		if err != nil {
			return err
		}
		if cur == nil {
			return model.ErrSubmissionNotFound
		}
		if cur.State != fromState {
			return model.ErrInvalidStateTransition
		}
		// 审计时间只取一次：事件行的 doc_revision 由它派生，两处不同源就没法用版本号反查审计行。
		auditCtime := nowUnix()
		evt, err := r.buildVisibilityEvent(ctx, cur, fromState, toState, reason, auditCtime)
		if err != nil {
			return err
		}
		if err := r.subMd.UpdateState(ctx, session, aid, toState); err != nil {
			return err
		}
		log := &model.VideoAuditLog{
			Aid:       aid,
			FromState: fromState,
			ToState:   toState,
			Operator:  operator,
			Reason:    reason,
			Ctime:     auditCtime,
		}
		if err := r.auditMd.Insert(ctx, session, log); err != nil {
			return err
		}
		if evt == nil {
			return nil
		}
		return r.outboxMd.Insert(ctx, session, &model.VideoOutbox{
			EventID:       evt.env.EventID,
			EventType:     evt.env.EventType,
			SchemaVersion: int32(evt.env.SchemaVersion),
			AggregateType: evt.env.AggregateType,
			AggregateID:   evt.env.AggregateID,
			Payload:       evt.payload,
			State:         model.OutboxStatePending,
			OccurredAt:    evt.occurredAt,
		})
	})
}

// InvalidateSubmissionCache 失效稿件详情缓存（状态流转后由 logic 调用）。
func (r *Repository) InvalidateSubmissionCache(ctx context.Context, aid int64) error {
	return r.cache.DelSubmission(ctx, aid)
}

// --- 稿件版次（播放来源解析） ---

// LatestPlayableVersion 返回稿件当前可对外播放的版次：
// video_version 按 version 倒序后第一个带非空 asset_id 的记录。
// 没有带媒资的版次时返回 nil, nil，由 logic 判定为不可播放（不返回半成品版次）。
func (r *Repository) LatestPlayableVersion(ctx context.Context, aid int64) (*model.VideoVersion, error) {
	rows, err := r.verMd.ListByAid(ctx, aid)
	if err != nil {
		return nil, err
	}
	for _, v := range rows {
		if v.AssetID != "" {
			return v, nil
		}
	}
	return nil, nil
}

// nowUnix 返回当前 Unix 秒时间戳（repository 内部用）。
func nowUnix() int64 { return time.Now().Unix() }
