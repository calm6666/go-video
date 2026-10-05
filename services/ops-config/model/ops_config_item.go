package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// configItemColumns 是 ops_config_item 的列清单，必须与
// deploy/migrations/ops-config/000001_create_ops_config_publish_tables.sql 一致。
const configItemColumns = "config_id, cfg_key, scope, value_type, title, description, state," +
	" latest_version, epoch, operator_id, ctime, mtime"

// cfgKeyRe 是配置键格式：小写字母/数字/下划线/点，最长 64。
// 固化格式的理由：键名会进 Redis key、进日志、进灰度分桶哈希，
// 允许任意字符会让「同一个键」出现多种写法，缓存与排障都会分裂。
var cfgKeyRe = regexp.MustCompile(`^[a-z0-9_.]{1,64}$`)

// ValidCfgKey 判定配置键格式。
func ValidCfgKey(s string) bool { return cfgKeyRe.MatchString(s) }

// ConfigItem 对应 ops_config_item 表：一个「键 + 生效范围」的配置项主记录。
//
// 与 operation.op_config 的分工（结论见 services/ops-config/README.md）：
// 本表是**发布式配置**——值不在本行上，而在 ops_config_version 的不可变快照里，
// latest_version 指向当前正式版本，epoch 是缓存代次。
// 因此本行只承载「身份 + 当前指针」，改值必然产生新版本行，历史永不被改写。
type ConfigItem struct {
	// ConfigID 自增主键，跨表引用用的就是它（灰度规则、版本快照都挂在这里）。
	ConfigID int64 `db:"config_id"`
	// CfgKey 配置键，如 home.topic.enabled。
	CfgKey string `db:"cfg_key"`
	// Scope 生效范围：global 或端标识（android/ios/harmony/desktop）。
	Scope string `db:"scope"`
	// ValueType 值类型：1 string、2 int、3 bool、4 json。发布时按类型校验值。
	ValueType int32 `db:"value_type"`
	// Title 中文名，后台展示用。
	Title string `db:"title"`
	// Description 说明：这个键影响什么、改了会怎样。
	Description string `db:"description"`
	// State 1 启用、2 停用。停用的键在 ResolveConfig 里按「未命中」处理。
	State int32 `db:"state"`
	// LatestVersion 当前正式版本号，0 表示从未发布（此时没有任何可读版本）。
	// 它同时是发布接口的乐观锁依据：expect_version 就是比对这一列。
	LatestVersion int64 `db:"latest_version"`
	// Epoch 缓存代次：每次发布与每次 RefreshCache 都 +1。
	// 读取方（网关）把它带在响应里，本地缓存可以据此判断自己拿的是哪一代数据；
	// 单靠 TTL 收敛的话，一次错误放量最多要等一个 TTL 才能被踢掉。
	Epoch int64 `db:"epoch"`
	// OperatorID 最后操作人 admin_id（引用 operation，不复制资料）。
	OperatorID int64 `db:"operator_id"`
	// Ctime 创建时间（Unix 秒）。
	Ctime int64 `db:"ctime"`
	// Mtime 最后修改时间（Unix 秒）。
	Mtime int64 `db:"mtime"`
}

// ConfigItemFilter 后台列表条件。
type ConfigItemFilter struct {
	Scope   string
	Keyword string
	State   int32
	Pn      int32
	Ps      int32
}

// ConfigItemModel 抽象 ops_config_item 表。
type ConfigItemModel interface {
	// Insert 新建配置项。冲突时返回 ErrConfigExists（不依赖驱动专有错误码）。
	Insert(ctx context.Context, item *ConfigItem) (int64, error)
	// FindOne 按 (cfg_key, scope) 查询；不存在返回 (nil, nil)，由 repository 决定语义。
	FindOne(ctx context.Context, cfgKey, scope string) (*ConfigItem, error)
	// FindByID 按主键查询；不存在返回 ErrConfigNotFound。
	FindByID(ctx context.Context, configID int64) (*ConfigItem, error)
	// FindByKeys 批量按 (cfg_key, scope) 查询（BatchResolveConfig 一次取回，避免 N 次往返）。
	FindByKeys(ctx context.Context, cfgKeys []string, scope string) (map[string]*ConfigItem, error)
	// AdvanceRelease 发布推进：仅当库里 latest_version == expectLatest 时，
	// 把 latest_version 置为 newLatest 并 epoch + 1。
	// 返回 updated=false 表示版本被他人抢先推进（repository 转成 ErrVersionConflict），
	// 绝不静默覆盖——那等于两个人的发布只有一人留下痕迹。
	AdvanceRelease(ctx context.Context, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error)
	// AdvanceReleaseTx 与 AdvanceRelease 同一条 SQL，差别只在它跑在调用方给的事务里。
	// 为什么必须存在：PublishConfig 的「插版本行」与「推指针」必须同生共死，
	// 走两条独立连接就会出现「版本行已提交、指针没动」的半发布（AGENTS.md §8）。
	AdvanceReleaseTx(ctx context.Context, session sqlx.Session, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error)
	// BumpEpoch 递增指定配置项的缓存代次（RefreshCache 用），返回受影响行数。
	BumpEpoch(ctx context.Context, configIDs []int64, ts int64) (int64, error)
	// BumpAllEpoch 递增所有启用配置项的缓存代次（target=all 的兜底刷新），返回行数。
	BumpAllEpoch(ctx context.Context, state int32, ts int64) (int64, error)
	// List 分页查询。
	List(ctx context.Context, f ConfigItemFilter) ([]*ConfigItem, int64, error)
}

