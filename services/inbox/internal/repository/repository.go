// Package repository 是 inbox 服务的数据访问层：组合 MySQL 模型与 Redis 未读
// 加速层，为 logic 与 internal/consumer 提供同一套写入入口。
//
// 一致性约定（AGENTS.md §5、docs/api-and-events.md §6）：
//   - 真值永远在 inbox_message / inbox_user_message；
//     inbox_unread_stat 是可由明细表重算的快照，Redis 只是快照的加速副本。
//   - 写操作在事务内同时改明细与快照，提交后只失效 Redis，不做增量加减，
//     因此进程崩溃或 Redis 丢数据都只会退化成回源查询，不会让计数长期漂移。
package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/inbox/internal/config"
	"go-video/services/inbox/model"
)

// DeliverResult 一次投递的结果。
type DeliverResult struct {
	MsgID        int64 // 消息主体 ID
	Delivered    int32 // 本次新增的收件行数
	Deduplicated bool  // true 表示命中 idempotency_key，未重复写入
	Ctime        int64 // 消息创建时间（Unix 秒）
}

// UnreadSnapshot 未读计数快照。
type UnreadSnapshot struct {
	ByCategory map[int32]int64
	Total      int64
	Mtime      int64  // 快照更新时间（Unix 秒）
	Source     string // 数据来源：cache / stat / recompute，便于排查计数漂移
}

// 未读快照来源标记。
const (
	SourceCache     = "cache"
	SourceStat      = "stat"
	SourceRecompute = "recompute"
)

// Repository 是 inbox 服务的数据访问入口。
type Repository struct {
	conn     sqlx.SqlConn
	cache    UnreadCache
	msgMd    model.InboxMessageModel
	userMd   model.InboxUserMessageModel
	statMd   model.UnreadStatModel
	offsetMd model.ConsumerOffsetModel
	dlqMd    model.DeadLetterModel
	cfg      config.InboxConf
}

// New 构造 Repository。
func New(rds *redis.Redis, conn sqlx.SqlConn, c config.Config) *Repository {
	return newRepository(conn, NewCache(rds, c.Inbox.UnreadCacheSeconds), c.Inbox,
		model.NewInboxMessageModel(conn),
		model.NewInboxUserMessageModel(conn),
		model.NewUnreadStatModel(conn),
		model.NewConsumerOffsetModel(conn),
		model.NewDeadLetterModel(conn),
	)
}

// NewWithDeps 用显式依赖构造 Repository，仅供单测注入内存替身（AGENTS.md §9：
// 不得让单测连真实 MySQL/Redis，也不得用「永不失败的假实现」）。
//
// 该构造函数不是生产入口：生产路径仍然只走 New，由它按配置装配真实依赖。
// 之所以暴露出来，是因为 logic 包在仓库的另一层目录，拿不到包内的 newRepository，
// 又没有别的途径把 fake model / fake cache 放进 Repository。
func NewWithDeps(
	conn sqlx.SqlConn, cache UnreadCache, cfg config.InboxConf,
	msgMd model.InboxMessageModel, userMd model.InboxUserMessageModel, statMd model.UnreadStatModel,
	offsetMd model.ConsumerOffsetModel, dlqMd model.DeadLetterModel,
) *Repository {
	return newRepository(conn, cache, cfg, msgMd, userMd, statMd, offsetMd, dlqMd)
}

// newRepository 把依赖显式化，供单测注入 fake model 与 fake cache。
func newRepository(
	conn sqlx.SqlConn, cache UnreadCache, cfg config.InboxConf,
	msgMd model.InboxMessageModel, userMd model.InboxUserMessageModel, statMd model.UnreadStatModel,
	offsetMd model.ConsumerOffsetModel, dlqMd model.DeadLetterModel,
) *Repository {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 20
	}
	if cfg.MaxPageSize <= 0 {
		cfg.MaxPageSize = 50
	}
	if cfg.MaxRecipients <= 0 {
		cfg.MaxRecipients = 500
	}
	return &Repository{
		conn: conn, cache: cache, cfg: cfg,
		msgMd: msgMd, userMd: userMd, statMd: statMd, offsetMd: offsetMd, dlqMd: dlqMd,
	}
}

// --- 投递 ---

