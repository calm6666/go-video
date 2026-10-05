package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 订单状态常量，与 to_order.state 列和 rpc.OrderState 取值严格一致。
// 变更取值会破坏已落库数据与下游对订单号的引用语义，禁止重排。
const (
	// StateCreated 已建单，尚未受理支付。
	StateCreated int32 = 1
	// StatePaying 已受理建单，正在/等待支付结论。
	StatePaying int32 = 2
	// StatePaid 支付结论已绑定（payment 回 PAID）。
	StatePaid int32 = 3
	// StateFulfilling 履约中：已尝试发放，结论未回。
	StateFulfilling int32 = 4
	// StateFulfilled 履约完成，权益/硬币已发放。
	StateFulfilled int32 = 5
	// StateCancelled 未支付前取消（终态）。
	StateCancelled int32 = 6
	// StateFailed 受理成功但无法履约，等人工介入（详见状态机注释）。
	StateFailed int32 = 7
	// StateRefundRequested 退款已申请，等审批。
	StateRefundRequested int32 = 8
	// StateRefundApproved 款已退但权益可能尚未回收（差异写在 fulfill_detail）。
	StateRefundApproved int32 = 9
	// StateRefunded 款已退且权益已回收（终态）。
	StateRefunded int32 = 10
	// StateRefundRejected 退款被驳回（本轮按「回到原状态」实现，见 CanTransition 注释）。
	StateRefundRejected int32 = 11
)

// 履约结果常量，与 to_order.fulfill_state 列和 rpc.FulfillState 一致。
// 履约状态与订单状态是两件事：前者回答「该给的东西给到没有」，后者回答「订单走到哪一步」。
const (
	FulfillPending int32 = 1
	FulfillDone    int32 = 2
	FulfillFailed  int32 = 3
)

// 业务类型常量，与 to_order.biz_type 列和 rpc.OrderBizType 一致。
const (
	BizMembership int32 = 1
	BizCoinPack   int32 = 2
)

// 支付方式常量，与 to_order.pay_method 列和 rpc.PayMethod 一致。
// 只有这两档，都不产生真实资金移动（沙箱台账）。
const (
	PayBalance int32 = 1
	PaySandbox int32 = 2
)

// 端常量，与 to_order.platform 列和 rpc.Platform 一致。
const (
	PlatformAndroid int32 = 1
	PlatformIOS     int32 = 2
	PlatformHarmony int32 = 3
	PlatformDesktop int32 = 4
	PlatformWeb     int32 = 5
)

// ValidState 判断状态取值落在已定义区间。
func ValidState(v int32) bool { return v >= StateCreated && v <= StateRefundRejected }

// ValidBizType 判断业务类型（只有两档商业订单，没有第三种）。
func ValidBizType(v int32) bool { return v == BizMembership || v == BizCoinPack }

// ValidPayMethod 判断支付方式（沙箱两档）。
func ValidPayMethod(v int32) bool { return v == PayBalance || v == PaySandbox }

// ValidPlatform 判断端。
func ValidPlatform(v int32) bool {
	return v >= PlatformAndroid && v <= PlatformWeb
}

// StateName 返回状态可读名，供错误信息与台账理由使用
// （不打印枚举数字，运营看到 PAID 才知道自己被什么挡住）。
func StateName(v int32) string {
	switch v {
	case StateCreated:
		return "CREATED"
	case StatePaying:
		return "PAYING"
	case StatePaid:
		return "PAID"
	case StateFulfilling:
		return "FULFILLING"
	case StateFulfilled:
		return "FULFILLED"
	case StateCancelled:
		return "CANCELLED"
	case StateFailed:
		return "FAILED"
	case StateRefundRequested:
		return "REFUND_REQUESTED"
	case StateRefundApproved:
		return "REFUND_APPROVED"
	case StateRefunded:
		return "REFUNDED"
	case StateRefundRejected:
		return "REFUND_REJECTED"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", v)
	}
}

