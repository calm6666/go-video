package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// RegionsCSVSeparator 是 regions 列表在 DB 中的分隔符。
const RegionsCSVSeparator = ","

// RegionsFromCSV 把 DB 中的 regions 字符串切回切片。空串返回 nil。
func RegionsFromCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, RegionsCSVSeparator)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RegionsToCSV 把 regions 切片拼成 DB 字符串。
func RegionsToCSV(rs []string) string {
	return strings.Join(rs, RegionsCSVSeparator)
}

// RightsContract 版权合同实体。
type RightsContract struct {
	ContractID int64  `db:"contract_id"` // 合同 ID
	OwnerID    int64  `db:"owner_id"`    // 版权方 ID
	Title      string `db:"title"`       // 合同标题
	SignDate   int64  `db:"sign_date"`   // 签订日期（Unix 秒）
	StartDate  int64  `db:"start_date"`  // 生效日期（Unix 秒）
	EndDate    int64  `db:"end_date"`    // 到期日期（Unix 秒）
	RegionsCSV string `db:"regions"`     // 授权地区代码（逗号分隔）
	State      int32  `db:"state"`       // 合同状态：1 active、2 terminated
	Ctime      int64  `db:"ctime"`       // 创建时间（Unix 秒）
	Mtime      int64  `db:"mtime"`       // 修改时间（Unix 秒）
}

// RightsContractModel rights_contract 表查询与写入接口。
type RightsContractModel interface {
	// Insert 新建合同；返回新 contract_id。
	Insert(ctx context.Context, c *RightsContract) (int64, error)
	// FindOne 按合同 ID 查询单个合同；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, contractID int64) (*RightsContract, error)
	// List 分页查询合同；owner_id=0 表示不筛选；state=0 表示不筛选。
	List(ctx context.Context, ownerID int64, state int32, pn, ps int32) ([]*RightsContract, int32, error)
}

type defaultRightsContractModel struct {
	conn sqlx.SqlConn
}

// NewRightsContractModel 创建 RightsContractModel 实现。
func NewRightsContractModel(conn sqlx.SqlConn) RightsContractModel {
	return &defaultRightsContractModel{conn: conn}
}

func (m *defaultRightsContractModel) Insert(ctx context.Context, c *RightsContract) (int64, error) {
	now := nowUnix()
	if c.Ctime == 0 {
		c.Ctime = now
	}
	if c.Mtime == 0 {
		c.Mtime = now
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO rights_contract (owner_id, title, sign_date, start_date, end_date, regions, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		c.OwnerID, c.Title, c.SignDate, c.StartDate, c.EndDate, c.RegionsCSV, c.State, c.Ctime, c.Mtime)
	if err != nil {
		return 0, fmt.Errorf("rights_contract Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("rights_contract Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultRightsContractModel) FindOne(ctx context.Context, contractID int64) (*RightsContract, error) {
	var c RightsContract
	query := "SELECT contract_id, owner_id, title, sign_date, start_date, end_date, regions, state, ctime, mtime FROM rights_contract WHERE contract_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &c, query, contractID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rights_contract FindOne: %w", err)
	}
	return &c, nil
}

func (m *defaultRightsContractModel) List(ctx context.Context, ownerID int64, state int32, pn, ps int32) ([]*RightsContract, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var (
		where = "WHERE 1=1"
		args  = []interface{}{}
	)
	if ownerID > 0 {
		where += " AND owner_id = ?"
		args = append(args, ownerID)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM rights_contract "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("rights_contract List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, ps, offset)
	var rows []*RightsContract
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT contract_id, owner_id, title, sign_date, start_date, end_date, regions, state, ctime, mtime FROM rights_contract "+where+" ORDER BY contract_id DESC LIMIT ? OFFSET ?",
		args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("rights_contract List list: %w", err)
	}
	return rows, total, nil
}

// RightsWindow 版权时间窗口实体。
type RightsWindow struct {
	WindowID    int64  `db:"window_id"`    // 窗口 ID
	ContractID  int64  `db:"contract_id"`  // 关联合同 ID
	ContentID   int64  `db:"content_id"`   // 内容 ID
	ContentType int32  `db:"content_type"` // 内容类型：1 pgc、2 ugc
	Region      string `db:"region"`       // 授权地区代码
	StartTime   int64  `db:"start_time"`   // 窗口开始时间（Unix 秒）
	EndTime     int64  `db:"end_time"`     // 窗口结束时间（Unix 秒）
	State       int32  `db:"state"`        // 窗口状态：1 active、2 expired、3 revoked
	Ctime       int64  `db:"ctime"`        // 创建时间（Unix 秒）
	Mtime       int64  `db:"mtime"`        // 修改时间（Unix 秒）
}

