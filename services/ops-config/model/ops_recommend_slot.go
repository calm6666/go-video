package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// slotColumns 是 ops_recommend_slot 的列清单，必须与
// deploy/migrations/ops-config/000003_create_ops_recommend_slot_tables.sql 一致。
const slotColumns = "slot_id, code, page, title, platforms, capacity, state," +
	" version, operator_id, remark, ctime, mtime"

// slotCodeRe 是坑位编码格式：小写字母/数字开头，可含下划线与点，长度 2..64。
// code 是客户端与服务端约定的**寻址标识**（如 home.banner、detail.below_player），
// 一旦下发就不能随意改名，因此格式必须固化，禁止空格与大小写混用。
var slotCodeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.]{1,63}$`)

// ValidSlotCode 判定坑位编码格式。
func ValidSlotCode(s string) bool { return slotCodeRe.MatchString(s) }

// DefaultSlotCapacity / MaxSlotCapacityHard 是坑位数量的服务端默认与硬上限。
// capacity 必须有上限：它同时是 ResolveSlot 的回参规模上界，
// 没有上限就等于给网关一个无界结果集（ErrSlotCapacityInvalid 的成因）。
const (
	DefaultSlotCapacity = 20
	MaxSlotCapacityHard = 200
)

// RecommendSlot 对应 ops_recommend_slot 表：一个坑位（推荐位）的定义。
//
// 本表只承载**内容分发**：坑位里挂的是稿件、作品、季/集或专题入口。
// 不存在广告主、出价、排期购买、投放计费、分成等任何字段（AGENTS.md §1）。
// 「page」也只是归属标识，服务端不据此下发布局指令（AGENTS.md §6）。
type RecommendSlot struct {
	// SlotID 自增主键。
	SlotID int64 `db:"slot_id"`
	// Code 坑位编码（uniq_code），端上按此寻址。
	Code string `db:"code"`
	// Page 归属页面标识（服务端不解释布局，只做归组与筛选）。
	Page string `db:"page"`
	// Title 坑位名，后台展示用。
	Title string `db:"title"`
	// Platforms 生效端列表，存储形态 ",1,2,3,4,"，空串表示不限端（见 platform.go）。
	Platforms string `db:"platforms"`
	// Capacity 坑位数量上限：ResolveSlot 未显式给 limit 时按它截断。
	Capacity int32 `db:"capacity"`
	// State 1 启用、2 停用。停用坑位在 ResolveSlot 里按「未命中」处理。
	State int32 `db:"state"`
	// Version 乐观锁版本，SaveSlotReq.expect_version 比对的就是它。
	Version int64 `db:"version"`
	// OperatorID 最后操作人 admin_id。
	OperatorID int64 `db:"operator_id"`
	// Remark 备注：这个坑位给谁用、什么时候撤。
	Remark string `db:"remark"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// PlatformList 供契约投影（rpc.RecommendSlot.platforms）。
func (s *RecommendSlot) PlatformList() []int32 {
	out, err := ValidatePlatformList(s.Platforms, maxPlatformCount)
	if err != nil {
		return nil
	}
	return out
}

// VisibleTo 判定坑位对某端是否可见（不限端视为全端可见）。
// 与灰度规则的 platforms 判定区别：坑位问的是「这块位置给不给这一端看」，
// 规则问的是「这份配置放量给不给这一端」。
func (s *RecommendSlot) VisibleTo(platform int32) bool {
	return PlatformMatchesAny(s.Platforms, platform)
}

// SlotFilter 后台列表条件（ListSlots）。
type SlotFilter struct {
	Page     string
	State    int32
	Platform int32
	Pn       int32
	Ps       int32
}

// RecommendSlotModel 抽象 ops_recommend_slot 表。
type RecommendSlotModel interface {
	// Insert 新建坑位；code 冲突返回 ErrSlotCodeConflict。
	Insert(ctx context.Context, slot *RecommendSlot) (int64, error)
	// FindByID 按主键查询；不存在返回 (nil, nil)。
	FindByID(ctx context.Context, slotID int64) (*RecommendSlot, error)
	// FindByCode 按编码查询（ResolveSlot 的入口）；不存在返回 (nil, nil)。
	FindByCode(ctx context.Context, code string) (*RecommendSlot, error)
	// UpdateWithVersion 乐观锁更新，仅当库里 version == expectVersion 时生效并 version+1。
	UpdateWithVersion(ctx context.Context, slot *RecommendSlot, expectVersion, ts int64) (bool, error)
	// List 分页查询（带 COUNT），固定 code ASC，必须 LIMIT。
	List(ctx context.Context, f SlotFilter) ([]*RecommendSlot, int64, error)
	// ListEnabled 运行时取启用坑位（缓存重建/后台下拉用），必须 LIMIT。
	ListEnabled(ctx context.Context, limit int) ([]*RecommendSlot, error)
}

type defaultRecommendSlotModel struct {
	conn sqlx.SqlConn
}

// NewRecommendSlotModel 构造 ops_recommend_slot 的 sqlx 实现。
func NewRecommendSlotModel(conn sqlx.SqlConn) RecommendSlotModel {
	return &defaultRecommendSlotModel{conn: conn}
}