// CanTransition 定义订单状态机的合法迁移，逐条对齐 rpc/tradeorder.proto 文件头注释：
//
//	CREATED      → PAYING | CANCELLED
//	PAYING       → PAID | CANCELLED
//	PAID         → FULFILLING | FAILED | REFUND_REQUESTED
//	FULFILLING   → FULFILLED | FAILED | PAID（重试回退：见下）
//	FULFILLED    → REFUND_REQUESTED
//	REFUND_REQUESTED → REFUND_APPROVED | 回到申请前的原状态（FULFILLED 或 PAID）
//	REFUND_APPROVED  → REFUNDED
//
// 三点必须读清楚，否则会把实现当成「放松约束」：
//  1. 同态迁移只放开 FULFILLING → FULFILLING：它代表一次「再试一轮履约」的尝试，
//     必须落一行台账并让 fulfill_attempts 可见增长，否则重试历史不可审计。
//     其余同态视为幂等空操作，由 logic 短路返回 duplicated，不走状态机。
//  2. proto 注释只写了「REFUND_REQUESTED → FULFILLED（驳回退款，回到原状态）」，
//     但退款也可以从 PAID 发起；这里按注释的意图实现为「回到原状态」，
//     原状态由 to_order_event 台账反查（logic 负责查出后传入），model 不猜。
//  3. FAILED 是 proto 列出的无出边状态：FulfillOrder 只受理 PAID/FULFILLING，
//     所以 FAILED 单在本轮契约下无法自助重试，必须人工介入或等契约补 FAILED → FULFILLING。
//     这条缺口已写进 README 与交付报告，不在代码里偷偷放宽。
func CanTransition(from, to int32) bool {
	if !ValidState(from) || !ValidState(to) {
		return false
	}
	if from == to {
		return from == StateFulfilling
	}
	switch from {
	case StateCreated:
		return to == StatePaying || to == StateCancelled
	case StatePaying:
		return to == StatePaid || to == StateCancelled
	case StatePaid:
		return to == StateFulfilling || to == StateFailed || to == StateRefundRequested
	case StateFulfilling:
		return to == StateFulfilled || to == StateFailed || to == StatePaid
	case StateFulfilled:
		return to == StateRefundRequested
	case StateRefundRequested:
		// 驳回退款按「回到申请前的原状态」实现（PAID 或 FULFILLED），见上面的注释 2；
		// 刻意不给 REFUND_REQUESTED → REFUND_REJECTED：那条边一旦走到就是死胡同
		// （proto 迁移表没给 REFUND_REJECTED 任何出边），而驳回事实由 to_order_event 留证。
		return to == StateRefundApproved || to == StateFulfilled || to == StatePaid
	case StateRefundApproved:
		return to == StateRefunded
	default:
		// CANCELLED / FAILED / REFUNDED / REFUND_REJECTED 为终态或人工态，无出边。
		return false
	}
}

// IsPaidOrLater 判断订单是否已跨过「钱已受理」这一步。
// BindPayment 的幂等语义（已 PAID 及之后返回 duplicated=true）依赖它。
func IsPaidOrLater(state int32) bool {
	switch state {
	case StatePaid, StateFulfilling, StateFulfilled, StateRefundRequested,
		StateRefundApproved, StateRefunded, StateRefundRejected, StateFailed:
		return true
	default:
		return false
	}
}

// maxFulfillDetailLen 是 to_order.fulfill_detail 的列宽（VARCHAR(500)），
// 与 deploy/migrations/trade-order/000001_create_trade_order_tables.sql 对齐。
const maxFulfillDetailLen = 500

// orderColumns 是 to_order 的完整列清单，
// 与 deploy/migrations/trade-order/000001_create_trade_order_tables.sql 一一对应。
const orderColumns = "id, order_no, request_id, mid, biz_type, plan_id, plan_code, title, quantity, " +
	"duration_days, coin_amount, unit_price_minor, amount_minor, refunded_minor, currency, pay_method, " +
	"state, fulfill_state, fulfill_attempts, fulfill_detail, payment_no, grant_ref, expire_at, platform, " +
	"client_trace_id, version, created_at, updated_at, paid_at, fulfilled_at, closed_at"

