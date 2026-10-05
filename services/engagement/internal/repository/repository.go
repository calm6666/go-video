// Package repository 是 engagement 服务的数据访问层。
// 组合 thumbup_like/thumbup_stat/favorite_item/favorite_folder/share_log 5 个 model，
// 为 logic 层提供统一数据访问入口。
// 点赞幂等：依赖 (business, mid, message_id) 唯一索引；计数更新通过查旧状态决定增量方向。
package repository

import (
	"context"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/engagement/model"
)

// Cache 封装 engagement 的 Redis 缓存操作。
// 点赞计数走 Redis 增量计数器（INCRBY）+ 定时回刷 DB 的策略；
// 收藏夹列表短缓存；用户/对象点赞列表不缓存（流量小，直接 DB）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixLikeCnt    = "eng:lc:%s:%d:%d"  // business:origin_id:message_id → 点赞数计数器
	prefixDislikeCnt = "eng:dc:%s:%d:%d"  // business:origin_id:message_id → 点踩数计数器
	prefixFolders    = "eng:fl:%d"        // mid → 收藏夹列表
	prefixIsFavored  = "eng:fav:%d:%d:%d" // mid:oid:tp → 是否已收藏

	cacheTTLLikeCnt   = 0 // 计数器不过期，由定时任务回刷后清零
	cacheTTLFolders   = 60
	cacheTTLIsFavored = 60
)

func keyLikeCnt(business string, originID, messageID int64) string {
	return fmt.Sprintf(prefixLikeCnt, business, originID, messageID)
}
func keyDislikeCnt(business string, originID, messageID int64) string {
	return fmt.Sprintf(prefixDislikeCnt, business, originID, messageID)
}
func keyFolders(mid int64) string                  { return fmt.Sprintf(prefixFolders, mid) }
func keyIsFavored(mid, oid int64, tp int32) string { return fmt.Sprintf(prefixIsFavored, mid, oid, tp) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("engagement/cache: redis ping failed")
}

// --- 点赞计数器 ---

// IncrLikeCount 点赞数计数器 +delta（可为负）。
func (c *Cache) IncrLikeCount(ctx context.Context, business string, originID, messageID int64, delta int64) error {
	_, err := c.rds.IncrbyCtx(ctx, keyLikeCnt(business, originID, messageID), delta)
	return err
}

// IncrDislikeCount 点踩数计数器 +delta（可为负）。
func (c *Cache) IncrDislikeCount(ctx context.Context, business string, originID, messageID int64, delta int64) error {
	_, err := c.rds.IncrbyCtx(ctx, keyDislikeCnt(business, originID, messageID), delta)
	return err
}

// --- 收藏夹缓存 ---

// GetFolders 读取收藏夹列表缓存。
func (c *Cache) GetFolders(ctx context.Context, mid int64) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyFolders(mid))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetFolders 写入收藏夹列表缓存。
func (c *Cache) SetFolders(ctx context.Context, mid int64, payload string) error {
	return c.rds.SetexCtx(ctx, keyFolders(mid), payload, cacheTTLFolders)
}

// DelFolders 删除收藏夹列表缓存。
func (c *Cache) DelFolders(ctx context.Context, mid int64) error {
	_, err := c.rds.DelCtx(ctx, keyFolders(mid))
	return err
}

// --- 是否已收藏缓存 ---

// GetIsFavored 读取是否已收藏缓存。
// 返回 (faved, hit, err)：hit=false 表示缓存 miss。
func (c *Cache) GetIsFavored(ctx context.Context, mid, oid int64, tp int32) (bool, bool, error) {
	bs, err := c.rds.GetCtx(ctx, keyIsFavored(mid, oid, tp))
	if err != nil {
		if err == redis.Nil {
			return false, false, nil
		}
		return false, false, err
	}
	return bs == "1", true, nil
}

// SetIsFavored 写入是否已收藏缓存。
func (c *Cache) SetIsFavored(ctx context.Context, mid, oid int64, tp int32, faved bool) error {
	v := "0"
	if faved {
		v = "1"
	}
	return c.rds.SetexCtx(ctx, keyIsFavored(mid, oid, tp), v, cacheTTLIsFavored)
}

// DelIsFavored 删除是否已收藏缓存。
func (c *Cache) DelIsFavored(ctx context.Context, mid, oid int64, tp int32) error {
	_, err := c.rds.DelCtx(ctx, keyIsFavored(mid, oid, tp))
	return err
}

