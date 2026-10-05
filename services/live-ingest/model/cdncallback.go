package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// cdnCallbackColumns 是 live_cdn_callback 的列清单，必须与
// deploy/migrations/live-ingest/000003_create_live_stream_event_tables.sql 完全一致。
const cdnCallbackColumns = "id, nonce, domain, event_type, stream_name, stream_id, key_id, room_id, " +
	"signature_hash, client_ip_hash, raw_params_digest, verify_result, suggest_state, handled, reason, " +
	"occurred_at, verified_at, trace_id, ctime"

// handled 取值（live_cdn_callback.handled 列）。
const (
	// CallbackUnhandled 尚未被入口处理（只完成鉴权留证）。
	CallbackUnhandled int32 = 0
	// CallbackHandled 入口已据此推进状态机。
	CallbackHandled int32 = 1
	// CallbackIgnored 判定不通过或被重放，不再处理。
	CallbackIgnored int32 = 2
)

// CdnCallback CDN/入口回调留证（live_cdn_callback 表投影）。
//
// 隐私与密钥约束（AGENTS.md §7）：签名原文、厂商密钥、来源 IP 明文都不入库，
// 只存不可逆摘要（signature_hash / client_ip_hash / raw_params_digest）。
// uniq_nonce 是防重放的最终防线：同一 nonce 第二次写入必然冲突，
// logic 据此回查首次结果并按 ErrCallbackReplayed 的幂等语义返回。
type CdnCallback struct {
	ID              int64  `db:"id"`                // 自增主键
	Nonce           string `db:"nonce"`             // 回调随机串（唯一索引）
	Domain          string `db:"domain"`            // 推流域名
	EventType       string `db:"event_type"`        // 厂商回调事件名（publish/publish_done 等）
	StreamName      string `db:"stream_name"`       // 回调解析出的流标识
	StreamID        string `db:"stream_id"`         // 归属流 ID，空串表示未解析
	KeyID           int64  `db:"key_id"`            // 关联密钥 ID，0 表示未解析
	RoomID          int64  `db:"room_id"`           // 房间引用
	SignatureHash   string `db:"signature_hash"`    // 签名摘要（不落厂商签名原文）
	ClientIPHash    string `db:"client_ip_hash"`    // 来源 IP 的 SHA-256（不存明文）
	RawParamsDigest string `db:"raw_params_digest"` // 原始参数摘要（SHA-256 hex）
	VerifyResult    int32  `db:"verify_result"`     // 判定结果，见 CallbackResult*
	SuggestState    int32  `db:"suggest_state"`     // 建议迁移到的流状态（仍需走 ReportStreamState）
	Handled         int32  `db:"handled"`           // 0 未处理、1 已处理、2 已忽略
	Reason          string `db:"reason"`            // 判定说明（不含密钥）
	OccurredAt      int64  `db:"occurred_at"`       // 回调时间戳（Unix 秒）
	VerifiedAt      int64  `db:"verified_at"`       // 服务端判定时间（Unix 秒）
	TraceID         string `db:"trace_id"`          // 链路追踪 ID
	Ctime           int64  `db:"ctime"`             // 落库时间（Unix 秒）
}

// CdnCallbackModel live_cdn_callback 表查询与写入接口（append-only + 判定回写）。
type CdnCallbackModel interface {
	// Insert 写入回调留证；返回 ErrCallbackReplayed 语义由调用方用 IsDuplicate 判定。
	Insert(ctx context.Context, cb *CdnCallback) (int64, error)
	// FindByNonce 按 nonce 查询（重放时回查首次判定）；不存在返回 (nil, nil)。
	FindByNonce(ctx context.Context, nonce string) (*CdnCallback, error)
	// FindOne 按记录 ID 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, id int64) (*CdnCallback, error)
	// MarkHandled 回写判定结果（verify_result / suggest_state / stream_id / handled）。
	// 仅当 handled 仍是期望的前值时生效，避免重复处理同一条回调。
	MarkHandled(ctx context.Context, id int64, verifyResult, suggestState int32, streamID string, keyID, roomID int64, reason string, handled int32, expectHandled int32) (bool, error)
	// ListRecentByStream 返回某流最近的回调留证（排障），limit 强制生效。
	ListRecentByStream(ctx context.Context, streamID string, limit int32) ([]*CdnCallback, error)
	// Prune 归档回调留证（由 services/cron 调用），返回影响行数。
	Prune(ctx context.Context, before int64, limit int32) (int64, error)
}

