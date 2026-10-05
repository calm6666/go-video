package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RankExperimentAssignment 主体在实验中的稳定分桶记录（rank_experiment_assignment 表）。
//
// 为什么必须落库而不只是每次现算：
//   - rpc GetExperimentAssignmentReply.newly_assigned 要求区分「首次分桶」与「sticky 复用」；
//     纯函数的 sha256 取模虽然稳定，但无法回答「这个用户是什么时候进组的」；
//   - 实验读数需要按组去重人数（同层同时有多个变体在跑时，桶号相同的主体必须同源），
//     这要求有一份「主体 → 桶号 → 变体」的事实表，而不是每次重算的推导结果。
//
// 唯一键含 hash_seed：改盐即产生新行（重新分桶），旧行保留，
// 于是「换盐前后各组人数变化」本身可查、可审计。
//
// 隐私约束（AGENTS.md §7）：subject_id 只存 mid 十进制串或设备 sha256 摘要，
// 明文设备号由 ValidateSubjectID 在写入前拒绝，本表因此不构成可反查的设备档案。
type RankExperimentAssignment struct {
	ID          int64  `db:"id"`           // 自增主键
	ExpKey      string `db:"exp_key"`      // 实验 key
	LayerKey    string `db:"layer_key"`    // 落库时的互斥层（冗余，便于按层核对分流均匀度）
	SubjectType int32  `db:"subject_type"` // 参见 Subject* 常量
	SubjectID   string `db:"subject_id"`   // mid 十进制串或设备 sha256 摘要
	HashSeed    string `db:"hash_seed"`    // 分桶哈希盐（唯一键成员）
	BucketCount int32  `db:"bucket_count"` // 分桶空间大小（落库口径，默认 1000）
	BucketNo    int32  `db:"bucket_no"`    // 命中的桶号，[0, bucket_count)
	VariantKey  string `db:"variant_key"`  // 由桶号解析出的变体（未命中为 control）
	Revision    int32  `db:"revision"`     // 命中变体当时的 revision（审计回指）
	AssignedAt  int64  `db:"assigned_at"`  // 首次分桶时间（Unix 秒）
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// RankExperimentAssignmentModel rank_experiment_assignment 表读写接口。
type RankExperimentAssignmentModel interface {
	// Insert 登记首次分桶；同一 (exp_key, subject_type, subject_id, hash_seed) 已存在时
	// 返回 existed=true 且不报错（sticky 语义，不依赖先查后插的竞态窗口）。
	Insert(ctx context.Context, a *RankExperimentAssignment) (existed bool, err error)
	// FindOne 查询主体在指定盐下的分桶；不存在返回 ErrAssignmentNotFound。
	FindOne(ctx context.Context, expKey string, subjectType int32, subjectID, hashSeed string) (*RankExperimentAssignment, error)
	// ListByVariant 按 (exp_key, variant_key) 分页列出主体（实验人数核对、灰度白名单复查）。
	ListByVariant(ctx context.Context, expKey, variantKey string, offset, limit int) ([]*RankExperimentAssignment, error)
	// CountByVariant 统计某实验各变体的已分桶主体数（GROUP BY 后按变体返回）。
	// 这是「分流是否均匀」的事实来源，不引入实时全表扫描：limit 限制返回的变体条数。
	CountByVariant(ctx context.Context, expKey, hashSeed string, limit int) ([]VariantAssignmentCount, error)
	// DeleteOlderThan 按 assigned_at 分批清理长期不参与实验的历史行（保留期见迁移文件说明）。
	// 单次最多删 limit 行，返回实际删除数；调用方必须循环到 0 而不是无限放大 limit。
	DeleteOlderThan(ctx context.Context, cutoff int64, limit int) (int64, error)
}

// VariantAssignmentCount 是某实验某变体的分桶主体计数。
type VariantAssignmentCount struct {
	VariantKey string `db:"variant_key"`
	HashSeed   string `db:"hash_seed"`
	Total      int64  `db:"total"`
}

type defaultRankExperimentAssignmentModel struct {
	conn sqlx.SqlConn
}

// NewRankExperimentAssignmentModel 创建 RankExperimentAssignmentModel 实现。
func NewRankExperimentAssignmentModel(conn sqlx.SqlConn) RankExperimentAssignmentModel {
	return &defaultRankExperimentAssignmentModel{conn: conn}
}

const assignmentSelect = "SELECT id, exp_key, layer_key, subject_type, subject_id, hash_seed, bucket_count, bucket_no, " +
	"variant_key, revision, assigned_at, ctime, mtime FROM rank_experiment_assignment"

// insertIgnore 用 INSERT IGNORE 而不是 ON DUPLICATE KEY UPDATE：
// 分桶记录一旦写入就是事实，任何字段都不该被后续请求改写；
// 冲突时 affected=0 即代表「已存在」，无需再读一次，也没有先查后插的竞态。
const insertIgnore = "INSERT IGNORE INTO rank_experiment_assignment (exp_key, layer_key, subject_type, subject_id, " +
	"hash_seed, bucket_count, bucket_no, variant_key, revision, assigned_at, ctime, mtime) VALUES ("

func (m *defaultRankExperimentAssignmentModel) Insert(ctx context.Context, a *RankExperimentAssignment) (bool, error) {
	if err := ValidateSubjectID(a.SubjectType, a.SubjectID); err != nil {
		return false, err
	}
	if a.ExpKey == "" || a.HashSeed == "" {
		return false, ErrHashSeedRequired
	}
	if a.BucketCount <= 0 {
		a.BucketCount = DefaultBucketCount
	}
	if a.BucketNo < 0 || a.BucketNo >= a.BucketCount {
		return false, ErrInvalidBucketRange
	}
	if a.VariantKey == "" {
		a.VariantKey = ControlVariant
	}
	now := nowUnix()
	if a.AssignedAt == 0 {
		a.AssignedAt = now
	}
	a.Ctime = now
	a.Mtime = now
	res, err := m.conn.ExecCtx(ctx, insertIgnore+inPlaceholders(12)+")",
		a.ExpKey, a.LayerKey, a.SubjectType, a.SubjectID, a.HashSeed, a.BucketCount, a.BucketNo,
		a.VariantKey, a.Revision, a.AssignedAt, a.Ctime, a.Mtime)
	if err != nil {
		return false, fmt.Errorf("rank_experiment_assignment Insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rank_experiment_assignment Insert RowsAffected: %w", err)
	}
	return affected == 0, nil
}

func (m *defaultRankExperimentAssignmentModel) FindOne(ctx context.Context, expKey string, subjectType int32, subjectID, hashSeed string) (*RankExperimentAssignment, error) {
	if err := ValidateSubjectID(subjectType, subjectID); err != nil {
		return nil, err
	}
	var row RankExperimentAssignment
	query := assignmentSelect + " WHERE exp_key = ? AND subject_type = ? AND subject_id = ? AND hash_seed = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, expKey, subjectType, subjectID, hashSeed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrAssignmentNotFound
		}
		return nil, fmt.Errorf("rank_experiment_assignment FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultRankExperimentAssignmentModel) ListByVariant(ctx context.Context, expKey, variantKey string,
	offset, limit int) ([]*RankExperimentAssignment, error) {
	if limit <= 0 || expKey == "" {
		return nil, nil
	}
	if offset < 0 {
		offset = 0
	}
	var conditions []string
	var args []interface{}
	conditions = append(conditions, "exp_key = ?")
	args = append(args, expKey)
	if variantKey != "" {
		conditions = append(conditions, "variant_key = ?")
		args = append(args, variantKey)
	}
	query := assignmentSelect + joinWhere(conditions) + " ORDER BY id ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	var rows []*RankExperimentAssignment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment_assignment ListByVariant: %w", err)
	}
	return rows, nil
}

