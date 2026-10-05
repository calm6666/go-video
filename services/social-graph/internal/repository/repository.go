// Package repository 是 social-graph 服务的数据访问层。
// 组合 relation_follow/relation_black/relation_stat/relation_special 4 个 model，
// 为 logic 层提供统一数据访问入口。
// 关注幂等：依赖 (mid, follower_mid) 唯一索引；计数通过 Redis 计数器维护权威值，
// 由定时任务回刷 relation_stat；列表查询直接走 DB（流量小，不缓存）。
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

// Cache 封装 social-graph 的 Redis 缓存操作。
// 关注集合走 SET（SISMEMBER O(1) 判断是否关注）；计数走 INCRBY 计数器；
// 粉丝时间轴走 ZSet（score=关注时间），由 repository 维护，
// 列表查询当前直接走 DB，ZSet 作为后续优化留接口（本期不强制使用）。
type Cache struct {
	rds *redis.Redis
}

// 缓存 key 与 TTL。
const (
	prefixFollowSet    = "sg:fs:%d" // mid → 关注集合（SET，member=follower_mid）
	prefixFollowerZ    = "sg:fr:%d" // mid → 粉丝时间轴（ZSET，member=follower_mid，score=ctime）
	prefixFollowingCnt = "sg:fc:%d" // mid → 关注数计数器
	prefixFollowerCnt  = "sg:rc:%d" // mid → 粉丝数计数器

	// cacheTTLStat 计数器回刷缓存 TTL（秒）。INCRBY 维护的计数器不过期，
	// Stat miss 时回查 DB 并用本 TTL 回填，超时后再次触发回刷。
	cacheTTLStat = 3600
)

func keyFollowSet(mid int64) string    { return fmt.Sprintf(prefixFollowSet, mid) }
func keyFollowerZ(mid int64) string    { return fmt.Sprintf(prefixFollowerZ, mid) }
func keyFollowingCnt(mid int64) string { return fmt.Sprintf(prefixFollowingCnt, mid) }
func keyFollowerCnt(mid int64) string  { return fmt.Sprintf(prefixFollowerCnt, mid) }

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("social-graph/cache: redis ping failed")
}

// --- 关注集合（SET） ---

// IsFollowing 从关注集合缓存查询 mid 是否关注 followerMid。
// 返回 (following, hit, err)：hit=false 表示缓存 miss，调用方应回查 DB 并回填。
func (c *Cache) IsFollowing(ctx context.Context, mid, followerMid int64) (bool, bool, error) {
	ok, err := c.rds.SismemberCtx(ctx, keyFollowSet(mid), followerMid)
	if err != nil {
		if err == redis.Nil {
			return false, false, nil
		}
		return false, false, err
	}
	// SISMEMBER 不区分 key 不存在与成员不存在，二者均返回 false。
	// 为了能区分 miss，单独用 EXISTS 判定 key 是否存在。
	exists, err := c.rds.ExistsCtx(ctx, keyFollowSet(mid))
	if err != nil {
		return false, false, err
	}
	if !exists {
		return false, false, nil
	}
	return ok, true, nil
}

// AddFollowing 把 followerMid 加入 mid 的关注集合。
func (c *Cache) AddFollowing(ctx context.Context, mid, followerMid int64) error {
	_, err := c.rds.SaddCtx(ctx, keyFollowSet(mid), followerMid)
	return err
}

// DelFollowing 把 followerMid 从 mid 的关注集合移除。
func (c *Cache) DelFollowing(ctx context.Context, mid, followerMid int64) error {
	_, err := c.rds.SremCtx(ctx, keyFollowSet(mid), followerMid)
	return err
}

// DelFollowSet 删除 mid 的整个关注集合（用于 key 失效或重建）。
func (c *Cache) DelFollowSet(ctx context.Context, mid int64) error {
	_, err := c.rds.DelCtx(ctx, keyFollowSet(mid))
	return err
}

// --- 粉丝时间轴（ZSET） ---

