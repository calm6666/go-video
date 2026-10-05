package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// VideoSubmission 稿件主表记录。
// aid 是稿件 ID；mid 是投稿用户 ID；state 是 SubmissionState 枚举值。
type VideoSubmission struct {
	Aid    int64  `db:"aid"`    // 稿件 ID
	Mid    int64  `db:"mid"`    // 投稿用户 ID
	Title  string `db:"title"`  // 标题
	Desc   string `db:"desc"`   // 简介
	Cover  string `db:"cover"`  // 封面 URL
	Typeid int32  `db:"typeid"` // 分区 ID
	Tag    string `db:"tag"`    // 标签（逗号分隔）
	State  int32  `db:"state"`  // 稿件状态
	Ctime  int64  `db:"ctime"`  // 创建时间（Unix 秒）
	Mtime  int64  `db:"mtime"`  // 修改时间（Unix 秒）
}

// VideoVersion 稿件版本记录。
type VideoVersion struct {
	Aid     int64  `db:"aid"`      // 稿件 ID
	Version int64  `db:"version"`  // 版本号
	AssetID string `db:"asset_id"` // 关联媒资 ID
	State   int32  `db:"state"`    // 版本状态
	Ctime   int64  `db:"ctime"`    // 创建时间（Unix 秒）
}

// VideoAuditLog 状态流转审计记录。
type VideoAuditLog struct {
	ID        int64  `db:"id"`         // 审计记录 ID
	Aid       int64  `db:"aid"`        // 稿件 ID
	FromState int32  `db:"from_state"` // 原状态
	ToState   int32  `db:"to_state"`   // 新状态
	Operator  string `db:"operator"`   // 操作人
	Reason    string `db:"reason"`     // 变更原因
	Ctime     int64  `db:"ctime"`      // 变更时间（Unix 秒）
}

// VideoSubmissionModel video_submission 表查询与写入接口。
type VideoSubmissionModel interface {
	// Insert 新建稿件（state=DRAFT），返回新 aid。
	Insert(ctx context.Context, s *VideoSubmission) (int64, error)
	// FindOne 按 aid 查询单个稿件。
	FindOne(ctx context.Context, aid int64) (*VideoSubmission, error)
	// List 分页查询稿件；mid=0 不按用户过滤，typeid=0 不按分区过滤。
	List(ctx context.Context, mid int64, typeid int32, pn, ps int32) ([]*VideoSubmission, int32, error)
	// ListByState 按状态分页查询。
	ListByState(ctx context.Context, state int32, pn, ps int32) ([]*VideoSubmission, int32, error)
	// UpdateFields 更新稿件元信息（仅 DRAFT 可改由调用方校验）。
	UpdateFields(ctx context.Context, aid int64, title, desc, cover string, typeid int32, tag string) error
	// UpdateState 更新稿件状态（在事务内调用，配合 InsertAuditLog）。
	UpdateState(ctx context.Context, tx sqlx.Session, aid int64, state int32) error
}

type defaultVideoSubmissionModel struct {
	conn sqlx.SqlConn
}

// NewVideoSubmissionModel 创建 VideoSubmissionModel 实现。
func NewVideoSubmissionModel(conn sqlx.SqlConn) VideoSubmissionModel {
	return &defaultVideoSubmissionModel{conn: conn}
}

