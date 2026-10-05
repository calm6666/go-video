package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 会员档位常量，与 rpc.VipType 取值严格一致。
// 档位序为真：PREMIUM(1) < PREMIUM_PLUS(2)，超级大会员权益是大会员的超集。
// 变更取值会破坏已落库数据与跨服务引用，禁止重排。
const (
	// VipTypePremium 大会员。
	VipTypePremium int32 = 1
	// VipTypePremiumPlus 超级大会员。
	VipTypePremiumPlus int32 = 2
)

// ValidVipType 判定档位是否可落库（0/负数/越界都不接受）。
func ValidVipType(v int32) bool { return v == VipTypePremium || v == VipTypePremiumPlus }

// 套餐售卖状态，与 rpc.PlanSaleState 取值一致。
// DRAFT 不对外可见也不可下单；OFF_SALE 只对存量续费可见（终端面由 ListPlans 屏蔽，
// 需要看草稿/已下架必须走 ListPlansAdmin）。
const (
	// PlanStateDraft 草稿。
	PlanStateDraft int32 = 1
	// PlanStateOnSale 在售。
	PlanStateOnSale int32 = 2
	// PlanStateOffSale 已下架。
	PlanStateOffSale int32 = 3
)

// ValidPlanState 判定套餐状态是否可落库。
func ValidPlanState(s int32) bool {
	return s == PlanStateDraft || s == PlanStateOnSale || s == PlanStateOffSale
}

// 平台位掩码位值，由 rpc.PlanPlatform 枚举（1..5）折算而来：
// 枚举本身是序数不是位值，落库存 platform_mask，Android=1<<0。
const (
	// PlatformBitAndroid Android 端。
	PlatformBitAndroid uint32 = 1 << 0
	// PlatformBitIOS iOS 端。
	PlatformBitIOS uint32 = 1 << 1
	// PlatformBitHarmony HarmonyOS 端。
	PlatformBitHarmony uint32 = 1 << 2
	// PlatformBitDesktop 电脑客户端。
	PlatformBitDesktop uint32 = 1 << 3
	// PlatformBitWeb Web 端（管理后台购买页等）。
	PlatformBitWeb uint32 = 1 << 4
)

// PlatformMaskOf 把单个 rpc.PlanPlatform 序数折算为位值；非法序数返回 error。
// 不做静默忽略：漏掉一个平台会让该端看不到套餐，且没有任何报错线索。
func PlatformMaskOf(p int32) (uint32, error) {
	switch p {
	case 1:
		return PlatformBitAndroid, nil
	case 2:
		return PlatformBitIOS, nil
	case 3:
		return PlatformBitHarmony, nil
	case 4:
		return PlatformBitDesktop, nil
	case 5:
		return PlatformBitWeb, nil
	default:
		return 0, fmt.Errorf("%w: %d", ErrInvalidPlatform, p)
	}
}

// PlatformMask 把平台序数列表折叠为位掩码，重复值等价，非法值整体报错。
func PlatformMask(platforms []int32) (uint32, error) {
	var mask uint32
	for _, p := range platforms {
		bit, err := PlatformMaskOf(p)
		if err != nil {
			return 0, err
		}
		mask |= bit
	}
	return mask, nil
}

// PlatformsOfMask 把位掩码还原为平台序数列表（升序），供 RPC 投影使用。
func PlatformsOfMask(mask uint32) []int32 {
	out := make([]int32, 0, 5)
	for i, bit := range []uint32{PlatformBitAndroid, PlatformBitIOS, PlatformBitHarmony, PlatformBitDesktop, PlatformBitWeb} {
		if mask&bit != 0 {
			out = append(out, int32(i+1))
		}
	}
	return out
}

// 套餐变更类型，与 mb_plan_change_log.change_type 一致。
const (
	// PlanChangeUpsert 草稿内容新建/修改（含价格）。
	PlanChangeUpsert = "UPSERT"
	// PlanChangeState 上下架。
	PlanChangeState = "STATE"
)

