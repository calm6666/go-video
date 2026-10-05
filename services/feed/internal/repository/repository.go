// Package repository 是 feed 服务的数据访问层。
// 组合 feed_outbox/feed_inbox/feed_pin/feed_unread 4 个 model，
// 为 logic 层提供统一数据访问入口。
// 写扩散策略：PushFeed 时把新动态 fan-out 到所有粉丝的 Redis ZSet（feed:inbox:{mid}），
// 未读计数同步累加；PullFeed 走 ZREVRANGEBYSCORE 翻页；粉丝集合由 social-graph 关注事件写入。
package repository

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/feed/model"
)

// Cache 封装 feed 的 Redis 缓存操作。
// 关注流走 ZSet（score=ctime，member=feed_id）；
// 未读走 INCRBY 计数器；
// 粉丝集合走 Set（member=follower_mid），由 social-graph 关注事件写入。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 前缀。
const (
	prefixInbox     = "feed:inbox:%d"     // mid → ZSet (score=ctime, member=feed_id)
	prefixOutbox    = "feed:outbox:%d"    // mid → ZSet (score=ctime, member=feed_id)
	prefixPin       = "feed:pin:%d"       // mid → Set of pinned feed_ids
	prefixUnread    = "feed:unread:%d"    // mid → 计数器
	prefixFollowers = "feed:followers:%d" // mid → Set of follower mids（由 social-graph 写入）
)

func keyInbox(mid int64) string     { return fmt.Sprintf(prefixInbox, mid) }
func keyOutbox(mid int64) string    { return fmt.Sprintf(prefixOutbox, mid) }
func keyPin(mid int64) string       { return fmt.Sprintf(prefixPin, mid) }
func keyUnread(mid int64) string    { return fmt.Sprintf(prefixUnread, mid) }
func keyFollowers(mid int64) string { return fmt.Sprintf(prefixFollowers, mid) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("feed/cache: redis ping failed")
}

// --- 粉丝集合 ---

// GetFollowers 读取作者粉丝集合。
// 此集合由 social-graph 关注事件消费者写入，本服务只读不写。
// 返回 (mids, hit, err)：hit=false 表示集合缺失（应跳过 fan-out）。
// 用 EXISTS 区分"集合未初始化"与"集合为空"。
func (c *Cache) GetFollowers(ctx context.Context, authorMid int64) ([]int64, bool, error) {
	exists, err := c.rds.ExistsCtx(ctx, keyFollowers(authorMid))
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	vals, err := c.rds.SmembersCtx(ctx, keyFollowers(authorMid))
	if err != nil {
		return nil, true, err
	}
	if len(vals) == 0 {
		return nil, true, nil
	}
	out := make([]int64, 0, len(vals))
	for _, v := range vals {
		mid, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			continue
		}
		out = append(out, mid)
	}
	return out, true, nil
}

// --- 收件箱 ZSet ---

// AddInbox 把动态 ID 写入用户的关注流 ZSet。
// score=ctime，member=feed_id。
func (c *Cache) AddInbox(ctx context.Context, mid, feedID, ctime int64) error {
	_, err := c.rds.ZaddCtx(ctx, keyInbox(mid), ctime, strconv.FormatInt(feedID, 10))
	if err != nil {
		return fmt.Errorf("AddInbox ZADD: %w", err)
	}
	return nil
}

// RemInbox 从用户关注流 ZSet 删除动态 ID。
func (c *Cache) RemInbox(ctx context.Context, mid, feedID int64) error {
	_, err := c.rds.ZremCtx(ctx, keyInbox(mid), strconv.FormatInt(feedID, 10))
	return err
}

// RangeInbox 翻页查询用户关注流 ZSet（ctime 倒序）。
// 返回 feed_id 列表与对应 ctime；limit 包含 has_more 探测的 +1。
// cursor=0 表示从头开始；否则只取 ctime < cursor 的条目（exclusive）。
func (c *Cache) RangeInbox(ctx context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error) {
	var max int64 = math.MaxInt64
	if cursor > 0 {
		max = cursor - 1
	}
	pairs, err := c.rds.ZrevrangebyscoreWithScoresAndLimitCtx(ctx, keyInbox(mid), 0, max, 0, int(limit))
	if err != nil {
		return nil, nil, fmt.Errorf("RangeInbox ZREVRANGEBYSCORE: %w", err)
	}
	if len(pairs) == 0 {
		return nil, nil, nil
	}
	ids := make([]int64, 0, len(pairs))
	ctimes := make([]int64, 0, len(pairs))
	for _, p := range pairs {
		id, perr := strconv.ParseInt(p.Key, 10, 64)
		if perr != nil {
			continue
		}
		ids = append(ids, id)
		ctimes = append(ctimes, p.Score)
	}
	return ids, ctimes, nil
}