// Order 是订单主表行（to_order 投影），订单事实的唯一所有者。
//
// 金额口径：unit_price_minor/amount_minor 恒为服务端向 membership.GetPlan 重算的快照，
// 客户端上报值只用于一致性校验（防改价），绝不作为扣款依据；
// duration_days/coin_amount 同理，是下单时冻结的履约快照，
// 之后套餐改价改名不影响历史单履约与退款。
type Order struct {
	ID              int64  `db:"id"`               // 自增主键（对外用 order_no）
	OrderNo         string `db:"order_no"`         // 订单号（唯一索引，utf8mb4_bin）
	RequestID       string `db:"request_id"`       // 建单幂等键（唯一索引）
	Mid             int64  `db:"mid"`              // 下单用户
	BizType         int32  `db:"biz_type"`         // 1 会员单、2 硬币包
	PlanID          int64  `db:"plan_id"`          // 套餐/SKU 主键（membership 侧持有）
	PlanCode        string `db:"plan_code"`        // 套餐稳定编码快照
	Title           string `db:"title"`            // 商品名快照
	Quantity        int32  `db:"quantity"`         // 份数
	DurationDays    int32  `db:"duration_days"`    // 会员单本次总时长
	CoinAmount      int32  `db:"coin_amount"`      // 硬币包本次发放枚数
	UnitPriceMinor  int64  `db:"unit_price_minor"` // 服务端重算单价快照（分）
	AmountMinor     int64  `db:"amount_minor"`     // 应付=实付总额（分）
	RefundedMinor   int64  `db:"refunded_minor"`   // 已退金额（分）
	Currency        string `db:"currency"`         // 币种，显式携带
	PayMethod       int32  `db:"pay_method"`       // 1 余额、2 沙箱渠道
	State           int32  `db:"state"`            // 订单状态，见 State*
	FulfillState    int32  `db:"fulfill_state"`    // 履约结果，见 Fulfill*
	FulfillAttempts int32  `db:"fulfill_attempts"` // 履约尝试次数
	FulfillDetail   string `db:"fulfill_detail"`   // 最近一次失败摘要（无堆栈、无 PII、无凭据）
	PaymentNo       string `db:"payment_no"`       // payment 支付单号引用
	GrantRef        string `db:"grant_ref"`        // 履约产物引用：会员 grant_id 或硬币 flow_id
	ExpireAt        int64  `db:"expire_at"`        // 未支付关单时间（Unix 秒）
	Platform        int32  `db:"platform"`         // 下单端
	ClientTraceID   string `db:"client_trace_id"`  // 客户端链路 ID
	Version         int64  `db:"version"`          // 乐观锁位点（CAS）
	CreatedAt       int64  `db:"created_at"`
	UpdatedAt       int64  `db:"updated_at"`
	PaidAt          int64  `db:"paid_at"`
	FulfilledAt     int64  `db:"fulfilled_at"`
	ClosedAt        int64  `db:"closed_at"`
}

// RefundableMinor 返回本单还能退的金额（分）。
// 退款金额一律由服务侧算，不接受客户端自报的部分退款额度。
func (o *Order) RefundableMinor() int64 {
	left := o.AmountMinor - o.RefundedMinor
	if left < 0 {
		return 0
	}
	return left
}

// OrderUpdate 描述一次状态推进附带写入的列。
//
// SET 片段全部由本文件硬编码生成（白名单方法，调用方无法传入列名），
// 因此 logic 侧不存在拼出任意 SQL 的可能；state/version/updated_at 由 TransitionTx 统一追加。
type OrderUpdate struct {
	sets []string
	args []any
}

// NewOrderUpdate 构造一个空的状态推进附加写入。
func NewOrderUpdate() *OrderUpdate { return &OrderUpdate{} }

func (u *OrderUpdate) add(clause string, args ...any) *OrderUpdate {
	u.sets = append(u.sets, clause)
	u.args = append(u.args, args...)
	return u
}

// PaymentNo 绑定支付单号。
func (u *OrderUpdate) PaymentNo(v string) *OrderUpdate { return u.add("payment_no = ?", v) }

