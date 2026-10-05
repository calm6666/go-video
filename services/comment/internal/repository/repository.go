// Package repository 是 comment 服务的数据访问层。
// 组合本地 MySQL 模型与 Redis 缓存，为 logic 层提供统一数据访问入口。
// 参考 obc reply 模块的 dao 缓存策略：评论列表短缓存，评论主体长缓存。
package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/comment/model"
)

// 缓存 key 约定。
const (
	prefixListByOid = "cmt:list:%d:%d:%s:%d:%d" // oid:tp:sort:pn:ps
	prefixReplies   = "cmt:rl:%d:%d:%d"         // root:pn:ps
	prefixOne       = "cmt:1:%d"                // rpid
	prefixStats     = "cmt:st:%d:%d"            // oid:tp

	cacheTTLList  = 30  // 评论列表 30 秒
	cacheTTLOne   = 300 // 评论主体 5 分钟
	cacheTTLStats = 60  // 计数快照 1 分钟

	emptyMark = "{}"
)

func keyList(oid int64, tp int32, sort string, pn, ps int32) string {
	return fmt.Sprintf(prefixListByOid, oid, tp, sort, pn, ps)
}
func keyReplies(root int64, pn, ps int32) string {
	return fmt.Sprintf(prefixReplies, root, pn, ps)
}
func keyOne(rpid int64) string            { return fmt.Sprintf(prefixOne, rpid) }
func keyStats(oid int64, tp int32) string { return fmt.Sprintf(prefixStats, oid, tp) }

// Cache 封装 comment 的 Redis 缓存操作。
type Cache struct {
	rds *redis.Redis
}

// NewCache 构造 Cache。
func NewCache(rds *redis.Redis) *Cache {
	return &Cache{rds: rds}
}

// Ping 检查 Redis 连通性。
func (c *Cache) Ping(ctx context.Context) error {
	if c.rds.Ping() {
		return nil
	}
	return errors.New("comment/cache: redis ping failed")
}

// --- 评论列表缓存 ---

// GetList 读取评论列表缓存。
// miss 返回 ("", nil)。
func (c *Cache) GetList(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32) (string, error) {
	bs, err := c.rds.GetCtx(ctx, keyList(oid, tp, sort, pn, ps))
	if err != nil {
		if err == redis.Nil {
			return "", nil
		}
		return "", err
	}
	return bs, nil
}

// SetList 写入评论列表缓存。
func (c *Cache) SetList(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32, payload string) error {
	return c.rds.SetexCtx(ctx, keyList(oid, tp, sort, pn, ps), payload, cacheTTLList)
}

// --- 评论主体缓存 ---

// GetOne 读取单条评论缓存。
func (c *Cache) GetOne(ctx context.Context, rpid int64) (*model.Comment, error) {
	bs, err := c.rds.GetCtx(ctx, keyOne(rpid))
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, err
	}
	if bs == "" || bs == emptyMark {
		return nil, nil // 空标记视为未命中（避免数据被改后回填过期）
	}
	var cm model.Comment
	if err := json.Unmarshal([]byte(bs), &cm); err != nil {
		return nil, fmt.Errorf("comment/cache GetOne unmarshal: %w", err)
	}
	return &cm, nil
}

// SetOne 写入单条评论缓存。
func (c *Cache) SetOne(ctx context.Context, cm *model.Comment) error {
	bs, err := json.Marshal(cm)
	if err != nil {
		return err
	}
	return c.rds.SetexCtx(ctx, keyOne(cm.Rpid), string(bs), cacheTTLOne)
}

// DelOne 删除单条评论缓存（写操作后失效）。
func (c *Cache) DelOne(ctx context.Context, rpid int64) error {
	_, err := c.rds.DelCtx(ctx, keyOne(rpid))
	return err
}

// --- 计数缓存 ---

