package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RankModelVersion 排序模型版本登记（rank_model_version 表）。
//
// "模型版本可审计"的落地点：一行 = 一个 (model_key, version)，登记它绑定的特征配置、
// 多目标权重、离线指标与工件引用。工件只存对象存储 key（引用），模型本体与任何密钥
// 都不入库（AGENTS.md §5 与迁移约定）。
// 同一 model_key 至多一行 state=ACTIVE，由 uniq_active 部分唯一约束 + 切换事务保证。
type RankModelVersion struct {
	ID                   int64  `db:"id"`                     // 自增主键
	ModelKey             string `db:"model_key"`              // 逻辑模型名（如 home_feed_multi_gate）
	Version              string `db:"version"`                // 版本号（登记后不可变）
	FeatureConfigVersion string `db:"feature_config_version"` // 绑定的特征配置版本
	ObjectiveWeights     string `db:"objective_weights"`      // 多目标权重 JSON（受控目标 key）
	ArtifactRef          string `db:"artifact_ref"`           // 模型工件引用（对象存储 key，非密钥）
	OfflineMetrics       string `db:"offline_metrics"`        // 离线指标 JSON（仅审计展示）
	State                int32  `db:"state"`                  // 参见 ModelState* 常量
	Revision             int32  `db:"revision"`               // 元数据修订号（非语义字段变更 +1）
	ActivatedAt          int64  `db:"activated_at"`           // 激活时间（Unix 秒，0 表示未激活）
	PreviousActive       string `db:"previous_active"`        // 激活时上一个 ACTIVE 版本（回滚线索）
	Operator             string `db:"operator"`               // 最后操作者
	Note                 string `db:"note"`                   // 变更原因（审计必填）
	Ctime                int64  `db:"ctime"`                  // 创建时间（Unix 秒）
	Mtime                int64  `db:"mtime"`                  // 修改时间（Unix 秒）
}

// RankModelVersionModel rank_model_version 表读写接口。
type RankModelVersionModel interface {
	// Insert 登记新版本（uniq_model_version 幂等；冲突返回 ErrModelVersionExists）。
	Insert(ctx context.Context, v *RankModelVersion) error
	// FindOne 按 (model_key, version) 查询；不存在返回 ErrModelNotFound。
	FindOne(ctx context.Context, modelKey, version string) (*RankModelVersion, error)
	// FindActive 查询某 model_key 当前 ACTIVE 版本；没有则返回 ErrNoActiveModel。
	FindActive(ctx context.Context, modelKey string) (*RankModelVersion, error)
	// List 分页列出（可按 model_key 与状态过滤，mtime DESC）。
	List(ctx context.Context, q ModelVersionQuery) ([]*RankModelVersion, error)
	// UpdateMetadata 更新非语义元数据（工件引用/离线指标/备注）并 revision+1；
	// 语义字段（权重、特征绑定）在 ACTIVE/RETIRED 后不可改，由调用方保证。
	UpdateMetadata(ctx context.Context, id int64, artifactRef, offlineMetrics, operator, note string) (bool, error)
	// UpdateWeights 更新多目标权重（仅 DRAFT/READY 可行，SQL 内带状态保护）。
	UpdateWeights(ctx context.Context, id int64, objectiveWeights, featureConfigVersion, operator, note string) (bool, error)
	// Deactivate 把某 model_key 下除 keepID 外的 ACTIVE 行置为 RETIRED（激活的事务前半段）。
	Deactivate(ctx context.Context, modelKey string, keepID, at int64) (int64, error)
	// Activate 条件激活：只有当前 state=READY 才置 ACTIVE，返回是否生效（并发切换保护）。
	Activate(ctx context.Context, id int64, previousActive string, operator string, at int64) (bool, error)
	// UpdateState 通用状态推进（含下线），带 fromState 条件保护。
	UpdateState(ctx context.Context, id int64, fromState, toState int32, operator, note string) (bool, error)
	// CountByFeatureConfig 统计引用某特征配置版本、且状态在 states 内的模型版本行数；
	// states 为空表示不限状态。用于「停用特征配置前先确认没有在线模型在用它」。
	CountByFeatureConfig(ctx context.Context, configVersion string, states []int32) (int64, error)

	// --- 事务内变体（激活必须是「旧 ACTIVE 置 RETIRED + 新版本置 ACTIVE」一个事务，
	//     所以这三个方法必须能接收 sqlx.Session，否则事务只是形同虚设） ---

	// FindActiveTx 事务内查询某 model_key 的 ACTIVE 版本；没有则返回 ErrNoActiveModel。
	FindActiveTx(ctx context.Context, session sqlx.Session, modelKey string) (*RankModelVersion, error)
	// DeactivateTx 事务内把除 keepID 外的 ACTIVE 行置为 RETIRED，返回影响行数。
	DeactivateTx(ctx context.Context, session sqlx.Session, modelKey string, keepID, at int64) (int64, error)
	// ActivateTx 事务内条件激活（只有 state=READY 才生效），返回是否命中。
	ActivateTx(ctx context.Context, session sqlx.Session, id int64, previousActive, operator string, at int64) (bool, error)
}

