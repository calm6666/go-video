package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// FingerprintTask 指纹抽取任务记录。
// (asset_id, fp_type) 唯一索引：同一媒资的同种指纹只允许一个进行中的任务。
type FingerprintTask struct {
	ID       int64  `db:"id"`        // 主键 ID
	TaskID   int64  `db:"task_id"`   // 任务 ID（业务主键，与 ID 同值，便于跨服务引用）
	AssetID  int64  `db:"asset_id"`  // 关联媒资 ID
	FpType   int32  `db:"fp_type"`   // 指纹类型：1=video、2=audio
	VideoKey string `db:"video_key"` // 视频指纹 key（SUCCEEDED 后由 Worker 回写）
	AudioKey string `db:"audio_key"` // 音频指纹 key（SUCCEEDED 后由 Worker 回写）
	State    int32  `db:"state"`     // 任务状态：1=PENDING、2=SUCCEEDED、3=FAILED
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`     // 修改时间（Unix 秒）
}

// FingerprintRecord 指纹记录表，存储 Worker 回写的指纹事实。
// (asset_id, fp_type) 唯一索引：同种指纹的同一媒资只保留最新一条。
type FingerprintRecord struct {
	ID      int64  `db:"id"`       // 主键 ID
	AssetID int64  `db:"asset_id"` // 媒资 ID
	FpType  int32  `db:"fp_type"`  // 指纹类型：1=video、2=audio
	Key     string `db:"key"`      // 指纹 key（用于检索）
	Hash    string `db:"hash"`     // 指纹哈希（用于精确比对）
	Ctime   int64  `db:"ctime"`    // 创建时间（Unix 秒）
}

// FingerprintTaskModel fingerprint_task 表查询与写入接口。
type FingerprintTaskModel interface {
	// Insert 创建任务；返回新任务 ID。
	Insert(ctx context.Context, t *FingerprintTask) (int64, error)
	// FindOne 按 task_id 查询任务详情；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, taskID int64) (*FingerprintTask, error)
	// List 分页查询；assetID<=0 表示不按 asset 过滤，state<=0 表示不按状态过滤。
	List(ctx context.Context, assetID int64, state int32, pn, ps int32) ([]*FingerprintTask, int32, error)
	// UpdateResult 回写任务结果（state、video_key、audio_key、mtime）。
	// 只允许 PENDING → SUCCEEDED / FAILED；其他状态返回 ErrIllegalState。
	UpdateResult(ctx context.Context, taskID int64, state int32, videoKey, audioKey string) (*FingerprintTask, error)
}

type defaultFingerprintTaskModel struct {
	conn sqlx.SqlConn
}

// NewFingerprintTaskModel 创建 FingerprintTaskModel 实现。
func NewFingerprintTaskModel(conn sqlx.SqlConn) FingerprintTaskModel {
	return &defaultFingerprintTaskModel{conn: conn}
}

func (m *defaultFingerprintTaskModel) Insert(ctx context.Context, t *FingerprintTask) (int64, error) {
	now := nowUnix()
	t.Ctime = now
	t.Mtime = now
	if t.State == 0 {
		t.State = TaskStatePending
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO fingerprint_task (task_id, asset_id, fp_type, video_key, audio_key, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		t.TaskID, t.AssetID, t.FpType, t.VideoKey, t.AudioKey, t.State, t.Ctime, t.Mtime)
	if err != nil {
		return 0, fmt.Errorf("fingerprint_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("fingerprint_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultFingerprintTaskModel) FindOne(ctx context.Context, taskID int64) (*FingerprintTask, error) {
	var t FingerprintTask
	query := "SELECT id, task_id, asset_id, fp_type, video_key, audio_key, state, ctime, mtime FROM fingerprint_task WHERE task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("fingerprint_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultFingerprintTaskModel) List(ctx context.Context, assetID int64, state int32, pn, ps int32) ([]*FingerprintTask, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	// 动态拼接 WHERE 条件
	where := "WHERE 1=1"
	args := []interface{}{}
	if assetID > 0 {
		where += " AND asset_id = ?"
		args = append(args, assetID)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	countQuery := "SELECT COUNT(*) FROM fingerprint_task " + where
	if err := m.conn.QueryRowCtx(ctx, &total, countQuery, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("fingerprint_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listQuery := "SELECT id, task_id, asset_id, fp_type, video_key, audio_key, state, ctime, mtime FROM fingerprint_task " +
		where + " ORDER BY ctime DESC LIMIT ? OFFSET ?"
	args = append(args, ps, offset)
	var rows []*FingerprintTask
	if err := m.conn.QueryRowsCtx(ctx, &rows, listQuery, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("fingerprint_task List list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultFingerprintTaskModel) UpdateResult(ctx context.Context, taskID int64, state int32, videoKey, audioKey string) (*FingerprintTask, error) {
	// 1. 查询旧任务，校验状态机
	old, err := m.FindOne(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if old == nil {
		return nil, ErrTaskNotFound
	}
	if old.State != TaskStatePending {
		return nil, fmt.Errorf("%w: task %d state=%d, expect PENDING", ErrIllegalState, taskID, old.State)
	}
	if state != TaskStateSucceeded && state != TaskStateFailed {
		return nil, fmt.Errorf("%w: target state=%d, expect SUCCEEDED/FAILED", ErrIllegalState, state)
	}

	now := nowUnix()
	_, err = m.conn.ExecCtx(ctx,
		"UPDATE fingerprint_task SET state = ?, video_key = ?, audio_key = ?, mtime = ? WHERE task_id = ? AND state = ?",
		state, videoKey, audioKey, now, taskID, TaskStatePending)
	if err != nil {
		return nil, fmt.Errorf("fingerprint_task UpdateResult: %w", err)
	}
	// 回读最新值
	return m.FindOne(ctx, taskID)
}

// FingerprintRecordModel fingerprint_record 表查询与写入接口。
type FingerprintRecordModel interface {
	// Upsert 新增或更新指纹记录；按 (asset_id, fp_type) 唯一索引幂等。
	Upsert(ctx context.Context, r *FingerprintRecord) error
	// FindByAsset 按 asset_id 查询其所有指纹记录；fpType<=0 表示不限定类型。
	FindByAsset(ctx context.Context, assetID int64, fpType int32) ([]*FingerprintRecord, error)
	// FindByKey 按指纹 key 查询匹配的 asset 列表（本期占位，返回空）。
	FindByKey(ctx context.Context, fpKey string, fpType int32, topN int32) ([]*FingerprintRecord, error)
}

type defaultFingerprintRecordModel struct {
	conn sqlx.SqlConn
}

// NewFingerprintRecordModel 创建 FingerprintRecordModel 实现。
func NewFingerprintRecordModel(conn sqlx.SqlConn) FingerprintRecordModel {
	return &defaultFingerprintRecordModel{conn: conn}
}

func (m *defaultFingerprintRecordModel) Upsert(ctx context.Context, r *FingerprintRecord) error {
	r.Ctime = nowUnix()
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO fingerprint_record (asset_id, fp_type, `key`, `hash`, ctime) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE `key` = VALUES(`key`), `hash` = VALUES(`hash`), ctime = VALUES(ctime)",
		r.AssetID, r.FpType, r.Key, r.Hash, r.Ctime)
	if err != nil {
		return fmt.Errorf("fingerprint_record Upsert: %w", err)
	}
	return nil
}

func (m *defaultFingerprintRecordModel) FindByAsset(ctx context.Context, assetID int64, fpType int32) ([]*FingerprintRecord, error) {
	query := "SELECT id, asset_id, fp_type, `key`, `hash`, ctime FROM fingerprint_record WHERE asset_id = ?"
	args := []interface{}{assetID}
	if fpType > 0 {
		query += " AND fp_type = ?"
		args = append(args, fpType)
	}
	var rows []*FingerprintRecord
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("fingerprint_record FindByAsset: %w", err)
	}
	return rows, nil
}

// FindByKey 本期占位：未接入指纹检索引擎，返回空列表。
// TODO(后续)：接入向量/倒排检索引擎（如 OpenSearch/自研索引），
//
//	按 fp_key 范围/相似度检索 TopN 候选 asset。
func (m *defaultFingerprintRecordModel) FindByKey(ctx context.Context, fpKey string, fpType int32, topN int32) ([]*FingerprintRecord, error) {
	_ = ctx
	_ = fpKey
	_ = fpType
	_ = topN
	return nil, nil
}