type defaultConfigItemModel struct {
	conn sqlx.SqlConn
}

// NewConfigItemModel 构造 ops_config_item 的 sqlx 实现。
func NewConfigItemModel(conn sqlx.SqlConn) ConfigItemModel {
	return &defaultConfigItemModel{conn: conn}
}

func (m *defaultConfigItemModel) Insert(ctx context.Context, item *ConfigItem) (int64, error) {
	if !ValidCfgKey(item.CfgKey) {
		return 0, ErrConfigKeyInvalid
	}
	if !ValidScope(item.Scope) {
		return 0, ErrScopeUnknown
	}
	if !ValidValueType(item.ValueType) {
		return 0, ErrValueTypeUnsupported
	}
	if item.Ctime == 0 {
		item.Ctime = nowUnix()
	}
	item.Mtime = item.Ctime
	if item.State == 0 {
		// 默认启用：新建即「刚发布就不可读」不是任何人想要的语义。
		// 需要「先配好不上线」请用 scope 或灰度规则，而不是把键停掉。
		item.State = StateOn
	}
	// 唯一键冲突时 ON DUPLICATE KEY UPDATE 为刻意空更新：
	// RowsAffected == 0 即冲突，不依赖 driver 专有错误类型。
	res, err := m.conn.ExecCtx(ctx,
		"INSERT INTO ops_config_item ("+configItemColumns+") VALUES ("+placeholders(12)+")"+
			" ON DUPLICATE KEY UPDATE mtime = mtime",
		item.ConfigID, item.CfgKey, item.Scope, item.ValueType, item.Title, item.Description,
		item.State, item.LatestVersion, item.Epoch, item.OperatorID, item.Ctime, item.Mtime)
	if err != nil {
		return 0, fmt.Errorf("ops_config_item Insert: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_config_item Insert RowsAffected: %w", err)
	}
	if aff == 0 {
		return 0, ErrConfigExists
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("ops_config_item Insert LastInsertId: %w", err)
	}
	item.ConfigID = id
	return id, nil
}

const configItemSelect = "SELECT " + configItemColumns + " FROM ops_config_item"

func (m *defaultConfigItemModel) FindOne(ctx context.Context, cfgKey, scope string) (*ConfigItem, error) {
	if cfgKey == "" {
		return nil, ErrConfigKeyRequired
	}
	var row ConfigItem
	query := configItemSelect + " WHERE cfg_key = ? AND scope = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, cfgKey, scope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("ops_config_item FindOne: %w", err)
	}
	return &row, nil
}

func (m *defaultConfigItemModel) FindByID(ctx context.Context, configID int64) (*ConfigItem, error) {
	var row ConfigItem
	query := configItemSelect + " WHERE config_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &row, query, configID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("ops_config_item FindByID: %w", err)
	}
	return &row, nil
}

func (m *defaultConfigItemModel) FindByKeys(ctx context.Context, cfgKeys []string, scope string) (map[string]*ConfigItem, error) {
	out := make(map[string]*ConfigItem, len(cfgKeys))
	if len(cfgKeys) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(cfgKeys)+1)
	for _, k := range cfgKeys {
		args = append(args, k)
	}
	var rows []*ConfigItem
	query := configItemSelect + " WHERE cfg_key IN (" + placeholders(len(cfgKeys)) + ") AND scope = ?"
	args = append(args, scope)
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return nil, fmt.Errorf("ops_config_item FindByKeys: %w", err)
	}
	for _, r := range rows {
		out[r.CfgKey] = r
	}
	return out, nil
}