// dbExecutor 让同一份 SQL 既能跑在连接池（sqlx.SqlConn）上，也能跑在事务会话（sqlx.Session）上，
// 避免为了支持事务而复制两份语句（两份语句迟早漂移）。
type dbExecutor interface {
	ExecCtx(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryRowCtx(ctx context.Context, v interface{}, query string, args ...interface{}) error
}

// ModelVersionQuery 模型版本分页查询条件。
type ModelVersionQuery struct {
	ModelKey string
	State    int32 // 0 表示不过滤
	Offset   int
	Limit    int
}

type defaultRankModelVersionModel struct {
	conn sqlx.SqlConn
}

// NewRankModelVersionModel 创建 RankModelVersionModel 实现。
func NewRankModelVersionModel(conn sqlx.SqlConn) RankModelVersionModel {
	return &defaultRankModelVersionModel{conn: conn}
}

const modelVersionSelect = "SELECT id, model_key, version, feature_config_version, objective_weights, artifact_ref, " +
	"offline_metrics, state, revision, activated_at, previous_active, operator, note, ctime, mtime FROM rank_model_version"

func (m *defaultRankModelVersionModel) Insert(ctx context.Context, v *RankModelVersion) error {
	if v.ModelKey == "" || v.Version == "" {
		return ErrModelNotFound
	}
	if !ValidModelState(v.State) {
		return ErrInvalidModelState
	}
	if v.Revision == 0 {
		v.Revision = 1
	}
	now := nowUnix()
	if v.Ctime == 0 {
		v.Ctime = now
	}
	v.Mtime = now
	query := "INSERT INTO rank_model_version (model_key, version, feature_config_version, objective_weights, " +
		"artifact_ref, offline_metrics, state, revision, activated_at, previous_active, operator, note, ctime, mtime) " +
		"VALUES (" + inPlaceholders(14) + ")"
	if _, err := m.conn.ExecCtx(ctx, query,
		v.ModelKey, v.Version, v.FeatureConfigVersion, v.ObjectiveWeights, v.ArtifactRef, v.OfflineMetrics,
		v.State, v.Revision, v.ActivatedAt, v.PreviousActive, v.Operator, v.Note, v.Ctime, v.Mtime); err != nil {
		if isDuplicateErr(err) {
			return ErrModelVersionExists
		}
		return fmt.Errorf("rank_model_version Insert: %w", err)
	}
	return nil
}

func (m *defaultRankModelVersionModel) FindOne(ctx context.Context, modelKey, version string) (*RankModelVersion, error) {
	var row RankModelVersion
	if err := m.conn.QueryRowCtx(ctx, &row, modelVersionSelect+" WHERE model_key = ? AND version = ?", modelKey, version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrModelNotFound
		}
		return nil, fmt.Errorf("rank_model_version FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRankModelVersionModel) FindActive(ctx context.Context, modelKey string) (*RankModelVersion, error) {
	return findActiveWith(ctx, m.conn, modelKey)
}

func (m *defaultRankModelVersionModel) FindActiveTx(ctx context.Context, session sqlx.Session, modelKey string) (*RankModelVersion, error) {
	return findActiveWith(ctx, session, modelKey)
}

// findActiveWith 是 FindActive 与 FindActiveTx 的共用实现：
// 每个 model_key 至多一行 ACTIVE（由迁移的 uniq_active 与激活事务保证），
// 这里仍按 activated_at DESC 取一行，是为了容忍历史上人工修数据造成的多行，
// 让在线读路径「总能选出一个确定答案」而不是随机命中。
func findActiveWith(ctx context.Context, exec dbExecutor, modelKey string) (*RankModelVersion, error) {
	var row RankModelVersion
	query := modelVersionSelect + " WHERE model_key = ? AND state = ? ORDER BY activated_at DESC, id DESC LIMIT 1"
	if err := exec.QueryRowCtx(ctx, &row, query, modelKey, ModelStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoActiveModel
		}
		return nil, fmt.Errorf("rank_model_version FindActive: %w", err)
	}
	return &row, nil
}

func (m *defaultRankModelVersionModel) List(ctx context.Context, q ModelVersionQuery) ([]*RankModelVersion, error) {
	if q.Limit <= 0 {
		return nil, nil
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	var conditions []string
	var args []interface{}
	if q.ModelKey != "" {
		conditions = append(conditions, "model_key = ?")
		args = append(args, q.ModelKey)
	}
	if q.State != 0 {
		if !ValidModelState(q.State) {
			return nil, ErrInvalidModelState
		}
		conditions = append(conditions, "state = ?")
		args = append(args, q.State)
	}
	query := modelVersionSelect + joinWhere(conditions) + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)
	var rows []*RankModelVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_model_version List: %w", err)
	}
	return rows, nil
}

