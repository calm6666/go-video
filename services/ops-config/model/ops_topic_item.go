package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// topicItemColumns 是 ops_topic_item 的列清单，必须与
// deploy/migrations/ops-config/000002_create_ops_topic_tables.sql 一致。
const topicItemColumns = "id, topic_id, item_type, item_id, position, state," +
	" operator_id, ctime, mtime"

// MaxTopicItems 单个专题的条目硬上限（契约 SaveTopicItemsReq 注明最多 500 条）。
// 配置 OpsTopic.MaxItems 只能等于或小于它：一个专题挂上千条已经不是一个「专题」，
// 而是在拿专题表当导出表用，端上也渲染不完。
const MaxTopicItems = 500

// TopicItem 对应 ops_topic_item 表：专题里的一个内容引用。
//
// 只存 item_type + item_id（引用主键），不复制标题、时长、UP 主、播放数等任何可变资料：
// 稿件改名/下架由 video/catalog/rights 判定，本表跟着复制就会出现
// 「专题里写着 3 分钟、点进去 10 分钟」的双主资料问题（AGENTS.md §5）。
type TopicItem struct {
	// ID 自增主键。
	ID int64 `db:"id"`
	// TopicID 所属专题（ops_topic.topic_id）。
	TopicID int64 `db:"topic_id"`
	// ItemType ugc_video / pgc_season / pgc_episode（专题里不允许 topic，见 ValidItemType）。
	ItemType string `db:"item_type"`
	// ItemID 被引用内容的主键字符串形式（aid / season_id / epid）：
	// 用 VARCHAR 而不是 BIGINT，因为不同域的主键形态不保证都是数字。
	ItemID string `db:"item_id"`
	// Position 专题内排序，从 1 连续递增（小者在前）。
	Position int32 `db:"position"`
	// State 1 生效、2 移除。移除保留行而不是删除，是为了留住「这条专题曾经挂过什么」。
	State int32 `db:"state"`
	// OperatorID 最后操作人 admin_id。
	OperatorID int64 `db:"operator_id"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// TopicItemModel 抽象 ops_topic_item 表。
type TopicItemModel interface {
	// ReplaceAll 在一个事务里全量覆盖某专题的条目：先 DELETE 该 topic_id 的所有行，
	// 再按入参顺序插入。幂等语义来自「全量覆盖」本身——同一份入参重放两次结果相同，
	// 不需要额外去重表（AGENTS.md §5 写接口幂等要求）。
	// 校验：条目数 <= maxItems、position 从 1 连续、item_type 合法（专题内不允许 topic）。
	ReplaceAll(ctx context.Context, topicID int64, items []*TopicItem, operatorID, ts int64, maxItems int) (int64, error)
	// ListByTopic 取专题条目：state=0 表示全部，按 position ASC，必须 LIMIT。
	ListByTopic(ctx context.Context, topicID int64, state int32, limit int) ([]*TopicItem, error)
	// CountByTopic 条目数（SaveTopicItems 前置校验与后台展示用）。
	CountByTopic(ctx context.Context, topicID int64, state int32) (int64, error)
	// FindByItemRef 反查引用了某内容的专题（内容下架时定位受影响的运营位）。
	// limit 必带：一个爆款稿件可能挂在几百个专题里，反查是排障入口而不是导出接口。
	FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*TopicItem, error)
}

type defaultTopicItemModel struct {
	conn sqlx.SqlConn
}

// NewTopicItemModel 构造 ops_topic_item 的 sqlx 实现。
func NewTopicItemModel(conn sqlx.SqlConn) TopicItemModel {
	return &defaultTopicItemModel{conn: conn}
}

// ValidateTopicItems 是 ReplaceAll 的全部前置校验，抽成纯函数的理由与 rollout.go 一致：
// 这些判定原本和 SQL 混在一起，无库环境下无法证明「position 有洞会不会被拒」。
// 写侧（ReplaceAll）与测试因此共用同一份实现，不存在校验口径分叉。
//
// maxItems<=0 时取硬上限 MaxTopicItems；配置只能收紧（logic 传已夹取的值）。
func ValidateTopicItems(topicID int64, items []*TopicItem, maxItems int) error {
	if topicID <= 0 {
		return ErrTopicNotFound
	}
	if len(items) == 0 {
		// 空数组按「清空专题」处理还是报错？报错。
		// 契约里 SaveTopicItems 是全量覆盖语义，一次上游拼接 bug 就会清空线上专题。
		return ErrBatchEmpty
	}
	limit := MaxTopicItems
	if maxItems > 0 && maxItems < limit {
		limit = maxItems
	}
	if len(items) > limit {
		return ErrTopicItemLimit
	}
	for i, it := range items {
		if it == nil {
			return ErrItemRefRequired
		}
		// position 必须从 1 连续：有空洞说明调用方拼接有误，
		// 直接入库会让专题顺序出现无法解释的跳跃。
		if int(it.Position) != i+1 {
			return ErrTopicPositionNotSequential
		}
		if it.ItemType == "" || it.ItemID == "" {
			return ErrItemRefRequired
		}
		// item_id 是 uniq_ref 反查（idx_ref）与网关展开用的引用主键，列宽 VARCHAR(32)。
		if exceedsColumn(it.ItemID, MaxItemIDChars) {
			return ErrItemIDTooLong
		}
		// 专题里挂专题会形成自引用环（A 专题含 B 专题含 A 专题），网关递归展开就死了。
		if !ValidItemType(it.ItemType, false) {
			return ErrItemTypeUnsupported
		}
	}
	return nil
}

func (m *defaultTopicItemModel) ReplaceAll(ctx context.Context, topicID int64, items []*TopicItem, operatorID, ts int64, maxItems int) (int64, error) {
	if err := ValidateTopicItems(topicID, items, maxItems); err != nil {
		return 0, err
	}
	var inserted int64
	err := m.conn.TransactCtx(ctx, func(tctx context.Context, session sqlx.Session) error {
		if _, err := session.ExecCtx(tctx, "DELETE FROM ops_topic_item WHERE topic_id = ?", topicID); err != nil {
			return fmt.Errorf("ops_topic_item ReplaceAll clear: %w", err)
		}
		// 批量插入而不是逐行：一次最多 500 行，逐行会把 500 次网络往返塞进一个事务，
		// 持锁时间与连接占用都会被放大。
		args := make([]any, 0, len(items)*9)
		groups := make([]string, 0, len(items))
		for _, it := range items {
			groups = append(groups, "("+placeholders(9)+")")
			// ctime 与 mtime 同批同值：整批条目是同一次覆盖写入的，
			// 出现两个时间就说明拼接有问题。
			args = append(args, it.ID, topicID, it.ItemType, it.ItemID, it.Position,
				orDefaultState(it.State), operatorID, ts, ts)
		}
		query := "INSERT INTO ops_topic_item (" + topicItemColumns + ") VALUES " +
			strings.Join(groups, ", ")
		res, err := session.ExecCtx(tctx, query, args...)
		if err != nil {
			return fmt.Errorf("ops_topic_item ReplaceAll insert: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("ops_topic_item ReplaceAll RowsAffected: %w", err)
		}
		inserted = n
		return nil
	})
	if err != nil {
		return 0, err
	}
	return inserted, nil
}

// orDefaultState 把契约里的 0 归一为「生效」：条目行是刚被运营确认放进专题的，
// 默认建成立即无效等于专题里凭空少一批条目。
func orDefaultState(v int32) int32 {
	if v == 0 {
		return StateOn
	}
	return v
}

const topicItemSelect = "SELECT " + topicItemColumns + " FROM ops_topic_item"

func (m *defaultTopicItemModel) ListByTopic(ctx context.Context, topicID int64, state int32, limit int) ([]*TopicItem, error) {
	if topicID <= 0 {
		return nil, ErrTopicNotFound
	}
	if limit <= 0 {
		limit = DefaultTopicListLimit
	}
	args := []any{topicID}
	query := topicItemSelect + " WHERE topic_id = ?"
	if state > 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	query += " ORDER BY position ASC LIMIT ?"
	args = append(args, limit)
	var rows []*TopicItem
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_topic_item ListByTopic: %w", err)
	}
	return rows, nil
}

func (m *defaultTopicItemModel) CountByTopic(ctx context.Context, topicID int64, state int32) (int64, error) {
	query := "SELECT COUNT(*) FROM ops_topic_item WHERE topic_id = ?"
	args := []any{topicID}
	if state > 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("ops_topic_item CountByTopic: %w", err)
	}
	return n, nil
}

func (m *defaultTopicItemModel) FindByItemRef(ctx context.Context, itemType, itemID string, limit int) ([]*TopicItem, error) {
	if itemType == "" || itemID == "" {
		return nil, ErrItemRefRequired
	}
	if limit <= 0 {
		limit = DefaultTopicListLimit
	}
	var rows []*TopicItem
	query := topicItemSelect + " WHERE item_type = ? AND item_id = ? AND state = ?" +
		" ORDER BY topic_id ASC, position ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, itemType, itemID, StateOn, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_topic_item FindByItemRef: %w", err)
	}
	return rows, nil
}
