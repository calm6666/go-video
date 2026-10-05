package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// versionColumns 是 ops_config_version 的列清单，必须与
// deploy/migrations/ops-config/000001_create_ops_config_publish_tables.sql 一致。
const versionColumns = "version_id, config_id, version, cfg_value, value_type, change_type," +
	" rollback_from, operator_id, operator_name, reason, audit_entry_id, request_id, published_at, ctime"

// ConfigVersion 对应 ops_config_version 表：一次发布的**不可变**值快照。
//
// 本表的接口只有 INSERT 与 SELECT，唯一的 UPDATE 是 SetAuditEntry，
// 且它只写 audit_entry_id 这个「补充引用」列，不改值。
// 这是「回滚不改写历史」的代码级体现：回滚是再发布一个新版本号，
// 值取自历史行，于是版本序列单调递增、任何人都能事后回答
// 「线上此刻跑的是哪一次改动、谁改的、为什么改」。
type ConfigVersion struct {
	// VersionID 自增主键。
	VersionID int64 `db:"version_id"`
	// ConfigID 所属配置项（ops_config_item.config_id）。
	ConfigID int64 `db:"config_id"`
	// Version 版本号，同一配置项内单调递增且唯一。
	Version int64 `db:"version"`
	// Value 本次发布的值（列名 cfg_value：与 operation.op_config 保持同名，
	// 同时避开 SQL 里 VALUE/VALUES 的读写歧义）。
	Value string `db:"cfg_value"`
	// ValueType 值类型快照：与 config_item.value_type 一致，冗余存一份是为了
	// 让「按 v3 解释这段值」这件事不依赖当前行的可变状态。
	ValueType int32 `db:"value_type"`
	// ChangeType create / publish / rollback。
	ChangeType string `db:"change_type"`
	// RollbackFrom 回滚来源版本号，0 表示非回滚。
	RollbackFrom int64 `db:"rollback_from"`
	// OperatorID 发布人 admin_id。
	OperatorID int64 `db:"operator_id"`
	// OperatorName 发布人展示名快照（改名不追改，排障时能看清当时是谁）。
	OperatorName string `db:"operator_name"`
	// Reason 变更原因（契约层必填：影响线上展示的动作必须能回答为什么）。
	Reason string `db:"reason"`
	// AuditEntryID audit.audit_entry 引用。写入方是本服务（见 rpc/opsconfig.proto 文件头），
	// 0 表示审计写入待补偿——宁可留 0 让缺口可见，也不假装已经存证。
	AuditEntryID int64 `db:"audit_entry_id"`
	// RequestID 触发本次写入的幂等键（唯一索引）：重放同一 request_id 不会产出第二个版本行。
	RequestID string `db:"request_id"`
	// PublishedAt 发布时间（Unix 秒）。
	PublishedAt int64 `db:"published_at"`
	// Ctime 入库时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
}

// ConfigVersionModel 抽象 ops_config_version 表。
type ConfigVersionModel interface {
	// Insert 追加一个版本快照。
	// (config_id, version) 或 request_id 命中唯一键时返回 ErrVersionExists：
	// 调用方先按 request_id 回查，命中即为幂等重放（reused=true），
	// 未命中则是版本号竞争，需重读 latest_version 后重试。
	Insert(ctx context.Context, v *ConfigVersion) (int64, error)
	// InsertTx 与 Insert 同一套校验与 SQL，差别只在跑在调用方的事务里。
	// 发布必须「插快照 + 推指针」原子完成，否则会出现只落了版本行、线上没动的半发布。
	InsertTx(ctx context.Context, session sqlx.Session, v *ConfigVersion) (int64, error)
	// FindOne 按 (config_id, version) 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, configID, version int64) (*ConfigVersion, error)
	// FindByRequestID 按幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*ConfigVersion, error)
	// FindLatest 取某配置项的最新版本（按 version 降序，而不是按 published_at，
	// 因为同一秒内可能有两个版本，版本号才是唯一确定的次序）。
	FindLatest(ctx context.Context, configID int64) (*ConfigVersion, error)
	// FindVersions 批量取指定版本号（灰度规则校验目标版本是否存在时用，一次取回）。
	FindVersions(ctx context.Context, configID int64, versions []int64) (map[int64]*ConfigVersion, error)
	// ListByConfig 版本历史分页，固定 version DESC。
	ListByConfig(ctx context.Context, configID int64, pn, ps int32) ([]*ConfigVersion, int64, error)
	// MaxVersion 返回某配置项已用过的最大版本号（0 表示无历史）；
	// 新版本号由此 +1 得到，避免并发下靠 count 推号撞唯一键。
	MaxVersion(ctx context.Context, configID int64) (int64, error)
	// SetAuditEntry 补写审计条目引用；仅当当前为 0 时生效（幂等，第二次返回 false）。
	SetAuditEntry(ctx context.Context, versionID, entryID int64) (bool, error)
}

type defaultConfigVersionModel struct {
	conn sqlx.SqlConn
}

// NewConfigVersionModel 构造 ops_config_version 的 sqlx 实现。
func NewConfigVersionModel(conn sqlx.SqlConn) ConfigVersionModel {
	return &defaultConfigVersionModel{conn: conn}
}

func (m *defaultConfigVersionModel) Insert(ctx context.Context, v *ConfigVersion) (int64, error) {
	return insertVersion(ctx, m.conn, v)
}

