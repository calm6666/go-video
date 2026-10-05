package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RankDecisionLog 一次排序的决策摘要（rank_decision_log 表）。
//
// 这张表是本服务「可审计」承诺的落地点。一行回答四个问题：
//  1. 用的哪套配置：model_key / model_version / feature_config_version /
//     exp_key / variant_key / bucket_no（+ 变体当时的 revision）；
//  2. 候选从哪来：input_count + source_summary（"1:12,2:8" 形式的来源分布）+
//     recall snapshot_id / batch_id，可回指 recommend-recall 的池版本；
//  3. 结果是什么：returned_count、result_digest（出参有序 aid 的 sha256）、
//     input_digest（入参有序 aid 的 sha256）、top_aids（前 N 个，人工排障直接可读）；
//     input_digest 与 result_digest 同时存在，才能证明「出参是入参的子集且没有编造 aid」；
//  4. 是否降级：degraded / degrade_reason / fallback_strategy / scored_count + 五个过滤计数，
//     被丢弃的候选必须留下计数，不允许静默吞。
//
// 保留期与归档（这张表增长最快，绝不能无限增长）：
//   - 每次在线排序写一行，量级 = 推荐请求 QPS × 实例数，日增量远大于其他表；
//   - 保留期由 config.Rank.DecisionRetentionDays 决定（示例配置 14 天）；
//   - 超期数据先按 ctime 归档到对象存储（导出任务见 services/cron 规划），再由
//     SelectExpiredBefore + DeleteExpiredBefore 两步分批删除，禁止一次大事务清空；
//   - 需要长期留存的不是明细而是聚合结果（spm 的完播率/留存特征），
//     因此本表按「短期排障明细」定位，不做长期分析仓库；
//   - 本期契约没有 prune RPC（见 README「已知缺口」），清理只能由运维巡检执行。
//
// 隐私约束：mid 与 subject_id 只存受控标识（设备侧存 sha256 摘要），
// 不存 IP、设备原文、任何用户画像文本。
type RankDecisionLog struct {
	ID                   int64  `db:"id"`                     // 自增主键（时间序，供分批清理与游标）
	DecisionID           string `db:"decision_id"`            // 对外审计 ID（唯一）
	RequestID            string `db:"request_id"`             // 幂等/重放锚点（唯一，同 request_id 只有一行）
	IdempotencyKey       string `db:"idempotency_key"`        // 调用方给的幂等提示（可空，按 key 回放用）
	TraceID              string `db:"trace_id"`               // 调用方透传 trace_id
	SnapshotID           string `db:"snapshot_id"`            // recommend-recall 召回快照 ID（审计回指）
	PoolVersion          int64  `db:"pool_version"`           // 召回池版本（审计回指，0 表示未透传）
	Mid                  int64  `db:"mid"`                    // 登录用户 ID，0 表示游客
	SubjectType          int32  `db:"subject_type"`           // 分桶主体类型（参见 Subject* 常量）
	SubjectID            string `db:"subject_id"`             // mid 十进制串或设备 sha256 摘要
	Scene                string `db:"scene"`                  // 场景稳定 key（home.feed ...）
	Platform             int32  `db:"platform"`               // 客户端平台（参见 Platform* 常量）
	AppVersion           string `db:"app_version"`            // 客户端版本号
	Region               string `db:"region"`                 // 地区代码
	ExpKey               string `db:"exp_key"`                // 命中的实验（未命中为空）
	VariantKey           string `db:"variant_key"`            // 命中的变体（未命中为 control）
	BucketNo             int32  `db:"bucket_no"`              // 命中的桶号
	ExpRevision          int32  `db:"exp_revision"`           // 命中变体当时的 revision
	ModelKey             string `db:"model_key"`              // 使用的逻辑模型名
	ModelVersion         string `db:"model_version"`          // 实际生效的模型版本
	FeatureConfigVersion string `db:"feature_config_version"` // 实际生效的特征配置版本
	InputCount           int32  `db:"input_count"`            // 入参候选条数
	ReturnedCount        int32  `db:"returned_count"`         // 出参条数
	ScoredCount          int32  `db:"scored_count"`           // 真正被模型打分的条数（降级时 < 出参数）
	SourceSummary        string `db:"source_summary"`         // 候选来源分布 csv（"1:12,2:8"，升序）
	InputDigest          string `db:"input_digest"`           // sha256(入参有序 aid)，证明子集关系
	ResultDigest         string `db:"result_digest"`          // sha256(出参有序 aid)，契约承诺的回放摘要
	TopAids              string `db:"top_aids"`               // 前 N 个 aid csv（N 由 MaxDigestAids 决定）
	Degraded             int32  `db:"degraded"`               // 0 正常、1 降级（落 TINYINT 便于索引过滤）
	DegradeReason        string `db:"degrade_reason"`         // 受控 key（参见 DegradeReason* 常量）
	FallbackStrategy     string `db:"fallback_strategy"`      // 受控 key（参见 Fallback* 常量）
	SafetyFiltered       int32  `db:"safety_filtered"`        // 内容安全/审核不可见被剔除
	FrequencyFiltered    int32  `db:"frequency_filtered"`     // 频控被剔除
	DedupFiltered        int32  `db:"dedup_filtered"`         // 入参重复 aid 去重
	DiversifiedMoved     int32  `db:"diversified_moved"`      // 打散导致位置移动
	Truncated            int32  `db:"truncated"`              // 超出 limit 被截断
	CostMs               int32  `db:"cost_ms"`                // 本次排序耗时（毫秒）
	DegradeDetail        string `db:"degrade_detail"`         // 排障文本（禁止包含用户敏感信息）
	Ctime                int64  `db:"ctime"`                  // 创建时间（Unix 秒），归档按此列
}

