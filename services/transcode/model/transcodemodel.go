package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// TranscodeTask 转码任务记录。
// transcode_task 表：task_id 自增主键，state 字段对应 TaskState 常量。
type TranscodeTask struct {
	TaskId       int64  `db:"task_id"`       // 任务 ID
	AssetId      int64  `db:"asset_id"`      // 媒资 ID
	TemplateId   int64  `db:"template_id"`   // 转码模板 ID
	InputBucket  string `db:"input_bucket"`  // 输入对象存储桶
	InputKey     string `db:"input_key"`     // 输入对象 key
	OutputBucket string `db:"output_bucket"` // 输出对象存储桶
	OutputKey    string `db:"output_key"`    // 输出对象 key
	State        int32  `db:"state"`         // 任务状态（见 TaskState 常量）
	Progress     int32  `db:"progress"`      // 进度（0-100）
	Errno        int32  `db:"errno"`         // 错误码
	ErrMsg       string `db:"err_msg"`       // 错误信息
	Ctime        int64  `db:"ctime"`         // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`         // 修改时间（Unix 秒）
}

// TranscodeTaskModel transcode_task 表查询与写入接口。
type TranscodeTaskModel interface {
	// Insert 新建转码任务，返回新 task_id。
	Insert(ctx context.Context, t *TranscodeTask) (int64, error)
	// FindOne 查询单个任务。
	FindOne(ctx context.Context, taskID int64) (*TranscodeTask, error)
	// List 分页查询任务；assetID<=0 表示不按媒资过滤，state<=0 表示不按状态过滤。
	List(ctx context.Context, assetID int64, state int32, pn, ps int32) ([]*TranscodeTask, int32, error)
	// UpdateProgress 更新任务的进度/状态/错误信息。mtime 由调用方传入。
	UpdateProgress(ctx context.Context, taskID int64, progress, state, errno int32, errMsg string, mtime int64) error
}

type defaultTranscodeTaskModel struct {
	conn sqlx.SqlConn
}

// NewTranscodeTaskModel 创建 TranscodeTaskModel 实现。
func NewTranscodeTaskModel(conn sqlx.SqlConn) TranscodeTaskModel {
	return &defaultTranscodeTaskModel{conn: conn}
}

func (m *defaultTranscodeTaskModel) Insert(ctx context.Context, t *TranscodeTask) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO transcode_task (asset_id, template_id, input_bucket, input_key, output_bucket, output_key, state, progress, errno, err_msg, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.AssetId, t.TemplateId, t.InputBucket, t.InputKey, t.OutputBucket, t.OutputKey, t.State, t.Progress, t.Errno, t.ErrMsg, t.Ctime, t.Mtime)
	if err != nil {
		return 0, fmt.Errorf("transcode_task Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("transcode_task Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultTranscodeTaskModel) FindOne(ctx context.Context, taskID int64) (*TranscodeTask, error) {
	var t TranscodeTask
	query := "SELECT task_id, asset_id, template_id, input_bucket, input_key, output_bucket, output_key, state, progress, errno, err_msg, ctime, mtime FROM transcode_task WHERE task_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, taskID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("transcode_task FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultTranscodeTaskModel) List(ctx context.Context, assetID int64, state int32, pn, ps int32) ([]*TranscodeTask, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	// 动态拼接 WHERE：assetID>0 或 state>0 时按对应字段过滤，否则全表分页。
	where := "WHERE 1=1"
	args := make([]interface{}, 0, 4)
	if assetID > 0 {
		where += " AND asset_id = ?"
		args = append(args, assetID)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM transcode_task "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("transcode_task List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(args, ps, offset)
	var rows []*TranscodeTask
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT task_id, asset_id, template_id, input_bucket, input_key, output_bucket, output_key, state, progress, errno, err_msg, ctime, mtime FROM transcode_task "+where+" ORDER BY task_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("transcode_task List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultTranscodeTaskModel) UpdateProgress(ctx context.Context, taskID int64, progress, state, errno int32, errMsg string, mtime int64) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE transcode_task SET progress = ?, state = ?, errno = ?, err_msg = ?, mtime = ? WHERE task_id = ?",
		progress, state, errno, errMsg, mtime, taskID)
	if err != nil {
		return fmt.Errorf("transcode_task UpdateProgress: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("transcode_task UpdateProgress RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrTaskNotFound
	}
	return nil
}

// TranscodeTemplate 转码模板记录。
type TranscodeTemplate struct {
	TemplateId     int64  `db:"template_id"`     // 模板 ID
	Name           string `db:"name"`            // 模板名
	Codec          string `db:"codec"`           // 编码器（h264/hevc/aac 等）
	Width          int32  `db:"width"`           // 视频宽（0 自适应）
	Height         int32  `db:"height"`          // 视频高（0 自适应）
	Bitrate        int32  `db:"bitrate"`         // 目标码率（kbps，0 自适应）
	Fps            int32  `db:"fps"`             // 帧率（0 跟随源）
	SegmentSeconds int32  `db:"segment_seconds"` // HLS 分片时长（秒，0 不分片）
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
	Mtime          int64  `db:"mtime"`           // 修改时间（Unix 秒）
}

// TranscodeTemplateModel transcode_template 表查询与写入接口。
type TranscodeTemplateModel interface {
	// Insert 新建模板，返回新 template_id。
	Insert(ctx context.Context, t *TranscodeTemplate) (int64, error)
	// FindOne 查询单个模板。
	FindOne(ctx context.Context, templateID int64) (*TranscodeTemplate, error)
	// List 分页查询模板列表。
	List(ctx context.Context, pn, ps int32) ([]*TranscodeTemplate, int32, error)
}

type defaultTranscodeTemplateModel struct {
	conn sqlx.SqlConn
}

// NewTranscodeTemplateModel 创建 TranscodeTemplateModel 实现。
func NewTranscodeTemplateModel(conn sqlx.SqlConn) TranscodeTemplateModel {
	return &defaultTranscodeTemplateModel{conn: conn}
}

func (m *defaultTranscodeTemplateModel) Insert(ctx context.Context, t *TranscodeTemplate) (int64, error) {
	now := time.Now().Unix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO transcode_template (name, codec, width, height, bitrate, fps, segment_seconds, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.Name, t.Codec, t.Width, t.Height, t.Bitrate, t.Fps, t.SegmentSeconds, now, now)
	if err != nil {
		return 0, fmt.Errorf("transcode_template Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("transcode_template Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultTranscodeTemplateModel) FindOne(ctx context.Context, templateID int64) (*TranscodeTemplate, error) {
	var t TranscodeTemplate
	query := "SELECT template_id, name, codec, width, height, bitrate, fps, segment_seconds, ctime, mtime FROM transcode_template WHERE template_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, templateID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("transcode_template FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultTranscodeTemplateModel) List(ctx context.Context, pn, ps int32) ([]*TranscodeTemplate, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM transcode_template"); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("transcode_template List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*TranscodeTemplate
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT template_id, name, codec, width, height, bitrate, fps, segment_seconds, ctime, mtime FROM transcode_template ORDER BY template_id ASC LIMIT ? OFFSET ?",
		ps, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("transcode_template List: %w", err)
	}
	return rows, total, nil
}
