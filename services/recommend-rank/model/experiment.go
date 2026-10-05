package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RankExperiment A/B 实验变体与分桶配置（rank_experiment 表）。
//
// 一行 = 一个 (exp_key, variant_key) 变体，而不是一个实验：
// 同一实验的多个变体各自持有互不重叠的桶区间 [bucket_start, bucket_end)，
// 同 layer_key 的变体互斥分流、不同层正交（rpc UpsertExperimentReq.layer_key 语义）。
// 这样「谁在什么桶」是登记出来的事实，可由 ListOverlappingBuckets 事先证明，
// 而不是运行时靠代码约定。
//
// 可审计语义：
//   - state 只经 UpdateState 的条件 UPDATE 推进（DRAFT→RUNNING⇄PAUSED→STOPPED），
//     RowsAffected=0 一律表示「并发下状态已被别人改掉」，调用方必须重读而不是当作成功；
//   - hash_seed 与桶区间在 RUNNING 时不可改（UpdateBucketConfig 的 SQL 自带状态保护），
//     否则同一主体会在实验中途跳组，实验结论直接失效；要改先 PAUSED；
//   - 每次语义变更 revision+1，并留下 operator/note（rpc reason 落在这列），
//     与 rank_decision_log 的 exp_key/variant_key/bucket_no 三元组对得上，
//     才能回答「这批流量当时跑的是哪个版本的变体」；
//   - 本表不提供任何「把某个 aid 推进/置顶」的字段（AGENTS.md §7 禁止手工修改推荐结果）。
type RankExperiment struct {
	ID                   int64  `db:"id"`                     // 自增主键
	ExpKey               string `db:"exp_key"`                // 实验稳定 key
	VariantKey           string `db:"variant_key"`            // 变体 key（同实验内唯一，含 control）
	LayerKey             string `db:"layer_key"`              // 互斥层（同层互斥、异层正交）
	HashSeed             string `db:"hash_seed"`              // 分桶哈希盐（RUNNING 期间不可变）
	BucketCount          int32  `db:"bucket_count"`           // 分桶空间大小（登记后不可变，默认 1000）
	BucketStart          int32  `db:"bucket_start"`           // 桶区间左闭端
	BucketEnd            int32  `db:"bucket_end"`             // 桶区间右开端（必须 > start）
	ModelKey             string `db:"model_key"`              // 绑定的逻辑模型名
	ModelVersion         string `db:"model_version"`          // 空表示沿用该 model_key 的 ACTIVE 版本
	FeatureConfigVersion string `db:"feature_config_version"` // 空表示沿用模型登记的版本
	Overrides            string `db:"overrides"`              // 参数覆盖 JSON，顶层 key 必须是 SupportedOverrides()（ValidateOverrideKeys 把守，禁止商业化字段与 aid 指定）
	State                int32  `db:"state"`                  // 参见 ExpState* 常量
	Revision             int32  `db:"revision"`               // 语义修订号（每次配置变更 +1）
	StartAt              int64  `db:"start_at"`               // 生效时间（Unix 秒）
	EndAt                int64  `db:"end_at"`                 // 结束时间（Unix 秒），0 表示未设定
	Operator             string `db:"operator"`               // 最后操作者（审计必填）
	Note                 string `db:"note"`                   // 变更原因（rpc reason 落库列）
	Ctime                int64  `db:"ctime"`                  // 创建时间（Unix 秒）
	Mtime                int64  `db:"mtime"`                  // 修改时间（Unix 秒）
}

