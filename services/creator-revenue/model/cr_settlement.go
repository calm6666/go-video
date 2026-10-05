package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 结算单状态常量，与 cr_settlement.state 列和 rpc.SettlementState 取值严格一致。
const (
	SettlementStateUnspecified int32 = 0
	SettlementStateDraft       int32 = 1 // 已算出，未确认
	SettlementStateConfirmed   int32 = 2 // 运营确认，金额冻结不可重算
	SettlementStateVoided      int32 = 3 // 作废（重算前的旧单）
)

// 出金状态常量，与 cr_settlement.payout_state 列和 rpc.PayoutState 取值一致。
//
// 本项目只有 PayoutStateNotPayable 一个真实取值：出金（提现/打款/发票/税务/对账）
// 不在范围内。写这个字段的唯一目的，是让「钱没出账」在数据里可见，
// 而不是靠 README 提醒（AGENTS.md §1「范围外能力不得假成功」）。
const (
	PayoutStateUnspecified int32 = 0
	PayoutStateNotPayable  int32 = 1
)

// voidSeqLive 是在效结算单的槽位值。
//
// cr_settlement 的唯一键是 (period, mid, void_seq)：在效行恒为 0，所以
// 「同一周期同一作者至多一张在效单」由数据库保证；作废时把 void_seq 改写成
// 该行的 settlement_id（天然唯一），VOIDED 历史行就还能留在同一 (period, mid) 下，
// 新单也能重新占住 void_seq=0 这个槽位。
const voidSeqLive int64 = 0

// settlementColumns 是 cr_settlement 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const settlementColumns = "settlement_id, settlement_no, period, mid, amount_minor, cap_applied_minor, " +
	"currency, metric_count, state, payout_state, confirmed_by, confirmed_at, void_reason, " +
	"request_id, void_seq, ctime, mtime"

// Settlement 结算单行（一周期一人一单）。
//
// amount_minor 是**应计金额**（分），不是已支付金额；cap_applied_minor 显式记录
// 因月度封顶被扣掉的额度，运营不必从「台账合计 − 结算金额」反推。
type Settlement struct {
	SettlementId    int64  `db:"settlement_id"`
	SettlementNo    string `db:"settlement_no"`
	Period          string `db:"period"`
	Mid             int64  `db:"mid"`
	AmountMinor     int64  `db:"amount_minor"`
	CapAppliedMinor int64  `db:"cap_applied_minor"`
	Currency        string `db:"currency"`
	MetricCount     int64  `db:"metric_count"`
	State           int32  `db:"state"`
	PayoutState     int32  `db:"payout_state"`
	ConfirmedBy     string `db:"confirmed_by"`
	ConfirmedAt     int64  `db:"confirmed_at"`
	VoidReason      string `db:"void_reason"`
	RequestId       string `db:"request_id"`
	VoidSeq         int64  `db:"void_seq"`
	Ctime           int64  `db:"ctime"`
	Mtime           int64  `db:"mtime"`
}

// SettlementModel cr_settlement 表读写接口。
type SettlementModel interface {
	// Insert 写入结算单，返回自增 settlement_id。
	// uniq_active_period_mid(period, mid, void_seq) 与 uniq_request_id 是幂等兜底。
	Insert(ctx context.Context, s *Settlement) (int64, error)
	// FindActive 取某周期某作者的在效单（void_seq=0，含 DRAFT/CONFIRMED）；无则 (nil, nil)。
	FindActive(ctx context.Context, period string, mid int64) (*Settlement, error)
	// LockActive 在效单加行锁读取（重算/强制作废前定位，需构造在事务会话上）。
	LockActive(ctx context.Context, period string, mid int64) (*Settlement, error)
	// FindOneByNo 按结算单号查询；不存在返回 (nil, nil)。
	FindOneByNo(ctx context.Context, settlementNo string) (*Settlement, error)
	// FindByRequest 按行级幂等键 <request_id>#<period>#<mid> 反查（判定重放）。
	// 必须能读到 VOIDED 行：首单被后续请求作废后，同一 request_id 重放
	// 仍要回到「本次首次产出的那张单」，而不是重新出一张新单（二次出单=二次应计）。
	// 不存在返回 (nil, nil)。
	FindByRequest(ctx context.Context, requestKey string) (*Settlement, error)
	// UpdateDraftAmounts 就地重算 DRAFT 单（settlement_no 不变，同单号覆盖）。
	// 带 state=DRAFT 且 void_seq=0 守卫：CONFIRMED 单金额冻结，只能走强制作废路径。
	UpdateDraftAmounts(
		ctx context.Context, settlementNo string, amountMinor, capAppliedMinor, metricCount int64,
		currency, requestID string,
	) (bool, error)
	// Confirm 以 CAS 把 DRAFT 单确认为 CONFIRMED，confirmed_by 记操作人。
	// 返回 false 表示状态不是 DRAFT（或已被并发确认）——调用方计入 failed_nos，不整批失败。
	Confirm(ctx context.Context, settlementNo, operator string) (bool, error)
	// Void 把在效单置为 VOIDED 并释放 void_seq 槽位，同时留 void_reason。
	// fromState 是 CAS 条件（DRAFT 或 CONFIRMED），未命中返回 false。
	Void(ctx context.Context, settlementNo string, fromState int32, reason string) (bool, error)
	// List 按周期/作者/状态分页，按 settlement_id 降序（新单优先）。
	List(
		ctx context.Context, period string, mid int64, state int32, offset, limit int64,
	) ([]*Settlement, error)
	// Count 同 List 条件总数。
	Count(ctx context.Context, period string, mid int64, state int32) (int64, error)
	// SumConfirmed 该作者已确认应计合计；cutoffPeriod 非空时只统计 period >= cutoffPeriod。
	SumConfirmed(ctx context.Context, mid int64, cutoffPeriod string) (int64, error)
	// LatestActivePeriod 最近一张在效单的周期（YYYYMM）；无则返回 ("", nil)。
	LatestActivePeriod(ctx context.Context, mid int64) (string, error)
	// CountByPeriodMid 该 (period, mid) 下所有行（含 VOIDED）数，用于生成新单号后缀。
	CountByPeriodMid(ctx context.Context, period string, mid int64) (int64, error)
}