// 套餐列宽（与 mb_plan DDL 逐字对齐）：logic 先按此校验再落库，
// 避免把超长值交给 MySQL 报「Data too long」——那种错误既难排也污染台账。
const (
	// MaxPlanCodeLength plan_code 最大字符数。
	MaxPlanCodeLength = 64
	// MaxPlanNameLength name 最大字符数。
	MaxPlanNameLength = 64
	// MaxPlanDescLength description 最大字符数。
	MaxPlanDescLength = 255
	// MaxOperatorLength operator/updated_by 最大字符数。
	MaxOperatorLength = 64
	// MaxBizNoLength biz_order_no / payment_no 引用最大字符数。
	MaxBizNoLength = 64
	// MaxAutoRenewChannelLength auto_renew_channel 最大字符数。
	MaxAutoRenewChannelLength = 32
	// AllowedAutoRenewChannel 沙箱环境唯一允许的签约渠道。
	// 真实代扣渠道一概不接：落一个永远不会生效的协议位比拒绝更危险。
	AllowedAutoRenewChannel = "SANDBOX"
)

// planColumns 是 mb_plan 的完整列清单，
// 与 deploy/migrations/membership/000001_create_membership_tables.sql 一一对应。
const planColumns = "plan_id, plan_code, name, description, vip_type, duration_days, unit_count, " +
	"price_minor, prom_price_minor, currency, platform_mask, auto_renew_supported, state, version, " +
	"created_by, updated_by, ctime, mtime"

// Plan 套餐（SKU）行。价格以最小货币单位（分）计，int64/int32 全程不出现浮点。
type Plan struct {
	PlanID             int64  `db:"plan_id"`              // 自增主键
	PlanCode           string `db:"plan_code"`            // 对外稳定编码（唯一，下单用它）
	Name               string `db:"name"`                 // 展示名
	Description        string `db:"description"`          // 描述
	VipType            int32  `db:"vip_type"`             // 档位，见 VipType*
	DurationDays       int32  `db:"duration_days"`        // 单个售卖单位时长（天）
	UnitCount          int32  `db:"unit_count"`           // 一次购买含几个 duration_days
	PriceMinor         int64  `db:"price_minor"`          // 原价（分）
	PromPriceMinor     int64  `db:"prom_price_minor"`     // 促销价（分），0 表示无促销
	Currency           string `db:"currency"`             // 币种，本轮固定 CNY
	PlatformMask       uint32 `db:"platform_mask"`        // 可见平台位掩码
	AutoRenewSupported int32  `db:"auto_renew_supported"` // 0/1 是否支持签约自动续费
	State              int32  `db:"state"`                // 售卖状态，见 PlanState*
	Version            int64  `db:"version"`              // CAS 位
	CreatedBy          string `db:"created_by"`           // 创建者
	UpdatedBy          string `db:"updated_by"`           // 最后修改者
	Ctime              int64  `db:"ctime"`                // 创建时间（Unix 秒）
	Mtime              int64  `db:"mtime"`                // 修改时间（Unix 秒）
}

// TotalDurationDays 是买一个该套餐得到的总天数（duration_days * unit_count）。
func (p *Plan) TotalDurationDays() int64 {
	return int64(p.DurationDays) * int64(p.UnitCount)
}

// IsOnSale 是否在售（只有在售套餐可被终端看到、可被下单）。
func (p *Plan) IsOnSale() bool { return p.State == PlanStateOnSale }

// PlatformList 返回可见平台序数列表，供 RPC 投影。
func (p *Plan) PlatformList() []int32 { return PlatformsOfMask(p.PlatformMask) }

// PlanQuery 是 admin 面套餐分页查询条件；零值字段表示不过滤。
type PlanQuery struct {
	State   int32
	VipType int32
	Keyword string // 匹配 plan_code 或 name 前缀
	Offset  int64
	Limit   int64
}

