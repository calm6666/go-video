package model

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// DeadLetter 死信留档行（inbox_dead_letter 表投影）。
// 只保存摘要与脱敏预览，不保存原始 payload，避免把上游敏感字段二次落库。
type DeadLetter struct {
	ID             int64  `db:"id"`              // 主键 ID
	EventID        string `db:"event_id"`        // 事件 ID（信封不可解析时为空串）
	EventType      string `db:"event_type"`      // 事件类型
	Topic          string `db:"topic"`           // 来源 topic
	PayloadDigest  string `db:"payload_digest"`  // sha256:<hex>
	PayloadPreview string `db:"payload_preview"` // 脱敏预览
	Reason         string `db:"reason"`          // 判死原因
	ConsumedAt     int64  `db:"consumed_at"`     // 判死时间（Unix 秒）
	State          string `db:"state"`           // open/replayed/ignored
	Ctime          int64  `db:"ctime"`           // 创建时间（Unix 秒）
}

// DeadLetterModel inbox_dead_letter 表读写接口。
type DeadLetterModel interface {
	// InsertIdempotent 按 (topic, payload_digest) 唯一键留档；
	// 同一条毒消息反复投递只会保留一行。返回 false 表示已留档。
	InsertIdempotent(ctx context.Context, dl *DeadLetter) (created bool, err error)
	// ListOpen 列出待人工处理的死信（按时间升序）。
	ListOpen(ctx context.Context, limit int32) ([]*DeadLetter, error)
	// MarkState 更新死信处理状态。
	MarkState(ctx context.Context, id int64, state string) error
}

type defaultDeadLetterModel struct {
	conn sqlx.SqlConn
}

// NewDeadLetterModel 创建 DeadLetterModel 实现。
func NewDeadLetterModel(conn sqlx.SqlConn) DeadLetterModel {
	return &defaultDeadLetterModel{conn: conn}
}

func (m *defaultDeadLetterModel) InsertIdempotent(ctx context.Context, dl *DeadLetter) (bool, error) {
	now := nowUnix()
	if dl.ConsumedAt == 0 {
		dl.ConsumedAt = now
	}
	if dl.State == "" {
		dl.State = DeadLetterStateOpen
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO inbox_dead_letter (event_id, event_type, topic, payload_digest, payload_preview,"+
			" reason, consumed_at, state, ctime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)"+
			" ON DUPLICATE KEY UPDATE id = id",
		dl.EventID, dl.EventType, dl.Topic, dl.PayloadDigest,
		truncate(dl.PayloadPreview, 256), truncate(dl.Reason, 512), dl.ConsumedAt, dl.State, now)
	if err != nil {
		return false, fmt.Errorf("inbox_dead_letter InsertIdempotent: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inbox_dead_letter InsertIdempotent RowsAffected: %w", err)
	}
	return affected == 1, nil
}

func (m *defaultDeadLetterModel) ListOpen(ctx context.Context, limit int32) ([]*DeadLetter, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT id, event_id, event_type, topic, payload_digest, payload_preview, reason," +
		" consumed_at, state, ctime FROM inbox_dead_letter WHERE state = ? ORDER BY id ASC LIMIT ?"
	var rows []*DeadLetter
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, DeadLetterStateOpen, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("inbox_dead_letter ListOpen: %w", err)
	}
	return rows, nil
}

func (m *defaultDeadLetterModel) MarkState(ctx context.Context, id int64, state string) error {
	_, err := m.conn.ExecCtx(ctx,
		"UPDATE inbox_dead_letter SET state = ? WHERE id = ?", state, id)
	if err != nil {
		return fmt.Errorf("inbox_dead_letter MarkState: %w", err)
	}
	return nil
}

// DigestPayload 计算原始消息体摘要，用于死信去重与审计定位。
func DigestPayload(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// RedactPreview 生成可落库的脱敏预览：
//   - 数字串（mid、手机号、订单号、IP 片段）统一掩码为 ***；
//   - 控制字符替换为空格，避免 JSON 原文里的转义噪音；
//   - 只保留前若干字节，并按 UTF-8 边界回退。
//
// 预览用于人工判断“这条消息为什么进死信”，不能用于还原原始内容。
func RedactPreview(raw []byte) string {
	const previewBytes = 200

	src := string(raw)
	if len(src) > previewBytes*2 {
		// 先按更大窗口截断，再掩码，避免长数字串被切断后残留明文片段。
		src = src[:previewBytes*2]
	}

	var (
		out      []rune
		inDigits bool
	)
	for _, r := range []rune(src) {
		// 非法字节在 rune 转换中已变成 U+FFFD，保证预览本身是合法 UTF-8。
		switch {
		case (r >= '0' && r <= '9') || r == '.':
			if !inDigits {
				out = append(out, '*', '*', '*')
				inDigits = true
			}
		case r < 0x20 || r == 0x7f:
			inDigits = false
			out = append(out, ' ')
		default:
			inDigits = false
			out = append(out, r)
		}
	}
	return truncate(string(out), previewBytes)
}