type defaultCdnCallbackModel struct {
	conn sqlx.SqlConn
}

// NewCdnCallbackModel 创建 CdnCallbackModel 实现。
func NewCdnCallbackModel(conn sqlx.SqlConn) CdnCallbackModel {
	return &defaultCdnCallbackModel{conn: conn}
}

func (m *defaultCdnCallbackModel) Insert(ctx context.Context, cb *CdnCallback) (int64, error) {
	if cb.Ctime == 0 {
		cb.Ctime = nowUnix()
	}
	if cb.OccurredAt == 0 {
		cb.OccurredAt = cb.Ctime
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_cdn_callback (nonce, domain, event_type, stream_name, stream_id, key_id, room_id, "+
			"signature_hash, client_ip_hash, raw_params_digest, verify_result, suggest_state, handled, reason, "+
			"occurred_at, verified_at, trace_id, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		cb.Nonce, truncate(cb.Domain, 191), truncate(cb.EventType, 32), truncate(cb.StreamName, 128),
		cb.StreamID, cb.KeyID, cb.RoomID, cb.SignatureHash, cb.ClientIPHash, cb.RawParamsDigest,
		cb.VerifyResult, cb.SuggestState, cb.Handled, truncate(cb.Reason, 255),
		cb.OccurredAt, cb.VerifiedAt, cb.TraceID, cb.Ctime)
	if err != nil {
		return 0, fmt.Errorf("live_cdn_callback Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_cdn_callback Insert LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultCdnCallbackModel) FindByNonce(ctx context.Context, nonce string) (*CdnCallback, error) {
	if nonce == "" {
		return nil, ErrIdempotencyKeyRequired
	}
	var cb CdnCallback
	query := "SELECT " + cdnCallbackColumns + " FROM live_cdn_callback WHERE nonce = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &cb, query, nonce); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_cdn_callback FindByNonce: %w", err)
	}
	return &cb, nil
}

func (m *defaultCdnCallbackModel) FindOne(ctx context.Context, id int64) (*CdnCallback, error) {
	var cb CdnCallback
	query := "SELECT " + cdnCallbackColumns + " FROM live_cdn_callback WHERE id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &cb, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_cdn_callback FindOne: %w", err)
	}
	return &cb, nil
}

func (m *defaultCdnCallbackModel) MarkHandled(
	ctx context.Context,
	id int64,
	verifyResult, suggestState int32,
	streamID string,
	keyID, roomID int64,
	reason string,
	handled int32,
	expectHandled int32,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_cdn_callback SET verify_result = ?, suggest_state = ?, stream_id = ?, key_id = ?, room_id = ?, "+
			"reason = ?, handled = ?, verified_at = ? WHERE id = ? AND handled = ?",
		verifyResult, suggestState, streamID, keyID, roomID, truncate(reason, 255), handled, nowUnix(), id, expectHandled)
	if err != nil {
		return false, fmt.Errorf("live_cdn_callback MarkHandled: %w", err)
	}
	return rowsAffected(res, "live_cdn_callback MarkHandled")
}

func (m *defaultCdnCallbackModel) ListRecentByStream(ctx context.Context, streamID string, limit int32) ([]*CdnCallback, error) {
	query := "SELECT " + cdnCallbackColumns + " FROM live_cdn_callback WHERE stream_id = ? " +
		"ORDER BY occurred_at DESC LIMIT ?"
	var rows []*CdnCallback
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, streamID, clampLimit(limit, 100)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_cdn_callback ListRecentByStream: %w", err)
	}
	return rows, nil
}

func (m *defaultCdnCallbackModel) Prune(ctx context.Context, before int64, limit int32) (int64, error) {
	res, err := m.conn.ExecCtx(ctx,
		"DELETE FROM live_cdn_callback WHERE occurred_at < ? ORDER BY id ASC LIMIT ?",
		before, clampLimit(limit, 2000))
	if err != nil {
		return 0, fmt.Errorf("live_cdn_callback Prune: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_cdn_callback Prune RowsAffected: %w", err)
	}
	return affected, nil
}
