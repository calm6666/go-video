package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// featureVersionSwitchColumns 是 feature_version_switch 的列清单（含自增主键）。
const featureVersionSwitchColumns = "switch_id, feature_key, switch_type, from_version, to_version," +
	" from_value, to_value, from_digest, to_digest, operator, reason, request_id, trace_id," +
	" rollback_switch_id, ctime"

// featureVersionSwitchInsertColumns 是写入列（不含自增主键，共 14 列）。
// ctime 必须在写入清单里：Append 之上已经 rec.Ctime = nowUnix()，漏列会让审计行的
// ctime 恒为 0，ListVersionSwitches 的 since/until 过滤与 idx_ctime 一起失效，
// 「什么时候改的」这个最基本的问题就没有答案了。
const featureVersionSwitchInsertColumns = "feature_key, switch_type, from_version, to_version," +
	" from_value, to_value, from_digest, to_digest, operator, reason, request_id, trace_id," +
	" rollback_switch_id, ctime"

// 审计类型：一行 = 一次「改变了对外可读口径」的动作。
// 六类变更共用一张表而不是六张表，是因为运维排查时的问句只有一个：
// 「这个 key 现在为什么是这个行为」——按 feature_key 一次倒序扫描就能看到全部原因。
const (
	// SwitchTypeActivate 首次上线某个版本（from_version=0）。
	SwitchTypeActivate = "activate"
	// SwitchTypeVersionSwitch 人工切换生效版本。
	SwitchTypeVersionSwitch = "version_switch"
	// SwitchTypeRollback 回滚：与 version_switch 同构，但必须引用被回滚的那条 switch_id，
	// 让「切错了又切回去」在审计里成对出现，而不是伪装成一次普通切换。
	SwitchTypeRollback = "rollback"
	// SwitchTypeStateChange 定义状态变更（from_value/to_value 记 FeatureState 数值）。
	SwitchTypeStateChange = "state_change"
	// SwitchTypePrivacyChange 隐私级别调整（from_value/to_value 记 PrivacyLevel 数值）。
	SwitchTypePrivacyChange = "privacy_change"
	// SwitchTypeBackfillAutoSwitch 回填成功后由作业自动切换（operator 必须是 offline-job:*）。
	SwitchTypeBackfillAutoSwitch = "backfill_auto_switch"
)

// ValidSwitchType 判断审计类型是否已声明。
func ValidSwitchType(t string) bool {
	switch t {
	case SwitchTypeActivate, SwitchTypeVersionSwitch, SwitchTypeRollback,
		SwitchTypeStateChange, SwitchTypePrivacyChange, SwitchTypeBackfillAutoSwitch:
		return true
	default:
		return false
	}
}

// SwitchTypeRequiresReason 判断该审计类型是否必须写理由。
// 当前全部为 true：这一列的存在意义就是解释「为什么变」，理由缺失的审计行等于没留痕。
// 保留成函数而不是常量，是为了让「新增一种可无理由的审计类型」必须显式改这里。
func SwitchTypeRequiresReason(_ string) bool { return true }

// featureVersionSwitchColumns 中 from_value/to_value 与 from_version/to_version 的分工：
//
//	版本类审计（activate/version_switch/rollback/backfill_auto_switch）：
//	    from_version -> to_version 是生效指针的移动，from_value/to_value 留空；
//	定义类审计（state_change/privacy_change）：
//	    from_version = to_version = 被改的版本，from_value/to_value 记改前改后的枚举值。
//
// 这样一张表既能回答「现在哪个版本生效、是怎么来的」，也能回答
// 「这个特征的隐私级别什么时候被人动过」，而契约的 ListVersionSwitches
// 只对外暴露版本相关列（见 rpc 里的 SwitchRecord），元数据列留在库内做内部审计。