// PaidAt 记录支付完成时间。
func (u *OrderUpdate) PaidAt(v int64) *OrderUpdate { return u.add("paid_at = ?", v) }

// FulfilledAt 记录履约完成时间。
func (u *OrderUpdate) FulfilledAt(v int64) *OrderUpdate { return u.add("fulfilled_at = ?", v) }

// ClosedAt 记录关单（取消/终态）时间。
func (u *OrderUpdate) ClosedAt(v int64) *OrderUpdate { return u.add("closed_at = ?", v) }

// GrantRef 记录履约产物引用（会员 grant_id / 硬币 flow_id）。
func (u *OrderUpdate) GrantRef(v string) *OrderUpdate { return u.add("grant_ref = ?", v) }

// FulfillState 更新履约结果列。
func (u *OrderUpdate) FulfillState(v int32) *OrderUpdate { return u.add("fulfill_state = ?", v) }

// FulfillDetail 覆盖最近一次履约摘要（空串表示清空，成功路径必须清，
// 否则历史失败信息会一直挂在已履约订单上）。
func (u *OrderUpdate) FulfillDetail(v string) *OrderUpdate { return u.add("fulfill_detail = ?", v) }

// IncFulfillAttempts 履约尝试次数 +1（在事务内 CAS 到 FULFILLING 时调用）。
func (u *OrderUpdate) IncFulfillAttempts() *OrderUpdate {
	return u.add("fulfill_attempts = fulfill_attempts + 1")
}

// AddRefunded 累加已退金额（分）。用增量而不是绝对值，避免并发的两次退款互相覆盖。
func (u *OrderUpdate) AddRefunded(v int64) *OrderUpdate {
	return u.add("refunded_minor = refunded_minor + ?", v)
}

// OrderFilter 是订单列表查询条件。
// 零值字段表示不过滤；Mid 为 0 时即跨用户（运营面），logic 必须在此情况下强制时间窗。
type OrderFilter struct {
	Mid       int64
	State     int32
	States    []int32 // 非空时优先于 State（IN 过滤）
	BizType   int32
	PayMethod int32
	OrderNo   string
	PaymentNo string
	FromTs    int64 // created_at >= FromTs
	ToTs      int64 // created_at <= ToTs
	Offset    int64
	Limit     int64
}

// OrderModel to_order 表读写接口。
//
// 状态推进只有 TransitionTx 一个入口，且必须是 CAS 条件更新
// （WHERE order_no = ? AND state = ? AND version = ?）：
// 「读—改—写」在并发下会把同一订单推回旧状态，钱和权益就乱了。
type OrderModel interface {
	// InsertTx 在事务内写入订单（与首行 to_order_event 同事务），返回自增 id。
	// 命中 uniq_request_id 表示幂等重放，返回 ErrDuplicateRequest（调用方回查首次订单）。
	InsertTx(ctx context.Context, session sqlx.Session, o *Order) (int64, error)
	// FindByOrderNo 按订单号查询；不存在返回 (nil, nil)。
	FindByOrderNo(ctx context.Context, orderNo string) (*Order, error)
	// FindByOrderNoTx 事务内读取（配合 TransitionTx 后回读最新行）。
	FindByOrderNoTx(ctx context.Context, session sqlx.Session, orderNo string) (*Order, error)
	// FindByRequestID 按建单幂等键查询；不存在返回 (nil, nil)。
	FindByRequestID(ctx context.Context, requestID string) (*Order, error)
	// TransitionTx CAS 推进状态：where order_no = ? AND state = from AND version = expected。
	// 返回 false 表示并发抢先（版本或状态已变），调用方必须回读而不是重试写入。
	// 迁移是否合法由 CanTransition 在本方法内二次把关（logic 与 model 双层护栏）。
	TransitionTx(ctx context.Context, session sqlx.Session, orderNo string, from, to int32,
		expectedVersion int64, upd *OrderUpdate) (bool, error)
	// AnnotateTx 在不改变状态的前提下补写 fulfill_detail，CAS 条件与 TransitionTx 相同
	// （WHERE order_no = ? AND state = ? AND version = ?，并自增 version）。
	// 存在的理由：状态机没给「款已退、权益未回收」这种差异留边（订单必须停在
	// REFUND_APPROVED），但差异本身必须落在主表上让运营列表页看得见。
	// 只写摘要列、不改 state，因此不构成状态迁移，不经 CanTransition 是安全的。
	AnnotateTx(ctx context.Context, session sqlx.Session, orderNo string, state int32,
		expectedVersion int64, detail string) (bool, error)
	// ListByFilter 分页查询并回总数（运营面与我面共用；无界扫描由 logic 拦住）。
	ListByFilter(ctx context.Context, f *OrderFilter) ([]*Order, int64, error)
	// ListStuck 扫描 updated_at 早于 updatedBefore 且处于 states 之一的订单，limit 截断。
	// 走 idx_state_updated，供 cron 巡检卡单；created_at 过期的未支付单由本方法一并暴露。
	ListStuck(ctx context.Context, states []int32, updatedBefore int64, limit int64) ([]*Order, error)
}

