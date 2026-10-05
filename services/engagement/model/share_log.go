package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ShareLog 分享记录表。
// (oid, mid, tp, day) 联合索引用于幂等去重：同一用户对同一对象同一天只计一次分享。
// 列名必须与 `deploy/migrations/engagement/000003_create_share.sql` 一致（`tp`，不是 `type`），
// 与 favorite_item/share_stat 同一口径。
type ShareLog struct {
	ID    int64 `db:"id"`    // 主键 ID
	Oid   int64 `db:"oid"`   // 目标 ID
	Mid   int64 `db:"mid"`   // 用户 ID
	Type  int32 `db:"tp"`    // 目标类型
	Day   int32 `db:"day"`   // 分享日期 YYYYMMDD
	Ctime int64 `db:"ctime"` // 分享时间（Unix 秒）
}

// ShareLogModel share_log 表查询与写入接口。
// 两个方法都接受可选事务会话（为空时退化为自动提交）：
// 分享写入与「分享数绝对快照」必须同事务，并且快照要在**同一会话**上 COUNT，
// 否则事件里的 share_count 读不到刚插入的这行。
type ShareLogModel interface {
	// AddIfNotExists 幂等记录分享；返回是否新增（用于决定计数是否累加）。
	AddIfNotExists(ctx context.Context, tx sqlx.Session, oid, mid int64, tp int32) (bool, error)
	// CountByOid 查询对象的分享总数。
	CountByOid(ctx context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error)
}

type defaultShareLogModel struct {
	conn sqlx.SqlConn
}

// NewShareLogModel 创建 ShareLogModel 实现。
func NewShareLogModel(conn sqlx.SqlConn) ShareLogModel {
	return &defaultShareLogModel{conn: conn}
}

func (m *defaultShareLogModel) AddIfNotExists(ctx context.Context, tx sqlx.Session, oid, mid int64, tp int32) (bool, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	day := currentDayYyyymmdd()
	now := nowUnix()
	res, err := session.ExecCtx(ctx,
		"INSERT IGNORE INTO share_log (oid, mid, tp, day, ctime) VALUES (?, ?, ?, ?, ?)",
		oid, mid, tp, day, now)
	if err != nil {
		return false, fmt.Errorf("share_log AddIfNotExists: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("share_log AddIfNotExists RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultShareLogModel) CountByOid(ctx context.Context, tx sqlx.Session, oid int64, tp int32) (int64, error) {
	var session sqlx.Session = tx
	if session == nil {
		session = m.conn
	}
	var count int64
	err := session.QueryRowCtx(ctx, &count,
		"SELECT COUNT(*) FROM share_log WHERE oid = ? AND tp = ?", oid, tp)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("share_log CountByOid: %w", err)
	}
	return count, nil
}
