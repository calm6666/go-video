package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// dailyTossColumns 与 000001_create_coin_tables.sql 的 cn_daily_toss 逐列对应。
const dailyTossColumns = "id, mid, date, tossed, ctime, mtime"

// DailyToss 单用户单自然日的投币量（cn_daily_toss 投影）。
//
// 一行一天，唯一键 (mid, date)：跨日不需要任何「清零」代码，
// 新日期没有行就是 0。日限额判定完全依赖 AccumulateTx 的条件更新，
// 因此同一天并发投币不会突破 DailyLimit（不会出现「两个请求都读到 9 再各写 10」）。
type DailyToss struct {
	ID     int64 `db:"id"`     // 自增主键
	Mid    int64 `db:"mid"`    // 用户 ID
	Date   int32 `db:"date"`   // 日桶编号 YYYYMMDD（本地时区自然日，见 now.go DayNo）
	Tossed int32 `db:"tossed"` // 当日已投出枚数（取消投币会回退本列，见 RollbackTx）
	Ctime  int64 `db:"ctime"`  // 创建时间（Unix 秒）
	Mtime  int64 `db:"mtime"`  // 修改时间（Unix 秒）
}

// DailyTossModel cn_daily_toss 表读写接口。
type DailyTossModel interface {
	// FindOne 读某日投币量；无行返回 (nil, nil)，语义即「当日 0 枚」。
	FindOne(ctx context.Context, mid int64, date int32) (*DailyToss, error)
	// EnsureTx 保证 (mid, date) 这一行存在（冲突不改值），让后续条件累加有行可锁。
	EnsureTx(ctx context.Context, session sqlx.Session, mid int64, date int32) error
	// AccumulateTx 日额度条件累加：tossed += delta，条件 tossed + delta <= dailyLimit。
	// 返回 false 即「今日额度不足」（调用方转 reason=DAILY_LIMIT 并回滚事务），不是错误。
	AccumulateTx(ctx context.Context, session sqlx.Session, mid int64, date, delta, dailyLimit int32) (bool, error)
	// RollbackTx 取消投币时回退当日计数，GREATEST 夹底到 0：
	// 宁可少给额度，也绝不因为重复回退而凭空多出借额度。
	RollbackTx(ctx context.Context, session sqlx.Session, mid int64, date, delta int32) error
}

type defaultDailyTossModel struct {
	conn sqlx.SqlConn
}

// NewDailyTossModel 构造 cn_daily_toss 的 sqlx 实现。
func NewDailyTossModel(conn sqlx.SqlConn) DailyTossModel {
	return &defaultDailyTossModel{conn: conn}
}

func (m *defaultDailyTossModel) FindOne(ctx context.Context, mid int64, date int32) (*DailyToss, error) {
	var row DailyToss
	query := "SELECT " + dailyTossColumns + " FROM cn_daily_toss WHERE mid = ? AND date = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, mid, date); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cn_daily_toss FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultDailyTossModel) EnsureTx(ctx context.Context, session sqlx.Session, mid int64, date int32) error {
	now := nowUnix()
	_, err := executor(session, m.conn).ExecCtx(ctx,
		"INSERT INTO cn_daily_toss (mid, date, tossed, ctime, mtime) VALUES (?, ?, 0, ?, ?)"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		mid, date, now, now)
	if err != nil {
		return fmt.Errorf("cn_daily_toss EnsureTx: %w", err)
	}
	return nil
}

func (m *defaultDailyTossModel) AccumulateTx(ctx context.Context, session sqlx.Session, mid int64, date, delta, dailyLimit int32) (bool, error) {
	if delta <= 0 {
		return false, ErrInvalidTossCount
	}
	if dailyLimit < 0 {
		dailyLimit = 0
	}
	// tossed + ? <= ? 是整个日限判定的唯一真值来源：MySQL 在同一行的条件更新上串行，
	// 因此并发下最多有一个请求把 tossed 从 limit-1 推到 limit，另一个拿 RowsAffected=0。
	res, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_daily_toss SET tossed = tossed + ?, mtime = ?"+
			" WHERE mid = ? AND date = ? AND tossed + ? <= ?",
		delta, nowUnix(), mid, date, delta, dailyLimit)
	if err != nil {
		return false, fmt.Errorf("cn_daily_toss AccumulateTx: %w", err)
	}
	return rowsAffectedOne(res, "cn_daily_toss AccumulateTx")
}

func (m *defaultDailyTossModel) RollbackTx(ctx context.Context, session sqlx.Session, mid int64, date, delta int32) error {
	if delta <= 0 {
		return ErrInvalidTossCount
	}
	// 行不存在（例如回滚的是历史日桶且已被清理）时 affected=0，不报错：
	// 取消退币的正确性只取决于余额与流水，日计数只是额度回收，缺行等价于 0。
	_, err := executor(session, m.conn).ExecCtx(ctx,
		"UPDATE cn_daily_toss SET tossed = GREATEST(tossed - ?, 0), mtime = ?"+
			" WHERE mid = ? AND date = ?",
		delta, nowUnix(), mid, date)
	if err != nil {
		return fmt.Errorf("cn_daily_toss RollbackTx: %w", err)
	}
	return nil
}