type defaultOrderModel struct {
	conn sqlx.SqlConn
}

// NewOrderModel 创建 OrderModel 实现。
func NewOrderModel(conn sqlx.SqlConn) OrderModel {
	return &defaultOrderModel{conn: conn}
}

// ErrDuplicateRequest 建单幂等键冲突（uniq_request_id 命中），不是故障：
// 调用方应回查首次订单并按 duplicated=true 返回。
var ErrDuplicateRequest = errors.New("trade-order: duplicate request_id, order already created")

const insertOrderSQL = "INSERT INTO to_order (order_no, request_id, mid, biz_type, plan_id, plan_code, title, " +
	"quantity, duration_days, coin_amount, unit_price_minor, amount_minor, refunded_minor, currency, pay_method, " +
	"state, fulfill_state, fulfill_attempts, fulfill_detail, payment_no, grant_ref, expire_at, platform, " +
	"client_trace_id, version, created_at, updated_at, paid_at, fulfilled_at, closed_at) " +
	"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"

func (m *defaultOrderModel) InsertTx(ctx context.Context, session sqlx.Session, o *Order) (int64, error) {
	if o.OrderNo == "" {
		return 0, ErrOrderNoRequired
	}
	if o.RequestID == "" {
		return 0, ErrRequestIdRequired
	}
	now := nowUnix()
	if o.CreatedAt == 0 {
		o.CreatedAt = now
	}
	if o.UpdatedAt == 0 {
		o.UpdatedAt = now
	}
	if o.Version == 0 {
		o.Version = 1 // 首版号：建单即 version=1，后续每次 CAS 自增。
	}
	res, err := pickConn(session, m.conn).ExecCtx(ctx, insertOrderSQL,
		o.OrderNo, o.RequestID, o.Mid, o.BizType, o.PlanID, o.PlanCode, o.Title,
		o.Quantity, o.DurationDays, o.CoinAmount, o.UnitPriceMinor, o.AmountMinor, o.RefundedMinor,
		o.Currency, o.PayMethod, o.State, o.FulfillState, o.FulfillAttempts, o.FulfillDetail,
		o.PaymentNo, o.GrantRef, o.ExpireAt, o.Platform, o.ClientTraceID,
		o.Version, o.CreatedAt, o.UpdatedAt, o.PaidAt, o.FulfilledAt, o.ClosedAt)
	if err != nil {
		// 幂等键冲突与订单号冲突是两件事，必须分开暴露：
		// 前者是正常重放，后者要重试建单，混淆会让重放请求丢单。
		if isDuplicateKeyErr(err) {
			if strings.Contains(err.Error(), "uniq_request_id") {
				return 0, ErrDuplicateRequest
			}
			if strings.Contains(err.Error(), "uniq_order_no") {
				return 0, ErrOrderNoCollision
			}
		}
		return 0, fmt.Errorf("to_order InsertTx: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("to_order InsertTx LastInsertId: %w", err)
	}
	return id, nil
}

func (m *defaultOrderModel) FindByOrderNo(ctx context.Context, orderNo string) (*Order, error) {
	return m.findByOrderNo(ctx, nil, orderNo)
}

func (m *defaultOrderModel) FindByOrderNoTx(ctx context.Context, session sqlx.Session, orderNo string) (*Order, error) {
	return m.findByOrderNo(ctx, session, orderNo)
}

func (m *defaultOrderModel) findByOrderNo(ctx context.Context, session sqlx.Session, orderNo string) (*Order, error) {
	if orderNo == "" {
		return nil, ErrOrderNoRequired
	}
	var o Order
	query := "SELECT " + orderColumns + " FROM to_order WHERE order_no = ? LIMIT 1"
	if err := pickConn(session, m.conn).QueryRowCtx(ctx, &o, query, orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("to_order FindByOrderNo: %w", err)
	}
	return &o, nil
}

func (m *defaultOrderModel) FindByRequestID(ctx context.Context, requestID string) (*Order, error) {
	if requestID == "" {
		return nil, nil
	}
	var o Order
	query := "SELECT " + orderColumns + " FROM to_order WHERE request_id = ? LIMIT 1"
	if err := m.conn.QueryRowCtx(ctx, &o, query, requestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("to_order FindByRequestID: %w", err)
	}
	return &o, nil
}

func (m *defaultOrderModel) TransitionTx(ctx context.Context, session sqlx.Session, orderNo string,
	from, to int32, expectedVersion int64, upd *OrderUpdate,
) (bool, error) {
	if orderNo == "" {
		return false, ErrOrderNoRequired
	}
	if !CanTransition(from, to) {
		return false, fmt.Errorf("%w: %s -> %s", ErrInvalidStateTransition, StateName(from), StateName(to))
	}
	if expectedVersion <= 0 {
		return false, ErrExpectedVersionRequired
	}
	sets := []string{"state = ?", "version = version + 1", "updated_at = ?"}
	args := []any{to, nowUnix()}
	if upd != nil {
		sets = append(sets, upd.sets...)
		args = append(args, upd.args...)
	}
	query := "UPDATE to_order SET " + strings.Join(sets, ", ") +
		" WHERE order_no = ? AND state = ? AND version = ?"
	args = append(args, orderNo, from, expectedVersion)

	res, err := pickConn(session, m.conn).ExecCtx(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("to_order TransitionTx: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("to_order TransitionTx RowsAffected: %w", err)
	}
	return aff > 0, nil
}

// AnnotateTx 只补写 fulfill_detail（+ version/updated_at），state 保持为 state 参数指定的那个值。
//
// 为什么不走 TransitionTx：状态机里不存在 REFUND_APPROVED → REFUND_APPROVED 这条边
// （同态只对 FULFILLING 的重试开放），而「款已退、权益未回收」这种差异恰恰需要
// 原地留痕。CAS 条件与 TransitionTx 完全一致，所以不会出现「摘要被并发推进覆盖」。
func (m *defaultOrderModel) AnnotateTx(ctx context.Context, session sqlx.Session, orderNo string,
	state int32, expectedVersion int64, detail string,
) (bool, error) {
	if orderNo == "" {
		return false, ErrOrderNoRequired
	}
	if !ValidState(state) {
		return false, fmt.Errorf("%w: state=%d", ErrInvalidStateTransition, state)
	}
	if expectedVersion <= 0 {
		return false, ErrExpectedVersionRequired
	}
	query := "UPDATE to_order SET fulfill_detail = ?, version = version + 1, updated_at = ? " +
		"WHERE order_no = ? AND state = ? AND version = ?"
	res, err := pickConn(session, m.conn).ExecCtx(ctx, query,
		truncateForCol(detail, maxFulfillDetailLen), nowUnix(), orderNo, state, expectedVersion)
	if err != nil {
		return false, fmt.Errorf("to_order AnnotateTx: %w", err)
	}
	aff, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("to_order AnnotateTx RowsAffected: %w", err)
	}
	return aff > 0, nil
}

func (m *defaultOrderModel) ListByFilter(ctx context.Context, f *OrderFilter) ([]*Order, int64, error) {
	if f == nil {
		return nil, 0, ErrFilterRequired
	}
	if f.Limit <= 0 {
		return nil, 0, ErrInvalidPage
	}
	where, args := f.build()

	var total int64
	countQuery := "SELECT COUNT(*) FROM to_order WHERE " + where
	if err := m.conn.QueryRowCtx(ctx, &total, countQuery, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			total = 0
		} else {
			return nil, 0, fmt.Errorf("to_order ListByFilter count: %w", err)
		}
	}
	if total == 0 {
		return nil, 0, nil
	}

	listArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	query := "SELECT " + orderColumns + " FROM to_order WHERE " + where +
		" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?"
	var rows []*Order
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, listArgs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, total, nil
		}
		return nil, 0, fmt.Errorf("to_order ListByFilter: %w", err)
	}
	return rows, total, nil
}

