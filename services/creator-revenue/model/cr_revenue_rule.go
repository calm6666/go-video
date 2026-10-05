package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 规则状态常量，与 cr_revenue_rule.state 列和 rpc.RuleState 取值严格一致。
// 变更取值会破坏已落库数据和已锁定的 rule_version 语义，禁止重排。
const (
	// RuleStateUnspecified 未指定，仅在查询语境表示「不过滤状态」。
	RuleStateUnspecified int32 = 0
	// RuleStateDraft 草稿：可编辑，不参与折算。
	RuleStateDraft int32 = 1
	// RuleStateActive 生效：同一 source_type 至多一条，计量台账只认它。
	RuleStateActive int32 = 2
	// RuleStateArchived 归档：终态，保留行作为历史口径证据。
	RuleStateArchived int32 = 3
)

// revenueRuleColumns 是 cr_revenue_rule 的完整列清单，
// 与 deploy/migrations/creator-revenue/000001_create_creator_revenue_tables.sql 一一对应。
const revenueRuleColumns = "rule_id, rule_code, source_type, name, description, " +
	"unit_price_per_1000_minor, currency, unit, min_quantity, monthly_cap_minor, " +
	"state, effective_from, version, created_by, updated_by, ctime, mtime"

// RevenueRule 分成规则行（DB 投影）。
//
// 版本语义：rule_code 唯一，一行代表一条规则；每次编辑 version 递增。
// 单价/状态的历史值不在本表，而在 cr_rule_change_log 的 from/to 列里
// （GetRevenueRule 按 version 复核时从那里还原），因为金额口径只由这两项决定。
type RevenueRule struct {
	RuleId           int64  `db:"rule_id"`
	RuleCode         string `db:"rule_code"`
	SourceType       int32  `db:"source_type"`
	Name             string `db:"name"`
	Description      string `db:"description"`
	UnitPricePer1000 int64  `db:"unit_price_per_1000_minor"`
	Currency         string `db:"currency"`
	Unit             string `db:"unit"`
	MinQuantity      int64  `db:"min_quantity"`
	MonthlyCapMinor  int64  `db:"monthly_cap_minor"`
	State            int32  `db:"state"`
	EffectiveFrom    int64  `db:"effective_from"`
	Version          int64  `db:"version"`
	CreatedBy        string `db:"created_by"`
	UpdatedBy        string `db:"updated_by"`
	Ctime            int64  `db:"ctime"`
	Mtime            int64  `db:"mtime"`
}

// RevenueRuleModel cr_revenue_rule 表读写接口。
type RevenueRuleModel interface {
	// Insert 写入新规则（首版 version=1），返回自增 rule_id。
	// uniq_rule_code 命中时返回可被 IsDuplicateErr 识别的错误。
	Insert(ctx context.Context, r *RevenueRule) (int64, error)
	// FindOne 按 rule_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, ruleID int64) (*RevenueRule, error)
	// FindByCode 按 rule_code 查询；不存在返回 (nil, nil)。
	FindByCode(ctx context.Context, code string) (*RevenueRule, error)
	// LockByCode 按 rule_code 加行锁读取（写侧 CAS 前的定位）。
	// 必须把 model 构造在事务会话上（sqlx.NewSqlConnFromSession）才有意义，
	// 脱离事务调用等价于普通读取（InnoDB 在 autocommit 下立即释放锁）。
	LockByCode(ctx context.Context, code string) (*RevenueRule, error)
	// LockActiveBySource 锁住「同一 source_type 下的规则行」并返回其中 ACTIVE 的那些，
	// 用于保证同一时刻一个来源至多一条 ACTIVE（切换必须与状态写入同事务）。
	//
	// 注意 SQL 比返回值锁得更多：它按 source_type 整段加锁（不限定 state），
	// 因为「并发把两条同来源规则都置成 ACTIVE」时，后到的事务必须被前一个挡在门外，
	// 而不是只锁住当前已存在的那批 ACTIVE（可能一条都没有，锁不到任何东西就等于没锁）。
	// 若将来给 (source_type, state) 加了唯一索引，可改回只锁 ACTIVE。
	LockActiveBySource(ctx context.Context, sourceType int32) ([]*RevenueRule, error)
	// List 按状态/来源分页列出，按 rule_id 升序；state/sourceType 为 0 表示不过滤。
	List(ctx context.Context, state, sourceType int32, offset, limit int64) ([]*RevenueRule, error)
	// Count 同 List 条件的总数。
	Count(ctx context.Context, state, sourceType int32) (int64, error)
	// UpdateDraft 以 expected_version 为条件就地更新草稿字段，version 递增 1。
	// 带 state=DRAFT 守卫：ACTIVE 单价直接决定应计金额，禁止就地改写。
	// 返回 false 表示 CAS 未命中（版本或状态已变）。
	UpdateDraft(ctx context.Context, r *RevenueRule, expectedVersion int64) (bool, error)
	// SetState 以 (expected_version, fromState) 为条件推进状态并递增 version。
	// 返回 false 表示 CAS 未命中。
	SetState(ctx context.Context, ruleID int64, fromState, toState, expectedVersion int64, operator string) (bool, error)
}

