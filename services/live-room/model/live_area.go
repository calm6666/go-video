package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// liveAreaColumns 与 000006_create_live_area.sql 逐列对应。
const liveAreaColumns = "area_id, area_name, parent_area_id, sort, state, operator_mid, ctime, mtime"

// 分区启用位与层级上限（两级：一级分区 + 二级分区，与客户端分区页一致）。
const (
	// AreaStateDisabled 停用：不再出现在客户端分区列表，也不能被新房间选用。
	AreaStateDisabled int32 = 0
	// AreaStateEnabled 启用。
	AreaStateEnabled int32 = 1
	// AreaMaxLevel 分区最大层级：1 级父 + 2 级子，禁止三级嵌套（客户端渲染与选区路径都按两级设计）。
	AreaMaxLevel = 2
	// AreaNameMaxRunes 分区名 rune 上限，与列宽 VARCHAR(32) 对应。
	AreaNameMaxRunes = 32
)

// LiveArea 直播分区行（live_area 表投影，对应 rpc.AreaInfo）。
// 分区是运营侧配置数据：只有本服务写；房间表只存 area_id，
// 分区改名不回写历史房间（房间表 title/area 快照在 live_session 里留存）。
type LiveArea struct {
	AreaID       int64  `db:"area_id"`        // 分区 ID（主键）
	AreaName     string `db:"area_name"`      // 分区名（uniq_area_name 全局唯一）
	ParentAreaID int64  `db:"parent_area_id"` // 上级分区 ID，0 表示一级分区
	Sort         int32  `db:"sort"`           // 排序权重（越小越前）
	State        int32  `db:"state"`          // 1 启用、0 停用
	OperatorMid  int64  `db:"operator_mid"`   // 最近操作运营 ID
	Ctime        int64  `db:"ctime"`          // 创建时间（Unix 秒）
	Mtime        int64  `db:"mtime"`          // 修改时间（Unix 秒）
}

// AreaListQuery 是 ListAreas 的过滤条件。
// ParentAreaID/State 用 -1 表达「不过滤」——因为 0 是合法取值（一级分区 / 停用）。
type AreaListQuery struct {
	ParentAreaID int64
	State        int32
	Offset       int32
	Limit        int32
}

// LiveAreaModel live_area 表读写接口。
type LiveAreaModel interface {
	// Insert 新建分区并返回 area_id；分区名重复返回 ErrAreaNameConflict。
	Insert(ctx context.Context, a *LiveArea) (int64, error)
	// Update 按 area_id 修改分区：空名/0 表示该列不改，state 与 sort 总是要写。
	// 返回 false 表示 area_id 不存在。名字冲突返回 ErrAreaNameConflict。
	Update(ctx context.Context, a *LiveArea) (bool, error)
	// FindOne 按 area_id 读分区；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, areaID int64) (*LiveArea, error)
	// FindByName 按分区名读分区（唯一键冲突前的预检，也供运营搜索）；不存在返回 (nil, nil)。
	FindByName(ctx context.Context, name string) (*LiveArea, error)
	// List 分页读分区，parent_area_id、sort 升序（分区页渲染顺序）。
	List(ctx context.Context, q AreaListQuery) ([]*LiveArea, error)
	// Count 返回 List 同条件下的总数。
	Count(ctx context.Context, q AreaListQuery) (int64, error)
	// IsUsable 判断分区是否可被房间选用（存在且启用）。
	IsUsable(ctx context.Context, areaID int64) (bool, error)
	// LevelOf 返回分区的层级（1 级分区返回 1，其子返回 2），
	// 并顺带给出父行；分区不存在时返回 (0, nil, ErrAreaNotFound)。
	LevelOf(ctx context.Context, areaID int64) (int, *LiveArea, error)
	// CountChildren 统计直接子分区数（停用父分区前的占用检查）。
	CountChildren(ctx context.Context, parentAreaID int64) (int64, error)
}

type defaultLiveAreaModel struct {
	conn sqlx.SqlConn
}

// NewLiveAreaModel 创建 LiveAreaModel 实现。
func NewLiveAreaModel(conn sqlx.SqlConn) LiveAreaModel {
	return &defaultLiveAreaModel{conn: conn}
}