func (m *defaultVideoSubmissionModel) Insert(ctx context.Context, s *VideoSubmission) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO video_submission (mid, title, `desc`, cover, typeid, tag, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		s.Mid, s.Title, s.Desc, s.Cover, s.Typeid, s.Tag, s.State, s.Ctime, s.Mtime)
	if err != nil {
		return 0, fmt.Errorf("video_submission Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("video_submission Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultVideoSubmissionModel) FindOne(ctx context.Context, aid int64) (*VideoSubmission, error) {
	var s VideoSubmission
	query := "SELECT aid, mid, title, `desc`, cover, typeid, tag, state, ctime, mtime FROM video_submission WHERE aid = ?"
	if err := m.conn.QueryRowCtx(ctx, &s, query, aid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("video_submission FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultVideoSubmissionModel) List(ctx context.Context, mid int64, typeid int32, pn, ps int32) ([]*VideoSubmission, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	where := "state <> ?"
	args := []interface{}{StateDeleted}
	if mid > 0 {
		where += " AND mid = ?"
		args = append(args, mid)
	}
	if typeid > 0 {
		where += " AND typeid = ?"
		args = append(args, typeid)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM video_submission WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("video_submission List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, ps, offset)
	var rows []*VideoSubmission
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT aid, mid, title, `desc`, cover, typeid, tag, state, ctime, mtime FROM video_submission WHERE "+where+" ORDER BY aid DESC LIMIT ? OFFSET ?",
		args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("video_submission List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultVideoSubmissionModel) ListByState(ctx context.Context, state int32, pn, ps int32) ([]*VideoSubmission, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM video_submission WHERE state = ?", state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("video_submission ListByState count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	var rows []*VideoSubmission
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT aid, mid, title, `desc`, cover, typeid, tag, state, ctime, mtime FROM video_submission WHERE state = ? ORDER BY aid DESC LIMIT ? OFFSET ?",
		state, ps, offset); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("video_submission ListByState: %w", err)
	}
	return rows, total, nil
}

func (m *defaultVideoSubmissionModel) UpdateFields(ctx context.Context, aid int64, title, desc, cover string, typeid int32, tag string) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE video_submission SET title = ?, `desc` = ?, cover = ?, typeid = ?, tag = ?, mtime = ? WHERE aid = ?",
		title, desc, cover, typeid, tag, nowUnix(), aid)
	if err != nil {
		return fmt.Errorf("video_submission UpdateFields: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("video_submission UpdateFields RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrSubmissionNotFound
	}
	return nil
}

func (m *defaultVideoSubmissionModel) UpdateState(ctx context.Context, tx sqlx.Session, aid int64, state int32) error {
	res, err := tx.ExecCtx(ctx,
		"UPDATE video_submission SET state = ?, mtime = ? WHERE aid = ?",
		state, nowUnix(), aid)
	if err != nil {
		return fmt.Errorf("video_submission UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("video_submission UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrSubmissionNotFound
	}
	return nil
}

// VideoVersionModel video_version 表查询与写入接口。
type VideoVersionModel interface {
	// Insert 新建稿件版本。
	Insert(ctx context.Context, v *VideoVersion) error
	// ListByAid 查询稿件的所有版本。
	ListByAid(ctx context.Context, aid int64) ([]*VideoVersion, error)
}

type defaultVideoVersionModel struct {
	conn sqlx.SqlConn
}

// NewVideoVersionModel 创建 VideoVersionModel 实现。
func NewVideoVersionModel(conn sqlx.SqlConn) VideoVersionModel {
	return &defaultVideoVersionModel{conn: conn}
}

func (m *defaultVideoVersionModel) Insert(ctx context.Context, v *VideoVersion) error {
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO video_version (aid, version, asset_id, state, ctime) VALUES (?, ?, ?, ?, ?)",
		v.Aid, v.Version, v.AssetID, v.State, v.Ctime)
	if err != nil {
		return fmt.Errorf("video_version Insert: %w", err)
	}
	return nil
}

func (m *defaultVideoVersionModel) ListByAid(ctx context.Context, aid int64) ([]*VideoVersion, error) {
	var rows []*VideoVersion
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT aid, version, asset_id, state, ctime FROM video_version WHERE aid = ? ORDER BY version DESC", aid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("video_version ListByAid: %w", err)
	}
	return rows, nil
}

// VideoAuditLogModel video_audit_log 表查询与写入接口。
type VideoAuditLogModel interface {
	// Insert 在事务内写状态流转审计记录。
	Insert(ctx context.Context, tx sqlx.Session, log *VideoAuditLog) error
	// ListByAid 查询稿件的状态流转历史。
	ListByAid(ctx context.Context, aid int64) ([]*VideoAuditLog, error)
}

type defaultVideoAuditLogModel struct {
	conn sqlx.SqlConn
}

// NewVideoAuditLogModel 创建 VideoAuditLogModel 实现。
func NewVideoAuditLogModel(conn sqlx.SqlConn) VideoAuditLogModel {
	return &defaultVideoAuditLogModel{conn: conn}
}

func (m *defaultVideoAuditLogModel) Insert(ctx context.Context, tx sqlx.Session, log *VideoAuditLog) error {
	_, err := tx.ExecCtx(ctx,
		"INSERT INTO video_audit_log (aid, from_state, to_state, operator, reason, ctime) VALUES (?, ?, ?, ?, ?, ?)",
		log.Aid, log.FromState, log.ToState, log.Operator, log.Reason, log.Ctime)
	if err != nil {
		return fmt.Errorf("video_audit_log Insert: %w", err)
	}
	return nil
}

func (m *defaultVideoAuditLogModel) ListByAid(ctx context.Context, aid int64) ([]*VideoAuditLog, error) {
	var rows []*VideoAuditLog
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT id, aid, from_state, to_state, operator, reason, ctime FROM video_audit_log WHERE aid = ? ORDER BY id ASC", aid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("video_audit_log ListByAid: %w", err)
	}
	return rows, nil
}