// RankExperimentModel rank_experiment 表读写接口。
type RankExperimentModel interface {
	// Insert 登记变体（uniq_variant 幂等；冲突返回 ErrExperimentExists）。
	Insert(ctx context.Context, e *RankExperiment) error
	// FindOne 按 (exp_key, variant_key) 查询；不存在返回 ErrExperimentNotFound。
	FindOne(ctx context.Context, expKey, variantKey string) (*RankExperiment, error)
	// FindByID 按自增主键查询（事务内锁定某变体时使用）。
	FindByID(ctx context.Context, id int64) (*RankExperiment, error)
	// ListRunning 列出当前时刻生效的 RUNNING 变体（在线分流转义用），按 exp_key、bucket_start 排序。
	// at 为 Unix 秒；limit<=0 时返回 nil（不隐式全表扫描）。
	ListRunning(ctx context.Context, at int64, limit int) ([]*RankExperiment, error)
	// ListByExpKey 列出某实验的全部变体（含历史状态，按 bucket_start 升序）。
	ListByExpKey(ctx context.Context, expKey string, limit int) ([]*RankExperiment, error)
	// ListOverlappingBuckets 返回同层内与 [start,end) 相交、且处于 RUNNING/PAUSED 的变体，
	// 用于「同层互斥必须不重叠」的前置校验；excludeVariant 允许改名时排除自身。
	ListOverlappingBuckets(ctx context.Context, layerKey string, start, end int32, excludeVariant string, limit int) ([]*RankExperiment, error)
	// List 分页查询（按 id DESC）；空字段与 0 值表示不过滤该维度。
	List(ctx context.Context, q ExperimentQuery) ([]*RankExperiment, error)
	// UpdateBucketConfig 更新分桶相关语义（区间/盐/层/模型与特征绑定/覆盖参数/时间窗），
	// 仅 DRAFT、PAUSED 可变（SQL 内带状态保护），revision+1；返回 false 表示状态不允许或并发丢失。
	// overrides 的 key 必须先过 ValidateOverrideKeys（未登记即返回错误，不落库）。
	UpdateBucketConfig(ctx context.Context, e *RankExperiment, operator, note string) (bool, error)
	// UpdateState 条件推进状态（from→to），带 CanTransitionExpState 校验，返回是否真正生效。
	UpdateState(ctx context.Context, expKey, variantKey string, fromState, toState int32, operator, note string) (bool, error)
	// StopExpired 把已过 end_at 的 RUNNING/PAUSED 变体推进到 STOPPED（供运维巡检调用）。
	// 单次最多处理 limit 行，返回实际影响行数；end_at=0 的变体永不被动停止。
	StopExpired(ctx context.Context, at int64, limit int) (int64, error)
}

// ExperimentQuery 实验变体分页查询条件。
type ExperimentQuery struct {
	ExpKey     string
	VariantKey string
	LayerKey   string
	ModelKey   string
	// State 0 表示不按状态过滤；否则必须是 ExpState* 之一。
	State int32
	// OnlyEffectiveNow 为 true 时只取 start_at<=at AND (end_at=0 OR end_at>at)。
	OnlyEffectiveNow bool
	At               int64
	Offset           int
	Limit            int
}

type defaultRankExperimentModel struct {
	conn sqlx.SqlConn
}

// NewRankExperimentModel 创建 RankExperimentModel 实现。
func NewRankExperimentModel(conn sqlx.SqlConn) RankExperimentModel {
	return &defaultRankExperimentModel{conn: conn}
}

const experimentSelect = "SELECT id, exp_key, variant_key, layer_key, hash_seed, bucket_count, bucket_start, bucket_end, " +
	"model_key, model_version, feature_config_version, overrides, state, revision, start_at, end_at, " +
	"operator, note, ctime, mtime FROM rank_experiment"

func (m *defaultRankExperimentModel) Insert(ctx context.Context, e *RankExperiment) error {
	if e.ExpKey == "" || e.VariantKey == "" {
		return ErrExperimentNotFound
	}
	if e.HashSeed == "" {
		return ErrHashSeedRequired
	}
	if e.BucketCount <= 0 {
		e.BucketCount = DefaultBucketCount
	}
	if !ValidBucketRange(e.BucketStart, e.BucketEnd, e.BucketCount) {
		return ErrInvalidBucketRange
	}
	if !ValidExpState(e.State) {
		return ErrInvalidExpState
	}
	if e.EndAt != 0 && e.EndAt <= e.StartAt {
		return ErrInvalidTimeRange
	}
	// 写库前先证明 overrides 没夹带未登记的参数（AGENTS.md §7：禁止借实验手工干预结果）。
	if err := ValidateOverrideKeys(e.Overrides, MaxOverridesBytes); err != nil {
		return err
	}
	if e.LayerKey == "" {
		e.LayerKey = "default" // 未指定层时归到默认层，保证同层互斥校验始终有作用域
	}
	if e.Revision == 0 {
		e.Revision = 1
	}
	now := nowUnix()
	e.Ctime = now
	e.Mtime = now
	query := "INSERT INTO rank_experiment (exp_key, variant_key, layer_key, hash_seed, bucket_count, bucket_start, " +
		"bucket_end, model_key, model_version, feature_config_version, overrides, state, revision, start_at, end_at, " +
		"operator, note, ctime, mtime) VALUES (" + inPlaceholders(19) + ")"
	if _, err := m.conn.ExecCtx(ctx, query,
		e.ExpKey, e.VariantKey, e.LayerKey, e.HashSeed, e.BucketCount, e.BucketStart, e.BucketEnd,
		e.ModelKey, e.ModelVersion, e.FeatureConfigVersion, e.Overrides, e.State, e.Revision, e.StartAt, e.EndAt,
		e.Operator, e.Note, e.Ctime, e.Mtime); err != nil {
		if isDuplicateErr(err) {
			return ErrExperimentExists
		}
		return fmt.Errorf("rank_experiment Insert: %w", err)
	}
	return nil
}

