// 手写文件（不属于 goctl 生成产物）：/admin/order 六条路由共用的身份渲染、
// 传输层门槛与 RPC→后台投影。
//
// 放在这里而不是各 logic 里重复一遍（AGENTS.md §4/§5）：
//   - operator 只能来自会话身份，且要满足 trade-order 侧 operator 列宽；
//   - 订单状态机与履约指令**只属于 trade-order**（§5）：网关不复算「这单是不是可退款」、
//     不推断合法迁移、不判断可退金额上限，非法前态由服务拒绝并原样上抛；
//   - 金额一律 int64 最小货币单位（分）原样转达，网关不做单位换算、不按比例折算退款额
//     （服务侧当前只支持全额退，见 ApproveRefund 路由注释）；
//   - 分页三元组照抄 reply，网关不复算；ListStuckOrders 契约里根本没有分页位。
//
// 本域是**沙箱订单**（AGENTS.md §1）：approve 是真会驱动 trade-order→payment 退钱的动作，
// 因此读侧一条都没配下游时也必须报错，不能回空列表或 found=false——那等于让后台把
// 「trade-order 没接」读成「这个人一单都没有」，进而去手工补单或重复审批。

package logic

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	tradeorderrpc "go-video/services/trade-order/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// orderOperatorPrefix 与 membership/payment 同口径：operator 前缀说明「哪个入口提交的」
// （服务名，不是实例地址），后面接会话 admin_id，让 to_order_event.operator 能回溯到人。
const orderOperatorPrefix = "gateway/admin:"

// orderMaxOperatorLength 是 to_order_event.operator 的列宽（VARCHAR(64)；服务侧收口在
// services/trade-order/internal/logic/helpers.go 的 maxOperatorLen，同样 64）。
// 前缀 14 + int64 最多 19 位，正常永远碰不到；留着是因为服务侧对超长是
// **truncate 而不是拒绝**（helpers.go 里 `truncate(operator, maxOperatorLen)`），
// 一旦真超限，台账里存下的就是一个被切过的人名，事后无法追责（§5 审计证据）。
// 注意：trade-order 没有像 membership 的 model.MaxOperatorLength 那样导出这个常量，
// 只在 internal/logic 里写死 64，所以这里是本仓库第二处副本（已作为契约缺口上报）。
const orderMaxOperatorLength = 64

// errOrderServiceNotConfigured 未配 TradeOrderRPC 时本域 6 条路由一律返回它。
// 不退化成空列表：订单检索与卡单扫描的「空」在后台就是一个结论（没有卡单），
// 用它冒充「下游没接」是 §1 禁止的假成功。
var errOrderServiceNotConfigured = errors.New("gateway/admin: trade-order service client not configured")

// errOrderRequestMissing 请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errOrderRequestMissing = errors.New("gateway/admin: request body required")

// errOrderSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed：
// /refund/approve 会真的驱动退款到余额并回收权益，没有主体就一个字都不写。
var errOrderSessionRequired = errors.New("gateway/admin: admin session identity required")

// orderOperator 渲染下传给 trade-order 的 operator。
//
// claimed 是请求体里的 operator 位（.api 注释「必须 > 0」）：它**只**用于两件事——
// 按契约确认表单确实填了审计主体，以及在与会话不一致时留一条越权线索日志。
// 真正进台账的永远是会话 admin_id 渲染出的 gateway/admin:<id>，否则请求体就等于
// 能自称是任意后台账号（AGENTS.md §5：订单事实归 trade-order 持有，网关不能投喂未证实主体）。
//
// 日志不打 reason 正文与 idempotency_key：前者是人读文案、后者可被重放（§7 脱敏口径）。
func orderOperator(ctx context.Context, route string, claimed int64) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errOrderSessionRequired
	}
	if err := requireOperatorID(id.AdminID); err != nil {
		return "", err
	}
	if err := requireOperator("operator", claimed); err != nil {
		return "", err
	}
	if claimed != id.AdminID {
		logx.WithContext(ctx).Errorf("gateway/admin/%s: operator mismatch session=%d claimed=%d",
			route, id.AdminID, claimed)
	}
	operator := fmt.Sprintf("%s%d", orderOperatorPrefix, id.AdminID)
	if utf8.RuneCountInString(operator) > orderMaxOperatorLength {
		return "", fmt.Errorf("gateway/admin: rendered operator exceeds %d characters", orderMaxOperatorLength)
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator=%s", route, id.AdminID, operator)
	return operator, nil
}

// --- 传输层门槛 ---

// orderNonNeg 只挡负数：0 在本域普遍是合法哨兵（page/size=0 用服务默认页、
// mid=0 跨用户查台账或「不校验归属」地看某一单、state/biz_type/pay_method=0 不按该位过滤、
// from_ts/to_ts=0 该侧不设界、max_window_seconds=0 用服务默认窗口、
// older_than_seconds=0 用 OrderExpireSeconds、limit=0 用批处理上限），
// 负数没有任何对应语义，透传只会换来一次无意义往返。
// 上限（MaxPageSize、StuckScanMaxLimit、MaxListWindowSeconds、列宽）与「这个状态值存不存在」
// 一律由服务拒绝，网关不复算。
func orderNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// orderStateNonNeg 给 repeated OrderState 的扫描位做同一件事：0 与「不在合法集合内」
// 的判定属于服务（stuckAllowedStates、ValidState），网关只挡负数——负数连枚举都不是。
// 网关不去重、不排序、不替调用方补默认状态集合（补了就等于替 cron 决定「哪些单算卡住」）。
func orderStatesNonNeg(list []int32) error {
	for i, v := range list {
		if v < 0 {
			return fmt.Errorf("gateway/admin: states[%d] must be >= 0, got %d", i, v)
		}
	}
	return nil
}