// ValidSlotCapacity 判定坑位容量。上限是硬编码的 MaxSlotCapacityHard，
// 配置 OpsSlot.MaxCapacity 只能收紧（config_load_test 有对应用例）。
func ValidSlotCapacity(capacity int32) bool {
	return capacity >= 1 && capacity <= int32(MaxSlotCapacityHard)
}

func normalizeSlotForWrite(slot *RecommendSlot) error {
	if !ValidSlotCode(slot.Code) {
		return ErrSlotCodeInvalid
	}
	if !ValidSlotCapacity(slot.Capacity) {
		return ErrSlotCapacityInvalid
	}
	if _, err := ValidatePlatformList(slot.Platforms, maxPlatformCount); err != nil {
		return err
	}
	if slot.Ctime == 0 {
		slot.Ctime = nowUnix()
	}
	slot.Mtime = slot.Ctime
	if slot.State == 0 {
		// 默认停用：新建坑位就对外可见会把一个还没排期的空位置推给端上。
		slot.State = StateOff
	}
	if slot.Version == 0 {
		slot.Version = 1
	}
	return nil
}

func (m *defaultRecommendSlotModel) Insert(ctx context.Context, slot *RecommendSlot) (int64, error) {
	if slot.Code == "" {
		return 0, ErrSlotCodeRequired
	}
	if err := normalizeSlotForWrite(slot); err != nil {
		return 0, err
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO ops_recommend_slot ("+slotColumns+") VALUES ("+placeholders(12)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		slot.SlotID, slot.Code, slot.Page, slot.Title, slot.Platforms, slot.Capacity,
		slot.State, slot.Version, slot.OperatorID, slot.Remark, slot.Ctime, slot.Mtime)
	if err != nil {
		return 0, fmt.Errorf("ops_recommend_slot Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_recommend_slot Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrSlotCodeConflict
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ops_recommend_slot Insert LastInsertId: %w", err)
	}
	slot.SlotID = id
	return id, nil
}

const slotSelect = "SELECT " + slotColumns + " FROM ops_recommend_slot"

func (m *defaultRecommendSlotModel) FindByID(ctx context.Context, slotID int64) (*RecommendSlot, error) {
	var row RecommendSlot
	query := slotSelect + " WHERE slot_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, slotID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultRecommendSlotModel) FindByCode(ctx context.Context, code string) (*RecommendSlot, error) {
	if code == "" {
		return nil, ErrSlotCodeRequired
	}
	var row RecommendSlot
	query := slotSelect + " WHERE code = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot FindByCode: %w", err)
	}
	return &row, nil
}

func (m *defaultRecommendSlotModel) UpdateWithVersion(ctx context.Context, slot *RecommendSlot, expectVersion, ts int64) (bool, error) {
	if slot.SlotID <= 0 {
		return false, ErrSlotNotFound
	}
	if err := normalizeSlotForWrite(slot); err != nil {
		return false, err
	}
	// code 可改，先探测是否占用，好把「编码冲突」和「版本冲突」区分开。
	var holder int64
	err := m.conn.QueryRowCtx(ctx, &holder,
		"SELECT slot_id FROM ops_recommend_slot WHERE code = ? AND slot_id <> ? LIMIT 1",
		slot.Code, slot.SlotID)
	if err != nil {
		// ErrNoRows = 没有别的坑位占着这个 code，可以继续更新（方向别搞反）。
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("ops_recommend_slot UpdateWithVersion code probe: %w", err)
		}
	} else if holder > 0 {
		return false, ErrSlotCodeConflict
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE ops_recommend_slot SET code = ?, page = ?, title = ?, platforms = ?, capacity = ?,"+
			" state = ?, version = version + 1, operator_id = ?, remark = ?, mtime = ?"+
			" WHERE slot_id = ? AND version = ?",
		slot.Code, slot.Page, slot.Title, slot.Platforms, slot.Capacity,
		slot.State, slot.OperatorID, slot.Remark, ts, slot.SlotID, expectVersion)
	if err != nil {
		return false, fmt.Errorf("ops_recommend_slot UpdateWithVersion: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_recommend_slot UpdateWithVersion RowsAffected: %w", err)
	}
	if aff == 0 {
		return false, nil
	}
	slot.Version = expectVersion + 1
	slot.Mtime = ts
	return true, nil
}

func (m *defaultRecommendSlotModel) List(ctx context.Context, f SlotFilter) ([]*RecommendSlot, int64, error) {
	where, args := "WHERE 1 = 1", make([]any, 0, 3)
	if f.Page != "" {
		where += " AND page = ?"
		args = append(args, f.Page)
	}
	if f.State > 0 {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Platform != 0 {
		// 与内存侧 PlatformMatchesAny 同口径：命中「含该端」或「不限端」。
		// 少了「不限端」这一支，后台按端筛选就会漏掉大部分坑位。
		where += " AND (platforms = ? OR platforms LIKE ?)"
		args = append(args, "", PlatformLikeArg(f.Platform))
	}
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ops_recommend_slot "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_recommend_slot List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*RecommendSlot
	query := slotSelect + where + " ORDER BY code ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_recommend_slot List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultRecommendSlotModel) ListEnabled(ctx context.Context, limit int) ([]*RecommendSlot, error) {
	if limit <= 0 {
		limit = DefaultTopicListLimit
	}
	var rows []*RecommendSlot
	query := slotSelect + " WHERE state = ? ORDER BY code ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, StateOn, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot ListEnabled: %w", err)
	}
	return rows, nil
}