// Cacher 是 Repository 对缓存的依赖面。生产实现是上面的 *Cache（Redis），
// logic 单测注入内存替身；跨包实现本接口不算破坏封装，因为「计数器 + 收藏夹短缓存」
// 的语义本身就是本服务对客户端的承诺（AGENTS.md §6 的 ttl 字段同源）。
// 注意接口按**结构化入参**划分，不像 *Cache 那样收 Redis key：
// key 前缀（eng:）属于本域命名空间的实现细节，留在 *Cache 内部，不进依赖面。
type Cacher interface {
	Ping(ctx context.Context) error
	// IncrLikeCount / IncrDislikeCount：delta 可为负（取消点赞、运营修正）。
	IncrLikeCount(ctx context.Context, business string, originID, messageID, delta int64) error
	IncrDislikeCount(ctx context.Context, business string, originID, messageID, delta int64) error
	GetFolders(ctx context.Context, mid int64) (string, error)
	SetFolders(ctx context.Context, mid int64, payload string) error
	DelFolders(ctx context.Context, mid int64) error
	// GetIsFavored 返回 (faved, hit, err)；hit=false 表示缓存 miss。
	GetIsFavored(ctx context.Context, mid, oid int64, tp int32) (bool, bool, error)
	SetIsFavored(ctx context.Context, mid, oid int64, tp int32, faved bool) error
	DelIsFavored(ctx context.Context, mid, oid int64, tp int32) error
}

var _ Cacher = (*Cache)(nil)

// --- Repository ---

// Repository 是 engagement 服务的数据访问入口。
// 持有 conn 只为一件事：Like/AddFav/DelFav/AddShare 的「关系行 + 计数 + 事件行」
// 必须走同一个 TransactCtx（AGENTS.md §5 的 Outbox 口径）。
// 本服务的每一条 SQL 仍然只在 model 包里，Repository 不直接拼语句，
// 所以逐列对账门禁（model/migration_parity_test.go）不会漏掉任何一句。
type Repository struct {
	conn      sqlx.SqlConn
	cache     Cacher
	likeMd    model.ThumbupLikeModel
	statMd    model.ThumbupStatModel
	favItemMd model.FavoriteItemModel
	favFolder model.FavoriteFolderModel
	shareMd   model.ShareLogModel
	outboxMd  model.EngagementOutboxModel
}

// New 构造 Repository（生产路径）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), conn,
		model.NewThumbupLikeModel(conn),
		model.NewThumbupStatModel(conn),
		model.NewFavoriteItemModel(conn),
		model.NewFavoriteFolderModel(conn),
		model.NewShareLogModel(conn),
		model.NewEngagementOutboxModel(conn))
}

// NewWithDeps 用显式依赖构造 Repository；只服务于测试注入，生产代码一律走 New。
// conn 用于事务；outboxMd 允许 nil（纯读侧用例不必造假的 outbox），
// 但四个互动写路径一旦要产事件就会报错，不会「写了库、静默丢了事件」。
func NewWithDeps(cache Cacher, conn sqlx.SqlConn, likeMd model.ThumbupLikeModel, statMd model.ThumbupStatModel,
	favItemMd model.FavoriteItemModel, favFolder model.FavoriteFolderModel, shareMd model.ShareLogModel,
	outboxMd model.EngagementOutboxModel) *Repository {
	return &Repository{
		conn:      conn,
		cache:     cache,
		likeMd:    likeMd,
		statMd:    statMd,
		favItemMd: favItemMd,
		favFolder: favFolder,
		shareMd:   shareMd,
		outboxMd:  outboxMd,
	}
}

// OutboxModel 暴露 engagement_outbox 的 model，供 internal/publisher 装配发布循环。
func (r *Repository) OutboxModel() model.EngagementOutboxModel { return r.outboxMd }

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 点赞 ---

// likeDeltas 把一次状态转换翻译成两个计数的增减方向。
// 撤销旧状态的贡献，再加上新状态的贡献；state 取值 0 取消、1 like、2 dislike。
func likeDeltas(oldState, newState int32) (likeDelta, dislikeDelta int64) {
	switch oldState {
	case 1:
		likeDelta--
	case 2:
		dislikeDelta--
	}
	switch newState {
	case 1:
		likeDelta++
	case 2:
		dislikeDelta++
	}
	return likeDelta, dislikeDelta
}