// AddOutbox 把动态 ID 写入作者个人发件箱 ZSet。
func (c *Cache) AddOutbox(ctx context.Context, mid, feedID, ctime int64) error {
	_, err := c.rds.ZaddCtx(ctx, keyOutbox(mid), ctime, strconv.FormatInt(feedID, 10))
	if err != nil {
		return fmt.Errorf("AddOutbox ZADD: %w", err)
	}
	return nil
}

// RemOutbox 从个人发件箱 ZSet 删除动态 ID。
func (c *Cache) RemOutbox(ctx context.Context, mid, feedID int64) error {
	_, err := c.rds.ZremCtx(ctx, keyOutbox(mid), strconv.FormatInt(feedID, 10))
	return err
}

// RangeOutbox 翻页查询作者个人发件箱 ZSet（ctime 倒序）。
func (c *Cache) RangeOutbox(ctx context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error) {
	var max int64 = math.MaxInt64
	if cursor > 0 {
		max = cursor - 1
	}
	pairs, err := c.rds.ZrevrangebyscoreWithScoresAndLimitCtx(ctx, keyOutbox(mid), 0, max, 0, int(limit))
	if err != nil {
		return nil, nil, fmt.Errorf("RangeOutbox ZREVRANGEBYSCORE: %w", err)
	}
	if len(pairs) == 0 {
		return nil, nil, nil
	}
	ids := make([]int64, 0, len(pairs))
	ctimes := make([]int64, 0, len(pairs))
	for _, p := range pairs {
		id, perr := strconv.ParseInt(p.Key, 10, 64)
		if perr != nil {
			continue
		}
		ids = append(ids, id)
		ctimes = append(ctimes, p.Score)
	}
	return ids, ctimes, nil
}

// --- 置顶 ---

// AddPin 把动态 ID 加入用户置顶集合。
func (c *Cache) AddPin(ctx context.Context, mid, feedID int64) error {
	_, err := c.rds.SaddCtx(ctx, keyPin(mid), strconv.FormatInt(feedID, 10))
	return err
}

// RemPin 从用户置顶集合移除动态 ID。
func (c *Cache) RemPin(ctx context.Context, mid, feedID int64) error {
	_, err := c.rds.SremCtx(ctx, keyPin(mid), strconv.FormatInt(feedID, 10))
	return err
}

// ListPins 列出用户置顶动态 ID。
// 用 EXISTS 区分"集合未初始化"与"集合为空"。
func (c *Cache) ListPins(ctx context.Context, mid int64) ([]int64, bool, error) {
	exists, err := c.rds.ExistsCtx(ctx, keyPin(mid))
	if err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, nil
	}
	vals, err := c.rds.SmembersCtx(ctx, keyPin(mid))
	if err != nil {
		return nil, true, err
	}
	if len(vals) == 0 {
		return nil, true, nil
	}
	out := make([]int64, 0, len(vals))
	for _, v := range vals {
		id, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			continue
		}
		out = append(out, id)
	}
	return out, true, nil
}

// --- 未读计数器 ---

// IncrUnread 未读数 +delta（可为负，由 DB 层 GREATEST 兜底到 0）。
func (c *Cache) IncrUnread(ctx context.Context, mid int64, delta int64) error {
	_, err := c.rds.IncrbyCtx(ctx, keyUnread(mid), delta)
	return err
}

// GetUnread 查询未读数；缓存缺失返回 0 hit=false。
func (c *Cache) GetUnread(ctx context.Context, mid int64) (int64, bool, error) {
	v, err := c.rds.GetCtx(ctx, keyUnread(mid))
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, false, perr
	}
	return n, true, nil
}

// SetUnread 直接写入未读数。
func (c *Cache) SetUnread(ctx context.Context, mid int64, n int64) error {
	return c.rds.SetCtx(ctx, keyUnread(mid), strconv.FormatInt(n, 10))
}

