package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// clientSwitchColumns 是 ops_client_switch 的列清单，必须与
// deploy/migrations/ops-config/000004_create_ops_client_switch_table.sql 一致。
const clientSwitchColumns = "switch_id, switch_key, platform, min_version, max_version," +
	" enabled, config_id, operator_id, remark, version, ctime, mtime"

// switchKeyRe 是开关键格式：小写字母开头，可含数字/下划线/点，长度 2..64。
var switchKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_.]{1,63}$`)

// ValidSwitchKey 判定开关键格式。
func ValidSwitchKey(s string) bool { return switchKeyRe.MatchString(s) }

// MaxSwitchListLimit 运行时按端取开关的单次上限。
// 一端的能力开关是「十几到几十」这个量级；命中上限说明有人在把开关当配置项用，
// 那种需求应走 ops_config_item（见 README「与 operation.op_config 的边界」）。
const MaxSwitchListLimit = 500

// ClientSwitch 对应 ops_client_switch 表：某端从哪个版本起具备某能力。
//
// 它表达的是**能力门槛**，不是 UI 指令：本服务不因为 platform 不同返回不同的
// 布局、文案或组件结构（AGENTS.md §6）。
// 版本区间用点分十进制字符串存储，比较只能走 Go 侧 AppVersionInRange：
// MySQL 的字符串序会把 "7.10.0" 判成小于 "7.2.0"，那是把新版本当旧版本的事故。
type ClientSwitch struct {
	// SwitchID 自增主键。
	SwitchID int64 `db:"switch_id"`
	// SwitchKey 开关键（如 vertical_feed）。
	SwitchKey string `db:"switch_key"`
	// Platform 生效端，取值见 platform.go（必填，0 禁止入库）。
	Platform int32 `db:"platform"`
	// MinVersion 具备能力的最小版本（闭区间），空表示不限。
	MinVersion string `db:"min_version"`
	// MaxVersion 具备能力的最大版本（闭区间），空表示不限。
	MaxVersion string `db:"max_version"`
	// Enabled 1 开、2 关。
	Enabled int32 `db:"enabled"`
	// ConfigID 可选关联的配置项（0 表示无关联）：开关本身不带值时，
	// 用它把「端有能力」和「配置已放量」串起来，仍然是一张表一个事实源。
	ConfigID int64 `db:"config_id"`
	// OperatorID 最后操作人 admin_id。
	OperatorID int64 `db:"operator_id"`
	// Remark 备注：为什么这一端要单独开。
	Remark string `db:"remark"`
	// Version 乐观锁版本，SaveClientSwitchReq.expect_version 比对的就是它。
	Version int64 `db:"version"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// Available 判定某版本是否具备该能力（enabled=ON 且版本落在区间内）。
// version 为空或非法时：只有「不限版本」的开关才算具备能力——
// 无法判定版本时宁可不给能力，也不让一个未知版本走进新链路。
func (s *ClientSwitch) Available(version string) bool {
	if s.Enabled != StateOn {
		return false
	}
	return AppVersionInRange(version, s.MinVersion, s.MaxVersion)
}

// ClientSwitchFilter 后台列表条件（ListClientSwitches）。
type ClientSwitchFilter struct {
	Platform  int32
	SwitchKey string
	Enabled   int32
	ConfigID  int64
	Pn        int32
	Ps        int32
}

// ClientSwitchModel 抽象 ops_client_switch 表。
type ClientSwitchModel interface {
	// Insert 新建开关；(switch_key, platform) 冲突返回 ErrSwitchConflict。
	Insert(ctx context.Context, sw *ClientSwitch) (int64, error)
	// FindByID 按主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, switchID int64) (*ClientSwitch, error)
	// FindByKeyPlatform 按 upsert 句柄查询；不存在返回 (nil, nil)。
	FindByKeyPlatform(ctx context.Context, switchKey string, platform int32) (*ClientSwitch, error)
	// UpdateWithVersion 乐观锁更新，仅当库里 version == expectVersion 时生效并 version+1。
	UpdateWithVersion(ctx context.Context, sw *ClientSwitch, expectVersion, ts int64) (bool, error)
	// List 后台分页查询（带 COUNT），固定 switch_key ASC, platform ASC，必须 LIMIT。
	List(ctx context.Context, f ClientSwitchFilter) ([]*ClientSwitch, int64, error)
	// ListForPlatform 运行时取某端的开关全集（enabled 过滤留给 Go 侧做版本比较）。
	// 不在 SQL 里比版本的原因见结构体注释：点分版本必须逐段比较。
	ListForPlatform(ctx context.Context, platform int32, limit int) ([]*ClientSwitch, error)
	// BoundConfig 把开关关联到配置项（PublishConfig 之后回填用）；返回受影响行数。
	BoundConfig(ctx context.Context, switchID, configID, ts int64) (int64, error)
}