func (m *defaultConfigItemModel) AdvanceRelease(ctx context.Context, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	return advanceRelease(ctx, m.conn, configID, expectLatest, newLatest, operatorID, ts)
}

func (m *defaultConfigItemModel) AdvanceReleaseTx(ctx context.Context, session sqlx.Session, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	return advanceRelease(ctx, session, configID, expectLatest, newLatest, operatorID, ts)
}

// advanceRelease 是 AdvanceRelease 与 AdvanceReleaseTx 的唯一实现体。
// session 同时接受 sqlx.SqlConn（自动提交）与 sqlx.Session（事务内），
// 这样两条入口共用一条 SQL 与一套 RowsAffected 语义，不会出现「事务版忘了 CAS」。
func advanceRelease(ctx context.Context, session sqlx.Session, configID, expectLatest, newLatest, operatorID, ts int64) (bool, error) {
	if newLatest <= expectLatest {
		// 版本号必须单调递增：回滚也是「发布一个更新的版本号」，
		// 否则历史序列会出现同一号码的两种内容，审计无法解释。
		return false, fmt.Errorf("ops_config_item AdvanceRelease: %w", ErrVersionConflict)
	}
	res, err := session.ExecCtx(ctx,
		"UPDATE ops_config_item SET latest_version = ?, epoch = epoch + 1, operator_id = ?, mtime = ?"+
			" WHERE config_id = ? AND latest_version = ?",
		newLatest, operatorID, ts, configID, expectLatest)
	if err != nil {
		return false, fmt.Errorf("ops_config_item AdvanceRelease: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("ops_config_item AdvanceRelease RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultConfigItemModel) BumpEpoch(ctx context.Context, configIDs []int64, ts int64) (int64, error) {
	if len(configIDs) == 0 {
		return 0, nil
	}
	args := make([]any, 0, len(configIDs)+1)
	for _, id := range configIDs {
		args = append(args, id)
	}
	query := "UPDATE ops_config_item SET epoch = epoch + 1, mtime = ? WHERE config_id IN (" +
		placeholders(len(configIDs)) + ")"
	args = append([]any{ts}, args...)
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ops_config_item BumpEpoch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_config_item BumpEpoch RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultConfigItemModel) BumpAllEpoch(ctx context.Context, state int32, ts int64) (int64, error) {
	query := "UPDATE ops_config_item SET epoch = epoch + 1, mtime = ?"
	args := []any{ts}
	if state > 0 {
		query += " WHERE state = ?"
		args = append(args, state)
	}
	res, err := m.conn.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("ops_config_item BumpAllEpoch: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("ops_config_item BumpAllEpoch RowsAffected: %w", err)
	}
	return n, nil
}

func (m *defaultConfigItemModel) List(ctx context.Context, f ConfigItemFilter) ([]*ConfigItem, int64, error) {
	where, args := f.build()
	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(*) FROM ops_config_item "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("ops_config_item List count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}
	listArgs := append(append([]any{}, args...), f.Ps, (f.Pn-1)*f.Ps)
	var rows []*ConfigItem
	// 排序固定 cfg_key, scope 升序：后台要的是「可对照的字典序清单」，
	// 按 mtime 排会让翻页结果在有人发布时抖动。
	query := configItemSelect + where + " ORDER BY cfg_key ASC, scope ASC LIMIT ? OFFSET ?"
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("ops_config_item List: %w", err)
	}
	return rows, total, nil
}

// build 拼 WHERE。Keyword 只在 cfg_key 与 title 上做前缀/包含匹配：
// 配置项总量在千级，不需要全文索引；引号与反斜杠按参数绑定处理，不做字符串拼接。
func (f ConfigItemFilter) build() (string, []any) {
	where := "WHERE 1 = 1"
	args := make([]any, 0, 3)
	if f.Scope != "" {
		where += " AND scope = ?"
		args = append(args, f.Scope)
	}
	if f.State > 0 {
		where += " AND state = ?"
		args = append(args, f.State)
	}
	if f.Keyword != "" {
		where += " AND (cfg_key LIKE ? OR title LIKE ?)"
		like := "%" + escapeLike(f.Keyword) + "%"
		args = append(args, like, like)
	}
	return where, args
}

// escapeLike 转义 LIKE 通配符，避免有人把配置键写成 "%" 就捞出全表。
func escapeLike(s string) string {
	out := strings.ReplaceAll(s, "\\", "\\\\")
	out = strings.ReplaceAll(out, "%", "\\%")
	out = strings.ReplaceAll(out, "_", "\\_")
	return out
}
