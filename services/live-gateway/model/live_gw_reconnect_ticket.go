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

// LiveGwReconnectTicket 断线重连票据（live_gw_reconnect_ticket）。
//
// 职责划分（README 安全约束）：
//   - Redis 承担"能不能换"的实时判定（TTL 有效位 + 一次性消费的原子 GETDEL），
//   - 本表承担"谁在什么时候签发/使用/撤销了哪张票"的审计与撤销名单，节点重启后可重建有效位。
//
// 只存票据摘要（ticket_hash = sha256(ticket) 的十六进制），绝不存票据原文：
// 票据是 HMAC 签名的凭据，落库等于把可重放的凭据写进主库；一旦 DB 泄露就能冒名换租约。
//
// 换取租约时必须三元组一致（room_id + mid 与票据登记值相同，且未过期）：
// 票据绑定的 role 直接沿用签发时的判定结果，不接受客户端重新声明的角色。
type LiveGwReconnectTicket struct {
	Id           int64  `db:"id"`
	TicketId     string `db:"ticket_id"`   // 票据 ID（ULID，撤销与审计主键）
	TicketHash   string `db:"ticket_hash"` // sha256(票据原文) hex（唯一索引，不落明文）
	RoomId       int64  `db:"room_id"`
	Mid          int64  `db:"mid"`            // 0 表示游客票据
	ConnId       string `db:"conn_id"`        // 签发时的连接标识
	NodeId       string `db:"node_id"`        // 签发时的承载节点
	Role         int32  `db:"role"`           // 签发时判定的角色（Role*）
	State        int32  `db:"state"`          // TicketState*
	IssuedAt     int64  `db:"issued_at"`      // Unix 秒
	ExpireAt     int64  `db:"expire_at"`      // Unix 秒
	UsedAt       int64  `db:"used_at"`        // Unix 秒，0 表示未使用
	LeaseId      string `db:"lease_id"`       // 签发时依托的租约（一次性凭据的来源）
	NewLeaseId   string `db:"new_lease_id"`   // 换取到的新租约（USED 时回填）
	IssueReason  string `db:"issue_reason"`   // normal/reconnect/room_switch
	RevokeReason string `db:"revoked_reason"` // banned/kicked/room_closed/risk
	RevokedBy    string `db:"revoked_by"`     // 撤销操作者（审计）
	RequestId    string `db:"request_id"`     // 签发幂等键（唯一索引）
	TraceId      string `db:"trace_id"`
	Ctime        int64  `db:"ctime"`
	Mtime        int64  `db:"mtime"`
}

// TicketFilter List 过滤条件。
type TicketFilter struct {
	RoomId      int64
	Mid         int64
	State       int32
	LeaseId     string
	Pn          int32
	Ps          int32
	MaxPageSize int32
}

// LiveGwReconnectTicketModel live_gw_reconnect_ticket 表接口。
type LiveGwReconnectTicketModel interface {
	// Insert 登记签发的票据；request_id 冲突返回 ErrRequestIdDuplicated（调用方按 request_id 回读）。
	Insert(ctx context.Context, t *LiveGwReconnectTicket) (int64, error)
	FindOne(ctx context.Context, ticketID string) (*LiveGwReconnectTicket, error)
	// FindByHash 按票据摘要反查（Redeem 路径：客户端只带票据原文，服务端存摘要）。
	// 不存在返回 (nil, nil)。
	FindByHash(ctx context.Context, ticketHash string) (*LiveGwReconnectTicket, error)
	FindByRequestID(ctx context.Context, requestID string) (*LiveGwReconnectTicket, error)
	List(ctx context.Context, f TicketFilter) ([]*LiveGwReconnectTicket, int32, error)
	// Consume 一次性消费：只有 state=ISSUED 且三元组匹配才置 USED，返回受影响行数。
	// 0 行必须回读区分「已被使用 / 已撤销 / 已过期 / 三元组不匹配」，不得当成成功。
	Consume(ctx context.Context, ticketID string, roomID, mid int64, newLeaseID string, usedAt int64) (int64, error)
	// RevokeByTicketID 撤销单张票据（state=ISSUED → REVOKED），返回受影响行数。
	RevokeByTicketID(ctx context.Context, ticketID, reason, operator string) (int64, error)
	// RevokeByRoomMid 撤销该用户在该房间的全部未使用票据（KickConnection/封禁场景），返回行数。
	RevokeByRoomMid(ctx context.Context, roomID, mid int64, reason, operator string, limit int32) (int64, error)
	// MarkExpired 把已过期但仍是 ISSUED 的票据批量置 EXPIRED（清扫任务），返回行数。
	MarkExpired(ctx context.Context, now int64, limit int32) (int64, error)
	// CountUnusedByRoomMid 该用户在该房间还有多少张可用票据（排障与"票据风暴"告警）。
	CountUnusedByRoomMid(ctx context.Context, roomID, mid int64) (int64, error)
}