type defaultRevenueRuleModel struct {
	conn sqlx.SqlConn
}

// NewRevenueRuleModel 创建 cr_revenue_rule 的数据访问对象。
//
// 需要加入外部事务时，用 NewRevenueRuleModel(sqlx.NewSqlConnFromSession(tx)) 构造
// 绑定到该事务的实例（SQL 仍只在本文件，logic 不拼语句，AGENTS.md §4）。
func NewRevenueRuleModel(conn sqlx.SqlConn) RevenueRuleModel {
	return &defaultRevenueRuleModel{conn: conn}
}

func (m *defaultRevenueRuleModel) Insert(ctx context.Context, r *RevenueRule) (int64, error) {
	now := nowUnix()
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO cr_revenue_rule ("+revenueRuleColumnsWithoutID+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		r.RuleCode, r.SourceType, r.Name, r.Description, r.UnitPricePer1000, r.Currency, r.Unit,
		r.MinQuantity, r.MonthlyCapMinor, r.State, r.EffectiveFrom, r.Version,
		r.CreatedBy, r.UpdatedBy, now, now)
	if err != nil {
		return 0, fmt.Errorf("cr_revenue_rule Insert(%s): %w", r.RuleCode, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("cr_revenue_rule Insert LastInsertId(%s): %w", r.RuleCode, err)
	}
	return id, nil
}

// revenueRuleColumnsWithoutID 是 Insert 用的列清单（去掉自增主键与时间列）。
const revenueRuleColumnsWithoutID = "rule_code, source_type, name, description, " +
	"unit_price_per_1000_minor, currency, unit, min_quantity, monthly_cap_minor, " +
	"state, effective_from, version, created_by, updated_by, ctime, mtime"

func (m *defaultRevenueRuleModel) FindOne(ctx context.Context, ruleID int64) (*RevenueRule, error) {
	var r RevenueRule
	query := "SELECT " + revenueRuleColumns + " FROM cr_revenue_rule WHERE rule_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, ruleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_revenue_rule FindOne(%d): %w", ruleID, err)
	}
	return &r, nil
}

func (m *defaultRevenueRuleModel) FindByCode(ctx context.Context, code string) (*RevenueRule, error) {
	var r RevenueRule
	query := "SELECT " + revenueRuleColumns + " FROM cr_revenue_rule WHERE rule_code = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &r, query, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_revenue_rule FindByCode(%s): %w", code, err)
	}
	return &r, nil
}

func (m *defaultRevenueRuleModel) LockByCode(ctx context.Context, code string) (*RevenueRule, error) {
	var r RevenueRule
	query := "SELECT " + revenueRuleColumns + " FROM cr_revenue_rule WHERE rule_code = ? LIMIT 1 FOR UPDATE"
	if err := m.conn.QueryRowCtx(ctx, &r, query, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_revenue_rule LockByCode(%s): %w", code, err)
	}
	return &r, nil
}