type defaultSettlementModel struct {
	conn sqlx.SqlConn
}

// NewSettlementModel 创建 cr_settlement 的数据访问对象。
func NewSettlementModel(conn sqlx.SqlConn) SettlementModel {
	return &defaultSettlementModel{conn: conn}
}

func (m *defaultSettlementModel) Insert(ctx context.Context, s *Settlement) (int64, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_settlement (settlement_no, period, mid, amount_minor, cap_applied_minor, currency, "+
			"metric_count, state, payout_state, confirmed_by, confirmed_at, void_reason, request_id, "+
			"void_seq, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		s.SettlementNo, s.Period, s.Mid, s.AmountMinor, s.CapAppliedMinor, s.Currency,
		s.MetricCount, s.State, s.PayoutState, s.ConfirmedBy, s.ConfirmedAt, s.VoidReason,
		s.RequestId, s.VoidSeq, now, now)
	if err != nil {
		return 0, fmt.Errorf("cr_settlement Insert(%s,%s,%d): %w", s.SettlementNo, s.Period, s.Mid, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_settlement LastInsertId(%s): %w", s.SettlementNo, err)
	}
	s.SettlementId = id
	return id, nil
}

func (m *defaultSettlementModel) FindActive(
	ctx context.Context, period string, mid int64,
) (*Settlement, error) {
	var s Settlement
	query := "SELECT " + settlementColumns + " FROM cr_settlement " +
		"WHERE period = ? AND mid = ? AND void_seq = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, period, mid, voidSeqLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement FindActive(%s,%d): %w", period, mid, err)
	}
	return &s, nil
}

func (m *defaultSettlementModel) LockActive(
	ctx context.Context, period string, mid int64,
) (*Settlement, error) {
	var s Settlement
	query := "SELECT " + settlementColumns + " FROM cr_settlement " +
		"WHERE period = ? AND mid = ? AND void_seq = ? LIMIT 1 FOR UPDATE"
	if err := m.conn.QueryRowCtx(ctx, &s, query, period, mid, voidSeqLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement LockActive(%s,%d): %w", period, mid, err)
	}
	return &s, nil
}

func (m *defaultSettlementModel) FindOneByNo(ctx context.Context, settlementNo string) (*Settlement, error) {
	var s Settlement
	query := "SELECT " + settlementColumns + " FROM cr_settlement WHERE settlement_no = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, settlementNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement FindOneByNo(%s): %w", settlementNo, err)
	}
	return &s, nil
}

func (m *defaultSettlementModel) FindByRequest(ctx context.Context, requestKey string) (*Settlement, error) {
	var s Settlement
	query := "SELECT " + settlementColumns + " FROM cr_settlement WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &s, query, requestKey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement FindByRequest(%s): %w", requestKey, err)
	}
	return &s, nil
}

func (m *defaultSettlementModel) UpdateDraftAmounts(
	ctx context.Context, settlementNo string, amountMinor, capAppliedMinor, metricCount int64,
	currency, requestID string,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_settlement SET amount_minor = ?, cap_applied_minor = ?, metric_count = ?, "+
			"currency = ?, request_id = ?, mtime = ? "+
			"WHERE settlement_no = ? AND state = ? AND void_seq = ?",
		amountMinor, capAppliedMinor, metricCount, currency, requestID, nowUnix(),
		settlementNo, SettlementStateDraft, voidSeqLive)
	if err != nil {
		return false, fmt.Errorf("cr_settlement UpdateDraftAmounts(%s): %w", settlementNo, err)
	}
	return rowsAffected(res)
}