func (m *defaultConfigVersionModel) InsertTx(ctx context.Context, session sqlx.Session, v *ConfigVersion) (int64, error) {
	return insertVersion(ctx, session, v)
}

// insertVersion 是唯一实现体：自动提交与事务内两条入口共用同一套校验、
// 同一份 SQL 与同一个「RowsAffected==0 即唯一键冲突」的判定，
// 避免出现「非事务版校验、事务版漏校验」这种只在并发时才暴露的分叉。
func insertVersion(ctx context.Context, session sqlx.Session, v *ConfigVersion) (int64, error) {
	if v.ConfigID <= 0 {
		return 0, ErrConfigNotFound
	}
	if v.Version <= 0 {
		return 0, ErrVersionNotFound
	}
	if v.RequestID == "" {
		return 0, ErrRequestIDRequired
	}
	if v.Reason == "" {
		return 0, ErrReasonRequired
	}
	switch v.ChangeType {
	case ChangeTypeCreate, ChangeTypePublish, ChangeTypeRollback:
	default:
		return 0, ErrChangeTypeInvalid
	}
	if !ValidValueType(v.ValueType) {
		return 0, ErrValueTypeUnsupported
	}
	if v.PublishedAt == 0 {
		v.PublishedAt = nowUnix()
	}
	if v.Ctime == 0 {
		v.Ctime = v.PublishedAt
	}
	res, err := session.ExecCtx(ctx,
		"INSERT INTO ops_config_version ("+versionColumns+") VALUES ("+placeholders(14)+")"+
			" ON DUPLICATE KEY UPDATE version_id = version_id",
		v.VersionID, v.ConfigID, v.Version, v.Value, v.ValueType, v.ChangeType, v.RollbackFrom,
		v.OperatorID, v.OperatorName, v.Reason, v.AuditEntryID, v.RequestID, v.PublishedAt, v.Ctime)
	if err != nil {
		return 0, fmt.Errorf("ops_config_version Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_config_version Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrVersionExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ops_config_version Insert LastInsertId: %w", err)
	}
	v.VersionID = id
	return id, nil
}

const versionSelect = "SELECT " + versionColumns + " FROM ops_config_version"

func (m *defaultConfigVersionModel) FindOne(ctx context.Context, configID, version int64) (*ConfigVersion, error) {
	var row ConfigVersion
	query := versionSelect + " WHERE config_id = ? AND version = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, configID, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_config_version FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultConfigVersionModel) FindByRequestID(ctx context.Context, requestID string) (*ConfigVersion, error) {
	if requestID == "" {
		return nil, ErrRequestIDRequired
	}
	var row ConfigVersion
	query := versionSelect + " WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_config_version FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultConfigVersionModel) FindLatest(ctx context.Context, configID int64) (*ConfigVersion, error) {
	var row ConfigVersion
	query := versionSelect + " WHERE config_id = ? ORDER BY version DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, configID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_config_version FindLatest: %w", err)
	}
	return &row, nil
}

func (m *defaultConfigVersionModel) FindVersions(ctx context.Context, configID int64, versions []int64) (map[int64]*ConfigVersion, error) {
	out := make(map[int64]*ConfigVersion, len(versions))
	if len(versions) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(versions)+1)
	for _, v := range versions {
		args = append(args, v)
	}
	var rows []*ConfigVersion
	query := versionSelect + " WHERE config_id = ? AND version IN (" + placeholders(len(versions)) + ")"
	args = append([]any{configID}, args...)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("ops_config_version FindVersions: %w", err)
	}
	for _, r := range rows {
		out[r.Version] = r
	}
	return out, nil
}

func (m *defaultConfigVersionModel) ListByConfig(ctx context.Context, configID int64, pn, ps int32) ([]*ConfigVersion, int64, error) {
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM ops_config_version WHERE config_id = ?", configID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_config_version ListByConfig count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	var rows []*ConfigVersion
	query := versionSelect + " WHERE config_id = ? ORDER BY version DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, configID, ps, (pn-1)*ps); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_config_version ListByConfig: %w", err)
	}
	return rows, total, nil
}

func (m *defaultConfigVersionModel) MaxVersion(ctx context.Context, configID int64) (int64, error) {
	var maxVer sql.NullInt64
	err := m.conn.QueryRowCtx(ctx, &maxVer, "SELECT MAX(version) FROM ops_config_version WHERE config_id = ?", configID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("ops_config_version MaxVersion: %w", err)
	}
	if !maxVer.Valid {
		return 0, nil
	}
	return maxVer.Int64, nil
}

// SetAuditEntry 只在当前值为 0 时写入：审计引用一旦落下就不再变动，
// 重复调用返回 updated=false，让「补写成功」与「早就写过」在调用侧可区分。
func (m *defaultConfigVersionModel) SetAuditEntry(ctx context.Context, versionID, entryID int64) (bool, error) {
	if versionID <= 0 {
		return false, ErrVersionNotFound
	}
	if entryID <= 0 {
		return false, ErrAuditRefAlreadySet
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ops_config_version SET audit_entry_id = ? WHERE version_id = ? AND audit_entry_id = 0",
		entryID, versionID)
	if err != nil {
		return false, fmt.Errorf("ops_config_version SetAuditEntry: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_config_version SetAuditEntry RowsAffected: %w", err)
	}
	return aff > 0, nil
}
