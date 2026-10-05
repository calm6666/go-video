package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// configColumns 是 op_config 的列清单，必须与
// deploy/migrations/operation/000002_create_operation_config_task_tables.sql 一致。
const configColumns = "id, cfg_key, cfg_value, value_type, scope, version, state, operator, remark, ctime, mtime"

// ErrConfigExists 表示 (cfg_key, scope) 已存在（唯一索引冲突）。
// repository 捕获后按“更新”路径重试，避免依赖 driver 专有错误类型。
var ErrConfigExists = errors.New("operation: ops config already exists")

// OpsConfig 对应 op_config 表：运营配置项。
// 只承载内容展示、审核阈值、灰度开关等运营参数；
// 不包含会员、订单、支付、广告投放等商业化配置（AGENTS.md §1）。
type OpsConfig struct {
	// ID 自增主键。
	ID int64 `db:"id"`
	// CfgKey 配置键（如 moderation.auto_publish_threshold）。
	CfgKey string `db:"cfg_key"`
	// CfgValue 配置值（文本承载，按 ValueType 解析）。
	CfgValue string `db:"cfg_value"`
	// ValueType 值类型：string/int/bool/json。
	ValueType string `db:"value_type"`
	// Scope 生效范围：global 或端标识（android/ios/harmony/desktop）。
	Scope string `db:"scope"`
	// Version 版本号：每次成功写入 +1，是乐观锁依据。
	Version int64 `db:"version"`
	// State 1 生效、2 下线。
	State int32 `db:"state"`
	// Operator 最后修改人 admin_id。
	Operator int64 `db:"operator"`
	// Remark 变更说明。
	Remark string `db:"remark"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// OpsConfigModel 抽象 op_config 表。
type OpsConfigModel interface {
	// Insert 新建配置；(cfg_key, scope) 冲突时返回 ErrConfigExists。
	Insert(ctx context.Context, c *OpsConfig) (int64, error)
	// FindOne 按 (cfg_key, scope) 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, cfgKey, scope string) (*OpsConfig, error)
	// UpdateWithVersion 乐观锁更新：仅当库中 version == expectVersion 时写入并把 version 置为 expectVersion+1。
	// 返回 (更新后配置, 是否更新成功)；未命中版本时 updated=false 且不报错，由 repository 转成冲突错误。
	UpdateWithVersion(ctx context.Context, c *OpsConfig, expectVersion int64) (*OpsConfig, bool, error)
	// List 分页查询；scope 为空表示全部范围。
	List(ctx context.Context, scope string, pn, ps int32) ([]*OpsConfig, int64, error)
}

type defaultOpsConfigModel struct {
	conn sqlx.SqlConn
}

// NewOpsConfigModel 构造 op_config 的 sqlx 实现。
func NewOpsConfigModel(conn sqlx.SqlConn) OpsConfigModel {
	return &defaultOpsConfigModel{conn: conn}
}

func (m *defaultOpsConfigModel) Insert(ctx context.Context, c *OpsConfig) (int64, error) {
	now := nowUnix()
	if c.Ctime == 0 {
		c.Ctime = now
	}
	c.Mtime = now
	if c.Version == 0 {
		c.Version = 1
	}
	// uniq_cfg_key_scope 命中时 ON DUPLICATE KEY UPDATE 为刻意空更新，
	// 用 RowsAffected==0 判定冲突，不依赖 driver 专有错误。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_config ("+configColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		c.ID, c.CfgKey, c.CfgValue, c.ValueType, c.Scope, c.Version, c.State, c.Operator, c.Remark, c.Ctime, c.Mtime)
	if err != nil {
		return 0, fmt.Errorf("op_config Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_config Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrConfigExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("op_config Insert LastInsertId: %w", err)
	}
	c.ID = id
	return id, nil
}

func (m *defaultOpsConfigModel) FindOne(ctx context.Context, cfgKey, scope string) (*OpsConfig, error) {
	var c OpsConfig
	query := "SELECT " + configColumns + " FROM op_config WHERE cfg_key = ? AND scope = ?"
	if err := m.conn.QueryRowCtx(ctx, &c, query, cfgKey, scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_config FindOne: %w", err)
	}
	return &c, nil
}

func (m *defaultOpsConfigModel) UpdateWithVersion(ctx context.Context, c *OpsConfig, expectVersion int64) (*OpsConfig, bool, error) {
	next := expectVersion + 1
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE op_config SET cfg_value = ?, value_type = ?, state = ?, operator = ?, remark = ?, version = ?, mtime = ?"+
			" WHERE cfg_key = ? AND scope = ? AND version = ?",
		c.CfgValue, c.ValueType, c.State, c.Operator, c.Remark, next, nowUnix(),
		c.CfgKey, c.Scope, expectVersion)
	if err != nil {
		return nil, false, fmt.Errorf("op_config UpdateWithVersion: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("op_config UpdateWithVersion RowsAffected: %w", err)
	}
	if aff == 0 {
		// 版本被其他操作者抢先推进：调用方需重新读取后重试。
		return nil, false, nil
	}
	out := *c
	out.Version = next
	return &out, true, nil
}

func (m *defaultOpsConfigModel) List(ctx context.Context, scope string, pn, ps int32) ([]*OpsConfig, int64, error) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 2)
	if scope != "" {
		where += " AND scope = ?"
		args = append(args, scope)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM op_config "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("op_config List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), ps, (pn-1)*ps)
	var rows []*OpsConfig
	query := "SELECT " + configColumns + " FROM op_config " + where + " ORDER BY cfg_key ASC, scope ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("op_config List: %w", err)
	}
	return rows, total, nil
}