// VersionSwitch 对应 feature_version_switch 表：只追加、不改写的变更审计。
type VersionSwitch struct {
	// SwitchID 自增主键，同时是 feature_active_version.last_switch_id 的指向目标。
	SwitchID int64 `db:"switch_id"`
	// FeatureKey 被变更的特征键。
	FeatureKey string `db:"feature_key"`
	// SwitchType 见 SwitchType* 常量。
	SwitchType string `db:"switch_type"`
	// FromVersion 变更前生效版本（0 = 之前没有生效版本）。
	FromVersion int32 `db:"from_version"`
	// ToVersion 变更后生效版本（state/privacy 变更时等于 FromVersion）。
	ToVersion int32 `db:"to_version"`
	// FromValue 变更前枚举值（状态/隐私类审计使用），空串 = 不适用。
	FromValue string `db:"from_value"`
	// ToValue 变更后枚举值。
	ToValue string `db:"to_value"`
	// FromDigest 变更前定义摘要（feature_definition.definition_digest），用于事后复算口径。
	FromDigest string `db:"from_digest"`
	// ToDigest 变更后定义摘要。
	ToDigest string `db:"to_digest"`
	// Operator 操作人（admin:<id> / system:<svc> / offline-job:<id>）。
	Operator string `db:"operator"`
	// Reason 变更理由（离线评估结论、回滚单号、工单号）：必填。
	Reason string `db:"reason"`
	// RequestID 触发本次变更的幂等键（与 feature_write_receipt 对齐）。
	RequestID string `db:"request_id"`
	// TraceID 链路 ID（不含任何主体标识明文）。
	TraceID string `db:"trace_id"`
	// RollbackSwitchID 仅 SwitchTypeRollback 使用：被回滚掉的那条 switch_id。
	RollbackSwitchID int64 `db:"rollback_switch_id"`
	// Ctime 变更时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
}

// VersionSwitchFilter 审计列表条件（零值 = 该维度不限）。
type VersionSwitchFilter struct {
	FeatureKey string
	SwitchType string
	Since      int64
	Until      int64
	Pn         int32
	Ps         int32
}

// Normalize 夹取分页参数（列表接口越界是「页开大了」，不是越权读，按上限夹取）。
func (f *VersionSwitchFilter) Normalize() {
	if f.Pn < 1 {
		f.Pn = 1
	}
	if f.Ps <= 0 || f.Ps > MaxListPageSize {
		f.Ps = MaxListPageSize
	}
}

// VersionSwitchModel 抽象 feature_version_switch 表：只追加，不 UPDATE、不 DELETE。
//
// 回滚的表示方式是「再写一条 rollback 审计 + 把指针切回去」，绝不是删掉那条切换记录：
// 删审计等于抹掉「曾经切错过」的事实，下一次评审就会重复同一个错误。
type VersionSwitchModel interface {
	// Append 追加一条审计，返回自增 switch_id（供指针行的 last_switch_id 引用）。
	Append(ctx context.Context, session sqlx.Session, rec *VersionSwitch) (int64, error)
	// FindOne 按主键查询；未命中返回 ErrSwitchNotFound。
	FindOne(ctx context.Context, switchID int64) (*VersionSwitch, error)
	// List 分页查询，固定 switch_id 倒序（自增主键即时间倒序，避免 filesort）。
	List(ctx context.Context, f VersionSwitchFilter) ([]*VersionSwitch, int64, error)
	// LatestByKey 取某 key 最近一条版本类审计（回滚前确认「要撤销的是哪一次」）。
	LatestByKey(ctx context.Context, featureKey string) (*VersionSwitch, error)
}

type defaultVersionSwitchModel struct {
	conn sqlx.SqlConn
}

// NewVersionSwitchModel 构造 feature_version_switch 的 sqlx 实现。
func NewVersionSwitchModel(conn sqlx.SqlConn) VersionSwitchModel {
	return &defaultVersionSwitchModel{conn: conn}
}

func (m *defaultVersionSwitchModel) exec(session sqlx.Session) sqlx.Session {
	if session != nil {
		return session
	}
	return m.conn
}

const versionSwitchSelect = "SELECT " + featureVersionSwitchColumns + " FROM feature_version_switch"