// orderTimeWindow 只挡「负数」与「倒着给」的窗口：服务侧 ListOrders 对
// from_ts>to_ts（两者都为正）返回 ErrListWindowTooLarge，单边的 0 表示该侧不设界。
// 「跨用户（mid=0）必须给完整窗口」「窗口跨度不得超过 MaxListWindowSeconds」同样是服务的
// 判定（它才知道配置值和哪个点位能替代时间窗），网关不复算。
func orderTimeWindow(from, to int64) error {
	if err := orderNonNeg("from_ts", from); err != nil {
		return err
	}
	if err := orderNonNeg("to_ts", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: from_ts must be <= to_ts")
	}
	return nil
}

// --- api → rpc ---

// orderStatesToRPC 把后台表单的状态编号列表转成 protobuf 枚举切片。
// 逐个 cast、不去重、不排序、不校验取值：哪些状态算「还会卡住」是 trade-order 的结论。
// 空列表原样传空切片（服务据此使用默认集合），网关不替它填 PAYING/PAID/FULFILLING。
func orderStatesToRPC(list []int32) []tradeorderrpc.OrderState {
	out := make([]tradeorderrpc.OrderState, 0, len(list))
	for _, v := range list {
		out = append(out, tradeorderrpc.OrderState(v))
	}
	return out
}

// --- rpc → 后台 types ---

// orderToAPI 投影订单行。
//
// 逐字段转达，尤其三位「不好看」的：state 与 fulfill_state 是两条独立口径（订单走到哪一步
// vs 该给的东西给到没有）、fulfill_detail 是最近一次失败摘要、refunded_minor 是已退金额。
// 网关不把 fulfill_state=FAILED 美化成 state=FULFILLED，也不拿 amount_minor 减去
// refunded_minor 冒充「可退余额」（RefundableMinor 的算法在 trade-order，且它还会在
// 审批那一刻重算）。version 是 CAS 位点，下一轮审批要带回去，裁掉它等于废掉乐观锁。
func orderToAPI(o *tradeorderrpc.OrderInfo) types.OrderItem {
	if o == nil {
		return types.OrderItem{}
	}
	return types.OrderItem{
		OrderNo:         o.GetOrderNo(),
		Mid:             o.GetMid(),
		BizType:         int32(o.GetBizType()),
		PlanId:          o.GetPlanId(),
		PlanCode:        o.GetPlanCode(),
		Title:           o.GetTitle(),
		Quantity:        o.GetQuantity(),
		DurationDays:    o.GetDurationDays(),
		CoinAmount:      o.GetCoinAmount(),
		UnitPriceMinor:  o.GetUnitPriceMinor(),
		AmountMinor:     o.GetAmountMinor(),
		RefundedMinor:   o.GetRefundedMinor(),
		Currency:        o.GetCurrency(),
		PayMethod:       int32(o.GetPayMethod()),
		State:           int32(o.GetState()),
		FulfillState:    int32(o.GetFulfillState()),
		FulfillAttempts: o.GetFulfillAttempts(),
		FulfillDetail:   o.GetFulfillDetail(),
		PaymentNo:       o.GetPaymentNo(),
		GrantRef:        o.GetGrantRef(),
		ExpireAt:        o.GetExpireAt(),
		ClientTraceId:   o.GetClientTraceId(),
		Platform:        int32(o.GetPlatform()),
		RequestId:       o.GetRequestId(),
		Version:         o.GetVersion(),
		CreatedAt:       o.GetCreatedAt(),
		UpdatedAt:       o.GetUpdatedAt(),
		PaidAt:          o.GetPaidAt(),
		FulfilledAt:     o.GetFulfilledAt(),
		ClosedAt:        o.GetClosedAt(),
	}
}

func ordersToAPI(list []*tradeorderrpc.OrderInfo) []types.OrderItem {
	out := make([]types.OrderItem, 0, len(list))
	for _, o := range list {
		out = append(out, orderToAPI(o))
	}
	return out
}

// orderEventToAPI 投影状态流转台账。operator/reason 是「谁在什么时候凭什么把这单推到
// 哪一步」的证据链本体，一位都不能裁：退款审批的追溯全靠它。
func orderEventToAPI(e *tradeorderrpc.OrderEventInfo) types.OrderEventItem {
	if e == nil {
		return types.OrderEventItem{}
	}
	return types.OrderEventItem{
		EventId:   e.GetEventId(),
		OrderNo:   e.GetOrderNo(),
		FromState: int32(e.GetFromState()),
		ToState:   int32(e.GetToState()),
		Operator:  e.GetOperator(),
		Reason:    e.GetReason(),
		Ctime:     e.GetCtime(),
	}
}

func orderEventsToAPI(list []*tradeorderrpc.OrderEventInfo) []types.OrderEventItem {
	out := make([]types.OrderEventItem, 0, len(list))
	for _, e := range list {
		out = append(out, orderEventToAPI(e))
	}
	return out
}
