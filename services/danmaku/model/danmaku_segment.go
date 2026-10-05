package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DanmakuSegment 弹幕分段计数索引行（danmaku_segment 表）。
// 该表是 danmaku 表的写放大派生数据：客户端按分段号预取窗口时，
// 只需读段计数即可决定拉取范围，不必对大表做 COUNT(*)。
// 计数允许与主表存在秒级偏差，由 cron 对账修复（见 README 缺口）。
type DanmakuSegment struct {
	ID    int64 `db:"id"`     // 自增主键
	Oid   int64 `db:"oid"`    // 内容主键
	SegNo int32 `db:"seg_no"` // 分段号
	Count int32 `db:"count"`  // 段内可见弹幕数
	Mtime int64 `db:"mtime"`  // 修改时间（Unix 秒）
}

// DanmakuSegmentModel danmaku_segment 表查询与写入接口。
type DanmakuSegmentModel interface {
	// Incr 段计数增量（delta 可为负，结果不小于 0）；(oid, seg_no) 唯一索引 upsert。
	Incr(ctx context.Context, oid int64, segNo int32, delta int32) error
	// ListBySegs 批量读取分段计数；缺失分段不返回行。
	ListBySegs(ctx context.Context, oid int64, segs []int32) ([]*DanmakuSegment, error)
	// DeleteByOid 内容删除/归档时清理分段索引。
	DeleteByOid(ctx context.Context, oid int64) error
}

type defaultDanmakuSegmentModel struct {
	conn sqlx.SqlConn
}

// NewDanmakuSegmentModel 创建 DanmakuSegmentModel 实现。
func NewDanmakuSegmentModel(conn sqlx.SqlConn) DanmakuSegmentModel {
	return &defaultDanmakuSegmentModel{conn: conn}
}

func (m *defaultDanmakuSegmentModel) Incr(ctx context.Context, oid int64, segNo int32, delta int32) error {
	// GREATEST 保证回删多于新增时计数不会变负（段表是派生数据，不做负值）。
	_, err := m.conn.ExecCtx(ctx,
		"INSERT INTO danmaku_segment (oid, seg_no, count, mtime) VALUES (?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE count = GREATEST(count + VALUES(count), 0), mtime = VALUES(mtime)",
		oid, segNo, delta, nowUnix())
	if err != nil {
		return fmt.Errorf("danmaku_segment Incr: %w", err)
	}
	return nil
}

func (m *defaultDanmakuSegmentModel) ListBySegs(ctx context.Context, oid int64, segs []int32) ([]*DanmakuSegment, error) {
	if len(segs) == 0 {
		return nil, nil
	}
	ph := make([]string, 0, len(segs))
	args := []interface{}{oid}
	for _, s := range segs {
		ph = append(ph, "?")
		args = append(args, s)
	}
	query := "SELECT id, oid, seg_no, count, mtime FROM danmaku_segment WHERE oid = ? AND seg_no IN (" + strings.Join(ph, ",") + ")"

	var rows []*DanmakuSegment
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("danmaku_segment ListBySegs: %w", err)
	}
	return rows, nil
}

func (m *defaultDanmakuSegmentModel) DeleteByOid(ctx context.Context, oid int64) error {
	if _, err := m.conn.ExecCtx(ctx, "DELETE FROM danmaku_segment WHERE oid = ?", oid); err != nil {
		return fmt.Errorf("danmaku_segment DeleteByOid: %w", err)
	}
	return nil
}
