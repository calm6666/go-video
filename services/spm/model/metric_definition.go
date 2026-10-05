package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// MetricDefinition 指标口径注册表行（spm_metric_definition 表投影）。
//
// 口径版本化的落点：(metric_key, metric_version) 一旦登记，formula/unit/supported_windows/
// source_event_types 都不允许原地改写——改口径必须新增版本，否则历史窗口数据会被静默
// 重新解释（AGENTS.md §7 第 1 条、README「口径版本策略」）。
type MetricDefinition struct {
	ID               int64  `db:"id"`                 // 主键 ID
	MetricKey        string `db:"metric_key"`         // 指标键
	MetricVersion    int32  `db:"metric_version"`     // 口径版本，>=1
	Name             string `db:"name"`               // 展示名
	Formula          string `db:"formula"`            // 口径公式说明（必须写清分子/分母/去重键）
	Unit             string `db:"unit"`               // 单位：count/ratio/seconds/score
	SupportedWindows string `db:"supported_windows"`  // 允许的窗口粒度，CSV，如 "1,2,3"
	SourceEventTypes string `db:"source_event_types"` // 依赖的事件类型，CSV
	State            int32  `db:"state"`              // 1 DRAFT、2 ACTIVE、3 RETIRED
	Description      string `db:"description"`        // 变更说明/状态变更理由
	CreatedBy        string `db:"created_by"`         // 登记人
	RequestID        string `db:"request_id"`         // 登记请求的幂等键
	Ctime            int64  `db:"ctime"`              // 创建时间（Unix 秒）
	Mtime            int64  `db:"mtime"`              // 修改时间（Unix 秒）
}

// MetricDefinitionFilter 口径列表过滤条件。零值表示不限。
type MetricDefinitionFilter struct {
	MetricKey string
	State     int32 // 0 = 不限
	Offset    int32
	Limit     int32
}

// MetricDefinitionModel spm_metric_definition 表读写接口。
// 本表是「口径目录」而不是投影：它只能由登记流程写入，没有重算概念。
type MetricDefinitionModel interface {
	// InsertIfAbsent 按 uniq(metric_key, metric_version) 尝试登记。
	// 返回 created=false 表示该版本已登记（调用方需自行比对是否完全一致，
	// 不允许用同名版本覆盖旧口径）。
	InsertIfAbsent(ctx context.Context, d *MetricDefinition) (bool, error)
	// FindByKeyVersion 精确查询一个口径版本；不存在返回 nil。
	FindByKeyVersion(ctx context.Context, metricKey string, version int32) (*MetricDefinition, error)
	// FindActive 查询某指标当前 ACTIVE 版本；不存在返回 nil。
	FindActive(ctx context.Context, metricKey string) (*MetricDefinition, error)
	// FindByRequestID 按登记请求的幂等键回查（UpsertMetricDefinition 的 reused 判定）。
	// 没有它，重复提交只能得到「该版本已存在」而分不清是重放还是撞名，
	// 契约里的 reused 语义就无从落实。
	FindByRequestID(ctx context.Context, requestID string) (*MetricDefinition, error)
	// CountActive 统计某指标处于 ACTIVE 的版本数，用于拒绝 version=0 语义二义。
	CountActive(ctx context.Context, metricKey string) (int64, error)
	// UpdateState 以 CAS 方式推进口径状态：只有当前状态仍是 fromState 时才写入 toState，
	// 并把 reason 落进 description 留痕。返回受影响行数（0 表示状态已被并发改变或版本不存在）。
	// fromState 不是可选装饰：没有期望态的 UPDATE 会让两个并发评审各自把目标态写进去，
	// 事后连「谁覆盖了谁」都查不出来（AGENTS.md §5：写接口要有状态版本或 CAS）。
	UpdateState(ctx context.Context, metricKey string, version, fromState, toState int32,
		reason string) (int64, error)
	// List 分页查询口径登记。
	List(ctx context.Context, f MetricDefinitionFilter) ([]*MetricDefinition, error)
	// Count 统计过滤条件下的口径数（分页 total）。
	Count(ctx context.Context, f MetricDefinitionFilter) (int64, error)
}

type defaultMetricDefinitionModel struct {
	conn sqlx.SqlConn
}

// NewMetricDefinitionModel 创建 MetricDefinitionModel 实现。
func NewMetricDefinitionModel(conn sqlx.SqlConn) MetricDefinitionModel {
	return &defaultMetricDefinitionModel{conn: conn}
}

const metricDefinitionColumns = "id, metric_key, metric_version, name, formula, unit," +
	" supported_windows, source_event_types, state, description, created_by, request_id, ctime, mtime"