// Like 点赞/取消点赞/点踩。
// 幂等：唯一键 (business, mid, message_id) + 事务内先查旧状态，按状态变化决定计数增量方向。
// 返回当前 like_number/dislike_number（本次事务内的真实值）。
//
// 关系行、计数增量、事件行三段写在同一个 TransactCtx 里（缺陷 #1 的修法）：
// 之前 Upsert 成功而 Incr 失败时，重试会走「状态已相同」短路，计数永远补不回来。
// Redis 影子计数器的推进挪到提交之后：事务回滚却已经把计数器加过一遍，
// 会让后续读到 Redis 的接口显示一个库内不存在的计数。
func (r *Repository) Like(ctx context.Context, l *model.ThumbupLike) (likeNum, dislikeNum int64, err error) {
	if r.conn == nil {
		return 0, 0, fmt.Errorf("engagement/repository: Like 需要事务会话，但未注入 conn")
	}
	var likeDelta, dislikeDelta int64
	occurred := nowUnix()
	err = r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		// 1. 事务内查旧状态
		oldStates, err := r.likeMd.FindStates(ctx, session, l.Business, l.Mid, []int64{l.MessageID})
		if err != nil {
			return err
		}
		var oldState int32
		if old, ok := oldStates[l.MessageID]; ok {
			oldState = old.State
		}

		// 2. 状态相同 ⇒ 计数不会变，幂等返回当前值，不产事件
		if oldState == l.State {
			stat, err := r.statMd.FindOne(ctx, session, l.Business, l.OriginID, l.MessageID)
			if err != nil {
				return err
			}
			if stat != nil {
				likeNum, dislikeNum = stat.LikeNumber, stat.DislikeNumber
			}
			return nil
		}

		// 3. upsert 点赞关系行
		if _, err := r.likeMd.Upsert(ctx, session, l); err != nil {
			return err
		}
		likeDelta, dislikeDelta = likeDeltas(oldState, l.State)

		// 4. 更新 DB 计数（INSERT...ON DUPLICATE KEY UPDATE）
		if err := r.statMd.Incr(ctx, session, l.Business, l.OriginID, l.MessageID, likeDelta, dislikeDelta); err != nil {
			return err
		}

		// 5. 同一会话回读计数：换连接会读到事务前的旧值，事件快照就少算这一次互动
		stat, err := r.statMd.FindOne(ctx, session, l.Business, l.OriginID, l.MessageID)
		if err != nil {
			return err
		}
		if stat == nil {
			return fmt.Errorf("engagement/repository: thumbup_stat Incr 之后仍查不到计数行 business=%s origin=%d message=%d",
				l.Business, l.OriginID, l.MessageID)
		}
		likeNum, dislikeNum = stat.LikeNumber, stat.DislikeNumber

		// 6. 事件：只在 like_count 变化时产（dislike_number 不在 engagement.action.v1 契约里，
		// 纯点踩与「点踩换点踩的取消」都不产事件，见 README 已知缺口 22）。
		if likeDelta == 0 {
			return nil
		}
		evt, err := r.buildActionEvent(ctx, actionNameForLike(oldState, l.State), l.MessageID, l.Mid,
			l.Business, l.OriginID, 0, 0, heatCounters{LikeCount: likeNum},
			[]string{SnapshotFieldLikeCount}, occurred)
		if err != nil {
			return err
		}
		return r.insertEvent(ctx, session, evt)
	})
	if err != nil {
		return 0, 0, err
	}
	// 7. 提交后同步 Redis 计数器；失败只记录，不回滚已提交的业务写（缺陷 #5 的口径）
	if likeDelta != 0 || dislikeDelta != 0 {
		_ = r.cache.IncrLikeCount(ctx, l.Business, l.OriginID, l.MessageID, likeDelta)
		_ = r.cache.IncrDislikeCount(ctx, l.Business, l.OriginID, l.MessageID, dislikeDelta)
	}
	return likeNum, dislikeNum, nil
}

// Stats 批量查询计数与当前用户状态。
func (r *Repository) Stats(ctx context.Context, business string, originID int64, messageIDs []int64, mid int64) (map[int64]*model.ThumbupStat, map[int64]*model.ThumbupLike, error) {
	stats, err := r.statMd.FindMany(ctx, business, originID, messageIDs)
	if err != nil {
		return nil, nil, err
	}
	var states map[int64]*model.ThumbupLike
	if mid > 0 {
		states, err = r.likeMd.FindStates(ctx, nil, business, mid, messageIDs)
		if err != nil {
			return nil, nil, err
		}
	}
	return stats, states, nil
}

// HasLike 批量查询用户是否点赞。
func (r *Repository) HasLike(ctx context.Context, business string, mid int64, messageIDs []int64) (map[int64]*model.ThumbupLike, error) {
	return r.likeMd.FindStates(ctx, nil, business, mid, messageIDs)
}

