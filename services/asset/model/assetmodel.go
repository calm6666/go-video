package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ==================== AssetMeta ====================

// AssetMeta 原文件元数据。
// 依据 AGENTS.md §5，asset 拥有原文件元数据；不把大文件本身写入 MySQL。
type AssetMeta struct {
	AssetID   int64  `db:"asset_id"`   // 媒资 ID
	UploadID  int64  `db:"upload_id"`  // 关联 upload ID
	Mid       int64  `db:"mid"`        // 上传用户 ID
	Bucket    string `db:"bucket"`     // 对象存储桶
	ObjectKey string `db:"object_key"` // 对象键
	Size      int64  `db:"size"`       // 文件大小（字节）
	Md5       string `db:"md5"`        // 文件 MD5
	Duration  int64  `db:"duration"`   // 时长（毫秒）
	Width     int32  `db:"width"`      // 视频宽
	Height    int32  `db:"height"`     // 视频高
	Codec     string `db:"codec"`      // 编码
	State     int32  `db:"state"`      // 状态：见 model 状态常量
	Ctime     int64  `db:"ctime"`      // 创建时间（Unix 秒）
	Mtime     int64  `db:"mtime"`      // 修改时间（Unix 秒）
}

// AssetMetaModel asset_meta 表查询与写入接口。
type AssetMetaModel interface {
	// Insert 创建 asset_meta 记录；返回 asset_id。
	Insert(ctx context.Context, m *AssetMeta) (int64, error)
	// FindOne 按 asset_id 查询单条；未找到返回 (nil, nil)。
	FindOne(ctx context.Context, assetID int64) (*AssetMeta, error)
	// List 分页查询；mid<=0 与 state<=0 表示不过滤。
	List(ctx context.Context, mid int64, state int32, pn, ps int32) ([]*AssetMeta, int32, error)
	// UpdateMeta 更新 duration/width/height/codec；同时刷新 mtime。
	UpdateMeta(ctx context.Context, assetID int64, duration int64, width, height int32, codec string) error
	// UpdateState 推进 state；由 logic 校验合法性。
	UpdateState(ctx context.Context, assetID int64, toState int32) error
}

type defaultAssetMetaModel struct {
	conn sqlx.SqlConn
}

// NewAssetMetaModel 创建 AssetMetaModel 实现。
func NewAssetMetaModel(conn sqlx.SqlConn) AssetMetaModel {
	return &defaultAssetMetaModel{conn: conn}
}

func (m *defaultAssetMetaModel) Insert(ctx context.Context, a *AssetMeta) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO asset_meta (upload_id, mid, bucket, object_key, size, md5, duration, width, height, codec, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		a.UploadID, a.Mid, a.Bucket, a.ObjectKey, a.Size, a.Md5, a.Duration, a.Width, a.Height, a.Codec, a.State, a.Ctime, a.Mtime)
	if err != nil {
		return 0, fmt.Errorf("asset_meta Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("asset_meta Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultAssetMetaModel) FindOne(ctx context.Context, assetID int64) (*AssetMeta, error) {
	var a AssetMeta
	query := "SELECT asset_id, upload_id, mid, bucket, object_key, size, md5, duration, width, height, codec, state, ctime, mtime FROM asset_meta WHERE asset_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &a, query, assetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("asset_meta FindOne: %w", err)
	}
	return &a, nil
}

func (m *defaultAssetMetaModel) List(ctx context.Context, mid int64, state int32, pn, ps int32) ([]*AssetMeta, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	where := "WHERE 1=1"
	args := []interface{}{}
	if mid > 0 {
		where += " AND mid = ?"
		args = append(args, mid)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM asset_meta "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("asset_meta List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(args, ps, offset)
	var rows []*AssetMeta
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT asset_id, upload_id, mid, bucket, object_key, size, md5, duration, width, height, codec, state, ctime, mtime FROM asset_meta "+where+" ORDER BY asset_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("asset_meta List list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultAssetMetaModel) UpdateMeta(ctx context.Context, assetID int64, duration int64, width, height int32, codec string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE asset_meta SET duration = ?, width = ?, height = ?, codec = ?, mtime = ? WHERE asset_id = ?",
		duration, width, height, codec, nowUnix(), assetID)
	if err != nil {
		return fmt.Errorf("asset_meta UpdateMeta: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("asset_meta UpdateMeta RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrAssetNotFound
	}
	return nil
}

func (m *defaultAssetMetaModel) UpdateState(ctx context.Context, assetID int64, toState int32) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE asset_meta SET state = ?, mtime = ? WHERE asset_id = ?",
		toState, nowUnix(), assetID)
	if err != nil {
		return fmt.Errorf("asset_meta UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("asset_meta UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrAssetNotFound
	}
	return nil
}

// ==================== AssetCover ====================

// AssetCover 封面元数据。
type AssetCover struct {
	CoverID   int64  `db:"cover_id"`   // 封面 ID
	AssetID   int64  `db:"asset_id"`   // 关联媒资 ID
	Bucket    string `db:"bucket"`     // 对象存储桶
	ObjectKey string `db:"object_key"` // 对象键
	Width     int32  `db:"width"`      // 宽
	Height    int32  `db:"height"`     // 高
	Ctime     int64  `db:"ctime"`      // 创建时间（Unix 秒）
}

// AssetCoverModel asset_cover 表查询与写入接口。
type AssetCoverModel interface {
	Insert(ctx context.Context, c *AssetCover) (int64, error)
	ListByAsset(ctx context.Context, assetID int64) ([]*AssetCover, error)
}

type defaultAssetCoverModel struct {
	conn sqlx.SqlConn
}

// NewAssetCoverModel 创建 AssetCoverModel 实现。
func NewAssetCoverModel(conn sqlx.SqlConn) AssetCoverModel {
	return &defaultAssetCoverModel{conn: conn}
}

func (m *defaultAssetCoverModel) Insert(ctx context.Context, c *AssetCover) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO asset_cover (asset_id, bucket, object_key, width, height, ctime) VALUES (?, ?, ?, ?, ?, ?)",
		c.AssetID, c.Bucket, c.ObjectKey, c.Width, c.Height, c.Ctime)
	if err != nil {
		return 0, fmt.Errorf("asset_cover Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("asset_cover Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultAssetCoverModel) ListByAsset(ctx context.Context, assetID int64) ([]*AssetCover, error) {
	var rows []*AssetCover
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT cover_id, asset_id, bucket, object_key, width, height, ctime FROM asset_cover WHERE asset_id = ? ORDER BY cover_id ASC",
		assetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("asset_cover ListByAsset: %w", err)
	}
	return rows, nil
}