func (m *defaultMetricDefinitionModel) InsertIfAbsent(
	ctx context.Context, d *MetricDefinition,
) (bool, error) {
	if strings.TrimSpace(d.MetricKey) == "" || d.MetricVersion <= 0 {
		return false, ErrMetricKeyEmpty
	}
	if d.State == DefinitionStateUnspecified {
		// 新登记口径一律先 DRAFT：ACTIVE 必须走 UpdateState 的显式评审迁移。
		d.State = DefinitionStateDraft
	}
	now := nowUnix()
	// 自赋值让重复键场景 affected=0，从而区分「新登记」与「该版本已存在」。
	dup := "ON DUPLICATE KEY UPDATE metric_key = metric_key"
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO spm_metric_definition (metric_key, metric_version, name, formula, unit,"+
			" supported_windows, source_event_types, state, description, created_by, request_id, ctime, mtime)"+
			" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"+dup,
		d.MetricKey, d.MetricVersion, truncate(d.Name, 100), truncate(d.Formula, 500),
		truncate(d.Unit, 32), truncate(d.SupportedWindows, 64), truncate(d.SourceEventTypes, 255),
		d.State, truncate(d.Description, 500), truncate(d.CreatedBy, 64),
		truncate(d.RequestID, 128), now, now)
	if err != nil {
		return false, fmt.Errorf("spm_metric_definition InsertIfAbsent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("spm_metric_definition InsertIfAbsent RowsAffected: %w", err)
	}
	if affected == 0 {
		return false, nil
	}
	// 只回读自增主键与时间列：ID 留给调用方复用（ChangeMetricDefinitionState 之类
	// 不需要再查一次）。把 now 直接写进 d.ID 会让上层拿一个不存在的行号去改状态。
	if id, idErr := res.LastInsertId(); idErr == nil && id > 0 {
		d.ID = id
	}
	d.Ctime, d.Mtime = now, now
	return true, nil
}

func (m *defaultMetricDefinitionModel) FindByKeyVersion(
	ctx context.Context, metricKey string, version int32,
) (*MetricDefinition, error) {
	var row MetricDefinition
	query := "SELECT " + metricDefinitionColumns +
		" FROM spm_metric_definition WHERE metric_key = ? AND metric_version = ?"
	if err := m.conn.QueryRowCtx(ctx, &row, query, metricKey, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_definition FindByKeyVersion: %w", err)
	}
	return &row, nil
}

func (m *defaultMetricDefinitionModel) FindActive(
	ctx context.Context, metricKey string,
) (*MetricDefinition, error) {
	var row MetricDefinition
	query := "SELECT " + metricDefinitionColumns +
		" FROM spm_metric_definition WHERE metric_key = ? AND state = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, metricKey, DefinitionStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_definition FindActive: %w", err)
	}
	return &row, nil
}

func (m *defaultMetricDefinitionModel) FindByRequestID(
	ctx context.Context, requestID string,
) (*MetricDefinition, error) {
	// request_id 列是 NOT NULL DEFAULT ''：空串会匹配到所有「未带幂等键登记」的历史行，
	// 那种行给出的随机结果比查不到更糟，所以直接返回 nil 让调用方走「未复用」分支。
	if strings.TrimSpace(requestID) == "" {
		return nil, nil
	}
	var row MetricDefinition
	query := "SELECT " + metricDefinitionColumns +
		" FROM spm_metric_definition WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_definition FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultMetricDefinitionModel) CountActive(ctx context.Context, metricKey string) (int64, error) {
	var n int64
	query := "SELECT COUNT(*) FROM spm_metric_definition WHERE metric_key = ? AND state = ?"
	if err := m.conn.QueryRowCtx(ctx, &n, query, metricKey, DefinitionStateActive); err != nil {
		return 0, fmt.Errorf("spm_metric_definition CountActive: %w", err)
	}
	return n, nil
}

func (m *defaultMetricDefinitionModel) UpdateState(
	ctx context.Context, metricKey string, version, fromState, toState int32, reason string,
) (int64, error) {
	if strings.TrimSpace(metricKey) == "" || version <= 0 {
		return 0, ErrMetricKeyEmpty
	}
	if !validDefinitionState(fromState) || !validDefinitionState(toState) {
		return 0, ErrInvalidDefinitionState
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE spm_metric_definition SET state = ?, description = ?, mtime = ?"+
			" WHERE metric_key = ? AND metric_version = ? AND state = ?",
		toState, truncate(reason, 500), nowUnix(), metricKey, version, fromState)
	if err != nil {
		return 0, fmt.Errorf("spm_metric_definition UpdateState: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("spm_metric_definition UpdateState RowsAffected: %w", err)
	}
	return n, nil
}

// validDefinitionState 判断状态是否可作 CAS 的期望态或目标态。
// UNSPECIFIED 既不是库里的取值也不是合法目标（契约里「不允许回到 UNSPECIFIED」）。
func validDefinitionState(s int32) bool {
	return s >= DefinitionStateDraft && s <= DefinitionStateRetired
}

func (m *defaultMetricDefinitionModel) List(
	ctx context.Context, f MetricDefinitionFilter,
) ([]*MetricDefinition, error) {
	query, args := buildMetricDefinitionQuery(f)
	query += " ORDER BY metric_key ASC, metric_version DESC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), clampOffset(f.Offset))
	var rows []*MetricDefinition
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("spm_metric_definition List: %w", err)
	}
	return rows, nil
}

func (m *defaultMetricDefinitionModel) Count(ctx context.Context, f MetricDefinitionFilter) (int64, error) {
	query, args := buildMetricDefinitionQuery(f)
	query = strings.Replace(query, "SELECT "+metricDefinitionColumns, "SELECT COUNT(*)", 1)
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		return 0, fmt.Errorf("spm_metric_definition Count: %w", err)
	}
	return n, nil
}

func buildMetricDefinitionQuery(f MetricDefinitionFilter) (string, []any) {
	query := "SELECT " + metricDefinitionColumns + " FROM spm_metric_definition WHERE 1 = 1"
	var args []any
	if f.MetricKey != "" {
		query += " AND metric_key = ?"
		args = append(args, f.MetricKey)
	}
	if f.State != DefinitionStateUnspecified {
		query += " AND state = ?"
		args = append(args, f.State)
	}
	return query, args
}