func (m *defaultRankModelVersionModel) UpdateMetadata(ctx context.Context, id int64, artifactRef, offlineMetrics, operator, note string) (bool, error) {
	query := "UPDATE rank_model_version SET artifact_ref = ?, offline_metrics = ?, operator = ?, note = ?, " +
		"revision = revision + 1, mtime = ? WHERE id = ? AND state <> ?"
	res, err := m.conn.ExecCtx(ctx, query, artifactRef, offlineMetrics, operator, note, nowUnix(), id, ModelStateActive)
	if err != nil {
		return false, fmt.Errorf("rank_model_version UpdateMetadata: %w", err)
	}
	return rowAffected(res, "rank_model_version UpdateMetadata")
}

func (m *defaultRankModelVersionModel) UpdateWeights(ctx context.Context, id int64, objectiveWeights, featureConfigVersion, operator, note string) (bool, error) {
	// 状态保护写进 SQL：ACTIVE/RETIRED 版本的目标权重与特征绑定不可变，
	// 需要改语义就登记新版本，保证"同版本号 = 同语义"这一审计前提。
	query := "UPDATE rank_model_version SET objective_weights = ?, feature_config_version = ?, operator = ?, " +
		"note = ?, revision = revision + 1, mtime = ? WHERE id = ? AND state IN (?, ?)"
	res, err := m.conn.ExecCtx(ctx, query, objectiveWeights, featureConfigVersion, operator, note,
		nowUnix(), id, ModelStateDraft, ModelStateReady)
	if err != nil {
		return false, fmt.Errorf("rank_model_version UpdateWeights: %w", err)
	}
	return rowAffected(res, "rank_model_version UpdateWeights")
}

