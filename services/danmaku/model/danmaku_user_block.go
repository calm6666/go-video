package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 用户屏蔽类型，与 danmaku_user_block.type 列和 rpc.UserBlockType 一致。
const (
	// UserBlockMid 屏蔽某用户发送的全部弹幕。
	UserBlockMid int32 = 1
	// UserBlockKeyword 屏蔽包含某关键词的弹幕。
	UserBlockKeyword int32 = 2
)

// 用户屏蔽状态。
const (
	// UserBlockOff 已解除（保留行做幂等锚点）。
	UserBlockOff int32 = 0
	// UserBlockOn 生效。
	UserBlockOn int32 = 1
)

// UserBlock 用户级屏蔽（danmaku_user_block 表）。
// 生效点在读取侧：ListDanmaku 按 viewer_mid 载入屏蔽列表后过滤，
// 不改动弹幕主表状态，因此解除屏蔽无需回填历史数据。
type UserBlock struct {
	ID         int64  `db:"id"`          // 自增主键
	Mid        int64  `db:"mid"`         // 所属用户
	Type       int32  `db:"type"`        // 屏蔽类型
	BlockedMid int64  `db:"blocked_mid"` // 被屏蔽用户（type=UserBlockMid 时非 0）
	Keyword    string `db:"keyword"`     // 被屏蔽关键词（type=UserBlockKeyword 时非空）
	State      int32  `db:"state"`       // 1 生效、0 已解除
	Ctime      int64  `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime      int64  `db:"mtime"`       // 修改时间（Unix 秒）
}

// UserBlockModel danmaku_user_block 表查询与写入接口。
type UserBlockModel interface {
	// Upsert 按 (mid, blocked_mid, keyword) 唯一索引写入或更新状态，实现幂等屏蔽。
	Upsert(ctx context.Context, b *UserBlock) (int64, error)
	// ListEnabled 列出该用户全部生效屏蔽项。
	ListEnabled(ctx context.Context, mid int64) ([]*UserBlock, error)
	// List 分页查询；blockType<=0 表示不过滤类型。
	List(ctx context.Context, mid int64, blockType int32, pn, ps int32) ([]*UserBlock, int32, error)
}

type defaultUserBlockModel struct {
	conn sqlx.SqlConn
}

// NewUserBlockModel 创建 UserBlockModel 实现。
func NewUserBlockModel(conn sqlx.SqlConn) UserBlockModel {
	return &defaultUserBlockModel{conn: conn}
}

func (m *defaultUserBlockModel) Upsert(ctx context.Context, b *UserBlock) (int64, error) {
	now := nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO danmaku_user_block (mid, type, blocked_mid, keyword, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE type = VALUES(type), state = VALUES(state), mtime = VALUES(mtime)",
		b.Mid, b.Type, b.BlockedMid, b.Keyword, b.State, now, now)
	if err != nil {
		return 0, fmt.Errorf("danmaku_user_block Upsert: %w", err)
	}
	var id int64
	err = m.conn.QueryRowCtx(ctx, &id,
		"SELECT id FROM danmaku_user_block WHERE mid = ? AND blocked_mid = ? AND keyword = ? LIMIT 1",
		b.Mid, b.BlockedMid, b.Keyword)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrInvalidUserBlock
		}
		return 0, fmt.Errorf("danmaku_user_block Upsert find: %w", err)
	}
	return id, nil
}

func (m *defaultUserBlockModel) ListEnabled(ctx context.Context, mid int64) ([]*UserBlock, error) {
	if mid <= 0 {
		return nil, nil
	}
	var rows []*UserBlock
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, mid, type, blocked_mid, keyword, state, ctime, mtime FROM danmaku_user_block WHERE mid = ? AND state = ? ORDER BY id ASC",
		mid, UserBlockOn)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_user_block ListEnabled: %w", err)
	}
	return rows, nil
}

func (m *defaultUserBlockModel) List(ctx context.Context, mid int64, blockType int32, pn, ps int32) ([]*UserBlock, int32, error) {
	if mid <= 0 {
		return nil, 0, ErrInvalidMid
	}
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 20
	}

	where := "WHERE mid = ?"
	args := []interface{}{mid}
	if blockType > 0 {
		where += " AND type = ?"
		args = append(args, blockType)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM danmaku_user_block "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("danmaku_user_block List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// 列表只展示生效项，total 为筛选条件下的总数（含已解除），
	// 便于运营判断是否需要清理历史屏蔽。
	where += " AND state = ?"
	listArgs := append(append([]interface{}{}, args...), UserBlockOn, ps, (pn-1)*ps)
	var rows []*UserBlock
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, mid, type, blocked_mid, keyword, state, ctime, mtime FROM danmaku_user_block "+
			where+" ORDER BY id DESC LIMIT ? OFFSET ?", listArgs...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("danmaku_user_block List: %w", err)
	}
	return rows, total, nil
}
