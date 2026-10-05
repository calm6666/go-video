package model

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Comment 评论主体行（DB 投影）。
// 状态机参见 AGENTS.md §8 与 comment.proto CommentState：
//
//	NORMAL/FOLDED/DELETED/PINNED/PENDING/REJECTED
//
// root=0 表示本身是根评论；parent=0 表示直接对 oid 评论。
type Comment struct {
	Rpid       int64  `db:"rpid"`        // 评论 ID
	Oid        int64  `db:"oid"`         // 目标 ID
	Tp         int32  `db:"tp"`          // 目标类型
	Root       int64  `db:"root"`        // 根评论 ID（0 表示本身是根）
	Parent     int64  `db:"parent"`      // 父评论 ID（0 表示直接对 oid）
	Mid        int64  `db:"mid"`         // 评论者用户 ID
	Content    string `db:"content"`     // 评论内容
	State      int32  `db:"state"`       // 状态
	Ctime      int64  `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime      int64  `db:"mtime"`       // 修改时间（Unix 秒）
	LikeCount  int32  `db:"like_count"`  // 点赞数快照
	ReplyCount int32  `db:"reply_count"` // 回复数快照
}

// CommentModel comment 表查询与写入接口。
type CommentModel interface {
	// Insert 发布评论/回复；返回新 rpid 与 ctime。
	Insert(ctx context.Context, c *Comment) (int64, int64, error)
	// SoftDelete 软删除（state=DELETED）；校验 mid 本人或 admin。
	SoftDelete(ctx context.Context, rpid, mid int64, admin bool) error
	// FindOne 查询单条评论；不存在返回 nil。
	FindOne(ctx context.Context, rpid int64) (*Comment, error)
	// ListRoots 分页查询目标下的根评论（root=0），按 sort 排序。
	// sort: "hot" 或 "time"；只返回非删除/非待审的可见评论。
	ListRoots(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32) ([]*Comment, int32, error)
	// ListReplies 分页查询某根评论下的回复（root=rpid）。
	ListReplies(ctx context.Context, root int64, pn, ps int32) ([]*Comment, int32, error)
	// SetPinned 置顶/取消置顶（state=PINNED / NORMAL）。
	// 同一 oid 下只能有一条置顶，置顶前先清除旧置顶。
	SetPinned(ctx context.Context, rpid, oid int64, pin bool) error
	// CountByTarget 统计目标下的评论总数与根评论数。
	CountByTarget(ctx context.Context, oid int64, tp int32) (total int64, rootTotal int64, err error)
	// IncrLikeCount 点赞数快照增量更新（由 engagement 点赞事件触发）。
	IncrLikeCount(ctx context.Context, rpid int64, delta int32) error
	// IncrReplyCount 回复数快照增量更新。
	IncrReplyCount(ctx context.Context, rpid int64, delta int32) error
}

type defaultCommentModel struct {
	conn sqlx.SqlConn
}

// NewCommentModel 创建 CommentModel 实现。
func NewCommentModel(conn sqlx.SqlConn) CommentModel {
	return &defaultCommentModel{conn: conn}
}

func (m *defaultCommentModel) Insert(ctx context.Context, c *Comment) (int64, int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO comment (oid, tp, root, parent, mid, content, state, ctime, mtime, like_count, reply_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)",
		c.Oid, c.Tp, c.Root, c.Parent, c.Mid, c.Content, c.State, c.Ctime, c.Mtime)
	if err != nil {
		return 0, 0, fmt.Errorf("comment Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("comment Insert LastInsertId: %w", err)
	}
	return id, c.Ctime, nil
}

func (m *defaultCommentModel) SoftDelete(ctx context.Context, rpid, mid int64, admin bool) error {
	if admin {
		// 管理员可删任意评论
		_, err := m.conn.ExecCtx(ctx,
			"UPDATE comment SET state = 2, mtime = ? WHERE rpid = ?", mid, rpid)
		return err
	}
	// 普通用户只能删自己的评论
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE comment SET state = 2, mtime = ? WHERE rpid = ? AND mid = ?", mid, rpid, mid)
	if err != nil {
		return fmt.Errorf("comment SoftDelete: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("comment SoftDelete RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrCommentNotFoundOrForbidden
	}
	return nil
}

func (m *defaultCommentModel) FindOne(ctx context.Context, rpid int64) (*Comment, error) {
	var c Comment
	query := "SELECT rpid, oid, tp, root, parent, mid, content, state, ctime, mtime, like_count, reply_count FROM comment WHERE rpid = ?"
	if err := m.conn.QueryRowCtx(ctx, &c, query, rpid); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("comment FindOne: %w", err)
	}
	return &c, nil
}

func (m *defaultCommentModel) ListRoots(ctx context.Context, oid int64, tp int32, sort string, pn, ps int32) ([]*Comment, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 49 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM comment WHERE oid = ? AND tp = ? AND root = 0 AND state NOT IN (2, 5)",
		oid, tp); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("comment ListRoots count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// 排序：置顶优先，其次按 sort
	order := "like_count DESC"
	if sort == "time" {
		order = "ctime DESC"
	}
	query := "SELECT rpid, oid, tp, root, parent, mid, content, state, ctime, mtime, like_count, reply_count FROM comment WHERE oid = ? AND tp = ? AND root = 0 AND state NOT IN (2, 5) ORDER BY state = 3 DESC, " + order + " LIMIT ? OFFSET ?"
	var rows []*Comment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, oid, tp, ps, offset); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("comment ListRoots list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultCommentModel) ListReplies(ctx context.Context, root int64, pn, ps int32) ([]*Comment, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 49 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM comment WHERE root = ? AND state NOT IN (2, 5)", root); err != nil {
		if err == sql.ErrNoRows {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("comment ListReplies count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*Comment
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT rpid, oid, tp, root, parent, mid, content, state, ctime, mtime, like_count, reply_count FROM comment WHERE root = ? AND state NOT IN (2, 5) ORDER BY ctime ASC LIMIT ? OFFSET ?",
		root, ps, offset); err != nil {
		if err == sql.ErrNoRows {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("comment ListReplies list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultCommentModel) SetPinned(ctx context.Context, rpid, oid int64, pin bool) error {
	// 同一 oid 下只能有一条置顶：置顶前先清除旧置顶
	if pin {
		// 清除旧置顶
		if _, err := m.conn.ExecCtx(ctx,
			"UPDATE comment SET state = 0 WHERE oid = ? AND state = 3", oid); err != nil {
			return fmt.Errorf("comment SetPinned clear old: %w", err)
		}
		// 设置新置顶
		if _, err := m.conn.ExecCtx(ctx,
			"UPDATE comment SET state = 3 WHERE rpid = ?", rpid); err != nil {
			return fmt.Errorf("comment SetPinned set new: %w", err)
		}
		return nil
	}
	// 取消置顶
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE comment SET state = 0 WHERE rpid = ? AND state = 3", rpid); err != nil {
		return fmt.Errorf("comment SetPinned unpin: %w", err)
	}
	return nil
}

func (m *defaultCommentModel) CountByTarget(ctx context.Context, oid int64, tp int32) (int64, int64, error) {
	var total, rootTotal int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM comment WHERE oid = ? AND tp = ? AND state NOT IN (2, 5)", oid, tp); err != nil {
		if err != sql.ErrNoRows {
			return 0, 0, fmt.Errorf("comment CountByTarget total: %w", err)
		}
	}
	if err := m.conn.QueryRowCtx(ctx, &rootTotal,
		"SELECT COUNT(*) FROM comment WHERE oid = ? AND tp = ? AND root = 0 AND state NOT IN (2, 5)", oid, tp); err != nil {
		if err != sql.ErrNoRows {
			return 0, 0, fmt.Errorf("comment CountByTarget rootTotal: %w", err)
		}
	}
	return total, rootTotal, nil
}

func (m *defaultCommentModel) IncrLikeCount(ctx context.Context, rpid int64, delta int32) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE comment SET like_count = like_count + ? WHERE rpid = ?", delta, rpid)
	return err
}

func (m *defaultCommentModel) IncrReplyCount(ctx context.Context, rpid int64, delta int32) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE comment SET reply_count = reply_count + ? WHERE rpid = ?", delta, rpid)
	return err
}