// TicketHash 计算票据摘要：sha256 十六进制。DB 与 Redis 都只用摘要定位票据。
func TicketHash(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

const liveGwReconnectTicketColumns = "SELECT id, ticket_id, ticket_hash, room_id, mid, conn_id, node_id, role, state, " +
	"issued_at, expire_at, used_at, lease_id, new_lease_id, issue_reason, revoked_reason, revoked_by, request_id, " +
	"trace_id, ctime, mtime"

type defaultLiveGwReconnectTicketModel struct {
	conn sqlx.SqlConn
}

// NewLiveGwReconnectTicketModel 构造 live_gw_reconnect_ticket 的 model。
func NewLiveGwReconnectTicketModel(conn sqlx.SqlConn) LiveGwReconnectTicketModel {
	return &defaultLiveGwReconnectTicketModel{conn: conn}
}

func (m *defaultLiveGwReconnectTicketModel) Insert(ctx context.Context, t *LiveGwReconnectTicket) (int64, error) {
	if t.TicketId == "" {
		return 0, ErrEmptyTicket
	}
	if t.RequestId == "" {
		return 0, ErrEmptyRequestID
	}
	if t.RoomId <= 0 {
		return 0, ErrInvalidRoomID
	}
	if t.Mid < 0 {
		return 0, ErrInvalidMid
	}
	if t.ExpireAt <= t.IssuedAt {
		return 0, fmt.Errorf("live_gw_reconnect_ticket Insert: expire_at=%d issued_at=%d %w",
			t.ExpireAt, t.IssuedAt, ErrTicketExpired)
	}
	if t.TicketHash == "" {
		return 0, ErrEmptyTicket // 调用方必须先用 TicketHash(ticket) 生成摘要，禁止落明文
	}
	now := nowUnix()
	if t.Ctime == 0 {
		t.Ctime = now
	}
	if t.Mtime == 0 {
		t.Mtime = now
	}
	if t.State == 0 {
		t.State = TicketStateIssued
	}
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO live_gw_reconnect_ticket (ticket_id, ticket_hash, room_id, mid, conn_id, node_id, role, state, "+
			"issued_at, expire_at, used_at, lease_id, new_lease_id, issue_reason, revoked_reason, revoked_by, "+
			"request_id, trace_id, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		t.TicketId, t.TicketHash, t.RoomId, t.Mid, t.ConnId, t.NodeId, NormalizeRole(t.Role), t.State,
		t.IssuedAt, t.ExpireAt, t.LeaseId, t.NewLeaseId, t.IssueReason, t.RevokeReason, t.RevokedBy,
		t.RequestId, t.TraceId, t.Ctime, t.Mtime)
	if err != nil {
		if isDuplicateErr(err) {
			return 0, fmt.Errorf("request_id=%s ticket_id=%s: %w", t.RequestId, t.TicketId, ErrRequestIdDuplicated)
		}
		return 0, fmt.Errorf("live_gw_reconnect_ticket Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("live_gw_reconnect_ticket Insert LastInsertId: %w", err)
	}
	return id, nil
}

// RoleNormalizer 导出给 logic 使用的角色收敛函数（未识别角色一律按 VIEWER）。
// model 层再兜一道底：即使调用方忘了 NormalizeRole，落库的 role 也不会是可越权的野值。
func RoleNormalizer(role int32) int32 { return NormalizeRole(role) }

func (m *defaultLiveGwReconnectTicketModel) FindOne(ctx context.Context, ticketID string) (*LiveGwReconnectTicket, error) {
	if ticketID == "" {
		return nil, ErrEmptyTicket
	}
	var t LiveGwReconnectTicket
	query := liveGwReconnectTicketColumns + " FROM live_gw_reconnect_ticket WHERE ticket_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, ticketID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_reconnect_ticket FindOne: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveGwReconnectTicketModel) FindByHash(ctx context.Context, ticketHash string) (*LiveGwReconnectTicket, error) {
	if ticketHash == "" {
		return nil, ErrEmptyTicket
	}
	var t LiveGwReconnectTicket
	query := liveGwReconnectTicketColumns + " FROM live_gw_reconnect_ticket WHERE ticket_hash = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, ticketHash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_reconnect_ticket FindByHash: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveGwReconnectTicketModel) FindByRequestID(ctx context.Context, requestID string) (*LiveGwReconnectTicket, error) {
	var t LiveGwReconnectTicket
	query := liveGwReconnectTicketColumns + " FROM live_gw_reconnect_ticket WHERE request_id = ?"
	if err := m.conn.QueryRowCtx(ctx, &t, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("live_gw_reconnect_ticket FindByRequestID: %w", err)
	}
	return &t, nil
}

func (m *defaultLiveGwReconnectTicketModel) List(ctx context.Context, f TicketFilter) ([]*LiveGwReconnectTicket, int32, error) {
	where, args := buildWhere(
		whereFragment{"room_id = ?", []any{}}.when(f.RoomId > 0, f.RoomId),
		whereFragment{"mid = ?", []any{}}.when(f.Mid > 0, f.Mid),
		whereFragment{"state = ?", []any{}}.when(f.State > 0, f.State),
		whereFragment{"lease_id = ?", []any{}}.when(f.LeaseId != "", f.LeaseId),
	)
	limit, offset := clampPage(f.Pn, f.Ps, f.MaxPageSize)

	var total int32
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM live_gw_reconnect_ticket "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("live_gw_reconnect_ticket List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), limit, offset)
	var rows []*LiveGwReconnectTicket
	if err := m.conn.QueryRowsCtx(ctx, &rows,
		liveGwReconnectTicketColumns+" FROM live_gw_reconnect_ticket "+where+" ORDER BY id DESC LIMIT ? OFFSET ?",
		listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("live_gw_reconnect_ticket List: %w", err)
	}
	return rows, total, nil
}