// RankDecisionLogModel rank_decision_log 表读写接口。
type RankDecisionLogModel interface {
	// Insert 写入决策摘要；request_id 或 decision_id 冲突返回 ErrDecisionExists
	// （调用方据此走幂等回放分支，读旧行返回给客户端）。
	Insert(ctx context.Context, l *RankDecisionLog) error
	// FindByDecisionID 按对外审计 ID 回放；不存在返回 ErrDecisionNotFound。
	FindByDecisionID(ctx context.Context, decisionID string) (*RankDecisionLog, error)
	// FindByRequestID 按幂等锚点回放；不存在返回 ErrDecisionNotFound。
	FindByRequestID(ctx context.Context, requestID string) (*RankDecisionLog, error)
	// FindByIdempotencyKey 按调用方幂等键取最近一条（同键多次请求时用于人工核对）；
	// 不存在返回 ErrDecisionNotFound。
	FindByIdempotencyKey(ctx context.Context, idempotencyKey string) (*RankDecisionLog, error)
	// List 分页查询（id DESC）。返回 hasMore=true 表示还有下一页，
	// 由多取一行判定，避免 COUNT(*) 全表扫描。
	List(ctx context.Context, q DecisionQuery) ([]*RankDecisionLog, bool, error)
	// CountDegradedSince 统计时间窗内降级次数（健康探针与告警用，limit 无意义故不传）。
	CountDegradedSince(ctx context.Context, from int64) (int64, error)
	// SelectExpiredBefore 按保留期挑出待归档行的主键（分批，单次最多 limit 行）。
	SelectExpiredBefore(ctx context.Context, cutoff int64, limit int) ([]int64, error)
	// DeleteExpiredBefore 按主键批量删除已归档行（调用方必须先归档成功再删，见迁移文件说明）。
	DeleteExpiredBefore(ctx context.Context, ids []int64) (int64, error)
}

// DecisionQuery 排序决策摘要的分页查询条件，与 rpc ListRankDecisionsReq 对齐。
type DecisionQuery struct {
	ExpKey       string
	VariantKey   string
	ModelKey     string
	ModelVersion string
	Scene        string
	FromTime     int64 // Unix 秒，含；0 表示下界不限
	ToTime       int64 // Unix 秒，含；0 表示上界不限
	OnlyDegraded bool
	Offset       int
	Limit        int
}