// UserLikes 用户点赞列表。
func (r *Repository) UserLikes(ctx context.Context, business string, mid int64, pn, ps int32) ([]*model.ThumbupLike, int32, error) {
	return r.likeMd.ListByMid(ctx, business, mid, pn, ps)
}

// ItemLikes 对象点赞人列表。
func (r *Repository) ItemLikes(ctx context.Context, business string, originID, messageID, lastMid int64, pn, ps int32) ([]*model.ThumbupLike, int32, error) {
	return r.likeMd.ListByItem(ctx, business, originID, messageID, lastMid, pn, ps)
}

// UpdateCount 运营修正计数增量。
//
// 刻意不产 engagement.action.v1：这条修正只改 like_change/dislike_change，
// 而事件快照取的是 like_number/favorite_count/share_count 这三列，
// 变更正口径要连消费方一起评审（README 已知缺口 23）。
// 因此运营修正之后，搜索索引里的热度要等下一次真实互动才会被纠正。
func (r *Repository) UpdateCount(ctx context.Context, business string, originID, messageID, likeChange, dislikeChange int64) error {
	if err := r.statMd.UpdateChange(ctx, business, originID, messageID, likeChange, dislikeChange); err != nil {
		return err
	}
	_ = r.cache.IncrLikeCount(ctx, business, originID, messageID, likeChange)
	_ = r.cache.IncrDislikeCount(ctx, business, originID, messageID, dislikeChange)
	return nil
}

// RawStat 查询原始计数（读路径不参与事务）。
func (r *Repository) RawStat(ctx context.Context, business string, originID, messageID int64) (*model.ThumbupStat, error) {
	return r.statMd.FindOne(ctx, nil, business, originID, messageID)
}

// --- 收藏 ---

// AddFav 添加收藏。
//
// 「原本是否已收藏」的判定、关系行、收藏数快照与事件行在同一事务里：
// favorite_item 的 Add 是 ODKU（重复收藏只把 fid 改掉、state 置回 0），
// 不前置读就分不清「新收藏」与「换夹子的重复收藏」，后者会让 favorite_count 不变却发一条事件，
// 消费方按绝对快照 last-write-wins，多发的这条虽然数值相同，但白白占掉一次投递与一次重试配额。
// Redis 与收藏夹缓存的失效挪到提交之后（同 Like 的口径）。
func (r *Repository) AddFav(ctx context.Context, f *model.FavoriteItem) error {
	if r.conn == nil {
		return fmt.Errorf("engagement/repository: AddFav 需要事务会话，但未注入 conn")
	}
	occurred := nowUnix()
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		already, err := r.favItemMd.IsFavored(ctx, session, f.Mid, f.Oid, f.Tp)
		if err != nil {
			return err
		}
		if err := r.favItemMd.Add(ctx, session, f); err != nil {
			return err
		}
		if already {
			return nil
		}
		cnt, err := r.favItemMd.CountByOid(ctx, session, f.Oid, f.Tp)
		if err != nil {
			return err
		}
		evt, err := r.buildActionEvent(ctx, model.ActionFavorite, f.Oid, f.Mid,
			"", 0, f.Tp, f.Otype, heatCounters{FavoriteCount: cnt},
			[]string{SnapshotFieldFavoriteCount}, occurred)
		if err != nil {
			return err
		}
		return r.insertEvent(ctx, session, evt)
	})
	if err != nil {
		return err
	}
	_ = r.cache.SetIsFavored(ctx, f.Mid, f.Oid, f.Tp, true)
	_ = r.cache.DelFolders(ctx, f.Mid)
	return nil
}

// DelFav 删除收藏（软删）。
// 与 AddFav 同构：未收藏过时 Del 是空操作，不产事件也不动计数。
func (r *Repository) DelFav(ctx context.Context, mid, oid int64, tp int32, fid int64) error {
	if r.conn == nil {
		return fmt.Errorf("engagement/repository: DelFav 需要事务会话，但未注入 conn")
	}
	occurred := nowUnix()
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		already, err := r.favItemMd.IsFavored(ctx, session, mid, oid, tp)
		if err != nil {
			return err
		}
		if err := r.favItemMd.Del(ctx, session, mid, oid, tp, fid); err != nil {
			return err
		}
		if !already {
			return nil
		}
		cnt, err := r.favItemMd.CountByOid(ctx, session, oid, tp)
		if err != nil {
			return err
		}
		evt, err := r.buildActionEvent(ctx, model.ActionCancelFavorite, oid, mid,
			"", 0, tp, 0, heatCounters{FavoriteCount: cnt},
			[]string{SnapshotFieldFavoriteCount}, occurred)
		if err != nil {
			return err
		}
		return r.insertEvent(ctx, session, evt)
	})
	if err != nil {
		return err
	}
	_ = r.cache.SetIsFavored(ctx, mid, oid, tp, false)
	_ = r.cache.DelFolders(ctx, mid)
	return nil
}