// PlanModel mb_plan 表查询与写入接口。
type PlanModel interface {
	// InsertTx 新建套餐，返回 plan_id；plan_code 唯一索引冲突需由调用方识别。
	InsertTx(ctx context.Context, session sqlx.Session, p *Plan) (int64, error)
	// UpdateTx 以 expectedVersion 为条件整行更新草稿字段（CAS）。
	// 返回 false 表示版本已被并发修改，本次未生效。
	UpdateTx(ctx context.Context, session sqlx.Session, p *Plan, expectedVersion int64) (bool, error)
	// CASStateTx 上下架：仅当 state=fromState 且 version=expectedVersion 时推进。
	CASStateTx(ctx context.Context, session sqlx.Session, planID int64, fromState, toState int32, expectedVersion int64, updatedBy string) (bool, error)
	// FindOne 按 plan_id 查询；不存在返回 (nil, nil)。
	FindOne(ctx context.Context, planID int64) (*Plan, error)
	// FindByCode 按对外编码查询；不存在返回 (nil, nil)。
	FindByCode(ctx context.Context, code string) (*Plan, error)
	// ListOnSale 终端面：只返回 state=ON_SALE 的套餐；
	// platformBit!=0 时再按平台位与过滤；vipType!=0 时按档位过滤。
	ListOnSale(ctx context.Context, platformBit uint32, vipType int32) ([]*Plan, error)
	// ListAdmin 运营面分页查询（含草稿与已下架），返回当页数据与总数。
	ListAdmin(ctx context.Context, q PlanQuery) ([]*Plan, int64, error)
}

type defaultPlanModel struct {
	conn sqlx.SqlConn
}

// NewPlanModel 创建 PlanModel 实现。
func NewPlanModel(conn sqlx.SqlConn) PlanModel {
	return &defaultPlanModel{conn: conn}
}

func (m *defaultPlanModel) InsertTx(ctx context.Context, session sqlx.Session, p *Plan) (int64, error) {
	const query = "INSERT INTO mb_plan (plan_code, name, description, vip_type, duration_days, unit_count, " +
		"price_minor, prom_price_minor, currency, platform_mask, auto_renew_supported, state, version, " +
		"created_by, updated_by, ctime, mtime) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	now := nowUnix()
	if p.Ctime == 0 {
		p.Ctime = now
	}
	p.Mtime = now
	p.Version = 1
	args := []interface{}{p.PlanCode, p.Name, p.Description, p.VipType, p.DurationDays, p.UnitCount,
		p.PriceMinor, p.PromPriceMinor, p.Currency, p.PlatformMask, p.AutoRenewSupported, p.State, p.Version,
		p.CreatedBy, p.UpdatedBy, p.Ctime, p.Mtime}

	ex := execerOr(session, m.conn)
	res, err := ex.ExecCtx(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("mb_plan Insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mb_plan Insert LastInsertId: %w", err)
	}
	p.PlanID = id
	return id, nil
}

func (m *defaultPlanModel) UpdateTx(ctx context.Context, session sqlx.Session, p *Plan, expectedVersion int64) (bool, error) {
	// version = expectedVersion 是 CAS 条件；updated_by/ctime 不在可改集合内。
	const query = "UPDATE mb_plan SET plan_code = ?, name = ?, description = ?, vip_type = ?, duration_days = ?, " +
		"unit_count = ?, price_minor = ?, prom_price_minor = ?, currency = ?, platform_mask = ?, " +
		"auto_renew_supported = ?, version = version + 1, updated_by = ?, mtime = ? " +
		"WHERE plan_id = ? AND version = ?"
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query,
		p.PlanCode, p.Name, p.Description, p.VipType, p.DurationDays, p.UnitCount,
		p.PriceMinor, p.PromPriceMinor, p.Currency, p.PlatformMask, p.AutoRenewSupported,
		p.UpdatedBy, nowUnix(), p.PlanID, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("mb_plan Update: %w", err)
	}
	return rowsAffected(res, "mb_plan Update")
}

func (m *defaultPlanModel) CASStateTx(ctx context.Context, session sqlx.Session, planID int64, fromState, toState int32, expectedVersion int64, updatedBy string) (bool, error) {
	const query = "UPDATE mb_plan SET state = ?, version = version + 1, updated_by = ?, mtime = ? " +
		"WHERE plan_id = ? AND state = ? AND version = ?"
	res, err := execerOr(session, m.conn).ExecCtx(ctx, query, toState, updatedBy, nowUnix(), planID, fromState, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("mb_plan CASState: %w", err)
	}
	return rowsAffected(res, "mb_plan CASState")
}