type defaultClientSwitchModel struct {
	conn sqlx.SqlConn
}

// NewClientSwitchModel 构造 ops_client_switch 的 sqlx 实现。
func NewClientSwitchModel(conn sqlx.SqlConn) ClientSwitchModel {
	return &defaultClientSwitchModel{conn: conn}
}

func normalizeSwitchForWrite(sw *ClientSwitch) error {
	if sw.SwitchKey == "" {
		return ErrSwitchKeyRequired
	}
	if !ValidSwitchKey(sw.SwitchKey) {
		return ErrSwitchKeyRequired
	}
	if !ValidPlatform(sw.Platform) {
		// 开关必须按端定义：不分端的「能力开关」会把四端耦合进同一次发布。
		return ErrPlatformRequired
	}
	if sw.MinVersion != "" {
		if exceedsColumn(sw.MinVersion, MaxAppVersionChars) {
			return ErrAppVersionTooLong
		}
		if _, ok := CompareAppVersion(sw.MinVersion, sw.MinVersion); !ok {
			return ErrAppVersionRangeInvalid
		}
	}
	if sw.MaxVersion != "" {
		if exceedsColumn(sw.MaxVersion, MaxAppVersionChars) {
			return ErrAppVersionTooLong
		}
		if _, ok := CompareAppVersion(sw.MaxVersion, sw.MaxVersion); !ok {
			return ErrAppVersionRangeInvalid
		}
	}
	if sw.MinVersion != "" && sw.MaxVersion != "" {
		if c, ok := CompareAppVersion(sw.MinVersion, sw.MaxVersion); !ok || c > 0 {
			return ErrAppVersionRangeInvalid
		}
	}
	if sw.Enabled == 0 {
		// 默认关闭：新建开关就放量出去是最典型的误操作。
		sw.Enabled = StateOff
	}
	if !ValidState(sw.Enabled) {
		return ErrRuleStateInvalid
	}
	if sw.Ctime == 0 {
		sw.Ctime = nowUnix()
	}
	sw.Mtime = sw.Ctime
	if sw.Version == 0 {
		sw.Version = 1
	}
	return nil
}

func (m *defaultClientSwitchModel) Insert(ctx context.Context, sw *ClientSwitch) (int64, error) {
	if err := normalizeSwitchForWrite(sw); err != nil {
		return 0, err
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO ops_client_switch ("+clientSwitchColumns+") VALUES ("+placeholders(12)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		sw.SwitchID, sw.SwitchKey, sw.Platform, sw.MinVersion, sw.MaxVersion, sw.Enabled,
		sw.ConfigID, sw.OperatorID, sw.Remark, sw.Version, sw.Ctime, sw.Mtime)
	if err != nil {
		return 0, fmt.Errorf("ops_client_switch Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_client_switch Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrSwitchConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ops_client_switch Insert LastInsertId: %w", err)
	}
	sw.SwitchID = id
	return id, nil
}

const clientSwitchSelect = "SELECT " + clientSwitchColumns + " FROM ops_client_switch"

func (m *defaultClientSwitchModel) FindByID(ctx context.Context, switchID int64) (*ClientSwitch, error) {
	var row ClientSwitch
	query := clientSwitchSelect + " WHERE switch_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, switchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_client_switch FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultClientSwitchModel) FindByKeyPlatform(ctx context.Context, switchKey string, platform int32) (*ClientSwitch, error) {
	if switchKey == "" {
		return nil, ErrSwitchKeyRequired
	}
	if !ValidPlatform(platform) {
		return nil, ErrPlatformRequired
	}
	var row ClientSwitch
	query := clientSwitchSelect + " WHERE switch_key = ? AND platform = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, switchKey, platform); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_client_switch FindByKeyPlatform: %w", err)
	}
	return &row, nil
}