func (m *defaultSettlementModel) Confirm(ctx context.Context, settlementNo, operator string) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_settlement SET state = ?, confirmed_by = ?, confirmed_at = ?, mtime = ? "+
			"WHERE settlement_no = ? AND state = ? AND void_seq = ?",
		SettlementStateConfirmed, operator, nowUnix(), nowUnix(),
		settlementNo, SettlementStateDraft, voidSeqLive)
	if err != nil {
		return false, fmt.Errorf("cr_settlement Confirm(%s): %w", settlementNo, err)
	}
	return rowsAffected(res)
}

func (m *defaultSettlementModel) Void(
	ctx context.Context, settlementNo string, fromState int32, reason string,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_settlement SET state = ?, void_reason = ?, void_seq = settlement_id, mtime = ? "+
			"WHERE settlement_no = ? AND state = ? AND void_seq = ?",
		SettlementStateVoided, reason, nowUnix(), settlementNo, fromState, voidSeqLive)
	if err != nil {
		return false, fmt.Errorf("cr_settlement Void(%s): %w", settlementNo, err)
	}
	return rowsAffected(res)
}

func (m *defaultSettlementModel) List(
	ctx context.Context, period string, mid int64, state int32, offset, limit int64,
) ([]*Settlement, error) {
	where, args := settlementFilter(period, mid, state)
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*Settlement
	query := "SELECT " + settlementColumns + " FROM cr_settlement WHERE " + where +
		" ORDER BY settlement_id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_settlement List: %w", err)
	}
	return rows, nil
}

func (m *defaultSettlementModel) Count(
	ctx context.Context, period string, mid int64, state int32,
) (int64, error) {
	where, args := settlementFilter(period, mid, state)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM cr_settlement WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_settlement Count: %w", err)
	}
	return total, nil
}

func (m *defaultSettlementModel) SumConfirmed(ctx context.Context, mid int64, cutoffPeriod string) (int64, error) {
	var total sql.NullInt64
	query := "SELECT SUM(amount_minor) FROM cr_settlement WHERE mid = ? AND state = ?"
	args := []any{mid, SettlementStateConfirmed}
	if cutoffPeriod != "" {
		query += " AND period >= ?"
		args = append(args, cutoffPeriod)
	}
	if err := m.conn.QueryRowCtx(ctx, &total, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_settlement SumConfirmed(%d): %w", mid, err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

func (m *defaultSettlementModel) LatestActivePeriod(ctx context.Context, mid int64) (string, error) {
	var period string
	query := "SELECT period FROM cr_settlement WHERE mid = ? AND void_seq = ? " +
		"ORDER BY period DESC LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &period, query, mid, voidSeqLive); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("cr_settlement LatestActivePeriod(%d): %w", mid, err)
	}
	return period, nil
}

func (m *defaultSettlementModel) CountByPeriodMid(
	ctx context.Context, period string, mid int64,
) (int64, error) {
	var total int64
	query := "SELECT COUNT(*) FROM cr_settlement WHERE period = ? AND mid = ?"
	if err := m.conn.QueryRowCtx(ctx, &total, query, period, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_settlement CountByPeriodMid(%s,%d): %w", period, mid, err)
	}
	return total, nil
}

// settlementFilter 构造结算单列表/计数共用 WHERE。
// state=0 不过滤（含 VOIDED，运营复核要看得到作废历史）。
func settlementFilter(period string, mid int64, state int32) (string, []any) {
	conds := make([]string, 0, 3)
	var args []any
	if period != "" {
		conds = append(conds, "period = ?")
		args = append(args, period)
	}
	if mid != 0 {
		conds = append(conds, "mid = ?")
		args = append(args, mid)
	}
	if state != SettlementStateUnspecified {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if len(conds) == 0 {
		return "1 = 1", args
	}
	return strings.Join(conds, " AND "), args
}

// BuildSettlementNo 生成结算单号：CRS<period>-<mid>-<rev>。
//
// rev 是该 (period, mid) 下已存在的行数（首次为 1，强制作废重算后递增），
// 保证「新单另起单号」，也让 VOIDED 旧单的单号继续在台账里可查。
func BuildSettlementNo(period string, mid, rev int64) string {
	if rev < 1 {
		rev = 1
	}
	return fmt.Sprintf("CRS%s-%d-%d", period, mid, rev)
}

// BuildSettlementRequestKey 把调用方的 request_id 折成行级幂等键。
//
// GenerateSettlement 的 mid=0 是「一批多单」，共用一个 request_id，
// 而 cr_settlement.request_id 上是唯一索引（AGENTS.md §5 要求写接口有幂等键），
// 所以落库的键是 <request_id>#<period>#<mid>：同一请求内每个 (period, mid) 只可能占一次。
// 空 request_id 由服务端补 "-"（唯一索引仍生效，但调用方失去重放保护，logic 侧会拒绝空值）。
func BuildSettlementRequestKey(requestID, period string, mid int64) string {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Sprintf("noreq#%s#%d", period, mid)
	}
	return fmt.Sprintf("%s#%s#%d", strings.TrimSpace(requestID), period, mid)
}