func (m *defaultPlanModel) FindOne(ctx context.Context, planID int64) (*Plan, error) {
	var p Plan
	query := "SELECT " + planColumns + " FROM mb_plan WHERE plan_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, planID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mb_plan FindOne: %w", err)
	}
	return &p, nil
}

func (m *defaultPlanModel) FindByCode(ctx context.Context, code string) (*Plan, error) {
	var p Plan
	query := "SELECT " + planColumns + " FROM mb_plan WHERE plan_code = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &p, query, code); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mb_plan FindByCode: %w", err)
	}
	return &p, nil
}

func (m *defaultPlanModel) ListOnSale(ctx context.Context, platformBit uint32, vipType int32) ([]*Plan, error) {
	var (
		where = "state = ?"
		args  = []interface{}{PlanStateOnSale}
	)
	if platformBit != 0 {
		where += " AND platform_mask & ? <> 0"
		args = append(args, platformBit)
	}
	if vipType != 0 {
		where += " AND vip_type = ?"
		args = append(args, vipType)
	}
	query := "SELECT " + planColumns + " FROM mb_plan WHERE " + where + " ORDER BY plan_id ASC LIMIT 200"

	var rows []*Plan
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("mb_plan ListOnSale: %w", err)
	}
	return rows, nil
}

func (m *defaultPlanModel) ListAdmin(ctx context.Context, q PlanQuery) ([]*Plan, int64, error) {
	where, args := planQueryClause(q)

	var total int64
	if err := m.conn.QueryRowCtx(ctx, &total, "SELECT COUNT(1) FROM mb_plan WHERE "+where, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			total = 0
		} else {
			return nil, 0, fmt.Errorf("mb_plan ListAdmin count: %w", err)
		}
	}
	if total == 0 {
		return nil, 0, nil
	}

	query := "SELECT " + planColumns + " FROM mb_plan WHERE " + where +
		" ORDER BY plan_id DESC LIMIT ? OFFSET ?"
	queryArgs := append(append([]interface{}{}, args...), q.Limit, q.Offset)

	var rows []*Plan
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, queryArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("mb_plan ListAdmin: %w", err)
	}
	return rows, total, nil
}

// planQueryClause 构造 admin 查询条件；keyword 走前缀匹配并转义 LIKE 通配符，
// 避免运营输入 "%" 变成全表扫（前缀命中 uniq_plan_code/name 索引）。
func planQueryClause(q PlanQuery) (string, []interface{}) {
	var (
		conds = make([]string, 0, 4)
		args  []interface{}
	)
	conds = append(conds, "1 = 1")
	if q.State != 0 {
		conds = append(conds, "state = ?")
		args = append(args, q.State)
	}
	if q.VipType != 0 {
		conds = append(conds, "vip_type = ?")
		args = append(args, q.VipType)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		conds = append(conds, "(plan_code LIKE ? OR name LIKE ?)")
		like := likePrefix(kw)
		args = append(args, like, like)
	}
	return strings.Join(conds, " AND "), args
}

// likePrefix 把用户输入转成安全的「前缀匹配」pattern。
func likePrefix(kw string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(kw) + "%"
}

// execer 抽象 sqlx.SqlConn 与 sqlx.Session 的 ExecCtx，避免复制写入逻辑。
type execer interface {
	ExecCtx(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}

// execerOr 返回本次写入实际使用的执行器：session 非空即事务内执行，
// 否则回落到连接自身（单条语句自带隐式提交）。
func execerOr(session sqlx.Session, conn sqlx.SqlConn) execer {
	if session != nil {
		return session
	}
	return conn
}

// noRowsAsNil 把「按唯一键查一行、不存在是正常结论」的查询归一化为 (nil, nil)；
// 其它错误原样带上操作名上抛，交给 logic 记日志——绝不把 DB 故障当成「没有这行」。
func noRowsAsNil[T any](row *T, err error, op string) (*T, error) {
	if err == nil {
		return row, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return nil, fmt.Errorf("%s: %w", op, err)
}

// rowsAffected 把 UPDATE 的受影响行数转成 CAS 命中与否；
// 影响 0 行不是错误，而是「并发已改动」的信号，由调用方决定重试或返回冲突。
func rowsAffected(res sql.Result, op string) (bool, error) {
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return aff > 0, nil
}