// ClearUnread 清零未读数。
func (c *Cache) ClearUnread(ctx context.Context, mid int64) error {
	return c.rds.SetCtx(ctx, keyUnread(mid), "0")
}

// Cacher 是 Repository 对缓存层的最小依赖契约，逐字对应本文件 *Cache 上被
// Repository 调用的方法集（keyInbox 这类只服务于 *Cache 自身的辅助方法不进契约）。
//
// 为什么要这个接口：ServiceContext.Repository 是具体类型 *repository.Repository，
// 生产构造走 New（真 Redis + 真 MySQL），logic 单测无处塞替身。有了 Cacher，测试就能用
// NewWithDeps(内存缓存, 内存 model…) 组装**真实的 Repository**，让「写扩散 fan-out、
// ZSet 游标翻页、缓存 miss 回源与预热、未读计数器与 DB 双写、置顶集合读写配对、
// 删除时的收件箱清理」整条判定链留在被测路径上，而不是把 Repository 整个 mock 掉。
// 生产路径仍然只走 New。
//
// 契约刻意**不含**任何写粉丝集合的方法：`feed:followers:{mid}` 由 social-graph 关注事件
// 写入，本服务只读不写（AGENTS.md §5 数据所有权），这条边界由接口形状在编译期锁死。
type Cacher interface {
	// Ping 健康探针。
	Ping(ctx context.Context) error

	// GetFollowers 读作者粉丝集合，返回 (mids, hit, err)；hit=false 表示集合缺失。
	GetFollowers(ctx context.Context, authorMid int64) ([]int64, bool, error)

	// 收件箱 ZSet。
	AddInbox(ctx context.Context, mid, feedID, ctime int64) error
	RemInbox(ctx context.Context, mid, feedID int64) error
	RangeInbox(ctx context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error)

	// 个人发件箱 ZSet。
	AddOutbox(ctx context.Context, mid, feedID, ctime int64) error
	RemOutbox(ctx context.Context, mid, feedID int64) error
	RangeOutbox(ctx context.Context, mid, cursor int64, limit int32) ([]int64, []int64, error)

	// 置顶集合。
	AddPin(ctx context.Context, mid, feedID int64) error
	RemPin(ctx context.Context, mid, feedID int64) error
	ListPins(ctx context.Context, mid int64) ([]int64, bool, error)

	// 未读计数器。
	IncrUnread(ctx context.Context, mid int64, delta int64) error
	GetUnread(ctx context.Context, mid int64) (int64, bool, error)
	SetUnread(ctx context.Context, mid int64, n int64) error
	ClearUnread(ctx context.Context, mid int64) error
}

// 编译期确认真实缓存实现满足契约；签名漂移在这里立刻炸出，而不是留到 logic 单测。
var _ Cacher = (*Cache)(nil)

// --- Repository ---

// Repository 是 feed 服务的数据访问入口。
type Repository struct {
	cache    Cacher
	conn     sqlx.SqlConn
	outboxMd model.FeedOutboxModel
	inboxMd  model.FeedInboxModel
	pinMd    model.FeedPinModel
	unreadMd model.FeedUnreadModel
}

// New 构造 Repository，是**唯一的生产构造入口**：真 Redis、真 MySQL 连接与 4 个 model。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(
		NewCache(rds),
		conn,
		model.NewFeedOutboxModel(conn),
		model.NewFeedInboxModel(conn),
		model.NewFeedPinModel(conn),
		model.NewFeedUnreadModel(conn),
	)
}