func (m *defaultRankExperimentAssignmentModel) CountByVariant(ctx context.Context, expKey, hashSeed string,
	limit int) ([]VariantAssignmentCount, error) {
	if limit <= 0 || expKey == "" {
		return nil, nil
	}
	query := "SELECT variant_key, hash_seed, COUNT(*) AS total FROM rank_experiment_assignment WHERE exp_key = ?"
	var args []interface{}
	args = append(args, expKey)
	if hashSeed != "" {
		query += " AND hash_seed = ?"
		args = append(args, hashSeed)
	}
	query += " GROUP BY variant_key, hash_seed ORDER BY variant_key ASC LIMIT ?"
	args = append(args, limit)
	var rows []VariantAssignmentCount
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rank_experiment_assignment CountByVariant: %w", err)
	}
	return rows, nil
}

func (m *defaultRankExperimentAssignmentModel) DeleteOlderThan(ctx context.Context, cutoff int64, limit int) (int64, error) {
	if limit <= 0 || cutoff <= 0 {
		return 0, nil
	}
	// 分批删除（LIMIT）而不是 TRUNCATE：分桶记录是实验归属的事实来源，
	// 只清理 assigned_at 早于保留期且实验已 STOPPED 的行，清理范围由调用方（运维巡检）决定。
	query := "DELETE FROM rank_experiment_assignment WHERE assigned_at < ? LIMIT ?"
	res, err := m.conn.ExecCtx(ctx, query, cutoff, limit)
	if err != nil {
		return 0, fmt.Errorf("rank_experiment_assignment DeleteOlderThan: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rank_experiment_assignment DeleteOlderThan RowsAffected: %w", err)
	}
	return affected, nil
}
