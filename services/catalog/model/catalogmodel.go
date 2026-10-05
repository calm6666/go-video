// Package model 是 catalog 服务的数据库模型与查询代码。
// 拥有 catalog_work / catalog_season / catalog_episode / catalog_zone / catalog_tag
// 五张表的数据访问接口；跨服务不得直连本表（见 AGENTS.md §5）。
package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// nowUnix 返回当前 Unix 秒时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// --- Work 作品 ---

// Work 作品主数据。season_id 既是作品 ID，也指向其主季 ID。
type Work struct {
	SeasonID int64  `db:"season_id"` // 作品主季 ID（主键）
	Title    string `db:"title"`     // 标题
	Cover    string `db:"cover"`     // 封面 URL
	TypeID   int32  `db:"typeid"`    // 作品类型
	Intro    string `db:"intro"`     // 简介
	State    int32  `db:"state"`     // 状态：0 草稿、1 上架、2 下架
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`     // 修改时间（Unix 秒）
}

// WorkModel catalog_work 表查询与写入接口。
type WorkModel interface {
	// Insert 新建作品；返回新 season_id。
	Insert(ctx context.Context, w *Work) (int64, error)
	// FindOne 查询单个作品。
	FindOne(ctx context.Context, seasonID int64) (*Work, error)
	// List 分页查询作品；typeid/state 为 0/-1 时不过滤。
	List(ctx context.Context, typeid, state int32, pn, ps int32) ([]*Work, int32, error)
	// UpdateState 更新作品状态。
	UpdateState(ctx context.Context, seasonID int64, state int32) error
}

type defaultWorkModel struct{ conn sqlx.SqlConn }

// NewWorkModel 创建 WorkModel 实现。
func NewWorkModel(conn sqlx.SqlConn) WorkModel {
	return &defaultWorkModel{conn: conn}
}

func (m *defaultWorkModel) Insert(ctx context.Context, w *Work) (int64, error) {
	now := nowUnix()
	w.Ctime = now
	w.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO catalog_work (title, cover, typeid, intro, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)",
		w.Title, w.Cover, w.TypeID, w.Intro, w.State, w.Ctime, w.Mtime)
	if err != nil {
		return 0, fmt.Errorf("catalog_work Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog_work Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultWorkModel) FindOne(ctx context.Context, seasonID int64) (*Work, error) {
	var w Work
	err := m.conn.QueryRowCtx(ctx, &w,
		"SELECT season_id, title, cover, typeid, intro, state, ctime, mtime FROM catalog_work WHERE season_id = ?",
		seasonID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_work FindOne: %w", err)
	}
	return &w, nil
}

func (m *defaultWorkModel) List(ctx context.Context, typeid, state int32, pn, ps int32) ([]*Work, int32, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps

	where := "WHERE 1=1"
	args := []interface{}{}
	if typeid > 0 {
		where += " AND typeid = ?"
		args = append(args, typeid)
	}
	if state >= 0 {
		where += " AND state = ?"
		args = append(args, state)
	}

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total,
		"SELECT COUNT(*) FROM catalog_work "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("catalog_work List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(args, ps, offset)
	var rows []*Work
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT season_id, title, cover, typeid, intro, state, ctime, mtime FROM catalog_work "+where+" ORDER BY season_id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("catalog_work List rows: %w", err)
	}
	return rows, total, nil
}

func (m *defaultWorkModel) UpdateState(ctx context.Context, seasonID int64, state int32) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE catalog_work SET state = ?, mtime = ? WHERE season_id = ?",
		state, nowUnix(), seasonID)
	if err != nil {
		return fmt.Errorf("catalog_work UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("catalog_work UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrWorkNotFound
	}
	return nil
}

// --- Season 季 ---

// Season 季主数据。season_id 是本季自身 ID；work_id 指向所属作品主季 ID。
type Season struct {
	SeasonID int64  `db:"season_id"` // 本季 ID（主键）
	WorkID   int64  `db:"work_id"`   // 所属作品主季 ID（FK → catalog_work.season_id）
	SeasonNo int32  `db:"season_no"` // 季编号
	Title    string `db:"title"`     // 季标题
	Cover    string `db:"cover"`     // 季封面
	State    int32  `db:"state"`     // 状态：0 草稿、1 上架、2 下架
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`     // 修改时间（Unix 秒）
}