func (m *defaultLiveGwReconnectTicketModel) Consume(ctx context.Context, ticketID string, roomID, mid int64,
	newLeaseID string, usedAt int64) (int64, error) {
	if ticketID == "" {
		return 0, ErrEmptyTicket
	}
	if usedAt <= 0 {
		usedAt = nowUnix()
	}
	// 三元组条件写在 WHERE 里：room_id/mid 与票据不一致时直接 0 行，
	// 由调用方回读给出 ErrTripletMismatch（越权）而不是"消费成功"。
	return conditionalUpdate(ctx, m.conn, "live_gw_reconnect_ticket",
		[]columnValue{
			{"state", TicketStateUsed},
			{"used_at", usedAt},
			{"new_lease_id", newLeaseID},
		}, false, []whereFragment{
			{"ticket_id = ?", []any{ticketID}},
			{"state = ?", []any{TicketStateIssued}},
			{"room_id = ?", []any{roomID}},
			{"mid = ?", []any{mid}},
			{"expire_at > ?", []any{usedAt}},
		})
}

func (m *defaultLiveGwReconnectTicketModel) RevokeByTicketID(ctx context.Context, ticketID, reason, operator string) (int64, error) {
	if ticketID == "" {
		return 0, ErrEmptyTicket
	}
	if reason == "" {
		return 0, ErrEmptyReason
	}
	if operator == "" {
		return 0, ErrEmptyOperator
	}
	return conditionalUpdate(ctx, m.conn, "live_gw_reconnect_ticket",
		[]columnValue{
			{"state", TicketStateRevoked},
			{"revoked_reason", reason},
			{"revoked_by", operator},
		}, false, []whereFragment{
			{"ticket_id = ?", []any{ticketID}},
			{"state = ?", []any{TicketStateIssued}},
		})
}

func (m *defaultLiveGwReconnectTicketModel) RevokeByRoomMid(ctx context.Context, roomID, mid int64,
	reason, operator string, limit int32) (int64, error) {
	if roomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	if mid <= 0 {
		// 游客票据（mid=0）按 (room, mid) 批量撤销等于"撤销该房间所有游客票据"，
		// 影响面远超一次封禁应有的范围，必须显式拒绝，由调用方按 ticket_id 精确撤销。
		return 0, ErrInvalidMid
	}
	if reason == "" {
		return 0, ErrEmptyReason
	}
	if operator == "" {
		return 0, ErrEmptyOperator
	}
	if limit <= 0 {
		limit = 50
	}
	// 带 LIMIT 的条件更新：单用户票据数量本该很小，加限制是防"票据风暴"时一次锁住过多行。
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_gw_reconnect_ticket SET state = ?, revoked_reason = ?, revoked_by = ?, mtime = ? "+
			"WHERE state = ? AND room_id = ? AND mid = ? LIMIT ?",
		TicketStateRevoked, reason, operator, nowUnix(), TicketStateIssued, roomID, mid, limit)
	if err != nil {
		return 0, fmt.Errorf("live_gw_reconnect_ticket RevokeByRoomMid: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_gw_reconnect_ticket RevokeByRoomMid RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveGwReconnectTicketModel) MarkExpired(ctx context.Context, now int64, limit int32) (int64, error) {
	if limit <= 0 {
		limit = 500
	}
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE live_gw_reconnect_ticket SET state = ?, mtime = ? WHERE state = ? AND expire_at > 0 AND expire_at < ? LIMIT ?",
		TicketStateExpired, now, TicketStateIssued, now, limit)
	if err != nil {
		return 0, fmt.Errorf("live_gw_reconnect_ticket MarkExpired: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("live_gw_reconnect_ticket MarkExpired RowsAffected: %w", err)
	}
	return aff, nil
}

func (m *defaultLiveGwReconnectTicketModel) CountUnusedByRoomMid(ctx context.Context, roomID, mid int64) (int64, error) {
	if roomID <= 0 {
		return 0, ErrInvalidRoomID
	}
	var n int64
	query := "SELECT COUNT(*) FROM live_gw_reconnect_ticket WHERE state = ? AND room_id = ? AND mid = ?"
	if err := m.conn.QueryRowCtx(ctx, &n, query, TicketStateIssued, roomID, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("live_gw_reconnect_ticket CountUnusedByRoomMid: %w", err)
	}
	return n, nil
}