// AddFollower 把 followerMid 加入 mid 的粉丝时间轴，score 为关注时间。
func (c *Cache) AddFollower(ctx context.Context, mid, followerMid, ctime int64) error {
	_, err := c.rds.ZaddCtx(ctx, keyFollowerZ(mid), ctime, fmt.Sprintf("%d", followerMid))
	return err
}

// DelFollower 把 followerMid 从 mid 的粉丝时间轴移除。
func (c *Cache) DelFollower(ctx context.Context, mid, followerMid int64) error {
	_, err := c.rds.ZremCtx(ctx, keyFollowerZ(mid), fmt.Sprintf("%d", followerMid))
	return err
}

// --- 计数器 ---

// IncrFollowingCount 关注数计数器 +delta（可为负）。
func (c *Cache) IncrFollowingCount(ctx context.Context, mid, delta int64) error {
	_, err := c.rds.IncrbyCtx(ctx, keyFollowingCnt(mid), delta)
	return err
}

// IncrFollowerCount 粉丝数计数器 +delta（可为负）。
func (c *Cache) IncrFollowerCount(ctx context.Context, mid, delta int64) error {
	_, err := c.rds.IncrbyCtx(ctx, keyFollowerCnt(mid), delta)
	return err
}

// GetFollowingCount 读取关注数计数器；返回 (val, hit, err)，hit=false 表示缓存 miss。
func (c *Cache) GetFollowingCount(ctx context.Context, mid int64) (int64, bool, error) {
	v, err := c.rds.GetCtx(ctx, keyFollowingCnt(mid))
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, err := parseInt64(v)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// GetFollowerCount 读取粉丝数计数器；返回 (val, hit, err)，hit=false 表示缓存 miss。
func (c *Cache) GetFollowerCount(ctx context.Context, mid int64) (int64, bool, error) {
	v, err := c.rds.GetCtx(ctx, keyFollowerCnt(mid))
	if err != nil {
		if err == redis.Nil {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, err := parseInt64(v)
	if err != nil {
		return 0, false, err
	}
	return n, true, nil
}

// SetFollowingCount 写入关注数计数器（回刷场景）。
func (c *Cache) SetFollowingCount(ctx context.Context, mid, val int64) error {
	return c.rds.SetexCtx(ctx, keyFollowingCnt(mid), fmt.Sprintf("%d", val), cacheTTLStat)
}

// SetFollowerCount 写入粉丝数计数器（回刷场景）。
func (c *Cache) SetFollowerCount(ctx context.Context, mid, val int64) error {
	return c.rds.SetexCtx(ctx, keyFollowerCnt(mid), fmt.Sprintf("%d", val), cacheTTLStat)
}

// --- Repository ---

// Cacher 是 Repository 对缓存层的最小依赖面（生产由 *Cache 实现）。
//
// 为什么要有这个接口：Repository.cache 原先是具体类型 *Cache，而 ServiceContext.Repository
// 又是具体类型 *repository.Repository，logic 单测无处塞替身，只能连真 Redis。抽出接口后，
// 测试用 NewWithDeps(内存缓存, 内存 followMd/blackMd/statMd/specialMd) 组装**真实的 Repository**，
// 只把它的 5 个依赖换成替身——这样「关注幂等 → 计数增量方向」「缓存读穿/回填」「计数器 miss 回刷」
// 这些判定整条都在被测路径上，而不是把 Repository 也 mock 掉。
// 只声明 Repository 实际调用的方法：Cache.DelFollowSet 目前零调用方，故不进入接口
// （进了就等于把未接线能力写成契约）。
// 语义约定与 *Cache 一致：Redis key 不存在按 miss 处理，返回 hit=false 且 err=nil，不视为错误。
type Cacher interface {
	Ping(ctx context.Context) error

	// 关注集合（SET，member=follower_mid）
	IsFollowing(ctx context.Context, mid, followerMid int64) (following, hit bool, err error)
	AddFollowing(ctx context.Context, mid, followerMid int64) error
	DelFollowing(ctx context.Context, mid, followerMid int64) error

	// 粉丝时间轴（ZSET，score=关注时间）
	AddFollower(ctx context.Context, mid, followerMid, ctime int64) error
	DelFollower(ctx context.Context, mid, followerMid int64) error

	// 计数器
	IncrFollowingCount(ctx context.Context, mid, delta int64) error
	IncrFollowerCount(ctx context.Context, mid, delta int64) error
	GetFollowingCount(ctx context.Context, mid int64) (val int64, hit bool, err error)
	GetFollowerCount(ctx context.Context, mid int64) (val int64, hit bool, err error)
	SetFollowingCount(ctx context.Context, mid, val int64) error
	SetFollowerCount(ctx context.Context, mid, val int64) error
}

// Repository 是 social-graph 服务的数据访问入口。
//
// 原 conn sqlx.SqlConn 字段是死重（本服务全部是单表单语句写，没有 TransactCtx 事务），
// 抽出注入缝后由 New 直接把它交给各 model，故不再持有。
type Repository struct {
	cache     Cacher
	followMd  model.RelationFollowModel
	blackMd   model.RelationBlackModel
	statMd    model.RelationStatModel
	specialMd model.RelationSpecialModel
}

// New 构造 Repository（生产路径唯一入口）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds),
		model.NewRelationFollowModel(conn),
		model.NewRelationBlackModel(conn),
		model.NewRelationStatModel(conn),
		model.NewRelationSpecialModel(conn),
	)
}

// NewWithDeps 用给定的依赖组装 Repository。
// 生产代码只应通过 New 构造；本函数为 logic 单测提供注入缝（见 Cacher 注释）。
func NewWithDeps(cache Cacher,
	followMd model.RelationFollowModel,
	blackMd model.RelationBlackModel,
	statMd model.RelationStatModel,
	specialMd model.RelationSpecialModel,
) *Repository {
	return &Repository{cache: cache, followMd: followMd, blackMd: blackMd, statMd: statMd, specialMd: specialMd}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// --- 关注 ---

// Follow 关注；幂等：重复关注不重复增加计数。
// 返回 (changed bool)：是否产生了状态变化（用于事件投递判断）。
func (r *Repository) Follow(ctx context.Context, mid, followerMid int64) (bool, error) {
	rec := &model.RelationFollow{Mid: mid, FollowerMid: followerMid}
	oldState, err := r.followMd.Upsert(ctx, rec, model.FollowStateNormal)
	if err != nil {
		return false, err
	}
	if oldState == model.FollowStateNormal {
		// 幂等：已关注，计数不变。
		return false, nil
	}
	// 计数器：mid 关注 +1，followerMid 粉丝 +1。
	if err := r.cache.IncrFollowingCount(ctx, mid, 1); err != nil {
		// 缓存失败不阻塞业务，由 DB 计数兜底。
		_ = err
	}
	if err := r.cache.IncrFollowerCount(ctx, followerMid, 1); err != nil {
		_ = err
	}
	_ = r.cache.AddFollowing(ctx, mid, followerMid)
	now := time.Now().Unix()
	_ = r.cache.AddFollower(ctx, followerMid, mid, now)
	// DB 计数快照同步增量（异步回刷也可，这里同步落库以保证一致性兜底）。
	if err := r.statMd.Incr(ctx, mid, 1, 0); err != nil {
		return true, err
	}
	if err := r.statMd.Incr(ctx, followerMid, 0, 1); err != nil {
		return true, err
	}
	return true, nil
}

// Unfollow 取关；幂等。
func (r *Repository) Unfollow(ctx context.Context, mid, followerMid int64) (bool, error) {
	oldState, err := r.followMd.Delete(ctx, mid, followerMid)
	if err != nil {
		return false, err
	}
	if oldState != model.FollowStateNormal {
		// 幂等：已取关或无记录。
		return false, nil
	}
	_ = r.cache.IncrFollowingCount(ctx, mid, -1)
	_ = r.cache.IncrFollowerCount(ctx, followerMid, -1)
	_ = r.cache.DelFollowing(ctx, mid, followerMid)
	_ = r.cache.DelFollower(ctx, followerMid, mid)
	if err := r.statMd.Incr(ctx, mid, -1, 0); err != nil {
		return true, err
	}
	if err := r.statMd.Incr(ctx, followerMid, 0, -1); err != nil {
		return true, err
	}
	return true, nil
}

// IsFollowing 查询 mid 是否关注 followerMid；优先走缓存。
func (r *Repository) IsFollowing(ctx context.Context, mid, followerMid int64) (bool, error) {
	ok, hit, err := r.cache.IsFollowing(ctx, mid, followerMid)
	if err != nil {
		return false, err
	}
	if hit {
		return ok, nil
	}
	// 缓存 miss：回查 DB。
	rec, err := r.followMd.FindOne(ctx, mid, followerMid)
	if err != nil {
		return false, err
	}
	following := rec != nil
	if following {
		_ = r.cache.AddFollowing(ctx, mid, followerMid)
	} else {
		_ = r.cache.DelFollowing(ctx, mid, followerMid)
	}
	return following, nil
}

// IsFollowingBatch 批量查询 mid 是否关注 owners 列表中的每一个。
// 批量直接走 DB，避免 N 次缓存查询。
func (r *Repository) IsFollowingBatch(ctx context.Context, mid int64, owners []int64) (map[int64]bool, error) {
	return r.followMd.FindFollowings(ctx, mid, owners)
}

// ListFollowing 分页查询 mid 的关注列表。
func (r *Repository) ListFollowing(ctx context.Context, mid int64, pn, ps int32) ([]*model.RelationFollow, int32, error) {
	return r.followMd.ListFollowings(ctx, mid, pn, ps)
}

// ListFollower 分页查询 mid 的粉丝列表。
func (r *Repository) ListFollower(ctx context.Context, mid int64, pn, ps int32) ([]*model.RelationFollow, int32, error) {
	return r.followMd.ListFollowers(ctx, mid, pn, ps)
}

// --- 计数 ---

// Stat 查询 mid 的关注数与粉丝数；优先走 Redis 计数器，miss 时回查 DB 并回填。
func (r *Repository) Stat(ctx context.Context, mid int64) (following, follower int64, err error) {
	fc, hit, err := r.cache.GetFollowingCount(ctx, mid)
	if err != nil {
		return 0, 0, err
	}
	rc, hitR, err := r.cache.GetFollowerCount(ctx, mid)
	if err != nil {
		return 0, 0, err
	}
	if hit && hitR {
		return fc, rc, nil
	}
	// 任一 miss：回查 DB 快照并回填两个计数器。
	stat, err := r.statMd.Find(ctx, mid)
	if err != nil {
		return 0, 0, err
	}
	if stat == nil {
		return 0, 0, nil
	}
	_ = r.cache.SetFollowingCount(ctx, mid, stat.Following)
	_ = r.cache.SetFollowerCount(ctx, mid, stat.Follower)
	return stat.Following, stat.Follower, nil
}

// --- 黑名单 ---

// AddBlack 拉黑；幂等。若 mid 已关注 blackMid，自动取关。
// 返回 (blackChanged, followChanged)。
func (r *Repository) AddBlack(ctx context.Context, mid, blackMid int64) (blackChanged, followChanged bool, err error) {
	rec := &model.RelationBlack{Mid: mid, BlackMid: blackMid}
	oldState, err := r.blackMd.Upsert(ctx, rec, model.BlackStateNormal)
	if err != nil {
		return false, false, err
	}
	blackChanged = oldState != model.BlackStateNormal
	// 拉黑时自动取关（双向：mid 对 blackMid 的关注 + blackMid 对 mid 的关注都不主动删，
	// 仅删 mid→blackMid 的关注记录，符合"拉黑后看不见对方"的语义）。
	changed, err := r.Unfollow(ctx, mid, blackMid)
	if err != nil {
		return blackChanged, changed, err
	}
	return blackChanged, changed, nil
}

// DelBlack 取消拉黑；幂等。
func (r *Repository) DelBlack(ctx context.Context, mid, blackMid int64) (bool, error) {
	oldState, err := r.blackMd.Delete(ctx, mid, blackMid)
	if err != nil {
		return false, err
	}
	return oldState == model.BlackStateNormal, nil
}

// IsBlacked 查询 mid 是否拉黑 blackMid。
func (r *Repository) IsBlacked(ctx context.Context, mid, blackMid int64) (bool, error) {
	rec, err := r.blackMd.FindOne(ctx, mid, blackMid)
	if err != nil {
		return false, err
	}
	return rec != nil, nil
}

// ListBlacks 分页查询 mid 的黑名单列表。
func (r *Repository) ListBlacks(ctx context.Context, mid int64, pn, ps int32) ([]*model.RelationBlack, int32, error) {
	return r.blackMd.ListByMid(ctx, mid, pn, ps)
}

// --- 特别关注 ---

// AddSpecial 特别关注；必先关注。未关注返回 ErrSpecialNeedFollow。
func (r *Repository) AddSpecial(ctx context.Context, mid, specialMid int64) (bool, error) {
	following, err := r.IsFollowing(ctx, mid, specialMid)
	if err != nil {
		return false, err
	}
	if !following {
		return false, model.ErrSpecialNeedFollow
	}
	rec := &model.RelationSpecial{Mid: mid, SpecialMid: specialMid}
	oldState, err := r.specialMd.Upsert(ctx, rec, model.SpecialStateNormal)
	if err != nil {
		return false, err
	}
	return oldState != model.SpecialStateNormal, nil
}

// DelSpecial 取消特别关注；幂等。
func (r *Repository) DelSpecial(ctx context.Context, mid, specialMid int64) (bool, error) {
	oldState, err := r.specialMd.Delete(ctx, mid, specialMid)
	if err != nil {
		return false, err
	}
	return oldState == model.SpecialStateNormal, nil
}

// --- 富关系（批量、四位掩码） ---

// RichRelations 批量查询 owner 与 mids 中每个用户之间的全部关系位。
// 返回 mid → rpc.RelationAttr 掩码；键集合恒等于 mids 的去集，无关系是 0 而不是缺键。
//
// 四条查询各负责一位，任一失败就整体报错：半张掩码表会被调用方读成「没关注/没拉黑」，
// 比报错危险（与 account 侧 Relations 按片切分的同一口径）。
// 不走缓存：批量走缓存要 N 次 SISMEMBER，而本 RPC 的用途正是一次拿全（同 IsFollowingBatch 的口径）。
//
// 四个位是各自表里的独立事实，不做优先级压制：拉黑 mid 只删 owner→mid 的关注行
// （见 AddBlack），所以「mid 关注 owner」与「owner 拉黑 mid」可以同时为真（掩码 6）；
// special 也不蕴含 following，因为 Unfollow 不清 relation_special。
// owner 出现在 mids 里时返回 0——本服务从不写自指行（写侧一律 ErrSelfAction 拒绝）。
func (r *Repository) RichRelations(ctx context.Context, owner int64, mids []int64) (map[int64]int32, error) {
	if len(mids) == 0 {
		return map[int64]int32{}, nil
	}
	following, err := r.followMd.FindFollowings(ctx, owner, mids)
	if err != nil {
		return nil, err
	}
	follower, err := r.followMd.FindFollowers(ctx, owner, mids)
	if err != nil {
		return nil, err
	}
	blacked, err := r.blackMd.FindBlacks(ctx, owner, mids)
	if err != nil {
		return nil, err
	}
	special, err := r.specialMd.FindSpecials(ctx, owner, mids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int32, len(mids))
	for _, mid := range mids {
		var attr int32
		if following[mid] {
			attr |= int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWING)
		}
		if follower[mid] {
			attr |= int32(rpc.RelationAttr_RELATION_ATTR_FOLLOWER)
		}
		if blacked[mid] {
			attr |= int32(rpc.RelationAttr_RELATION_ATTR_BLACKED)
		}
		if special[mid] {
			attr |= int32(rpc.RelationAttr_RELATION_ATTR_SPECIAL)
		}
		out[mid] = attr
	}
	return out, nil
}
