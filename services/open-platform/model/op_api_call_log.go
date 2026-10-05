package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 判定结果，与 op_api_call_log.allowed 一致。
const (
	// CallAllowed 放行。
	CallAllowed int8 = 1
	// CallDenied 拒绝（deny_reason 记录原因码）。
	CallDenied int8 = 2
)

// ApiCallLog 接口调用流水（op_api_call_log 表）：配额与审计的事实来源。
//
// request_id 唯一索引 = AuthorizeRequest 的幂等锚点：
// 网关重试同一 request_id 时本表命中既有行，logic 直接重放首次判定结果，
// 既不重复扣配额也不重复写流水（AGENTS.md §5「写接口必须幂等」）。
//
// 隐私口径：只记 body_digest（请求体 sha256）与脱敏后的 client_ip，
// 不记请求体、不记 token 明文；本表内容只对运营开放，不出现在任何开放接口响应中。
type ApiCallLog struct {
	// CallLogID 流水 ID（主键）
	CallLogID int64 `db:"call_log_id"`
	// RequestID 网关请求幂等键（唯一索引）
	RequestID string `db:"request_id"`
	// AppID 应用 ID
	AppID int64 `db:"app_id"`
	// APICode 接口标识（配额键）
	APICode string `db:"api_code"`
	// Mid 授权用户 mid（应用级签名为 0）
	Mid int64 `db:"mid"`
	// TokenID 使用的 token 行（签名模式为 0）
	TokenID int64 `db:"token_id"`
	// GrantID 授权关系（签名模式为 0）
	GrantID int64 `db:"grant_id"`
	// Allowed 判定结果，见 CallAllowed/CallDenied
	Allowed int8 `db:"allowed"`
	// DenyReason 拒绝原因码（inactive/expired/revoked/app_suspended/scope_missing/quota_exceeded/signature_invalid）
	DenyReason string `db:"deny_reason"`
	// Method HTTP 方法
	Method string `db:"method"`
	// Path 请求路径（不含查询串，避免把参数写进流水）
	Path string `db:"path"`
	// BodyDigest 请求体摘要，格式 sha256:<hex>，不含原文
	BodyDigest string `db:"body_digest"`
	// ClientIPMasked 脱敏后的来源 IP（IPv4 抹掉末段，IPv6 抹掉后半部分）
	ClientIPMasked string `db:"client_ip_masked"`
	// QuotaLimit 判定时生效限额
	QuotaLimit int64 `db:"quota_limit"`
	// QuotaRemaining 判定后剩余额度
	QuotaRemaining int64 `db:"quota_remaining"`
	// Ctime 判定时间（Unix 秒，配额重算按此列取窗）
	Ctime int64 `db:"ctime"`
}

// ApiCallLogModel op_api_call_log 表读写接口。
type ApiCallLogModel interface {
	// Insert 写流水；命中 uniq_request_id 时返回既有行且 created=false（幂等重放）。
	Insert(ctx context.Context, l *ApiCallLog) (callLogID int64, created bool, err error)
	// FindByRequestID 幂等重放入口；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*ApiCallLog, error)
	// ListByCursor 运营侧观测分页：cursor 为 (ctime, call_log_id) 倒序位点。
	ListByCursor(ctx context.Context, appID int64, apiCode string, onlyDenied bool, cursorTime,
		cursorID int64, ps int32) ([]*ApiCallLog, error)
	// CountByWindow 统计 [from,to) 区间调用数（配额重算的最小粒度单元）。
	CountByWindow(ctx context.Context, appID int64, apiCode string, from, to int64) (int64, error)
	// ListWindowTotals 按窗口分组统计流水（RecomputeQuota 用一次查询覆盖多个窗口）。
	ListWindowTotals(ctx context.Context, appID int64, apiCode string, windowSeconds,
		from, to int64) ([]*QuotaWindowTotal, error)
	// ListPurgeCandidates 返回超出保留期的流水主键（只取 ID，禁止把整表读进内存）。
	ListPurgeCandidates(ctx context.Context, before int64, limit int32) ([]int64, error)
	// PurgeByIDs 物理删除流水（保留期内已由归档/离线仓承担长期审计，详见 README）。
	PurgeByIDs(ctx context.Context, ids []int64) (int64, error)
}

// QuotaWindowTotal 分组统计结果（配额重算的中转结构）。
type QuotaWindowTotal struct {
	// WindowStart 对齐后的窗口起点（Unix 秒）
	WindowStart int64 `db:"window_start"`
	// Total 该窗口内的流水条数
	Total int64 `db:"total"`
}

type defaultApiCallLogModel struct {
	conn sqlx.SqlConn
}

// NewApiCallLogModel 创建 ApiCallLogModel 实现。
func NewApiCallLogModel(conn sqlx.SqlConn) ApiCallLogModel {
	return &defaultApiCallLogModel{conn: conn}
}

const apiCallLogColumns = `call_log_id, request_id, app_id, api_code, mid, token_id, grant_id, allowed,
	deny_reason, method, path, body_digest, client_ip_masked, quota_limit, quota_remaining, ctime`