// ==================== AssetSubtitle ====================

// AssetSubtitle 字幕元数据。
type AssetSubtitle struct {
	SubID     int64  `db:"sub_id"`     // 字幕 ID
	AssetID   int64  `db:"asset_id"`   // 关联媒资 ID
	Lang      string `db:"lang"`       // 语言代码（如 zh-CN、en-US）
	Bucket    string `db:"bucket"`     // 对象存储桶
	ObjectKey string `db:"object_key"` // 对象键
	Ctime     int64  `db:"ctime"`      // 创建时间（Unix 秒）
}

// AssetSubtitleModel asset_subtitle 表查询与写入接口。
type AssetSubtitleModel interface {
	Insert(ctx context.Context, s *AssetSubtitle) (int64, error)
	ListByAsset(ctx context.Context, assetID int64) ([]*AssetSubtitle, error)
}

type defaultAssetSubtitleModel struct {
	conn sqlx.SqlConn
}

// NewAssetSubtitleModel 创建 AssetSubtitleModel 实现。
func NewAssetSubtitleModel(conn sqlx.SqlConn) AssetSubtitleModel {
	return &defaultAssetSubtitleModel{conn: conn}
}

func (m *defaultAssetSubtitleModel) Insert(ctx context.Context, s *AssetSubtitle) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO asset_subtitle (asset_id, lang, bucket, object_key, ctime) VALUES (?, ?, ?, ?, ?)",
		s.AssetID, s.Lang, s.Bucket, s.ObjectKey, s.Ctime)
	if err != nil {
		return 0, fmt.Errorf("asset_subtitle Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("asset_subtitle Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultAssetSubtitleModel) ListByAsset(ctx context.Context, assetID int64) ([]*AssetSubtitle, error) {
	var rows []*AssetSubtitle
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT sub_id, asset_id, lang, bucket, object_key, ctime FROM asset_subtitle WHERE asset_id = ? ORDER BY sub_id ASC",
		assetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("asset_subtitle ListByAsset: %w", err)
	}
	return rows, nil
}

// ==================== AssetScreenshot ====================

// AssetScreenshot 截图元数据。
type AssetScreenshot struct {
	ShotID    int64  `db:"shot_id"`    // 截图 ID
	AssetID   int64  `db:"asset_id"`   // 关联媒资 ID
	Bucket    string `db:"bucket"`     // 对象存储桶
	ObjectKey string `db:"object_key"` // 对象键
	Timestamp int64  `db:"timestamp"`  // 截图时间点（毫秒）
	Ctime     int64  `db:"ctime"`      // 创建时间（Unix 秒）
}

// AssetScreenshotModel asset_screenshot 表查询与写入接口。
type AssetScreenshotModel interface {
	Insert(ctx context.Context, s *AssetScreenshot) (int64, error)
}

type defaultAssetScreenshotModel struct {
	conn sqlx.SqlConn
}

// NewAssetScreenshotModel 创建 AssetScreenshotModel 实现。
func NewAssetScreenshotModel(conn sqlx.SqlConn) AssetScreenshotModel {
	return &defaultAssetScreenshotModel{conn: conn}
}

func (m *defaultAssetScreenshotModel) Insert(ctx context.Context, s *AssetScreenshot) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO asset_screenshot (asset_id, bucket, object_key, timestamp, ctime) VALUES (?, ?, ?, ?, ?)",
		s.AssetID, s.Bucket, s.ObjectKey, s.Timestamp, s.Ctime)
	if err != nil {
		return 0, fmt.Errorf("asset_screenshot Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("asset_screenshot Insert LastInsertId: %w", err)
	}
	return id, nil
}