// NewWithDeps 是注入缝：显式给出缓存与 4 个 model，供 logic 单测用内存依赖组装真实
// Repository（见上方 Cacher 注释）。生产代码不得调用本函数，一律走 New；
// 这里只开放构造入口，不改变任何查询语义。
// conn 在本 Repository 中只用于 New 里装配 model（没有 TransactCtx 路径），
// 单测传 nil 即可——若哪天 repository 直接用 r.conn，nil 会立刻 panic，用例即红。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn,
	outboxMd model.FeedOutboxModel, inboxMd model.FeedInboxModel,
	pinMd model.FeedPinModel, unreadMd model.FeedUnreadModel,
) *Repository {
	return &Repository{
		cache:    cache,
		conn:     conn,
		outboxMd: outboxMd,
		inboxMd:  inboxMd,
		pinMd:    pinMd,
		unreadMd: unreadMd,
	}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// PushFeed 推送新动态：写 outbox DB → 写 outbox ZSet → fan-out 到粉丝 inbox ZSet + DB + 未读计数。
// 简化：所有用户都写扩散，不实现大 V 限流策略。
// 返回新建的动态 ID。
func (r *Repository) PushFeed(ctx context.Context, f *model.FeedOutbox) (int64, error) {
	// 1. 写 outbox DB，取 feed_id
	feedID, err := r.outboxMd.Insert(ctx, f)
	if err != nil {
		return 0, err
	}
	// 2. 写作者个人发件箱 ZSet
	if err := r.cache.AddOutbox(ctx, f.Mid, feedID, f.Ctime); err != nil {
		// ZSet 写失败不影响 DB 已落地，记录即可
		_ = err
	}
	// 3. 读粉丝集合（由 social-graph 关注事件写入）
	followers, hit, err := r.cache.GetFollowers(ctx, f.Mid)
	if err != nil {
		return feedID, nil // 粉丝集合读失败不影响 outbox 已落地
	}
	if !hit || len(followers) == 0 {
		// 集合缺失或无粉丝：仅写 outbox，不做 fan-out
		return feedID, nil
	}
	// 4. 写扩散：每个粉丝 inbox ZSet + DB feed_inbox + 未读 +1
	for _, followerMid := range followers {
		_ = r.cache.AddInbox(ctx, followerMid, feedID, f.Ctime)
		_ = r.inboxMd.Add(ctx, &model.FeedInbox{
			Mid:       followerMid,
			FeedID:    feedID,
			AuthorMid: f.Mid,
			Ctime:     f.Ctime,
			State:     model.FeedStateNormal,
		})
		_ = r.cache.IncrUnread(ctx, followerMid, 1)
		_ = r.unreadMd.IncrBy(ctx, followerMid, 1)
	}
	return feedID, nil
}

// PullFeed 拉取关注流：ZSet 翻页 → 按 ID 批量加载 FeedOutbox。
// 返回 (items, nextCursor, hasMore, err)。
func (r *Repository) PullFeed(ctx context.Context, mid, cursor int64, ps int32) ([]*model.FeedOutbox, int64, bool, error) {
	if ps <= 0 || ps > 50 {
		ps = 20
	}
	// 多取一条用于判断 has_more
	limit := ps + 1
	ids, ctimes, err := r.cache.RangeInbox(ctx, mid, cursor, int32(limit))
	if err != nil {
		return nil, 0, false, err
	}
	if len(ids) == 0 {
		// ZSet 缓存缺失：回查 DB 并预热
		rows, err := r.inboxMd.ListByMid(ctx, mid, cursor, int32(limit))
		if err != nil {
			return nil, 0, false, err
		}
		if len(rows) == 0 {
			return nil, 0, false, nil
		}
		ids = make([]int64, 0, len(rows))
		ctimes = make([]int64, 0, len(rows))
		for _, row := range rows {
			ids = append(ids, row.FeedID)
			ctimes = append(ctimes, row.Ctime)
			// 预热 ZSet
			_ = r.cache.AddInbox(ctx, mid, row.FeedID, row.Ctime)
		}
	}
	return r.loadFeedItems(ctx, ids, ctimes, ps)
}

// ListUserFeed 查询某用户主页动态：个人发件箱 ZSet 翻页。
func (r *Repository) ListUserFeed(ctx context.Context, vmid, cursor int64, ps int32) ([]*model.FeedOutbox, int64, bool, error) {
	if ps <= 0 || ps > 50 {
		ps = 20
	}
	limit := ps + 1
	ids, ctimes, err := r.cache.RangeOutbox(ctx, vmid, cursor, int32(limit))
	if err != nil {
		return nil, 0, false, err
	}
	return r.loadFeedItems(ctx, ids, ctimes, ps)
}

// loadFeedItems 把 feed_id 列表转成 FeedOutbox 列表，处理 has_more 与 next_cursor。
func (r *Repository) loadFeedItems(ctx context.Context, ids, ctimes []int64, ps int32) ([]*model.FeedOutbox, int64, bool, error) {
	if len(ids) == 0 {
		return nil, 0, false, nil
	}
	hasMore := len(ids) > int(ps)
	if hasMore {
		ids = ids[:ps]
		ctimes = ctimes[:ps]
	}
	items, err := r.outboxMd.FindMany(ctx, ids)
	if err != nil {
		return nil, 0, false, err
	}
	// 过滤掉已删除的（DB 中 state=2 或缺失），按 ctime 倒序返回
	out := make([]*model.FeedOutbox, 0, len(ids))
	var nextCursor int64
	for i, id := range ids {
		item, ok := items[id]
		if !ok || item.State == model.FeedStateDeleted {
			continue
		}
		out = append(out, item)
		nextCursor = ctimes[i]
	}
	if !hasMore {
		nextCursor = 0
	}
	return out, nextCursor, hasMore, nil
}

// PinFeed 置顶动态：DB feed_pin + Redis Set。
func (r *Repository) PinFeed(ctx context.Context, mid, feedID int64) error {
	// 校验动态存在且属于本人
	item, err := r.outboxMd.FindOne(ctx, feedID)
	if err != nil {
		return err
	}
	if item == nil || item.State == model.FeedStateDeleted || item.Mid != mid {
		return model.ErrFeedNotFound
	}
	if err := r.pinMd.Add(ctx, &model.FeedPin{
		Mid:    mid,
		FeedID: feedID,
	}); err != nil {
		return err
	}
	return r.cache.AddPin(ctx, mid, feedID)
}

// UnpinFeed 取消置顶。
func (r *Repository) UnpinFeed(ctx context.Context, mid, feedID int64) error {
	if err := r.pinMd.Del(ctx, mid, feedID); err != nil {
		return err
	}
	return r.cache.RemPin(ctx, mid, feedID)
}

// ListPins 列出用户置顶动态详情。
func (r *Repository) ListPins(ctx context.Context, mid int64) ([]*model.FeedOutbox, error) {
	ids, hit, err := r.cache.ListPins(ctx, mid)
	if err != nil {
		return nil, err
	}
	if !hit {
		// 缓存缺失：回查 DB 并预热
		ids, err = r.pinMd.ListByMid(ctx, mid)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			_ = r.cache.AddPin(ctx, mid, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	items, err := r.outboxMd.FindMany(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*model.FeedOutbox, 0, len(ids))
	for _, id := range ids {
		if item, ok := items[id]; ok && item.State != model.FeedStateDeleted {
			out = append(out, item)
		}
	}
	return out, nil
}

// GetUnreadCount 查询未读数：Redis 计数器优先，缓存缺失回查 DB。
func (r *Repository) GetUnreadCount(ctx context.Context, mid int64) (int64, error) {
	n, hit, err := r.cache.GetUnread(ctx, mid)
	if err != nil {
		return 0, err
	}
	if hit {
		return n, nil
	}
	n, err = r.unreadMd.Get(ctx, mid)
	if err != nil {
		return 0, err
	}
	_ = r.cache.SetUnread(ctx, mid, n)
	return n, nil
}

// ClearUnread 清零未读：Redis + DB 双写。
func (r *Repository) ClearUnread(ctx context.Context, mid int64) error {
	if err := r.cache.ClearUnread(ctx, mid); err != nil {
		return err
	}
	return r.unreadMd.Clear(ctx, mid)
}

// DeleteFeed 删除动态：DB outbox 软删除 + ZSet 清理（作者 outbox + 所有粉丝 inbox）。
func (r *Repository) DeleteFeed(ctx context.Context, mid, feedID int64) error {
	// 1. 软删除 outbox
	if err := r.outboxMd.SoftDelete(ctx, feedID, mid); err != nil {
		return err
	}
	// 2. 清理作者个人发件箱 ZSet
	_ = r.cache.RemOutbox(ctx, mid, feedID)
	// 3. 清理粉丝关注流 ZSet（读粉丝集合）
	followers, hit, err := r.cache.GetFollowers(ctx, mid)
	if err == nil && hit {
		for _, followerMid := range followers {
			_ = r.cache.RemInbox(ctx, followerMid, feedID)
		}
	}
	// 4. 软删除 feed_inbox 投影（按 feed_id 全量）
	return r.inboxMd.DeleteByFeedID(ctx, feedID)
}