// GetStats 读取评论计数快照。
func (c *Cache) GetStats(ctx context.Context, oid int64, tp int32) (total, rootTotal int64, hit bool, err error) {
	bs, err := c.rds.GetCtx(ctx, keyStats(oid, tp))
	if err != nil {
		if err == redis.Nil {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	if bs == "" || bs == emptyMark {
		return 0, 0, true, nil
	}
	var s struct {
		Total     int64 `json:"total"`
		RootTotal int64 `json:"root_total"`
	}
	if err := json.Unmarshal([]byte(bs), &s); err != nil {
		return 0, 0, false, fmt.Errorf("comment/cache GetStats unmarshal: %w", err)
	}
	return s.Total, s.RootTotal, true, nil
}

// SetStats 写入评论计数快照。
func (c *Cache) SetStats(ctx context.Context, oid int64, tp int32, total, rootTotal int64) error {
	s := struct {
		Total     int64 `json:"total"`
		RootTotal int64 `json:"root_total"`
	}{Total: total, RootTotal: rootTotal}
	bs, _ := json.Marshal(s)
	return c.rds.SetexCtx(ctx, keyStats(oid, tp), string(bs), cacheTTLStats)
}

// --- 失效辅助 ---

// InvalidateListByOid 失效某 oid 下所有列表缓存（key 含通配）。
func (c *Cache) InvalidateListByOid(ctx context.Context, oid int64, tp int32) error {
	pattern := fmt.Sprintf("cmt:list:%d:%d:*", oid, tp)
	keys, err := c.rds.KeysCtx(ctx, pattern)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := c.rds.DelCtx(ctx, k); err != nil {
			return err
		}
	}
	return nil
}

// DelStats 删除计数缓存。
func (c *Cache) DelStats(ctx context.Context, oid int64, tp int32) error {
	_, err := c.rds.DelCtx(ctx, keyStats(oid, tp))
	return err
}

// --- Repository ---

// Cacher 是 Repository 对缓存层的最小依赖面（生产由 *Cache 实现）。
//
// 为什么要有这个接口：Repository.cache 原先是具体类型 *Cache，而 ServiceContext.Repository
// 又是具体类型 *repository.Repository，logic 单测无处塞替身，只能连真 Redis。这里抽出接口后，
// 测试用 NewWithDeps(内存缓存, 内存 commentMd, 内存 reportMd) 组装**真实的 Repository**，
// 缓存读穿/回填、失效顺序、计数口径这些判定仍然整条在被测路径上。
// 只声明 Repository 实际调用的方法（Cache.GetOne 目前没有调用方，故不进入接口）。
// 语义约定与 *Cache 一致：miss 返回零值且 err=nil，不视为错误。
type Cacher interface {
	Ping(ctx context.Context) error
	GetList(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32) (string, error)
	SetList(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32, payload string) error
	SetOne(ctx context.Context, cm *model.Comment) error
	DelOne(ctx context.Context, rpid int64) error
	GetStats(ctx context.Context, oid int64, tp int32) (total, rootTotal int64, hit bool, err error)
	SetStats(ctx context.Context, oid int64, tp int32, total, rootTotal int64) error
	InvalidateListByOid(ctx context.Context, oid int64, tp int32) error
	DelStats(ctx context.Context, oid int64, tp int32) error
}

// Repository 是 comment 服务的数据访问入口。
type Repository struct {
	cache     Cacher
	commentMd model.CommentModel
	reportMd  model.CommentReportModel
}

// New 构造 Repository（生产路径唯一入口）。
func New(rds *redis.Redis, conn sqlx.SqlConn) *Repository {
	return NewWithDeps(NewCache(rds), model.NewCommentModel(conn), model.NewCommentReportModel(conn))
}

// NewWithDeps 用给定的依赖组装 Repository。
// 生产代码只应通过 New 构造；本函数为 logic 单测提供注入缝（见 Cacher 注释）。
func NewWithDeps(cache Cacher, commentMd model.CommentModel, reportMd model.CommentReportModel) *Repository {
	return &Repository{cache: cache, commentMd: commentMd, reportMd: reportMd}
}

// Ping 检查 Redis 连通性。
func (r *Repository) Ping(ctx context.Context) error {
	return r.cache.Ping(ctx)
}

// PostComment 发布评论。
func (r *Repository) PostComment(ctx context.Context, c *model.Comment) (int64, int64, error) {
	rpid, ctime, err := r.commentMd.Insert(ctx, c)
	if err != nil {
		return 0, 0, err
	}
	c.Rpid = rpid
	_ = r.cache.SetOne(ctx, c)
	_ = r.cache.InvalidateListByOid(ctx, c.Oid, c.Tp)
	_ = r.cache.DelStats(ctx, c.Oid, c.Tp)
	return rpid, ctime, nil
}

// DeleteComment 删除评论。
func (r *Repository) DeleteComment(ctx context.Context, rpid, mid int64, admin bool) error {
	c, err := r.commentMd.FindOne(ctx, rpid)
	if err != nil {
		return err
	}
	if c == nil {
		return model.ErrCommentNotFoundOrForbidden
	}
	if err := r.commentMd.SoftDelete(ctx, rpid, mid, admin); err != nil {
		return err
	}
	_ = r.cache.DelOne(ctx, rpid)
	_ = r.cache.InvalidateListByOid(ctx, c.Oid, c.Tp)
	_ = r.cache.DelStats(ctx, c.Oid, c.Tp)
	return nil
}

// ListComments 分页查询根评论。
func (r *Repository) ListComments(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32) ([]*model.Comment, int32, error) {
	payload, err := r.cache.GetList(ctx, oid, tp, sort, pn, ps)
	if err != nil {
		return nil, 0, err
	}
	if payload != "" {
		var resp struct {
			Comments []*model.Comment `json:"comments"`
			Total    int32            `json:"total"`
		}
		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return nil, 0, fmt.Errorf("ListComments unmarshal cache: %w", err)
		}
		return resp.Comments, resp.Total, nil
	}
	comments, total, err := r.commentMd.ListRoots(ctx, oid, tp, sort, pn, ps)
	if err != nil {
		return nil, 0, err
	}
	resp := struct {
		Comments []*model.Comment `json:"comments"`
		Total    int32            `json:"total"`
	}{Comments: comments, Total: total}
	bs, _ := json.Marshal(resp)
	_ = r.cache.SetList(ctx, oid, tp, sort, pn, ps, string(bs))
	return comments, total, nil
}