// SeasonModel catalog_season 表查询与写入接口。
type SeasonModel interface {
	// Insert 新建季；返回新 season_id。
	Insert(ctx context.Context, s *Season) (int64, error)
	// FindOne 查询单个季。
	FindOne(ctx context.Context, seasonID int64) (*Season, error)
	// ListByWork 查询某作品的全部季（按 season_no 升序）。
	ListByWork(ctx context.Context, workID int64) ([]*Season, error)
}

type defaultSeasonModel struct{ conn sqlx.SqlConn }

// NewSeasonModel 创建 SeasonModel 实现。
func NewSeasonModel(conn sqlx.SqlConn) SeasonModel {
	return &defaultSeasonModel{conn: conn}
}

func (m *defaultSeasonModel) Insert(ctx context.Context, s *Season) (int64, error) {
	now := nowUnix()
	s.Ctime = now
	s.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO catalog_season (work_id, season_no, title, cover, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.WorkID, s.SeasonNo, s.Title, s.Cover, s.State, s.Ctime, s.Mtime)
	if err != nil {
		return 0, fmt.Errorf("catalog_season Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog_season Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultSeasonModel) FindOne(ctx context.Context, seasonID int64) (*Season, error) {
	var s Season
	err := m.conn.QueryRowCtx(ctx, &s,
		"SELECT season_id, work_id, season_no, title, cover, state, ctime, mtime FROM catalog_season WHERE season_id = ?",
		seasonID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_season FindOne: %w", err)
	}
	return &s, nil
}

func (m *defaultSeasonModel) ListByWork(ctx context.Context, workID int64) ([]*Season, error) {
	var rows []*Season
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT season_id, work_id, season_no, title, cover, state, ctime, mtime FROM catalog_season WHERE work_id = ? ORDER BY season_no ASC",
		workID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_season ListByWork: %w", err)
	}
	return rows, nil
}

// --- Episode 集 ---

// Episode 集主数据。epid 是集主键；season_id 指向所属季 ID。
type Episode struct {
	Epid     int64  `db:"epid"`      // 集 ID（主键）
	SeasonID int64  `db:"season_id"` // 所属季 ID（FK → catalog_season.season_id）
	EpNo     int32  `db:"ep_no"`     // 集编号
	Title    string `db:"title"`     // 集标题
	AssetID  int64  `db:"asset_id"`  // 关联媒资 ID（由 asset 服务拥有）
	Duration int64  `db:"duration"`  // 时长（秒）
	State    int32  `db:"state"`     // 状态：0 草稿、1 上架、2 下架
	Ctime    int64  `db:"ctime"`     // 创建时间（Unix 秒）
	Mtime    int64  `db:"mtime"`     // 修改时间（Unix 秒）
}

// EpisodeModel catalog_episode 表查询与写入接口。
type EpisodeModel interface {
	// Insert 新建集；返回新 epid。
	Insert(ctx context.Context, e *Episode) (int64, error)
	// FindOne 查询单个集。
	FindOne(ctx context.Context, epid int64) (*Episode, error)
	// ListBySeason 查询某季的全部集（按 ep_no 升序）。
	ListBySeason(ctx context.Context, seasonID int64) ([]*Episode, error)
	// UpdateState 更新集状态（用于上架/下架流转）。
	UpdateState(ctx context.Context, epid int64, state int32) error
}

type defaultEpisodeModel struct{ conn sqlx.SqlConn }

// NewEpisodeModel 创建 EpisodeModel 实现。
func NewEpisodeModel(conn sqlx.SqlConn) EpisodeModel {
	return &defaultEpisodeModel{conn: conn}
}

func (m *defaultEpisodeModel) Insert(ctx context.Context, e *Episode) (int64, error) {
	now := nowUnix()
	e.Ctime = now
	e.Mtime = now
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO catalog_episode (season_id, ep_no, title, asset_id, duration, state, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		e.SeasonID, e.EpNo, e.Title, e.AssetID, e.Duration, e.State, e.Ctime, e.Mtime)
	if err != nil {
		return 0, fmt.Errorf("catalog_episode Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog_episode Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultEpisodeModel) FindOne(ctx context.Context, epid int64) (*Episode, error) {
	var e Episode
	err := m.conn.QueryRowCtx(ctx, &e,
		"SELECT epid, season_id, ep_no, title, asset_id, duration, state, ctime, mtime FROM catalog_episode WHERE epid = ?",
		epid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_episode FindOne: %w", err)
	}
	return &e, nil
}

func (m *defaultEpisodeModel) ListBySeason(ctx context.Context, seasonID int64) ([]*Episode, error) {
	var rows []*Episode
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT epid, season_id, ep_no, title, asset_id, duration, state, ctime, mtime FROM catalog_episode WHERE season_id = ? ORDER BY ep_no ASC",
		seasonID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_episode ListBySeason: %w", err)
	}
	return rows, nil
}

func (m *defaultEpisodeModel) UpdateState(ctx context.Context, epid int64, state int32) error {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE catalog_episode SET state = ?, mtime = ? WHERE epid = ?",
		state, nowUnix(), epid)
	if err != nil {
		return fmt.Errorf("catalog_episode UpdateState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("catalog_episode UpdateState RowsAffected: %w", err)
	}
	if aff == 0 {
		return ErrEpisodeNotFound
	}
	return nil
}

// --- Zone 分区 ---

// Zone 分区。parent=0 表示顶级分区。
type Zone struct {
	ZoneID int32  `db:"zoneid"` // 分区 ID
	Name   string `db:"name"`   // 分区名
	Parent int32  `db:"parent"` // 父分区 ID（0 为顶级）
}

// ZoneModel catalog_zone 表查询接口。分区由运营维护，本期不提供写入 RPC。
type ZoneModel interface {
	// ListAll 查询全部分区（扁平，由调用方组装树）。
	ListAll(ctx context.Context) ([]*Zone, error)
}

type defaultZoneModel struct{ conn sqlx.SqlConn }

// NewZoneModel 创建 ZoneModel 实现。
func NewZoneModel(conn sqlx.SqlConn) ZoneModel {
	return &defaultZoneModel{conn: conn}
}

func (m *defaultZoneModel) ListAll(ctx context.Context) ([]*Zone, error) {
	var rows []*Zone
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT zoneid, name, parent FROM catalog_zone ORDER BY zoneid ASC")
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_zone ListAll: %w", err)
	}
	return rows, nil
}

// --- Tag 标签 ---

// Tag 标签。
type Tag struct {
	TagID int64  `db:"tagid"` // 标签 ID
	Name  string `db:"name"`  // 标签名
}

// TagModel catalog_tag 表查询接口。
type TagModel interface {
	// FindByIDs 按 ID 列表查询标签。
	FindByIDs(ctx context.Context, ids []int64) ([]*Tag, error)
	// SearchByName 按名字模糊查询标签（分页）。
	SearchByName(ctx context.Context, name string, pn, ps int32) ([]*Tag, error)
}

type defaultTagModel struct{ conn sqlx.SqlConn }

// NewTagModel 创建 TagModel 实现。
func NewTagModel(conn sqlx.SqlConn) TagModel {
	return &defaultTagModel{conn: conn}
}

func (m *defaultTagModel) FindByIDs(ctx context.Context, ids []int64) ([]*Tag, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []*Tag
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT tagid, name FROM catalog_tag WHERE tagid IN (?)", ids); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_tag FindByIDs: %w", err)
	}
	return rows, nil
}

func (m *defaultTagModel) SearchByName(ctx context.Context, name string, pn, ps int32) ([]*Tag, error) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 || ps > 50 {
		ps = 20
	}
	offset := (pn - 1) * ps
	var rows []*Tag
	err := m.conn.QueryRowsCtx(ctx, &rows,
		"SELECT tagid, name FROM catalog_tag WHERE name LIKE ? ORDER BY tagid ASC LIMIT ? OFFSET ?",
		"%"+name+"%", ps, offset)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("catalog_tag SearchByName: %w", err)
	}
	return rows, nil
}