func (m *defaultClientSwitchModel) UpdateWithVersion(ctx context.Context, sw *ClientSwitch, expectVersion, ts int64) (bool, error) {
	if sw.SwitchID <= 0 {
		return false, ErrSwitchNotFound
	}
	if err := normalizeSwitchForWrite(sw); err != nil {
		return false, err
	}
	// (switch_key, platform) 是唯一键：改键或改端可能撞到别人已有的行，
	// 先探测以便把「冲突」与「版本被抢先」区分开。
	var holder int64
	err := m.conn.QueryRowCtx(ctx, &holder,
		"SELECT switch_id FROM ops_client_switch WHERE switch_key = ? AND platform = ? AND switch_id <> ? LIMIT 1",
		sw.SwitchKey, sw.Platform, sw.SwitchID)
	if err != nil {
		// ErrNoRows = 这个 (switch_key, platform) 没被别人占着，可以继续。
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ops_client_switch UpdateWithVersion probe: %w", err)
		}
	} else if holder > 0 {
		return false, ErrSwitchConflict
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ops_client_switch SET switch_key = ?, platform = ?, min_version = ?, max_version = ?,"+
			" enabled = ?, config_id = ?, operator_id = ?, remark = ?,"+
			" version = version + 1, mtime = ?"+
			" WHERE switch_id = ? AND version = ?",
		sw.SwitchKey, sw.Platform, sw.MinVersion, sw.MaxVersion, sw.Enabled,
		sw.ConfigID, sw.OperatorID, sw.Remark, ts, sw.SwitchID, expectVersion)
	if err != nil {
		return false, fmt.Errorf("ops_client_switch UpdateWithVersion: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_client_switch UpdateWithVersion RowsAffected: %w", err)
	}
	if aff == 0 {
		return false, nil
	}
	sw.Version = expectVersion + 1
	sw.Mtime = ts
	return true, nil
}

func (m *defaultClientSwitchModel) List(ctx context.Context, f ClientSwitchFilter) ([]*ClientSwitch, int64, error) {
	where, args := "WHERE 1 = 1", make([]any, 0, 4)
	if f.Platform != 0 {
		where += " AND platform = ?"
		args = append(args, f.Platform)
	}
	if f.SwitchKey != "" {
		where += " AND switch_key LIKE ?"
		args = append(args, "%"+escapeLike(f.SwitchKey)+"%")
	}
	if f.Enabled > 0 {
		where += " AND enabled = ?"
		args = append(args, f.Enabled)
	}
	if f.ConfigID > 0 {
		where += " AND config_id = ?"
		args = append(args, f.ConfigID)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ops_client_switch "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_client_switch List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*ClientSwitch
	query := clientSwitchSelect + where + " ORDER BY switch_key ASC, platform ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_client_switch List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultClientSwitchModel) ListForPlatform(ctx context.Context, platform int32, limit int) ([]*ClientSwitch, error) {
	if !ValidPlatform(platform) {
		return nil, ErrPlatformRequired
	}
	if limit <= 0 || limit > MaxSwitchListLimit {
		limit = MaxSwitchListLimit
	}
	var rows []*ClientSwitch
	query := clientSwitchSelect + " WHERE platform = ? ORDER BY switch_key ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, platform, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_client_switch ListForPlatform: %w", err)
	}
	return rows, nil
}

func (m *defaultClientSwitchModel) BoundConfig(ctx context.Context, switchID, configID, ts int64) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ops_client_switch SET config_id = ?, mtime = ? WHERE switch_id = ?",
		configID, ts, switchID)
	if err != nil {
		return 0, fmt.Errorf("ops_client_switch BoundConfig: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_client_switch BoundConfig RowsAffected: %w", err)
	}
	return n, nil
}