func (m *defaultVersionSwitchModel) Append(ctx context.Context, session sqlx.Session,
	rec *VersionSwitch) (int64, error) {
	if rec == nil {
		return 0, ErrFeatureKeyRequired
	}
	if strings.TrimSpace(rec.FeatureKey) == "" {
		return 0, ErrFeatureKeyRequired
	}
	if !ValidSwitchType(rec.SwitchType) {
		return 0, fmt.Errorf("%w: switch_type %q", ErrFeatureStateTransition, rec.SwitchType)
	}
	if strings.TrimSpace(rec.Operator) == "" {
		return 0, ErrOperatorRequired
	}
	if SwitchTypeRequiresReason(rec.SwitchType) && strings.TrimSpace(rec.Reason) == "" {
		return 0, ErrReasonRequired
	}
	if strings.TrimSpace(rec.RequestID) == "" {
		return 0, ErrRequestIdRequired
	}
	if rec.Ctime == 0 {
		rec.Ctime = nowUnix()
	}
	res, err := m.exec(session).ExecCtx(ctx,
		"INSERT INTO feature_version_switch ("+featureVersionSwitchInsertColumns+") VALUES ("+placeholders(14)+")",
		rec.FeatureKey, rec.SwitchType, rec.FromVersion, rec.ToVersion, rec.FromValue, rec.ToValue,
		rec.FromDigest, rec.ToDigest, rec.Operator, rec.Reason, rec.RequestID, rec.TraceID,
		rec.RollbackSwitchID, rec.Ctime)
	if err != nil {
		// uniq_request_switch 冲突 = 同一 request_id 重放：调用方必须回回放首次结果，
		// 而不是把审计写第二遍（AGENTS.md §5 幂等要求）。
		if isDuplicateKey(err) {
			return 0, ErrRequestIdReused
		}
		return 0, fmt.Errorf("feature_version_switch Append: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("feature_version_switch Append LastInsertId: %w", err)
	}
	rec.SwitchID = id
	return id, nil
}

// isDuplicateKey 只依赖 go-sql-driver 暴露的错误文本判断唯一键冲突：
// 不在 model 里 import 驱动专有错误码常量，也不因为「大概是 1062」就吞掉其它失败。
func isDuplicateKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}

func (m *defaultVersionSwitchModel) FindOne(ctx context.Context, switchID int64) (*VersionSwitch, error) {
	if switchID <= 0 {
		return nil, ErrLimitTooLarge
	}
	var row VersionSwitch
	err := m.conn.QueryRowCtx(ctx, &row, versionSwitchSelect+" WHERE switch_id = ? LIMIT 1", switchID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSwitchNotFound
		}
		return nil, fmt.Errorf("feature_version_switch FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultVersionSwitchModel) List(ctx context.Context, f VersionSwitchFilter) ([]*VersionSwitch, int64, error) {
	f.Normalize()
	where := "WHERE 1 = 1"
	args := make([]any, 0, 4)
	if key := strings.TrimSpace(f.FeatureKey); key != "" {
		where += " AND feature_key = ?"
		args = append(args, key)
	}
	if f.SwitchType != "" {
		if !ValidSwitchType(f.SwitchType) {
			return nil, 0, fmt.Errorf("%w: switch_type %q", ErrFeatureStateTransition, f.SwitchType)
		}
		where += " AND switch_type = ?"
		args = append(args, f.SwitchType)
	}
	if f.Since > 0 {
		where += " AND ctime >= ?"
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where += " AND ctime < ?"
		args = append(args, f.Until)
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM feature_version_switch "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("feature_version_switch List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*VersionSwitch
	query := versionSwitchSelect + where + " ORDER BY switch_id DESC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("feature_version_switch List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultVersionSwitchModel) LatestByKey(ctx context.Context, featureKey string) (*VersionSwitch, error) {
	if strings.TrimSpace(featureKey) == "" {
		return nil, ErrFeatureKeyRequired
	}
	var row VersionSwitch
	err := m.conn.QueryRowCtx(ctx, &row,
		versionSwitchSelect+" WHERE feature_key = ? ORDER BY switch_id DESC LIMIT 1", featureKey)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSwitchNotFound
		}
		return nil, fmt.Errorf("feature_version_switch LatestByKey: %w", err)
	}
	return &row, nil
}