// build 组装 WHERE 片段与参数。全部走占位符，筛选值不拼进 SQL 文本。
func (f *OrderFilter) build() (string, []any) {
	conds := make([]string, 0, 8)
	args := make([]any, 0, 8)
	if f.Mid > 0 {
		conds = append(conds, "mid = ?")
		args = append(args, f.Mid)
	}
	switch {
	case len(f.States) > 0:
		conds = append(conds, "state IN ("+placeholders(len(f.States))+")")
		for _, s := range f.States {
			args = append(args, s)
		}
	case f.State > 0:
		conds = append(conds, "state = ?")
		args = append(args, f.State)
	}
	if f.BizType > 0 {
		conds = append(conds, "biz_type = ?")
		args = append(args, f.BizType)
	}
	if f.PayMethod > 0 {
		conds = append(conds, "pay_method = ?")
		args = append(args, f.PayMethod)
	}
	if f.OrderNo != "" {
		conds = append(conds, "order_no = ?")
		args = append(args, f.OrderNo)
	}
	if f.PaymentNo != "" {
		conds = append(conds, "payment_no = ?")
		args = append(args, f.PaymentNo)
	}
	if f.FromTs > 0 {
		conds = append(conds, "created_at >= ?")
		args = append(args, f.FromTs)
	}
	if f.ToTs > 0 {
		conds = append(conds, "created_at <= ?")
		args = append(args, f.ToTs)
	}
	if len(conds) == 0 {
		// 兜底：无过滤条件时不给全表扫（调用方 logic 也会先拦一次）。
		conds = append(conds, "1 = 0")
	}
	return strings.Join(conds, " AND "), args
}