func (m *defaultRankExperimentModel) FindOne(ctx context.Context, expKey, variantKey string) (*RankExperiment, error) {
	var row RankExperiment
	query := experimentSelect + " WHERE exp_key = ? AND variant_key = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, expKey, variantKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExperimentNotFound
		}
		return nil, fmt.Errorf("rank_experiment FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRankExperimentModel) FindByID(ctx context.Context, id int64) (*RankExperiment, error) {
	var row RankExperiment
	if err := m.conn.QueryRowCtx(ctx, &row, experimentSelect+" WHERE id = ? LIMIT 1", id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrExperimentNotFound
		}
		return nil, fmt.Errorf("rank_experiment FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultRankExperimentModel) ListRunning(ctx context.Context, at int64, limit int) ([]*RankExperiment, error) {
	if limit <= 0 {
		return nil, nil
	}
	// end_at=0 表示未设定结束时间，长期实验常用，因此不能写成 end_at>at 单一条件。
	query := experimentSelect + " WHERE state = ? AND start_at <= ? AND (end_at = 0 OR end_at > ?)" +
		" ORDER BY exp_key ASC, bucket_start ASC LIMIT ?"
	var rows []*RankExperiment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, ExpStateRunning, at, at, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment ListRunning: %w", err)
	}
	return rows, nil
}

func (m *defaultRankExperimentModel) ListByExpKey(ctx context.Context, expKey string, limit int) ([]*RankExperiment, error) {
	if limit <= 0 || expKey == "" {
		return nil, nil
	}
	query := experimentSelect + " WHERE exp_key = ? ORDER BY bucket_start ASC, id ASC LIMIT ?"
	var rows []*RankExperiment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, expKey, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment ListByExpKey: %w", err)
	}
	return rows, nil
}

func (m *defaultRankExperimentModel) ListOverlappingBuckets(ctx context.Context, layerKey string, start, end int32,
	excludeVariant string, limit int) ([]*RankExperiment, error) {
	if limit <= 0 {
		return nil, nil
	}
	if layerKey == "" {
		layerKey = "default"
	}
	// 左闭右开相交判定：[s1,e1) 与 [s2,e2) 相交 <=> s1 < e2 && s2 < e1。
	// DRAFT 变体不参与冲突判定（尚未分流），STOPPED 保留但已不占用桶位，因此只查 RUNNING/PAUSED。
	query := experimentSelect + " WHERE layer_key = ? AND state IN (?, ?) AND bucket_start < ? AND ? < bucket_end" +
		" ORDER BY bucket_start ASC LIMIT ?"
	var rows []*RankExperiment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, layerKey, ExpStateRunning, ExpStatePaused, end, start, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment ListOverlappingBuckets: %w", err)
	}
	if excludeVariant == "" {
		return rows, nil
	}
	out := rows[:0]
	for _, r := range rows {
		if r.VariantKey != excludeVariant {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *defaultRankExperimentModel) List(ctx context.Context, q ExperimentQuery) ([]*RankExperiment, error) {
	if q.Limit <= 0 {
		return nil, nil
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	var conditions []string
	var args []interface{}
	if q.ExpKey != "" {
		conditions = append(conditions, "exp_key = ?")
		args = append(args, q.ExpKey)
	}
	if q.VariantKey != "" {
		conditions = append(conditions, "variant_key = ?")
		args = append(args, q.VariantKey)
	}
	if q.LayerKey != "" {
		conditions = append(conditions, "layer_key = ?")
		args = append(args, q.LayerKey)
	}
	if q.ModelKey != "" {
		conditions = append(conditions, "model_key = ?")
		args = append(args, q.ModelKey)
	}
	if q.State != 0 {
		if !ValidExpState(q.State) {
			return nil, ErrInvalidExpState
		}
		conditions = append(conditions, "state = ?")
		args = append(args, q.State)
	}
	if q.OnlyEffectiveNow {
		conditions = append(conditions, "start_at <= ?", "(end_at = 0 OR end_at > ?)")
		args = append(args, q.At, q.At)
	}
	query := experimentSelect + joinWhere(conditions) + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)
	var rows []*RankExperiment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment List: %w", err)
	}
	return rows, nil
}

