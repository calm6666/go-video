package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// slotItemColumns 是 ops_recommend_slot_item 的列清单，必须与
// deploy/migrations/ops-config/000003_create_ops_recommend_slot_tables.sql 一致。
const slotItemColumns = "id, slot_id, position, item_type, item_id, weight," +
	" start_at, end_at, state, operator_id, ctime, mtime"

// MaxSlotItems 单个坑位的条目硬上限（契约 SaveSlotItemsReq 注明最多 200 条）。
// 坑位是给一屏渲染用的，超过这个量级说明有人在拿坑位当列表接口用。
const MaxSlotItems = 200

// SlotItem 对应 ops_recommend_slot_item 表：坑位里一个带排期的内容引用。
//
// 与 ops_topic_item 的差别只有两点：允许挂 topic（坑位可以放专题入口），
// 以及带 position + 排期窗口。同样**不复制**任何可变主资料（AGENTS.md §5）。
// weight 只是同 position 冲突时的次序依据：本服务不做推荐排序，
// 排序属于 recommend-rank（见 README「已知缺口」）。
type SlotItem struct {
	// ID 自增主键。
	ID int64 `db:"id"`
	// SlotID 所属坑位（ops_recommend_slot.slot_id）。
	SlotID int64 `db:"slot_id"`
	// Position 坑位内位置，必须落在 1..capacity（SaveSlotItemsReq 约束）。
	Position int32 `db:"position"`
	// ItemType ugc_video / pgc_season / pgc_episode / topic。
	ItemType string `db:"item_type"`
	// ItemID 被引用内容的主键字符串形式。
	ItemID string `db:"item_id"`
	// Weight 同位置并列时的次序，大者在前。
	Weight int32 `db:"weight"`
	// StartAt 排期生效起（Unix 秒），0 表示立即。
	StartAt int64 `db:"start_at"`
	// EndAt 排期生效止（Unix 秒），0 表示不设截止。
	EndAt int64 `db:"end_at"`
	// State 1 生效、2 停用（停用不删行，保留排期证据）。
	State int32 `db:"state"`
	// OperatorID 最后操作人 admin_id。
	OperatorID int64 `db:"operator_id"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// InWindow 判定排期在 ts 时刻是否生效（与 Topic/Rule 的时间窗口语义一致）。
func (s *SlotItem) InWindow(ts int64) bool {
	if s.StartAt > 0 && ts < s.StartAt {
		return false
	}
	if s.EndAt > 0 && ts >= s.EndAt {
		return false
	}
	return true
}

// SlotItemModel 抽象 ops_recommend_slot_item 表。
type SlotItemModel interface {
	// ReplaceAll 在一个事务里全量覆盖某坑位的条目与排期。
	// 校验：条目数 <= min(capacity, maxItems)、position 不重复且不越界、
	// 同一坑位内不重复挂同一内容、item_type 允许 topic。
	// 排期窗口非法（end_at<=start_at）同样拒绝：那是一条永远不生效的静默配置。
	ReplaceAll(ctx context.Context, slotID int64, capacity int32, items []*SlotItem, operatorID, ts int64, maxItems int) (int64, error)
	// ListEffective 运行时视图：state=ON 且排期覆盖 at，按 position ASC, weight DESC 取回，
	// limit<=0 时按 capacity 截断（ResolveSlotReq.limit 的语义）。
	ListEffective(ctx context.Context, slotID, at int64, limit int) ([]*SlotItem, error)
	// ListAll 后台视图：含停用与过期排期（排障要看「为什么没出」，必须能看到被过滤的行）。
	ListAll(ctx context.Context, slotID int64, limit int) ([]*SlotItem, error)
	// CountBySlot 条目数（含停用），后台展示与容量校验用。
	CountBySlot(ctx context.Context, slotID int64, state int32) (int64, error)
	// FindByItemRef 反查引用了某内容的坑位（内容下架时定位受影响的运营位）。
	FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*SlotItem, error)
}

type defaultSlotItemModel struct {
	conn sqlx.SqlConn
}

// NewSlotItemModel 构造 ops_recommend_slot_item 的 sqlx 实现。
func NewSlotItemModel(conn sqlx.SqlConn) SlotItemModel {
	return &defaultSlotItemModel{conn: conn}
}

// ValidateSlotItems 是 ReplaceAll 的全部前置校验（纯函数，理由见 ValidateTopicItems 注释）。
//
// 与专题条目的三处差异都在这里生效：position 允许有空洞但必须在 1..capacity 内且不重复、
// 同一坑位不重复挂同一内容、item_type 允许 topic（坑位挂专题是正常编排）。
func ValidateSlotItems(slotID int64, capacity int32, items []*SlotItem, maxItems int) error {
	if slotID <= 0 {
		return ErrSlotNotFound
	}
	if len(items) == 0 {
		// 同专题：空数组一律报错，不当成「清空坑位」。
		// 清空首页 banner 这种事必须由调用方显式表达（走停用坑位），不能靠传空。
		return ErrBatchEmpty
	}
	if !ValidSlotCapacity(capacity) {
		return ErrSlotCapacityInvalid
	}
	limit := MaxSlotItems
	if maxItems > 0 && maxItems < limit {
		limit = maxItems
	}
	if int(capacity) < limit {
		limit = int(capacity)
	}
	if len(items) > limit {
		return ErrSlotItemLimit
	}
	seenPos := make(map[int32]struct{}, len(items))
	seenRef := make(map[string]struct{}, len(items))
	for _, it := range items {
		if it == nil {
			return ErrItemRefRequired
		}
		if it.Position < 1 || it.Position > capacity {
			return ErrSlotPositionOutOfRange
		}
		if _, ok := seenPos[it.Position]; ok {
			return ErrSlotPositionDuplicated
		}
		seenPos[it.Position] = struct{}{}
		if it.ItemType == "" || it.ItemID == "" {
			return ErrItemRefRequired
		}
		// item_id 列宽 VARCHAR(32)（与专题条目同口径）：超长必须在整批校验阶段拒掉，
		// 而不是等 ReplaceAll 的批量 INSERT 让 MySQL 报 1406。
		if exceedsColumn(it.ItemID, MaxItemIDChars) {
			return ErrItemIDTooLong
		}
		if !ValidItemType(it.ItemType, true) {
			return ErrItemTypeUnsupported
		}
		ref := it.ItemType + ":" + it.ItemID
		if _, ok := seenRef[ref]; ok {
			return ErrSlotItemDuplicated
		}
		seenRef[ref] = struct{}{}
		if it.EndAt > 0 && it.StartAt > 0 && it.EndAt <= it.StartAt {
			return ErrRuleTimeRangeInvalid
		}
	}
	return nil
}

func (m *defaultSlotItemModel) ReplaceAll(ctx context.Context, slotID int64, capacity int32, items []*SlotItem, operatorID, ts int64, maxItems int) (int64, error) {
	if err := ValidateSlotItems(slotID, capacity, items, maxItems); err != nil {
		return 0, err
	}
	var inserted int64
	err := m.conn.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(tctx, "DELETE FROM ops_recommend_slot_item WHERE slot_id = ?", slotID); err != nil {
			return fmt.Errorf("ops_recommend_slot_item ReplaceAll clear: %w", err)
		}
		args := make([]any, 0, len(items)*12)
		groups := make([]string, 0, len(items))
		for _, it := range items {
			groups = append(groups, "("+placeholders(12)+")")
			// ctime 与 mtime 在同一批里取同一时刻，
			// 这样「这次排期是什么时候改的」在整批行上只有一个答案。
			args = append(args, it.ID, slotID, it.Position, it.ItemType, it.ItemID, it.Weight,
				it.StartAt, it.EndAt, orDefaultState(it.State), operatorID, ts, ts)
		}
		query := "INSERT INTO ops_recommend_slot_item (" + slotItemColumns + ") VALUES " +
			strings.Join(groups, ", ")
		res, err := session.ExecCtx(tctx, query, args...)
		if err != nil {
			return fmt.Errorf("ops_recommend_slot_item ReplaceAll insert: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("ops_recommend_slot_item ReplaceAll RowsAffected: %w", err)
		}
		inserted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

const slotItemSelect = "SELECT " + slotItemColumns + " FROM ops_recommend_slot_item"

func (m *defaultSlotItemModel) ListEffective(ctx context.Context, slotID, at int64, limit int) ([]*SlotItem, error) {
	if slotID <= 0 {
		return nil, ErrSlotNotFound
	}
	if limit <= 0 {
		limit = DefaultSlotCapacity
	}
	var rows []*SlotItem
	// 排期窗口下推到 SQL（与 SlotItem.InWindow 同语义），
	// 过期的排期不该被取回内存再判：它们只会占带宽。
	query := slotItemSelect +
		" WHERE slot_id = ? AND state = ?" +
		" AND (start_at = 0 OR start_at <= ?) AND (end_at = 0 OR end_at > ?)" +
		" ORDER BY position ASC, weight DESC, id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, slotID, StateOn, at, at, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot_item ListEffective: %w", err)
	}
	return rows, nil
}

func (m *defaultSlotItemModel) ListAll(ctx context.Context, slotID int64, limit int) ([]*SlotItem, error) {
	if slotID <= 0 {
		return nil, ErrSlotNotFound
	}
	if limit <= 0 {
		limit = MaxSlotItems
	}
	var rows []*SlotItem
	query := slotItemSelect + " WHERE slot_id = ? ORDER BY position ASC, weight DESC, id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, slotID, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot_item ListAll: %w", err)
	}
	return rows, nil
}

func (m *defaultSlotItemModel) CountBySlot(ctx context.Context, slotID int64, state int32) (int64, error) {
	query := "SELECT COUNT(*) FROM ops_recommend_slot_item WHERE slot_id = ?"
	args := []any{slotID}
	if state > 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("ops_recommend_slot_item CountBySlot: %w", err)
	}
	return n, nil
}

func (m *defaultSlotItemModel) FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*SlotItem, error) {
	if itemType == "" || itemID == "" {
		return nil, ErrItemRefRequired
	}
	if limit <= 0 {
		limit = DefaultSlotCapacity
	}
	var rows []*SlotItem
	query := slotItemSelect + " WHERE item_type = ? AND item_id = ? AND state = ?" +
		" ORDER BY slot_id ASC, position ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, itemType, itemID, StateOn, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_recommend_slot_item FindByItemRef: %w", err)
	}
	return rows, nil
}