// Deliver 在同一事务内写消息主体与每个收件人的明细行，并同步未读快照。
// 幂等由两层保证：inbox_message.idempotency_key 唯一键阻止重复消息主体，
// inbox_user_message.uniq(mid,msg_id) 阻止同一收件人重复收件。
func (r *Repository) Deliver(
	ctx context.Context, msg *model.InboxMessage, mids []int64,
) (*DeliverResult, error) {
	recipients, err := r.normalizeRecipients(mids)
	if err != nil {
		return nil, err
	}
	if err := r.normalizeMessage(msg); err != nil {
		return nil, err
	}

	var (
		res     DeliverResult
		newMids []int64
	)
	err = r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		msgID, created, err := r.msgMd.InsertIdempotent(c, tx, msg)
		if err != nil {
			return err
		}
		res.MsgID = msgID
		res.Ctime = msg.Ctime
		if !created {
			// 消息主体已存在（含其收件行与快照），说明整个事务此前已提交过。
			res.Deduplicated = true
			return nil
		}
		for _, mid := range recipients {
			inserted, err := r.userMd.InsertIdempotent(c, tx, &model.InboxUserMessage{
				Mid:       mid,
				MsgID:     msgID,
				Category:  msg.Category,
				ReadState: model.ReadStateUnread,
				DelState:  model.DelStateNormal,
				Ctime:     msg.Ctime,
			})
			if err != nil {
				return err
			}
			if !inserted {
				continue
			}
			newMids = append(newMids, mid)
			if err := r.statMd.IncrBy(c, tx, mid, msg.Category, 1); err != nil {
				return err
			}
		}
		res.Delivered = int32(len(newMids))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(newMids) > 0 {
		r.cache.Invalidate(ctx, newMids...)
	}
	return &res, nil
}

// normalizeMessage 补齐默认值并校验内容约束（消费端与 RPC 端共用）。
func (r *Repository) normalizeMessage(msg *model.InboxMessage) error {
	if msg == nil {
		return fmt.Errorf("inbox: nil message")
	}
	if msg.Category == 0 {
		msg.Category = model.CategorySystem
	}
	if !model.ValidCategory(msg.Category) {
		return model.ErrInvalidCategory
	}
	if msg.MsgType == 0 {
		msg.MsgType = model.MsgTypeText
	}
	if msg.Title == "" && strings.TrimSpace(msg.Content) == "" {
		return model.ErrEmptyContent
	}
	if r.cfg.MaxContentBytes > 0 &&
		(len(msg.Title) > r.cfg.MaxContentBytes || len(msg.Content) > r.cfg.MaxContentBytes) {
		return fmt.Errorf("inbox: title or content exceeds %d bytes", r.cfg.MaxContentBytes)
	}
	if len(msg.IdempotencyKey) == 0 || len(msg.IdempotencyKey) > 128 {
		return model.ErrInvalidIdempotencyKey
	}
	if msg.Extra == "" {
		msg.Extra = "{}"
	}
	return nil
}

// normalizeRecipients 去掉非法 mid 并按出现顺序去重，超过上限直接拒绝。
func (r *Repository) normalizeRecipients(mids []int64) ([]int64, error) {
	if len(mids) == 0 {
		return nil, model.ErrEmptyRecipients
	}
	seen := make(map[int64]struct{}, len(mids))
	out := make([]int64, 0, len(mids))
	for _, mid := range mids {
		if mid <= 0 {
			continue
		}
		if _, ok := seen[mid]; ok {
			continue
		}
		seen[mid] = struct{}{}
		out = append(out, mid)
	}
	if len(out) == 0 {
		return nil, model.ErrEmptyRecipients
	}
	if r.cfg.MaxRecipients > 0 && len(out) > int(r.cfg.MaxRecipients) {
		return nil, model.ErrTooManyRecipients
	}
	return out, nil
}

// --- 列表 ---

// ListMessages 游标分页拉取收件箱，返回 (行, next_cursor, has_more, error)。
func (r *Repository) ListMessages(
	ctx context.Context, mid int64, category int32, cursor string, ps int32, unreadOnly bool,
) ([]*model.MessageRow, string, bool, error) {
	if mid <= 0 {
		return nil, "", false, model.ErrInvalidMid
	}
	if category != model.CategoryAll && !model.ValidCategory(category) {
		return nil, "", false, model.ErrInvalidCategory
	}
	size, err := r.effectivePageSize(ps)
	if err != nil {
		return nil, "", false, err
	}
	cur, err := DecodeCursor(cursor)
	if err != nil {
		return nil, "", false, err
	}
	// 多取一条用于判断是否还有下一页，避免额外 COUNT(*) 深分页开销。
	rows, err := r.userMd.List(ctx, model.ListFilter{
		Mid:        mid,
		Category:   category,
		UnreadOnly: unreadOnly,
		CursorTime: cur.Time,
		CursorID:   cur.ID,
		Limit:      size + 1,
	})
	if err != nil {
		return nil, "", false, err
	}
	hasMore := len(rows) > int(size)
	if hasMore {
		rows = rows[:size]
	}
	next := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = EncodeCursor(Cursor{Time: last.Ctime, ID: last.ID})
	}
	return rows, next, hasMore, nil
}

// effectivePageSize 归一化每页条数并限制上限（docs/api-and-events.md §2）。
func (r *Repository) effectivePageSize(ps int32) (int32, error) {
	if ps <= 0 {
		return r.cfg.PageSize, nil
	}
	if r.cfg.MaxPageSize > 0 && ps > r.cfg.MaxPageSize {
		return 0, model.ErrPsTooLarge
	}
	return ps, nil
}

// --- 已读 / 删除 ---

// MarkReadBatch 幂等批量标记已读，返回真实变更行数。
func (r *Repository) MarkReadBatch(ctx context.Context, mid int64, msgIDs []int64) (int64, error) {
	if len(msgIDs) == 0 {
		return 0, nil
	}
	return r.markReadBatch(ctx, mid, msgIDs)
}