type defaultRankDecisionLogModel struct {
	conn sqlx.SqlConn
}

// NewRankDecisionLogModel 创建 RankDecisionLogModel 实现。
func NewRankDecisionLogModel(conn sqlx.SqlConn) RankDecisionLogModel {
	return &defaultRankDecisionLogModel{conn: conn}
}

const decisionLogSelect = "SELECT id, decision_id, request_id, idempotency_key, trace_id, snapshot_id, pool_version, " +
	"mid, subject_type, subject_id, scene, platform, app_version, region, exp_key, variant_key, bucket_no, " +
	"exp_revision, model_key, model_version, feature_config_version, input_count, returned_count, scored_count, " +
	"source_summary, input_digest, result_digest, top_aids, degraded, degrade_reason, fallback_strategy, " +
	"safety_filtered, frequency_filtered, dedup_filtered, diversified_moved, truncated, cost_ms, degrade_detail, ctime " +
	"FROM rank_decision_log"

func (m *defaultRankDecisionLogModel) Insert(ctx context.Context, l *RankDecisionLog) error {
	if l.DecisionID == "" || l.RequestID == "" {
		return ErrDecisionIDRequired
	}
	if l.Degraded != 0 && l.Degraded != 1 {
		return ErrDegradationDisabled
	}
	if !ValidDegradeReason(l.DegradeReason) {
		return ErrInvalidDegradeReason
	}
	if !ValidFallback(l.FallbackStrategy) {
		return ErrInvalidFallbackStrategy
	}
	// 主体标识允许「两者皆空」（游客且未带设备摘要），但只要给出就必须受控：
	// 明文设备号在这一步被拒绝，本表因此不会变成可反查的设备档案。
	if l.SubjectType != 0 || l.SubjectID != "" {
		if err := ValidateSubjectID(l.SubjectType, l.SubjectID); err != nil {
			return err
		}
	}
	if l.Ctime == 0 {
		l.Ctime = nowUnix()
	}
	query := "INSERT INTO rank_decision_log (decision_id, request_id, idempotency_key, trace_id, snapshot_id, pool_version, " +
		"mid, subject_type, subject_id, scene, platform, app_version, region, exp_key, variant_key, bucket_no, " +
		"exp_revision, model_key, model_version, feature_config_version, input_count, returned_count, scored_count, " +
		"source_summary, input_digest, result_digest, top_aids, degraded, degrade_reason, fallback_strategy, " +
		"safety_filtered, frequency_filtered, dedup_filtered, diversified_moved, truncated, cost_ms, degrade_detail, ctime) " +
		"VALUES (" + inPlaceholders(38) + ")"
	if _, err := m.conn.ExecCtx(ctx, query,
		l.DecisionID, l.RequestID, l.IdempotencyKey, l.TraceID, l.SnapshotID, l.PoolVersion,
		l.Mid, l.SubjectType, l.SubjectID, l.Scene, l.Platform, l.AppVersion, l.Region, l.ExpKey, l.VariantKey, l.BucketNo,
		l.ExpRevision, l.ModelKey, l.ModelVersion, l.FeatureConfigVersion, l.InputCount, l.ReturnedCount, l.ScoredCount,
		l.SourceSummary, l.InputDigest, l.ResultDigest, l.TopAids, l.Degraded, l.DegradeReason, l.FallbackStrategy,
		l.SafetyFiltered, l.FrequencyFiltered, l.DedupFiltered, l.DiversifiedMoved, l.Truncated, l.CostMs,
		l.DegradeDetail, l.Ctime); err != nil {
		if isDuplicateErr(err) {
			return ErrDecisionExists
		}
		return fmt.Errorf("rank_decision_log Insert: %w", err)
	}
	return nil
}

