package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RelationSpecial 特别关注关系记录。
// (mid, special_mid) 唯一索引保证幂等。
// 约束：特别关注必先关注（由 repository 校验 relation_follow 中存在正常关注记录）。
type RelationSpecial struct {
	ID         int64 `db:"id"`          // 主键 ID
	Mid        int64 `db:"mid"`         // 操作用户 ID
	SpecialMid int64 `db:"special_mid"` // 被特别关注者 ID
	State      int32 `db:"state"`       // 0 正常、1 已取消（软删除）
	Ctime      int64 `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime      int64 `db:"mtime"`       // 修改时间（Unix 秒）
}

// RelationSpecialModel relation_special 表查询与写入接口。
type RelationSpecialModel interface {
	// Upsert 特别关注/恢复；幂等。返回 oldState：无记录返回 -1。
	Upsert(ctx context.Context, s *RelationSpecial, newState int32) (oldState int32, err error)
	// Delete 取消特别关注（软删除 state=1）；幂等。
	Delete(ctx context.Context, mid, specialMid int64) (oldState int32, err error)
	// FindOne 查询 mid 是否特别关注 special_mid，仅返回 state=0 的记录。
	FindOne(ctx context.Context, mid, specialMid int64) (*RelationSpecial, error)
	// FindSpecials 批量查询 mid 特别关注了 mids 中的哪些。
	// 返回 specialMid → 是否特别关注；口径与 FindOne 一致（只看 state=0）。
	FindSpecials(ctx context.Context, mid int64, mids []int64) (map[int64]bool, error)
}

type defaultRelationSpecialModel struct {
	conn sqlx.SqlConn
}

// NewRelationSpecialModel 创建 RelationSpecialModel 实现。
func NewRelationSpecialModel(conn sqlx.SqlConn) RelationSpecialModel {
	return &defaultRelationSpecialModel{conn: conn}
}

func (m *defaultRelationSpecialModel) Upsert(ctx context.Context, s *RelationSpecial, newState int32) (int32, error) {
	var oldState int32 = -1
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_special WHERE mid = ? AND special_mid = ?",
		s.Mid, s.SpecialMid)
	if err == nil {
		oldState = cur
	} else if !errors.Is(err, sql.ErrNoRows) {
		return -1, fmt.Errorf("relation_special Upsert select: %w", err)
	}

	if oldState == newState {
		return oldState, nil
	}

	now := nowUnix()
	if _, err := m.conn.ExecCtx(ctx,
		"INSERT INTO relation_special (mid, special_mid, state, ctime, mtime) VALUES (?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE state = VALUES(state), mtime = VALUES(mtime)",
		s.Mid, s.SpecialMid, newState, now, now); err != nil {
		return -1, fmt.Errorf("relation_special Upsert: %w", err)
	}
	return oldState, nil
}

func (m *defaultRelationSpecialModel) Delete(ctx context.Context, mid, specialMid int64) (int32, error) {
	var cur int32
	err := m.conn.QueryRowCtx(ctx, &cur,
		"SELECT state FROM relation_special WHERE mid = ? AND special_mid = ?",
		mid, specialMid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, nil
		}
		return -1, fmt.Errorf("relation_special Delete select: %w", err)
	}
	if cur == SpecialStateDeleted {
		return cur, nil
	}
	if _, err := m.conn.ExecCtx(ctx,
		"UPDATE relation_special SET state = ?, mtime = ? WHERE mid = ? AND special_mid = ? AND state = ?",
		SpecialStateDeleted, nowUnix(), mid, specialMid, SpecialStateNormal); err != nil {
		return -1, fmt.Errorf("relation_special Delete: %w", err)
	}
	return cur, nil
}

func (m *defaultRelationSpecialModel) FindOne(ctx context.Context, mid, specialMid int64) (*RelationSpecial, error) {
	var s RelationSpecial
	query := "SELECT id, mid, special_mid, state, ctime, mtime FROM relation_special WHERE mid = ? AND special_mid = ? AND state = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, mid, specialMid, SpecialStateNormal); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("relation_special FindOne: %w", err)
	}
	return &s, nil
}

// FindSpecials 复刻 WHERE mid=? AND state=? AND special_mid IN (?)，走 uniq_mid_special (mid, special_mid)。
// 这是本服务第一个「特别关注集合」的读入口：契约此前只有 AddSpecial/DelSpecial 两个写 RPC，
// 没有任何读口，导致调用方无法判断 special 位（account 的 RichRelations 因此长期只能返回未接线）。
func (m *defaultRelationSpecialModel) FindSpecials(ctx context.Context, mid int64, mids []int64) (map[int64]bool, error) {
	if len(mids) == 0 {
		return map[int64]bool{}, nil
	}
	type row struct {
		SpecialMid int64 `db:"special_mid"`
	}
	var rows []row
	query := "SELECT special_mid FROM relation_special WHERE mid = ? AND state = ? AND special_mid IN (?)"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, mid, SpecialStateNormal, mids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[int64]bool{}, nil
		}
		return nil, fmt.Errorf("relation_special FindSpecials: %w", err)
	}
	out := make(map[int64]bool, len(mids))
	for _, specialMid := range mids {
		out[specialMid] = false
	}
	for _, r := range rows {
		out[r.SpecialMid] = true
	}
	return out, nil
}