func (m *defaultRankModelVersionModel) Deactivate(ctx context.Context, modelKey string, keepID, at int64) (int64, error) {
	return deactivateWith(ctx, m.conn, modelKey, keepID, at)
}

func (m *defaultRankModelVersionModel) DeactivateTx(ctx context.Context, session sqlx.Session, modelKey string, keepID, at int64) (int64, error) {
	return deactivateWith(ctx, session, modelKey, keepID, at)
}

func deactivateWith(ctx context.Context, exec dbExecutor, modelKey string, keepID, at int64) (int64, error) {
	query := "UPDATE rank_model_version SET state = ?, mtime = ? WHERE model_key = ? AND state = ? AND id <> ?"
	res, err := exec.ExecCtx(ctx, query, ModelStateRetired, at, modelKey, ModelStateActive, keepID)
	if err != nil {
		return 0, fmt.Errorf("rank_model_version Deactivate: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rank_model_version Deactivate RowsAffected: %w", err)
	}
	return affected, nil
}

func (m *defaultRankModelVersionModel) Activate(ctx context.Context, id int64, previousActive string, operator string, at int64) (bool, error) {
	return activateWith(ctx, m.conn, id, previousActive, operator, at)
}

func (m *defaultRankModelVersionModel) ActivateTx(ctx context.Context, session sqlx.Session, id int64, previousActive, operator string, at int64) (bool, error) {
	return activateWith(ctx, session, id, previousActive, operator, at)
}

// activateWith 只允许 READY -> ACTIVE（SQL 内带 state 条件），
// 返回 false 表示「已被别的实例抢先切换」，调用方必须重读而不是当成成功。
func activateWith(ctx context.Context, exec dbExecutor, id int64, previousActive, operator string, at int64) (bool, error) {
	query := "UPDATE rank_model_version SET state = ?, activated_at = ?, previous_active = ?, operator = ?, mtime = ? " +
		"WHERE id = ? AND state = ?"
	res, err := exec.ExecCtx(ctx, query, ModelStateActive, at, previousActive, operator, at, id, ModelStateReady)
	if err != nil {
		return false, fmt.Errorf("rank_model_version Activate: %w", err)
	}
	return rowAffected(res, "rank_model_version Activate")
}

func (m *defaultRankModelVersionModel) UpdateState(ctx context.Context, id int64, fromState, toState int32, operator, note string) (bool, error) {
	if !CanTransitionModelState(fromState, toState) {
		return false, ErrModelStateTransition
	}
	query := "UPDATE rank_model_version SET state = ?, operator = ?, note = ?, mtime = ? WHERE id = ? AND state = ?"
	res, err := m.conn.ExecCtx(ctx, query, toState, operator, note, nowUnix(), id, fromState)
	if err != nil {
		return false, fmt.Errorf("rank_model_version UpdateState: %w", err)
	}
	return rowAffected(res, "rank_model_version UpdateState")
}

// rowAffected 把 sql.Result 的 RowsAffected 转成"是否命中行"，0 行表示条件不满足（并发或状态不符）。
func rowAffected(res sql.Result, op string) (bool, error) {
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return affected > 0, nil
}

func (m *defaultRankModelVersionModel) CountByFeatureConfig(ctx context.Context, configVersion string, states []int32) (int64, error) {
	if configVersion == "" {
		return 0, ErrFeatureConfigNotFound
	}
	query := "SELECT COUNT(*) FROM rank_model_version WHERE feature_config_version = ?"
	args := []interface{}{configVersion}
	if len(states) > 0 {
		placeholders := make([]string, 0, len(states))
		for _, s := range states {
			if !ValidModelState(s) {
				return 0, ErrInvalidModelState
			}
			placeholders = append(placeholders, "?")
			args = append(args, s)
		}
		query += " AND state IN (" + strings.Join(placeholders, ",") + ")"
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("rank_model_version CountByFeatureConfig: %w", err)
	}
	return total, nil
}