func (m *defaultRankDecisionLogModel) FindByDecisionID(ctx context.Context, decisionID string) (*RankDecisionLog, error) {
	if decisionID == "" {
		return nil, ErrDecisionIDRequired
	}
	var row RankDecisionLog
	if err := m.conn.QueryRowCtx(ctx, &row, decisionLogSelect+" WHERE decision_id = ? LIMIT 1", decisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDecisionNotFound
		}
		return nil, fmt.Errorf("rank_decision_log FindByDecisionID: %w", err)
	}
	return &row, nil
}

func (m *defaultRankDecisionLogModel) FindByRequestID(ctx context.Context, requestID string) (*RankDecisionLog, error) {
	if requestID == "" {
		return nil, ErrDecisionIDRequired
	}
	var row RankDecisionLog
	if err := m.conn.QueryRowCtx(ctx, &row, decisionLogSelect+" WHERE request_id = ? LIMIT 1", requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDecisionNotFound
		}
		return nil, fmt.Errorf("rank_decision_log FindByRequestID: %w", err)
	}
	return &row, nil
}

func (m *defaultRankDecisionLogModel) FindByIdempotencyKey(ctx context.Context, idempotencyKey string) (*RankDecisionLog, error) {
	if idempotencyKey == "" {
		return nil, ErrDecisionIDRequired
	}
	var row RankDecisionLog
	query := decisionLogSelect + " WHERE idempotency_key = ? ORDER BY id DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, idempotencyKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrDecisionNotFound
		}
		return nil, fmt.Errorf("rank_decision_log FindByIdempotencyKey: %w", err)
	}
	return &row, nil
}

func (m *defaultRankDecisionLogModel) List(ctx context.Context, q DecisionQuery) ([]*RankDecisionLog, bool, error) {
	if q.Limit <= 0 {
		return nil, false, nil
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
	if q.ModelKey != "" {
		conditions = append(conditions, "model_key = ?")
		args = append(args, q.ModelKey)
	}
	if q.ModelVersion != "" {
		conditions = append(conditions, "model_version = ?")
		args = append(args, q.ModelVersion)
	}
	if q.Scene != "" {
		conditions = append(conditions, "scene = ?")
		args = append(args, q.Scene)
	}
	if q.FromTime > 0 {
		conditions = append(conditions, "ctime >= ?")
		args = append(args, q.FromTime)
	}
	if q.ToTime > 0 {
		conditions = append(conditions, "ctime <= ?")
		args = append(args, q.ToTime)
	}
	if q.OnlyDegraded {
		conditions = append(conditions, "degraded = 1")
	}
	// 多取一行得到 hasMore，避免对这张大表做 COUNT(*)。
	query := decisionLogSelect + joinWhere(conditions) + " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, q.Limit+1, q.Offset)
	var rows []*RankDecisionLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("rank_decision_log List: %w", err)
	}
	hasMore := len(rows) > q.Limit
	if hasMore {
		rows = rows[:q.Limit]
	}
	return rows, hasMore, nil
}

func (m *defaultRankDecisionLogModel) CountDegradedSince(ctx context.Context, from int64) (int64, error) {
	var total int64
	query := "SELECT COUNT(*) FROM rank_decision_log WHERE degraded = 1 AND ctime >= ?"
	if err := m.conn.QueryRowCtx(ctx, &total, query, from); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("rank_decision_log CountDegradedSince: %w", err)
	}
	return total, nil
}

func (m *defaultRankDecisionLogModel) SelectExpiredBefore(ctx context.Context, cutoff int64, limit int) ([]int64, error) {
	if limit <= 0 || cutoff <= 0 {
		return nil, nil
	}
	var ids []int64
	query := "SELECT id FROM rank_decision_log WHERE ctime < ? ORDER BY id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &ids, query, cutoff, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_decision_log SelectExpiredBefore: %w", err)
	}
	return ids, nil
}

func (m *defaultRankDecisionLogModel) DeleteExpiredBefore(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	query := "DELETE FROM rank_decision_log WHERE id IN (" + inPlaceholders(len(ids)) + ")"
	args := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("rank_decision_log DeleteExpiredBefore: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rank_decision_log DeleteExpiredBefore RowsAffected: %w", err)
	}
	return affected, nil
}
