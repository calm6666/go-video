package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 结算分项的来源聚合规则：一个结算单内同一 source_type 只有一行
// （uniq_no_source(settlement_no, source_type)），因此 rule_code 记录的是
// 「该来源下应计金额最高的规则」（主规则）。同来源多规则的明细在 cr_metric 台账里，
// 分项只是给运营一眼看懂钱从哪来，不替代明细。
const settlementItemColumns = "item_id, settlement_no, source_type, rule_code, quantity, amount_minor, ctime"

// SettlementItem 结算单分项行。
type SettlementItem struct {
	ItemId       int64  `db:"item_id"`
	SettlementNo string `db:"settlement_no"`
	SourceType   int32  `db:"source_type"`
	RuleCode     string `db:"rule_code"`
	Quantity     int64  `db:"quantity"`
	AmountMinor  int64  `db:"amount_minor"`
	Ctime        int64  `db:"ctime"`
}

// SettlementItemModel cr_settlement_item 表读写接口。
type SettlementItemModel interface {
	// InsertBatch 批量写入分项（同一事务内与结算单主体一起提交）。
	InsertBatch(ctx context.Context, items []*SettlementItem) error
	// DeleteByNo 删除某单全部分项（DRAFT 重算时先清后写，保证分项与台账一致）。
	DeleteByNo(ctx context.Context, settlementNo string) error
	// ListByNo 按 source_type 升序返回分项；无分项返回空切片。
	ListByNo(ctx context.Context, settlementNo string) ([]*SettlementItem, error)
}

type defaultSettlementItemModel struct {
	conn sqlx.SqlConn
}

// NewSettlementItemModel 创建 cr_settlement_item 的数据访问对象。
func NewSettlementItemModel(conn sqlx.SqlConn) SettlementItemModel {
	return &defaultSettlementItemModel{conn: conn}
}

func (m *defaultSettlementItemModel) InsertBatch(ctx context.Context, items []*SettlementItem) error {
	if len(items) == 0 {
		return nil
	}
	// 单条多值 INSERT：分批出单时减少往返，同时保证「有单必有分项」在一次执行里完成。
	build := strings.Builder{}
	build.WriteString("INSERT INTO cr_settlement_item " +
		"(settlement_no, source_type, rule_code, quantity, amount_minor, ctime) VALUES ")
	args := make([]any, 0, len(items)*6)
	for i, it := range items {
		if i > 0 {
			build.WriteString(",")
		}
		build.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args, it.SettlementNo, it.SourceType, it.RuleCode, it.Quantity, it.AmountMinor,
			orNow(it.Ctime))
	}
	if _, err := m.conn.ExecCtx(ctx, build.String(), args...); err != nil {
		return fmt.Errorf("cr_settlement_item InsertBatch(%d rows): %w", len(items), err)
	}
	return nil
}

func (m *defaultSettlementItemModel) DeleteByNo(ctx context.Context, settlementNo string) error {
	if _, err := m.conn.ExecCtx(ctx,
		"DELETE FROM cr_settlement_item WHERE settlement_no = ?", settlementNo); err != nil {
		return fmt.Errorf("cr_settlement_item DeleteByNo(%s): %w", settlementNo, err)
	}
	return nil
}

func (m *defaultSettlementItemModel) ListByNo(
	ctx context.Context, settlementNo string,
) ([]*SettlementItem, error) {
	var rows []*SettlementItem
	query := "SELECT " + settlementItemColumns + " FROM cr_settlement_item " +
		"WHERE settlement_no = ? ORDER BY source_type ASC"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, settlementNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement_item ListByNo(%s): %w", settlementNo, err)
	}
	return rows, nil
}

// orNow 时间列为 0 时取当前秒（分项行由结算逻辑现场组装，允许调用方指定统一时间）。
func orNow(v int64) int64 {
	if v > 0 {
		return v
	}
	return nowUnix()
}