// RightsWindowModel rights_window 表查询与写入接口。
type RightsWindowModel interface {
	// Insert 新建窗口；返回新 window_id。
	Insert(ctx context.Context, w *RightsWindow) (int64, error)
	// FindOne 按窗口 ID 查询单个窗口；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, windowID int64) (*RightsWindow, error)
	// List 分页查询窗口；content_id/contract_id/content_type/state 为 0 表示不筛选。
	List(ctx context.Context, contentID, contractID int64, contentType, state int32, pn, ps int32) ([]*RightsWindow, int32, error)
	// FindActiveByContentRegion 查询某内容在某地区的有效窗口（state=active）。
	// 多条命中时返回 end_time 最大的一条；不存在返回 (nil, nil)。
	FindActiveByContentRegion(ctx context.Context, contentID int64, contentType int32, region string) (*RightsWindow, error)
	// UpdateState 更新窗口状态；返回受影响行数。
	UpdateState(ctx context.Context, windowID int64, state int32) (int64, error)
	// ListExpiring 查询 state=active 且 end_time <= now+within 的窗口，按 end_time 升序。
	ListExpiring(ctx context.Context, withinSeconds int32, pn, ps int32) ([]*RightsWindow, int32, error)
}

type defaultRightsWindowModel struct {
	conn sqlx.SqlConn
}

// NewRightsWindowModel 创建 RightsWindowModel 实现。
func NewRightsWindowModel(conn sqlx.SqlConn) RightsWindowModel {
	return &defaultRightsWindowModel{conn: conn}
}

func (m *defaultRightsWindowModel) Insert(ctx context.Context, w *RightsWindow) (int64, error) {
	now := nowUnix()
	if w.Ctime == 0 {
		w.Ctime = now
	}
	if w.Mtime == 0 {
		w.Mtime = now
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO rights_window (contract_id, content_id, content_type, region, start_time, end_time, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		w.ContractID, w.ContentID, w.ContentType, w.Region, w.StartTime, w.EndTime, w.State, w.Ctime, w.Mtime)
	if err != nil {
		return 0, fmt.Errorf("rights_window Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("rights_window Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultRightsWindowModel) FindOne(ctx context.Context, windowID int64) (*RightsWindow, error) {
	var w RightsWindow
	query := "SELECT window_id, contract_id, content_id, content_type, region, start_time, end_time, state, ctime, mtime FROM rights_window WHERE window_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &w, query, windowID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rights_window FindOne: %w", err)
	}
	return &w, nil
}

func (m *defaultRightsWindowModel) List(ctx context.Context, contentID, contractID int64, contentType, state int32, pn, ps int32) ([]*RightsWindow, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	var (
		where = "WHERE 1=1"
		args  = []interface{}{}
	)
	if contentID > 0 {
		where += " AND content_id = ?"
		args = append(args, contentID)
	}
	if contractID > 0 {
		where += " AND contract_id = ?"
		args = append(args, contractID)
	}
	if contentType > 0 {
		where += " AND content_type = ?"
		args = append(args, contentType)
	}
	if state > 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM rights_window "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("rights_window List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, ps, offset)
	var rows []*RightsWindow
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT window_id, contract_id, content_id, content_type, region, start_time, end_time, state, ctime, mtime FROM rights_window "+where+" ORDER BY window_id DESC LIMIT ? OFFSET ?",
		args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("rights_window List list: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRightsWindowModel) FindActiveByContentRegion(ctx context.Context, contentID int64, contentType int32, region string) (*RightsWindow, error) {
	var w RightsWindow
	query := "SELECT window_id, contract_id, content_id, content_type, region, start_time, end_time, state, ctime, mtime FROM rights_window WHERE content_id = ? AND content_type = ? AND region = ? AND state = ? ORDER BY end_time DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &w, query, contentID, contentType, region, WindowStateActive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rights_window FindActiveByContentRegion: %w", err)
	}
	return &w, nil
}

func (m *defaultRightsWindowModel) UpdateState(ctx context.Context, windowID int64, state int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE rights_window SET state = ?, mtime = ? WHERE window_id = ?",
		state, nowUnix(), windowID)
	if err != nil {
		return 0, fmt.Errorf("rights_window UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rights_window UpdateState RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultRightsWindowModel) ListExpiring(ctx context.Context, withinSeconds int32, pn, ps int32) ([]*RightsWindow, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 100 {
		ps = 50
	}
	offset := (pn - 1) * ps

	threshold := nowUnix() + int64(withinSeconds)
	where := "WHERE state = ? AND end_time <= ?"
	args := []interface{}{WindowStateActive, threshold}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM rights_window "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("rights_window ListExpiring count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	args = append(args, ps, offset)
	var rows []*RightsWindow
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT window_id, contract_id, content_id, content_type, region, start_time, end_time, state, ctime, mtime FROM rights_window "+where+" ORDER BY end_time ASC LIMIT ? OFFSET ?",
		args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("rights_window ListExpiring list: %w", err)
	}
	return rows, total, nil
}