func (m *defaultRevenueRuleModel) LockActiveBySource(
	ctx context.Context, sourceType int32,
) ([]*RevenueRule, error) {
	var rows []*RevenueRule
	// WHERE 不带 state：见接口注释——锁的是整个 source_type（走 idx_source_state 前缀），
	// 否则「当前没有 ACTIVE」时锁不到任何行，两条同来源规则可以并发各自生效。
	query := "SELECT " + revenueRuleColumns + " FROM cr_revenue_rule " +
		"WHERE source_type = ? ORDER BY rule_id ASC FOR UPDATE"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, sourceType); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_revenue_rule LockActiveBySource(%d): %w", sourceType, err)
	}
	active := make([]*RevenueRule, 0, len(rows))
	for _, r := range rows {
		if r.State == RuleStateActive {
			active = append(active, r)
		}
	}
	return active, nil
}

func (m *defaultRevenueRuleModel) List(
	ctx context.Context, state, sourceType int32, offset, limit int64,
) ([]*RevenueRule, error) {
	where, args := ruleFilter(state, sourceType)
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	var rows []*RevenueRule
	query := "SELECT " + revenueRuleColumns + " FROM cr_revenue_rule WHERE " + where +
		" ORDER BY rule_id ASC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("cr_revenue_rule List: %w", err)
	}
	return rows, nil
}

func (m *defaultRevenueRuleModel) Count(ctx context.Context, state, sourceType int32) (int64, error) {
	where, args := ruleFilter(state, sourceType)
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM cr_revenue_rule WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("cr_revenue_rule Count: %w", err)
	}
	return total, nil
}

func (m *defaultRevenueRuleModel) UpdateDraft(
	ctx context.Context, r *RevenueRule, expectedVersion int64,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_revenue_rule SET source_type = ?, name = ?, description = ?, "+
			"unit_price_per_1000_minor = ?, currency = ?, unit = ?, min_quantity = ?, "+
			"monthly_cap_minor = ?, effective_from = ?, version = version + 1, updated_by = ?, mtime = ? "+
			"WHERE rule_id = ? AND version = ? AND state = ?",
		r.SourceType, r.Name, r.Description, r.UnitPricePer1000, r.Currency, r.Unit,
		r.MinQuantity, r.MonthlyCapMinor, r.EffectiveFrom, r.UpdatedBy, nowUnix(),
		r.RuleId, expectedVersion, RuleStateDraft)
	if err != nil {
		return false, fmt.Errorf("cr_revenue_rule UpdateDraft(%d): %w", r.RuleId, err)
	}
	return rowsAffected(res)
}

func (m *defaultRevenueRuleModel) SetState(
	ctx context.Context, ruleID int64, fromState, toState, expectedVersion int64, operator string,
) (bool, error) {
	res, err := m.conn.ExecCtx(ctx,
		"UPDATE cr_revenue_rule SET state = ?, version = version + 1, updated_by = ?, mtime = ? "+
			"WHERE rule_id = ? AND version = ? AND state = ?",
		toState, operator, nowUnix(), ruleID, expectedVersion, fromState)
	if err != nil {
		return false, fmt.Errorf("cr_revenue_rule SetState(%d %d->%d): %w", ruleID, fromState, toState, err)
	}
	return rowsAffected(res)
}

// ruleFilter 构造列表/计数共用的 WHERE 片段与参数，参数化避免拼接注入。
func ruleFilter(state, sourceType int32) (string, []any) {
	conds := make([]string, 0, 2)
	var args []any
	if state != RuleStateUnspecified {
		conds = append(conds, "state = ?")
		args = append(args, state)
	}
	if sourceType != 0 {
		conds = append(conds, "source_type = ?")
		args = append(args, sourceType)
	}
	if len(conds) == 0 {
		return "1 = 1", args
	}
	return strings.Join(conds, " AND "), args
}

// rowsAffected 把 sql.Result 折算成「本次 CAS 是否命中」。
// 命中与否直接决定 logic 是否回幂等成功，因此 RowsAffected 取不到值时按错误处理，
// 绝不把「不知道有没有写进去」当成写成功。
func rowsAffected(res sql.Result) (bool, error) {
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}
	return aff > 0, nil
}