func (r *Repository) markReadBatch(ctx context.Context, mid int64, msgIDs []int64) (int64, error) {
	var changed int64
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		n, err := r.userMd.MarkReadBatch(c, tx, mid, msgIDs)
		if err != nil {
			return err
		}
		changed = n
		if n == 0 {
			return nil // 重复调用：不触发快照重算，保持幂等且零副作用
		}
		return r.refreshStatTx(c, tx, mid)
	})
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		r.cache.Invalidate(ctx, mid)
	}
	return changed, nil
}

// MarkAllRead 幂等把某分类（0 全部）标记已读。
func (r *Repository) MarkAllRead(ctx context.Context, mid int64, category int32) (int64, error) {
	if category != model.CategoryAll && !model.ValidCategory(category) {
		return 0, model.ErrInvalidCategory
	}
	var changed int64
	err := r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		n, err := r.userMd.MarkAllReadBatch(c, tx, mid, category)
		if err != nil {
			return err
		}
		changed = n
		if n == 0 {
			return nil
		}
		return r.refreshStatTx(c, tx, mid)
	})
	if err != nil {
		return 0, err
	}
	if changed > 0 {
		r.cache.Invalidate(ctx, mid)
	}
	return changed, nil
}

// DeleteMessages 用户侧软删除：只影响本人收件行，不删除消息主体，也不影响其它收件人。
// 返回 (真实变更行数, 是否全部存在, error)。
func (r *Repository) DeleteMessages(ctx context.Context, mid int64, msgIDs []int64) (int64, bool, error) {
	if len(msgIDs) == 0 {
		return 0, false, nil
	}
	owned, err := r.userMd.CountOwned(ctx, mid, msgIDs)
	if err != nil {
		return 0, false, err
	}
	if owned == 0 {
		return 0, false, nil // 一条都不属于该用户：越权或不存在
	}
	var changed int64
	err = r.conn.TransactCtx(ctx, func(c context.Context, tx sqlx.Session) error {
		n, err := r.userMd.SoftDeleteBatch(c, tx, mid, msgIDs)
		if err != nil {
			return err
		}
		changed = n
		if n == 0 {
			return nil // 已删除过：幂等返回
		}
		return r.refreshStatTx(c, tx, mid)
	})
	if err != nil {
		return 0, true, err
	}
	if changed > 0 {
		r.cache.Invalidate(ctx, mid)
	}
	return changed, true, nil
}

// --- 未读计数 ---

// GetUnread 读未读快照：Redis -> inbox_unread_stat -> 明细表重算。
// force=true 时跳过前两级，直接从明细表重算，是计数漂移的修复入口。
func (r *Repository) GetUnread(ctx context.Context, mid int64, force bool) (*UnreadSnapshot, error) {
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if !force {
		if snapshot, ok := r.cache.Get(ctx, mid); ok {
			return &UnreadSnapshot{
				ByCategory: snapshot,
				Total:      model.SnapshotTotal(snapshot),
				Source:     SourceCache,
			}, nil
		}
		stats, err := r.statMd.ListByMid(ctx, mid)
		if err != nil {
			return nil, err
		}
		if len(stats) > 0 {
			snapshot := make(map[int32]int64, len(model.AllCategories()))
			var mtime int64
			for _, s := range stats {
				snapshot[s.Category] = s.Unread
				if s.Mtime > mtime {
					mtime = s.Mtime
				}
			}
			r.cache.Set(ctx, mid, snapshot)
			return &UnreadSnapshot{
				ByCategory: snapshot, Total: model.SnapshotTotal(snapshot), Mtime: mtime, Source: SourceStat,
			}, nil
		}
	}
	return r.RecomputeUnread(ctx, mid)
}

// RecomputeUnread 从 inbox_user_message 重算未读，回写快照并刷新 Redis。
// 快照与 Redis 都只是派生数据，因此该方法可任意次重复执行。
func (r *Repository) RecomputeUnread(ctx context.Context, mid int64) (*UnreadSnapshot, error) {
	if mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	rows, err := r.userMd.CountUnreadByCategory(ctx, mid)
	if err != nil {
		return nil, err
	}
	snapshot := model.Snapshot(rows)
	if err := r.statMd.ReplaceByMid(ctx, nil, mid, snapshot); err != nil {
		return nil, err
	}
	r.cache.Set(ctx, mid, snapshot)
	return &UnreadSnapshot{
		ByCategory: snapshot,
		Total:      model.SnapshotTotal(snapshot),
		Mtime:      time.Now().Unix(),
		Source:     SourceRecompute,
	}, nil
}

// refreshStatTx 在事务内用明细表结果覆盖快照，保证明细与快照同提交。
func (r *Repository) refreshStatTx(ctx context.Context, tx sqlx.Session, mid int64) error {
	rows, err := r.userMd.CountUnreadByCategory(ctx, mid)
	if err != nil {
		return err
	}
	return r.statMd.ReplaceByMid(ctx, tx, mid, model.Snapshot(rows))
}
