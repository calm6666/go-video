package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// rolloutRuleColumns 是 ops_rollout_rule 的列清单，必须与
// deploy/migrations/ops-config/000001_create_ops_config_publish_tables.sql 一致。
const rolloutRuleColumns = "rule_id, config_id, version, name, mode, percentage," +
	" app_version_min, app_version_max, platforms, mid_suffixes, whitelist_mids," +
	" priority, state, operator_id, remark, start_at, end_at, ctime, mtime"

// 端标识的存储与类型口径见 platform.go：本表的 platforms 列复用 ID 列表的
// 存储形态 ",1,4,"（首尾带逗号），Go 侧一律 int32，唯一的 int32↔int64 转换
// 收在 PlatformListContains / NormalizePlatformList 里。

// RolloutRule 对应 ops_rollout_rule 表：作用于 (config_id, version) 的灰度规则。
//
// 本表是**放量决策的证据**，不是计数器：
//   - 不做物理删除，收口靠 state=2（SetRolloutRuleState），
//     事后要回答「昨天 10 点谁被放量了」只能靠这些行；
//   - name 在同 (config_id, version) 内唯一，是 upsert 的幂等句柄
//     （PublishConfigReq 一次带多条规则、gRPC 重试不会产出重复规则）；
//   - 判定语义的全部实现在 rollout.go（纯函数），本文件只负责存取。
type RolloutRule struct {
	// RuleID 自增主键（ConfigView.rollout_rule_id 回的就是它）。
	RuleID int64 `db:"rule_id"`
	// ConfigID 所属配置项（ops_config_item.config_id）。
	ConfigID int64 `db:"config_id"`
	// Version 本规则放量的版本号：必须指向 ops_config_version 里已存在的版本，
	// 否则规则会把一个不存在的值推给线上。
	Version int64 `db:"version"`
	// Name 规则名，(config_id, version) 内唯一，upsert 幂等句柄。
	Name string `db:"name"`
	// Mode 主判定维度，取值见 RolloutMode 常量。
	Mode int32 `db:"mode"`
	// Percentage mode=percentage 时的放量百分比（0..100）。
	Percentage int32 `db:"percentage"`
	// AppVersionMin 版本区间闭下界（点分十进制）。
	AppVersionMin string `db:"app_version_min"`
	// AppVersionMax 版本区间闭上界，空表示不设上界。
	AppVersionMax string `db:"app_version_max"`
	// Platforms 生效端列表，存储形态 ",1,4,"，空串表示不限端。
	Platforms string `db:"platforms"`
	// MidSuffixes mid 十进制尾号列表，归一化形态 "0,3,7"（升序去重），空表示不限。
	MidSuffixes string `db:"mid_suffixes"`
	// WhitelistMids mid 白名单，存储形态 ",11,22,"，空表示不限。
	WhitelistMids string `db:"whitelist_mids"`
	// Priority 判定次序：小者先；同 priority 时按 rule_id 升序，保证结果稳定。
	Priority int32 `db:"priority"`
	// State 1 生效、2 停用（停用不等于删除，见文件头）。
	State int32 `db:"state"`
	// OperatorID 最后操作人 admin_id（引用 operation，不复制资料）。
	OperatorID int64 `db:"operator_id"`
	// Remark 备注：为什么放这批人、预计什么时候收口。
	Remark string `db:"remark"`
	// StartAt 生效窗口起（Unix 秒），0 表示立即。
	StartAt int64 `db:"start_at"`
	// EndAt 生效窗口止（Unix 秒），0 表示不设截止。
	EndAt int64 `db:"end_at"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// PlatformList 供契约投影使用（rpc.RolloutRule.platforms）。
func (r *RolloutRule) PlatformList() []int32 {
	ids := ParseIDList(r.Platforms)
	out := make([]int32, 0, len(ids))
	for _, id := range ids {
		if id > int64(PlatformDesktop) {
			// 脏数据（历史遗留或人为改库）里超出值域的端标识被丢弃而不是报错：
			// 读路径必须能返回其余有效端，写路径已在落库前用 NormalizePlatformList 校验。
			continue
		}
		out = append(out, int32(id))
	}
	return out
}

// WhitelistList 供契约投影使用（rpc.RolloutRule.whitelist_mids）。
func (r *RolloutRule) WhitelistList() []int64 { return ParseIDList(r.WhitelistMids) }

// RolloutRuleFilter 后台列表条件（ListRolloutRules）。
type RolloutRuleFilter struct {
	// ConfigID 0 表示全部配置项（契约里 cfg_key 为空即此语义，此时 Version 仍可单独过滤）。
	ConfigID int64
	Version  int64
	State    int32
	Mode     int32
	Pn       int32
	Ps       int32
}

// RolloutRuleModel 抽象 ops_rollout_rule 表。
type RolloutRuleModel interface {
	// Upsert 按 (config_id, version, name) 写入：不存在则插入，存在则整行覆盖
	// （除了 rule_id/ctime）。created=false 表示这是一次更新而非新建。
	// 写入前调用 ValidateRuleShape 做形态校验（maxWhitelist 取硬上限 MaxWhitelistMids，
	// 配置里的 Rollout.MaxWhitelistMids 只能更严，不能更松）。
	Upsert(ctx context.Context, rule *RolloutRule) (int64, bool, error)
	// UpsertTx 与 Upsert 同一套校验与 SQL，跑在调用方事务里。
	// 灰度发布要「版本行 + 规则」一起落地：分开提交会留下一个没人放量也无人回收的孤版。
	UpsertTx(ctx context.Context, session sqlx.Session, rule *RolloutRule) (int64, bool, error)
	// FindByID 按主键查询；不存在返回 ErrRuleNotFound。
	FindByID(ctx context.Context, ruleID int64) (*RolloutRule, error)
	// FindByName 按幂等句柄 (config_id, version, name) 查询；不存在返回 (nil, nil)。
	FindByName(ctx context.Context, configID, version int64, name string) (*RolloutRule, error)
	// ListCandidates 取某配置项此刻**可能**命中的规则：state=ON 且落在时间窗口内，
	// 按 priority ASC, rule_id ASC 返回。
	// 排序在 SQL 里做（而不是 Go 里 sort），这样 PickRollout 的「首个命中」与
	// 库里声明的优先级次序不可能分叉；limit 必须存在，防止一个键挂满规则时扫全表。
	ListCandidates(ctx context.Context, configID, ts int64, limit int) ([]*RolloutRule, error)
	// List 后台分页查询（带 COUNT）。
	List(ctx context.Context, f RolloutRuleFilter) ([]*RolloutRule, int64, error)
	// SetState 启停规则：仅当当前 state == expectState 时生效，返回 updated。
	// updated=false 让调用方能区分「切换成功」与「已被别人改过」（ErrRuleStateUnchanged /
	// 需重读），不做无条件覆盖。本方法不做物理删除。
	SetState(ctx context.Context, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error)
	// SetStateTx 与 SetState 同一条条件 UPDATE，跑在调用方事务里。
	// 回滚必须「推指针 + 关掉全部在跑的灰度」原子完成：只推指针不关闸，
	// 灰度规则会把用户继续导回那个刚被判定有问题的版本。
	SetStateTx(ctx context.Context, session sqlx.Session, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error)
	// CountByVersion 某配置项某版本当前生效中的规则数（发布收口时判断能否安全下线）。
	CountByVersion(ctx context.Context, configID, version int64, state int32) (int64, error)
}

type defaultRolloutRuleModel struct {
	conn sqlx.SqlConn
}

// NewRolloutRuleModel 构造 ops_rollout_rule 的 sqlx 实现。
func NewRolloutRuleModel(conn sqlx.SqlConn) RolloutRuleModel {
	return &defaultRolloutRuleModel{conn: conn}
}

func (m *defaultRolloutRuleModel) Upsert(ctx context.Context, rule *RolloutRule) (int64, bool, error) {
	return upsertRule(ctx, m.conn, rule)
}

func (m *defaultRolloutRuleModel) UpsertTx(ctx context.Context, session sqlx.Session, rule *RolloutRule) (int64, bool, error) {
	return upsertRule(ctx, session, rule)
}

// upsertRule 是 Upsert/UpsertTx 的唯一实现体：两条入口共用形态校验、归一化、
// ON DUPLICATE KEY UPDATE 与 created 判定，不会出现「事务版少校验一层」的分叉。
func upsertRule(ctx context.Context, session sqlx.Session, rule *RolloutRule) (int64, bool, error) {
	if rule.Name == "" {
		return 0, false, ErrRuleNameRequired
	}
	// 白名单硬上限取包常量：配置只能收紧不能放宽（config_load_test 有对应用例）。
	if err := ValidateRuleShape(rule, MaxWhitelistMids); err != nil {
		return 0, false, err
	}
	suffixes, err := NormalizeMidSuffixes(rule.MidSuffixes)
	if err != nil {
		return 0, false, err
	}
	rule.MidSuffixes = suffixes
	if rule.State == 0 {
		// 默认停用：新建灰度规则就立刻放量不是任何人想要的语义，
		// 运营必须在 SetRolloutRuleState 上再点一次，形成「写规则」与「开闸」两步。
		rule.State = StateOff
	}
	if rule.Priority == 0 {
		rule.Priority = 100
	}
	if rule.Ctime == 0 {
		rule.Ctime = nowUnix()
	}
	rule.Mtime = rule.Ctime
	// 唯一键 uniq_config_version_name 冲突时走 UPDATE 分支：这是 upsert 的本体，
	// 因此重放同一 request_id 不会产生第二条同名规则（AGENTS.md §5 幂等写入要求）。
	// created 的判定交给 ROW_COUNT()：MySQL 对「更新为新值」返回 2、对「值未变化」返回 0，
	// 对「插入」返回 1；这里只区分「插入」与「非插入」，值未变化的重放按更新处理即可。
	res, err := session.ExecCtx(ctx,
		"INSERT INTO ops_rollout_rule ("+rolloutRuleColumns+") VALUES ("+placeholders(19)+")"+
			" ON DUPLICATE KEY UPDATE mode = VALUES(mode), percentage = VALUES(percentage),"+
			" app_version_min = VALUES(app_version_min), app_version_max = VALUES(app_version_max),"+
			" platforms = VALUES(platforms), mid_suffixes = VALUES(mid_suffixes),"+
			" whitelist_mids = VALUES(whitelist_mids), priority = VALUES(priority),"+
			" state = VALUES(state), operator_id = VALUES(operator_id), remark = VALUES(remark),"+
			" start_at = VALUES(start_at), end_at = VALUES(end_at), mtime = VALUES(mtime)",
		rule.RuleID, rule.ConfigID, rule.Version, rule.Name, rule.Mode, rule.Percentage,
		rule.AppVersionMin, rule.AppVersionMax, rule.Platforms, rule.MidSuffixes, rule.WhitelistMids,
		rule.Priority, rule.State, rule.OperatorID, rule.Remark, rule.StartAt, rule.EndAt,
		rule.Ctime, rule.Mtime)
	if err != nil {
		return 0, false, fmt.Errorf("ops_rollout_rule Upsert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, false, fmt.Errorf("ops_rollout_rule Upsert RowsAffected: %w", err)
	}
	created := aff == 1
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("ops_rollout_rule Upsert LastInsertId: %w", err)
	}
	if created && id > 0 {
		rule.RuleID = id
	} else {
		// 走 UPDATE 分支时 LastInsertId 无意义，必须回查主键，
		// 否则调用方会拿到 0 或上一次插入留下的脏 ID。
		exist, ferr := findRuleByName(ctx, session, rule.ConfigID, rule.Version, rule.Name)
		if ferr != nil {
			return 0, false, ferr
		}
		if exist == nil {
			return 0, false, ErrRuleNotFound
		}
		rule.RuleID = exist.RuleID
	}
	return rule.RuleID, created, nil
}

const rolloutRuleSelect = "SELECT " + rolloutRuleColumns + " FROM ops_rollout_rule"

func (m *defaultRolloutRuleModel) FindByID(ctx context.Context, ruleID int64) (*RolloutRule, error) {
	var row RolloutRule
	query := rolloutRuleSelect + " WHERE rule_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, ruleID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRuleNotFound
		}
		return nil, fmt.Errorf("ops_rollout_rule FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultRolloutRuleModel) FindByName(ctx context.Context, configID, version int64, name string) (*RolloutRule, error) {
	return findRuleByName(ctx, m.conn, configID, version, name)
}

func findRuleByName(ctx context.Context, session sqlx.Session, configID, version int64, name string) (*RolloutRule, error) {
	if name == "" {
		return nil, ErrRuleNameRequired
	}
	var row RolloutRule
	query := rolloutRuleSelect + " WHERE config_id = ? AND version = ? AND name = ? LIMIT 1"
	if err := session.QueryRowCtx(ctx, &row, query, configID, version, name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_rollout_rule FindByName: %w", err)
	}
	return &row, nil
}

func (m *defaultRolloutRuleModel) ListCandidates(ctx context.Context, configID, ts int64, limit int) ([]*RolloutRule, error) {
	if configID <= 0 {
		return nil, ErrConfigNotFound
	}
	if limit <= 0 {
		limit = MaxRolloutCandidates
	}
	var rows []*RolloutRule
	// 时间窗口下推到 SQL（start_at<=ts / end_at>ts / 0 表示不限），
	// 与 EffectiveAt 的判定等价；把明显过期的规则取回来再过滤只会浪费内存。
	query := rolloutRuleSelect +
		" WHERE config_id = ? AND state = ?" +
		" AND (start_at = 0 OR start_at <= ?) AND (end_at = 0 OR end_at > ?)" +
		" ORDER BY priority ASC, rule_id ASC LIMIT ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, configID, StateOn, ts, ts, limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_rollout_rule ListCandidates: %w", err)
	}
	return rows, nil
}

func (m *defaultRolloutRuleModel) List(ctx context.Context, f RolloutRuleFilter) ([]*RolloutRule, int64, error) {
	where, args := f.build()
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ops_rollout_rule "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_rollout_rule List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*RolloutRule
	// 后台列表按 (config_id, priority, rule_id) 聚簇：同一名次的规则排在相邻行，
	// 与运行时判定次序一致，运营在页面上看到的先后就是真实的先后。
	query := rolloutRuleSelect + where + " ORDER BY config_id ASC, priority ASC, rule_id ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_rollout_rule List: %w", err)
	}
	return rows, total, nil
}

func (f RolloutRuleFilter) build() (string, []any) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 4)
	if f.ConfigID > 0 {
		where += " AND config_id = ?"
		args = append(args, f.ConfigID)
	}
	if f.Version > 0 {
		where += " AND version = ?"
		args = append(args, f.Version)
	}
	if f.State > 0 {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Mode > 0 {
		where += " AND mode = ?"
		args = append(args, f.Mode)
	}
	return where, args
}

func (m *defaultRolloutRuleModel) SetState(ctx context.Context, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	return setRuleState(ctx, m.conn, ruleID, state, expectState, operatorID, ts)
}

func (m *defaultRolloutRuleModel) SetStateTx(ctx context.Context, session sqlx.Session, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	return setRuleState(ctx, session, ruleID, state, expectState, operatorID, ts)
}

func setRuleState(ctx context.Context, session sqlx.Session, ruleID int64, state, expectState int32, operatorID, ts int64) (bool, error) {
	if !ValidState(state) {
		return false, ErrRuleStateInvalid
	}
	if ruleID <= 0 {
		return false, ErrRuleNotFound
	}
	res, err := session.ExecCtx(ctx,
		"UPDATE ops_rollout_rule SET state = ?, mtime = ?, operator_id = ?"+
			" WHERE rule_id = ? AND state = ?",
		state, ts, operatorID, ruleID, expectState)
	if err != nil {
		return false, fmt.Errorf("ops_rollout_rule SetState: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_rollout_rule SetState RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultRolloutRuleModel) CountByVersion(ctx context.Context, configID, version int64, state int32) (int64, error) {
	query := "SELECT COUNT(*) FROM ops_rollout_rule WHERE config_id = ? AND version = ?"
	args := []any{configID, version}
	if state > 0 {
		query += " AND state = ?"
		args = append(args, state)
	}
	var n int64
	if err := m.conn.QueryRowCtx(ctx, &n, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("ops_rollout_rule CountByVersion: %w", err)
	}
	return n, nil
}

// MaxRolloutCandidates 单次解析取回的候选规则上限。
// 一个配置项挂几十条规则已经说明灰度策略该收敛了；不设上限的话，
// 一次 ResolveConfig 的耗时会随规则数线性增长，而这正是最需要它快的时刻。
const MaxRolloutCandidates = 50
