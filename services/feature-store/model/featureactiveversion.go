package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// featureActiveVersionColumns 是 feature_active_version 的完整列清单（含自增主键）。
const featureActiveVersionColumns = "pointer_id, feature_key, active_version, previous_version," +
	" last_switch_id, ctime, mtime"

// featureActiveVersionInsertColumns 是写入列（不含自增主键，共 6 列）。
// 显式给自增列传 0 依赖 MySQL 的 AUTO_INCREMENT 语义，换个 sql_mode 就会插入 0 主键，
// 因此这里把主键从写入清单里摘掉。
const featureActiveVersionInsertColumns = "feature_key, active_version, previous_version," +
	" last_switch_id, ctime, mtime"

// NoActiveVersion 是「该 key 还没有对外生效版本」的哨兵值。
//
// 用 0 而不是 NULL：NULL 在 SQL 里参与比较永远是 unknown，
// 「指针为 0」能被 WHERE 直接筛出来，也能被 CAS 条件更新安全比较。
const NoActiveVersion int32 = 0

// ActiveVersion 对应 feature_active_version 表：每个 feature_key 一行的对外读版本指针。
//
// 为什么单独一张指针表而不是在 feature_definition.state 上放一个 is_active 标记：
//   - 读路径每次都要「先确定版本再取值」，指针表让这一步是一次主键/唯一索引命中，
//     而 state 标记要靠扫描该 key 的所有版本才知道哪个生效；
//   - 「同一 key 只能有一个生效版本」在指针表上是行内一列（天然唯一），
//     在定义表上则要靠 (feature_key, state=ACTIVE) 的唯一索引表达，
//     而 MySQL 不支持「部分唯一索引」，只能靠额外表或生成列。
//   - 切换因此变成一行上的条件更新：原子、可回滚（切回 previous_version）、并且只锁一行。
//
// previous_version 是降级与回滚的依据：PREVIOUS_VERSION 降级、以及
// 「切坏了立刻切回去」都直接读这两列，不需要再去审计表里找上一个生效版本。
type ActiveVersion struct {
	// PointerID 自增主键。
	PointerID int64 `db:"pointer_id"`
	// FeatureKey 特征键（唯一索引）。
	FeatureKey string `db:"feature_key"`
	// ActiveVersion 当前对外生效的版本；NoActiveVersion 表示该 key 没有可读版本。
	ActiveVersion int32 `db:"active_version"`
	// PreviousVersion 上一个生效过的版本（PREVIOUS_VERSION 降级与回滚的来源），0 = 没有。
	PreviousVersion int32 `db:"previous_version"`
	// LastSwitchID 最近一次改变该指针的审计行 ID（feature_version_switch.switch_id）。
	// 有这一列，「现在生效的版本是哪条审计决定的」可以一跳到位，不必按时间猜。
	LastSwitchID int64 `db:"last_switch_id"`
	// Ctime 建行时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后一次指针变更时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// HasActive 判断是否有对外生效版本。
func (a *ActiveVersion) HasActive() bool { return a != nil && a.ActiveVersion > NoActiveVersion }

// ActiveVersionModel 抽象 feature_active_version 表。
//
// 这里的每个写方法都要求传入事务 session：指针变更必须与审计行同生共死，
// 「审计写了、指针没动」等于切换丢失，「指针动了、审计没写」等于不可审计，
// 两种都不可接受（AGENTS.md §5 的写接口幂等与留痕要求）。
type ActiveVersionModel interface {
	// Ensure 幂等创建指针行（active_version=0）。注册新 key 时调用，不覆盖已有指针。
	Ensure(ctx context.Context, session sqlx.Session, featureKey string) error
	// FindOne 按 key 查询；不存在返回 (nil, nil)（未注册与「注册了但没有指针」是两件事）。
	FindOne(ctx context.Context, featureKey string) (*ActiveVersion, error)
	// ListByKeys 批量解析 ACTIVE 版本（批量读把 N 次指针查询压成一次）。
	ListByKeys(ctx context.Context, featureKeys []string) (map[string]*ActiveVersion, error)
	// LockForUpdate 事务内锁住指针行，供「校验 + 切指针 + 写审计」使用。
	// session 为 nil 时直接返回 ErrTransactionRequired：脱离事务的 FOR UPDATE 会立刻放锁，
	// 那是假的串行化保证，宁可失败也不能假装安全。
	LockForUpdate(ctx context.Context, session sqlx.Session, featureKey string) (*ActiveVersion, error)
	// Switch 乐观切换：WHERE active_version = expectFrom。
	// 返回 false 表示当前指针已不是 expectFrom（并发切换），调用方转 ErrVersionConflict。
	// expectFrom 传 NoActiveVersion 即「首次上线」（从无到有），此时 previous_version 保持 0。
	Switch(ctx context.Context, session sqlx.Session, featureKey string, expectFrom, toVersion int32,
		switchID int64) (bool, error)
	// RetireActive 下线当前生效版本：指针回到 NoActiveVersion，previous_version 记下被下线的版本，
	// 使读侧仍能按 PREVIOUS_VERSION→DEFAULT_VALUE 的顺序降级而不是直接报错。
	RetireActive(ctx context.Context, session sqlx.Session, featureKey string, expectActive int32,
		switchID int64) (bool, error)
}

type defaultActiveVersionModel struct {
	conn sqlx.SqlConn
}

// NewActiveVersionModel 构造 feature_active_version 的 sqlx 实现。
func NewActiveVersionModel(conn sqlx.SqlConn) ActiveVersionModel {
	return &defaultActiveVersionModel{conn: conn}
}

// exec 选择执行器：事务内必须传 session，nil 表示用连接自建的单语句事务。
func (m *defaultActiveVersionModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

const activeVersionSelect = "SELECT " + featureActiveVersionColumns + " FROM feature_active_version"

func (m *defaultActiveVersionModel) Ensure(ctx context.Context, session sqlx.Session, featureKey string) error {
	if strings.TrimSpace(featureKey) == "" {
		return ErrFeatureKeyRequired
	}
	now := nowUnix()
	_, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO feature_active_version ("+featureActiveVersionInsertColumns+") VALUES ("+placeholders(6)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		featureKey, NoActiveVersion, NoActiveVersion, 0, now, now)
	if err != nil {
		return fmt.Errorf("feature_active_version Ensure: %w", err)
	}
	return nil
}

func (m *defaultActiveVersionModel) FindOne(ctx context.Context, featureKey string) (*ActiveVersion, error) {
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	var row ActiveVersion
	err := m.conn.QueryRowCtx(ctx, &row, activeVersionSelect+" WHERE feature_key = ? LIMIT 1", featureKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("feature_active_version FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultActiveVersionModel) ListByKeys(ctx context.Context, featureKeys []string) (map[string]*ActiveVersion, error) {
	if len(featureKeys) == 0 {
		return nil, nil
	}
	if len(featureKeys) > MaxBatchFeatures {
		return nil, fmt.Errorf("%w: %d keys > %d", ErrTooManyEntries, len(featureKeys), MaxBatchFeatures)
	}
	args := make([]any, 0, len(featureKeys))
	for _, k := range featureKeys {
		if strings.TrimSpace(k) == "" {
			return nil, ErrFeatureKeyRequired
		}
		args = append(args, k)
	}
	query := activeVersionSelect + " WHERE feature_key IN (" + placeholders(len(featureKeys)) + ")"
	var rows []*ActiveVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return map[string]*ActiveVersion{}, nil
		}
		return nil, fmt.Errorf("feature_active_version ListByKeys: %w", err)
	}
	out := make(map[string]*ActiveVersion, len(rows))
	for _, r := range rows {
		out[r.FeatureKey] = r
	}
	return out, nil
}

func (m *defaultActiveVersionModel) LockForUpdate(ctx context.Context, session sqlx.Session,
	featureKey string) (*ActiveVersion, error) {
	if session == nil {
		return nil, ErrTransactionRequired
	}
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	var row ActiveVersion
	err := session.QueryRowCtx(ctx, &row,
		activeVersionSelect+" WHERE feature_key = ? LIMIT 1 FOR UPDATE", featureKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 指针行不存在：先注册流程没走完，不能让切换「顺手」创建指针行，
			// 那会绕开 RegisterFeature 的定义校验。
			return nil, ErrActiveVersionMissing
		}
		return nil, fmt.Errorf("feature_active_version LockForUpdate: %w", err)
	}
	return &row, nil
}

func (m *defaultActiveVersionModel) Switch(ctx context.Context, session sqlx.Session, featureKey string,
	expectFrom, toVersion int32, switchID int64) (bool, error) {
	if session == nil {
		return false, ErrTransactionRequired
	}
	if strings.TrimSpace(featureKey) == "" {
		return false, ErrFeatureKeyRequired
	}
	if toVersion < 1 {
		return false, ErrFeatureVersionRequired
	}
	if expectFrom == toVersion {
		// 「切到同一个版本」不是幂等重放而是调用方算错了基线：
		// 放过它会让 previous_version 被写成自己，降级路径当场失效。
		return false, ErrVersionConflict
	}
	// previous_version 只在真的有旧版本时覆盖：首次上线（expectFrom=0）保持 0，
	// 否则 PREVIOUS_VERSION 降级会指向一个从来没生效过的版本。
	res, err := session.ExecCtx(ctx,
		"UPDATE feature_active_version SET active_version = ?,"+
			" previous_version = IF(? = 0, previous_version, ?), last_switch_id = ?, mtime = ?"+
			" WHERE feature_key = ? AND active_version = ?",
		toVersion, expectFrom, expectFrom, switchID, nowUnix(), featureKey, expectFrom)
	if err != nil {
		return false, fmt.Errorf("feature_active_version Switch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_active_version Switch RowsAffected: %w", err)
	}
	// 不依赖 clientFoundRows：上面已经拒绝 expectFrom == toVersion，
	// 因此 active_version 这一列必然改变，「实际改变行数」与「匹配行数」在这里恒等，
	// n == 0 只可能是指针已被并发切走（与全仓 DSN 不开 clientFoundRows 的约定一致）。
	return n == 1, nil
}

func (m *defaultActiveVersionModel) RetireActive(ctx context.Context, session sqlx.Session, featureKey string,
	expectActive int32, switchID int64) (bool, error) {
	if session == nil {
		return false, ErrTransactionRequired
	}
	if strings.TrimSpace(featureKey) == "" {
		return false, ErrFeatureKeyRequired
	}
	if expectActive < 1 {
		return false, ErrFeatureVersionRequired
	}
	res, err := session.ExecCtx(ctx,
		"UPDATE feature_active_version SET active_version = ?, previous_version = ?, last_switch_id = ?,"+
			" mtime = ? WHERE feature_key = ? AND active_version = ?",
		NoActiveVersion, expectActive, switchID, nowUnix(), featureKey, expectActive)
	if err != nil {
		return false, fmt.Errorf("feature_active_version RetireActive: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("feature_active_version RetireActive RowsAffected: %w", err)
	}
	return n == 1, nil
}