func (m *defaultOrderModel) ListStuck(ctx context.Context, states []int32, updatedBefore int64, limit int64) ([]*Order, error) {
	if len(states) == 0 || limit <= 0 {
		return nil, nil
	}
	query := "SELECT " + orderColumns + " FROM to_order WHERE state IN (" + placeholders(len(states)) +
		") AND updated_at < ? ORDER BY updated_at ASC LIMIT ?"
	args := make([]any, 0, len(states)+2)
	for _, s := range states {
		args = append(args, s)
	}
	args = append(args, updatedBefore, limit)

	var rows []*Order
	if err := m.conn.QueryRowsCtx(ctx, &rows, query, args...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("to_order ListStuck: %w", err)
	}
	return rows, nil
}

// placeholders 生成 n 个逗号分隔的 "?"。
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// truncateForCol 按 rune 截断到列宽（VARCHAR(500)），避免严格模式直接报错、
// 也避免半句中文被切断。逻辑层的同名工具只保护入口，这里是最后一道防线。
func truncateForCol(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// pickConn 在事务内用 session，否则走连接池。
// sqlx.SqlConn 本身满足 sqlx.Session，所以两者可以统一到 Session 上，
// 让每个方法只写一份 SQL（避免「事务版」和「非事务版」语句漂移）。
func pickConn(session sqlx.Session, conn sqlx.SqlConn) sqlx.Session {
	if session != nil {
		return session
	}
	return conn
}

// isDuplicateKeyErr 判断是否唯一键冲突（MySQL 1062）。
// go-zero 会把驱动错误包进 fmt 链，这里按错误码文本识别，避免为此引入驱动依赖。
func isDuplicateKeyErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Error 1062")
}