// IsFavored 查询是否已收藏（缓存 miss 才穿库，读路径不参与事务）。
func (r *Repository) IsFavored(ctx context.Context, mid, oid int64, tp int32) (bool, error) {
	faved, hit, err := r.cache.GetIsFavored(ctx, mid, oid, tp)
	if err != nil {
		return false, err
	}
	if hit {
		return faved, nil
	}
	faved, err = r.favItemMd.IsFavored(ctx, nil, mid, oid, tp)
	if err != nil {
		return false, err
	}
	_ = r.cache.SetIsFavored(ctx, mid, oid, tp, faved)
	return faved, nil
}

// IsFavoreds 批量查询是否已收藏。
func (r *Repository) IsFavoreds(ctx context.Context, mid int64, oids []int64, tp int32) (map[int64]bool, error) {
	return r.favItemMd.IsFavoreds(ctx, mid, oids, tp)
}

// UserFolders 查询用户收藏夹列表。
func (r *Repository) UserFolders(ctx context.Context, mid, vmid int64) ([]*model.FavoriteFolder, error) {
	// 只缓存本人查询（mid == vmid）
	if mid == vmid {
		payload, err := r.cache.GetFolders(ctx, mid)
		if err != nil {
			return nil, err
		}
		if payload != "" {
			var folders []*model.FavoriteFolder
			if err := jsonUnmarshal([]byte(payload), &folders); err != nil {
				return nil, fmt.Errorf("UserFolders unmarshal cache: %w", err)
			}
			return folders, nil
		}
	}
	folders, err := r.favFolder.ListByUser(ctx, mid, vmid)
	if err != nil {
		return nil, err
	}
	if mid == vmid {
		_ = r.cache.SetFolders(ctx, mid, jsonMustMarshal(folders))
	}
	return folders, nil
}

// AddFolder 创建收藏夹。
func (r *Repository) AddFolder(ctx context.Context, f *model.FavoriteFolder) (int64, error) {
	fid, err := r.favFolder.Add(ctx, f)
	if err != nil {
		return 0, err
	}
	_ = r.cache.DelFolders(ctx, f.Mid)
	return fid, nil
}

// DelFolder 删除收藏夹。
func (r *Repository) DelFolder(ctx context.Context, fid, mid int64) error {
	if err := r.favFolder.Del(ctx, fid, mid); err != nil {
		return err
	}
	_ = r.cache.DelFolders(ctx, mid)
	return nil
}

// --- 分享 ---

// AddShare 记录分享并返回分享数。
//
// share_log 的唯一键 (oid, mid, tp, day) 是「同一用户同一对象同一天只算一次」，
// 所以 AddIfNotExists 的 added=false 分支不产事件（计数没动）。
// 之前这里把 added 丢弃、每次都实时 COUNT 返回，语义上没错但白丢了一次幂等判定；
// 现在它决定发不发事件，同时分享数仍在同一会话里 COUNT（否则读不到刚插入的这行）。
func (r *Repository) AddShare(ctx context.Context, oid, mid int64, tp int32) (int64, error) {
	if r.conn == nil {
		return 0, fmt.Errorf("engagement/repository: AddShare 需要事务会话，但未注入 conn")
	}
	var count int64
	occurred := nowUnix()
	err := r.conn.TransactCtx(ctx, func(ctx context.Context, session sqlx.Session) error {
		added, err := r.shareMd.AddIfNotExists(ctx, session, oid, mid, tp)
		if err != nil {
			return err
		}
		cnt, err := r.shareMd.CountByOid(ctx, session, oid, tp)
		if err != nil {
			return err
		}
		count = cnt
		if !added {
			return nil
		}
		evt, err := r.buildActionEvent(ctx, model.ActionShare, oid, mid,
			"", 0, tp, 0, heatCounters{ShareCount: cnt},
			[]string{SnapshotFieldShareCount}, occurred)
		if err != nil {
			return err
		}
		return r.insertEvent(ctx, session, evt)
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