func (m *defaultRankExperimentModel) UpdateBucketConfig(ctx context.Context, e *RankExperiment, operator, note string) (bool, error) {
	if e == nil || e.ID == 0 {
		return false, ErrExperimentNotFound
	}
	if e.HashSeed == "" {
		return false, ErrHashSeedRequired
	}
	if !ValidBucketRange(e.BucketStart, e.BucketEnd, e.BucketCount) {
		return false, ErrInvalidBucketRange
	}
	if e.EndAt != 0 && e.EndAt <= e.StartAt {
		return false, ErrInvalidTimeRange
	}
	if err := ValidateOverrideKeys(e.Overrides, MaxOverridesBytes); err != nil {
		return false, err
	}
	// 状态保护写进 SQL：RUNNING 变体的分桶语义不可热改（返回 0 行），
	// 需要改就 PAUSED 之后再改，历史 rank_decision_log 仍指向旧 revision。
	query := "UPDATE rank_experiment SET layer_key = ?, hash_seed = ?, bucket_start = ?, bucket_end = ?, " +
		"model_key = ?, model_version = ?, feature_config_version = ?, overrides = ?, start_at = ?, end_at = ?, " +
		"revision = revision + 1, operator = ?, note = ?, mtime = ? " +
		"WHERE id = ? AND state IN (?, ?) AND revision = ?"
	res, err := m.conn.ExecCtx(ctx, query,
		e.LayerKey, e.HashSeed, e.BucketStart, e.BucketEnd, e.ModelKey, e.ModelVersion, e.FeatureConfigVersion,
		e.Overrides, e.StartAt, e.EndAt, operator, note, nowUnix(), e.ID, ExpStateDraft, ExpStatePaused, e.Revision)
	if err != nil {
		return false, fmt.Errorf("rank_experiment UpdateBucketConfig: %w", err)
	}
	return rowAffected(res, "rank_experiment UpdateBucketConfig")
}

func (m *defaultRankExperimentModel) UpdateState(ctx context.Context, expKey, variantKey string, fromState, toState int32, operator, note string) (bool, error) {
	if !ValidExpState(fromState) || !ValidExpState(toState) {
		return false, ErrInvalidExpState
	}
	if !CanTransitionExpState(fromState, toState) {
		return false, ErrExpStateTransition
	}
	if note == "" {
		return false, ErrReasonRequired
	}
	query := "UPDATE rank_experiment SET state = ?, revision = revision + 1, operator = ?, note = ?, mtime = ? " +
		"WHERE exp_key = ? AND variant_key = ? AND state = ?"
	res, err := m.conn.ExecCtx(ctx, query, toState, operator, note, nowUnix(), expKey, variantKey, fromState)
	if err != nil {
		return false, fmt.Errorf("rank_experiment UpdateState: %w", err)
	}
	return rowAffected(res, "rank_experiment UpdateState")
}

func (m *defaultRankExperimentModel) StopExpired(ctx context.Context, at int64, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	// LIMIT 让巡检可分批，避免一次更新锁住大区间；本表行数很小（变体数量级为百），
	// 分批只是为了让主从延迟可控，不是为了拆分大事务。
	query := "UPDATE rank_experiment SET state = ?, revision = revision + 1, operator = ?, note = ?, mtime = ? " +
		"WHERE state IN (?, ?) AND end_at > 0 AND end_at <= ? LIMIT ?"
	res, err := m.conn.ExecCtx(ctx, query, ExpStateStopped, "system", "auto stop: end_at reached", nowUnix(),
		ExpStateRunning, ExpStatePaused, at, limit)
	if err != nil {
		return 0, fmt.Errorf("rank_experiment StopExpired: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rank_experiment StopExpired RowsAffected: %w", err)
	}
	return affected, nil
}