func (m *defaultApiCallLogModel) Insert(ctx context.Context, l *ApiCallLog) (int64, bool, error) {
	if strings.TrimSpace(l.RequestID) == "" {
		return 0, false, ErrRequestIDRequired
	}
	if l.AppID <= 0 {
		return 0, false, ErrInvalidAppID
	}
	if l.Allowed != CallAllowed && l.Allowed != CallDenied {
		return 0, false, ErrInvalidStateTransition
	}
	now := nowUnix()
	if l.Ctime == 0 {
		l.Ctime = now
	}
	// request_id 唯一：重试路径不写第二条流水（幂等锚点）。
	// ON DUPLICATE 子句写成自赋值， MySQL 对插入返回 1、对命中唯一键返回 0，据此区分首次与重放。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO op_api_call_log (request_id, app_id, api_code, mid, token_id, grant_id, allowed, "+
			"deny_reason, method, path, body_digest, client_ip_masked, quota_limit, quota_remaining, ctime) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE call_log_id = call_log_id",
		l.RequestID, l.AppID, l.APICode, l.Mid, l.TokenID, l.GrantID, l.Allowed, l.DenyReason,
		l.Method, l.Path, l.BodyDigest, l.ClientIPMasked, l.QuotaLimit, l.QuotaRemaining, l.Ctime)
	if err != nil {
		return 0, false, fmt.Errorf("op_api_call_log Insert: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("op_api_call_log Insert RowsAffected: %w", err)
	}
	if affected == 1 {
		id, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("op_api_call_log Insert LastInsertId: %w", err)
		}
		return id, true, nil
	}
	// 命中幂等：回查首次判定结果，logic 直接重放该行的 allowed/quota 字段。
	old, err := m.FindByRequestID(ctx, l.RequestID)
	if err != nil {
		return 0, false, err
	}
	if old == nil {
		return 0, false, ErrRequestIDRequired
	}
	return old.CallLogID, false, nil
}

func (m *defaultApiCallLogModel) FindByRequestID(ctx context.Context, requestID string) (*ApiCallLog, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, ErrRequestIDRequired
	}
	var l ApiCallLog
	err := m.conn.QueryRowCtx(ctx, &l,
		"SELECT "+apiCallLogColumns+" FROM op_api_call_log WHERE request_id = ? LIMIT 1", requestID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_api_call_log FindByRequestID: %w", err)
	}
	return &l, nil
}

func (m *defaultApiCallLogModel) ListByCursor(ctx context.Context, appID int64, apiCode string,
	onlyDenied bool, cursorTime, cursorID int64, ps int32) ([]*ApiCallLog, error) {
	if ps <= 0 {
		return nil, ErrInvalidPage
	}
	conds := []string{"1 = 1"}
	var args []any
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	if apiCode != "" {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	if onlyDenied {
		conds = append(conds, "allowed = ?")
		args = append(args, CallDenied)
	}
	if cursorTime > 0 {
		conds = append(conds, "(ctime < ? OR (ctime = ? AND call_log_id < ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}
	query := "SELECT " + apiCallLogColumns + " FROM op_api_call_log WHERE " +
		strings.Join(conds, " AND ") + " ORDER BY ctime DESC, call_log_id DESC LIMIT ?"
	args = append(args, ps)

	var rows []*ApiCallLog
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_api_call_log ListByCursor: %w", err)
	}
	return rows, nil
}

func (m *defaultApiCallLogModel) CountByWindow(ctx context.Context, appID int64, apiCode string,
	from, to int64) (int64, error) {
	if to <= from {
		return 0, ErrWindowInvalid
	}
	conds := []string{"ctime >= ?", "ctime < ?"}
	args := []any{from, to}
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	if apiCode != "" && apiCode != AnyAPICode {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	var cnt int64
	query := "SELECT COUNT(*) FROM op_api_call_log WHERE " + strings.Join(conds, " AND ")
	if err := m.conn.QueryRowCtx(ctx, &cnt, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("op_api_call_log CountByWindow: %w", err)
	}
	return cnt, nil
}

func (m *defaultApiCallLogModel) ListWindowTotals(ctx context.Context, appID int64, apiCode string,
	windowSeconds, from, to int64) ([]*QuotaWindowTotal, error) {
	if windowSeconds <= 0 {
		return nil, ErrWindowInvalid
	}
	if to <= from {
		return nil, ErrWindowInvalid
	}
	conds := []string{"ctime >= ?", "ctime < ?"}
	args := []any{windowSeconds, windowSeconds, from, to}
	if appID > 0 {
		conds = append(conds, "app_id = ?")
		args = append(args, appID)
	}
	if apiCode != "" && apiCode != AnyAPICode {
		conds = append(conds, "api_code = ?")
		args = append(args, apiCode)
	}
	// FLOOR(ctime/?)*? 与 Go 侧 AlignWindow 取齐规则一致，重算与扣减共用同一窗口边界。
	query := "SELECT FLOOR(ctime / ?) * ? AS window_start, COUNT(*) AS total FROM op_api_call_log WHERE " +
		strings.Join(conds, " AND ") + " GROUP BY window_start ORDER BY window_start ASC"

	var rows []*QuotaWindowTotal
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_api_call_log ListWindowTotals: %w", err)
	}
	return rows, nil
}

func (m *defaultApiCallLogModel) ListPurgeCandidates(ctx context.Context, before int64,
	limit int32) ([]int64, error) {
	if limit <= 0 {
		return nil, ErrInvalidPage
	}
	var ids []int64
	err := m.conn.QueryRowsCtx(ctx, &ids,
		"SELECT call_log_id FROM op_api_call_log WHERE ctime < ? ORDER BY ctime ASC LIMIT ?", before, limit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("op_api_call_log ListPurgeCandidates: %w", err)
	}
	return ids, nil
}

func (m *defaultApiCallLogModel) PurgeByIDs(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(ids))
	query := "DELETE FROM op_api_call_log WHERE call_log_id IN (?"
	for _, id := range ids {
		args = append(args, id)
		query += ",?"
	}
	query += ")"
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("op_api_call_log PurgeByIDs: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("op_api_call_log PurgeByIDs RowsAffected: %w", err)
	}
	return aff, nil
}