func (m *defaultLiveAreaModel) Insert(ctx context.Context, a *LiveArea) (int64, error) {
	if strings.TrimSpace(a.AreaName) == "" || runeLen(a.AreaName) > AreaNameMaxRunes {
		return 0, ErrAreaNameInvalid
	}
	if a.OperatorMid <= 0 {
		return 0, ErrOperatorRequired
	}
	if a.State != AreaStateEnabled && a.State != AreaStateDisabled {
		return 0, ErrAreaStateInvalid
	}
	const query = "INSERT INTO live_area (area_name, parent_area_id, sort, state, operator_mid, ctime, mtime) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	if a.Ctime == 0 {
		a.Ctime = now
	}
	a.Mtime = now
	res, err := m.conn.ExecCtx(ctx, query, a.AreaName, a.ParentAreaID, a.Sort, a.State, a.OperatorMid, a.Ctime, a.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, ErrAreaNameConflict
		}
		return 0, fmt.Errorf("live_area Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_area Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultLiveAreaModel) Update(ctx context.Context, a *LiveArea) (bool, error) {
	if a.AreaID <= 0 {
		return false, ErrInvalidAreaID
	}
	if a.OperatorMid <= 0 {
		return false, ErrOperatorRequired
	}
	if a.AreaName != "" && (strings.TrimSpace(a.AreaName) == "" || runeLen(a.AreaName) > AreaNameMaxRunes) {
		return false, ErrAreaNameInvalid
	}
	if a.State != AreaStateEnabled && a.State != AreaStateDisabled {
		return false, ErrAreaStateInvalid
	}
	set := []string{"state = ?", "sort = ?", "operator_mid = ?", "mtime = ?"}
	args := []interface{}{a.State, a.Sort, a.OperatorMid, nowUnix()}
	if a.AreaName != "" {
		set = append(set, "area_name = ?")
		args = append(args, a.AreaName)
	}
	if a.ParentAreaID >= 0 {
		set = append(set, "parent_area_id = ?")
		args = append(args, a.ParentAreaID)
	}
	query := "UPDATE live_area SET " + strings.Join(set, ", ") + " WHERE area_id = ?"
	args = append(args, a.AreaID)

	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		if isDuplicateErr(err) {
			return false, ErrAreaNameConflict
		}
		return false, fmt.Errorf("live_area Update: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("live_area Update RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultLiveAreaModel) FindOne(ctx context.Context, areaID int64) (*LiveArea, error) {
	var a LiveArea
	query := "SELECT " + liveAreaColumns + " FROM live_area WHERE area_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, areaID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_area FindOne: %w", err)
	}
	return &a, nil
}

func (m *defaultLiveAreaModel) FindByName(ctx context.Context, name string) (*LiveArea, error) {
	if name == "" {
		return nil, ErrAreaNameInvalid
	}
	var a LiveArea
	query := "SELECT " + liveAreaColumns + " FROM live_area WHERE area_name = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &a, query, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_area FindByName: %w", err)
	}
	return &a, nil
}

func (m *defaultLiveAreaModel) List(ctx context.Context, q AreaListQuery) ([]*LiveArea, error) {
	if q.Limit <= 0 {
		q.Limit = defaultListLimit
	}
	where, args := areaWhere(q)
	query := "SELECT " + liveAreaColumns + " FROM live_area WHERE " + where +
		" ORDER BY parent_area_id ASC, sort ASC, area_id ASC LIMIT ? OFFSET ?"
	args = append(args, q.Limit, q.Offset)

	var rows []*LiveArea
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_area List: %w", err)
	}
	return rows, nil
}

func (m *defaultLiveAreaModel) Count(ctx context.Context, q AreaListQuery) (int64, error) {
	where, args := areaWhere(q)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_area WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_area Count: %w", err)
	}
	return total, nil
}

func (m *defaultLiveAreaModel) IsUsable(ctx context.Context, areaID int64) (bool, error) {
	if areaID <= 0 {
		return false, ErrInvalidAreaID
	}
	var hit int64
	err := m.conn.QueryRowCtx(ctx, &hit,
		"SELECT COUNT(*) FROM (SELECT area_id FROM live_area WHERE area_id = ? AND state = ? LIMIT 1) AS t",
		areaID, AreaStateEnabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("live_area IsUsable: %w", err)
	}
	return hit > 0, nil
}

func (m *defaultLiveAreaModel) LevelOf(ctx context.Context, areaID int64) (int, *LiveArea, error) {
	area, err := m.FindOne(ctx, areaID)
	if err != nil {
		return 0, nil, err
	}
	if area == nil {
		return 0, nil, ErrAreaNotFound
	}
	if area.ParentAreaID == 0 {
		return 1, area, nil
	}
	parent, err := m.FindOne(ctx, area.ParentAreaID)
	if err != nil {
		return 0, nil, err
	}
	if parent == nil {
		// 父分区被删（本服务不提供删除，只停用）：视为脏引用，调用方按 ErrAreaParentInvalid 拒绝。
		return 0, nil, ErrAreaParentInvalid
	}
	if parent.ParentAreaID != 0 {
		// 三级嵌套：分区树只设计到两级，直接拒绝（level 归 0，由调用方按错误分支处理）。
		return 0, parent, ErrAreaParentInvalid
	}
	return 2, parent, nil
}

func (m *defaultLiveAreaModel) CountChildren(ctx context.Context, parentAreaID int64) (int64, error) {
	if parentAreaID <= 0 {
		return 0, ErrInvalidAreaID
	}
	var cnt int64
	if err := m.conn.QueryRowCtx(ctx, &cnt,
		"SELECT COUNT(*) FROM live_area WHERE parent_area_id = ?", parentAreaID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_area CountChildren: %w", err)
	}
	return cnt, nil
}

// areaWhere 构造 List/Count 共用的 WHERE 片段。
// -1 表示该维度不过滤：0 对 parent_area_id（一级分区）和 state（停用）都是合法值。
func areaWhere(q AreaListQuery) (string, []interface{}) {
	var (
		sb   strings.Builder
		args []interface{}
	)
	sb.WriteString("area_id > 0")
	if q.ParentAreaID >= 0 {
		sb.WriteString(" AND parent_area_id = ?")
		args = append(args, q.ParentAreaID)
	}
	if q.State >= 0 {
		sb.WriteString(" AND state = ?")
		args = append(args, q.State)
	}
	return sb.String(), args
}

// runeLen 按 rune 计长度：分区名/标题的长度上限都必须是字符数而不是字节数，
// 否则中文标题会被按 3 倍截断（AGENTS.md §6 多端一致）。
func runeLen(s string) int { return len([]rune(s)) }