// ListReplies 分页查询回复。
func (r *Repository) ListReplies(ctx context.Context, root int64, pn, ps int32) ([]*model.Comment, int32, error) {
	return r.commentMd.ListReplies(ctx, root, pn, ps)
}

// PinComment 置顶/取消置顶。
func (r *Repository) PinComment(ctx context.Context, rpid, oid int64, pin bool) error {
	if err := r.commentMd.SetPinned(ctx, rpid, oid, pin); err != nil {
		return err
	}
	_ = r.cache.DelOne(ctx, rpid)
	_ = r.cache.InvalidateListByOid(ctx, oid, 0)
	return nil
}

// ReportComment 记录举报。
func (r *Repository) ReportComment(ctx context.Context, rpt *model.CommentReport) (int64, error) {
	return r.reportMd.Insert(ctx, rpt)
}

// CommentStats 查询计数。
func (r *Repository) CommentStats(ctx context.Context, oid int64, tp int32) (int64, int64, error) {
	total, rootTotal, hit, err := r.cache.GetStats(ctx, oid, tp)
	if err != nil {
		return 0, 0, err
	}
	if hit {
		return total, rootTotal, nil
	}
	total, rootTotal, err = r.commentMd.CountByTarget(ctx, oid, tp)
	if err != nil {
		return 0, 0, err
	}
	_ = r.cache.SetStats(ctx, oid, tp, total, rootTotal)
	return total, rootTotal, nil
}

// IncrLikeCount 点赞数快照增量更新（由 engagement 事件触发）。
func (r *Repository) IncrLikeCount(ctx context.Context, rpid int64, delta int32) error {
	if err := r.commentMd.IncrLikeCount(ctx, rpid, delta); err != nil {
		return err
	}
	_ = r.cache.DelOne(ctx, rpid)
	return nil
}
