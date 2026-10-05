package model

import "errors"

// coin 域哨兵错误。
//
// 分两类，用途不能混：
//  1. 入参非法/契约违反（ErrInvalid*、ErrRequestIDRequired、ErrIdempotencyConflict 等）
//     直接上抛为 gRPC 错误，gateway 映射成 HTTP 信封的 code；
//  2. 事务内的判定结论（ErrInsufficientBalance、ErrDailyLimitExceeded、
//     ErrTargetLimitExceeded、ErrCancelWindowExpired 等）不是错误，
//     它们是「本次投币被拒的业务结论」，logic 必须把它转成
//     accepted=false + reason + reject_detail 的正常响应（proto 注释已锁定该口径），
//     只在 reply 无法表达结论的服务（GrantCoin 无 reason 字段）才允许以错误形式上抛。
var (
	// ErrNotImplemented 该方法本轮未实现。
	//
	// 存在意义：goctl 生成的桩是 `return &rpc.XxxReply{}, nil`，即「伪成功」——
	// 调用方会把空响应当成真实结论（余额 0、汇总 0、台账为空）。
	// 任何未落地的方法都必须改成返回本错误，并在 README「已知缺口」登记；
	// 实现完成后从对应方法上移除，不允许既留着伪成功又留着本错误。
	//
	// 本轮（投币域 model + logic）交付后仍缺的能力不是「方法未实现」，
	// 而是下面这些外部条件，故 ErrNotImplemented 只在收口时兜底使用：
	//   - coin_count 投影回写 engagement/video（跨服务，不由本服务写）
	//   - 风控门禁（risk-control 未接线）、作者自投判定（需 video 侧作者身份）
	//   - 领域事件 outbox（AGENTS.md §5 的投递器尚未接入本服务）
	ErrNotImplemented = errors.New("coin: not implemented yet, see services/coin/README.md")

	// ErrInvalidMid mid <= 0：投币与发放都必须指明用户。
	ErrInvalidMid = errors.New("coin: invalid mid")
	// ErrRequestIDRequired 写接口缺少幂等键。
	ErrRequestIDRequired = errors.New("coin: request_id required")
	// ErrInvalidTargetAid target_aid <= 0（投币记录写入路径用；
	// TossCoin 的判定不走这个错误，而是回 TOSS_REJECT_TARGET_INVALID 结论）。
	ErrInvalidTargetAid = errors.New("coin: invalid target aid")
	// ErrInvalidTossCount 投币枚数非法（归一化后仍不在 [1, PerTargetLimit] 之外才会命中，
	// 语义是「负数或溢出」这类无法解释的入参）。
	ErrInvalidTossCount = errors.New("coin: invalid toss count")
	// ErrIdempotencyConflict 同一 request_id 携带了不同参数（mid/aid/枚数/流水类型/订单号）。
	// 这是调用方 bug 或串号，必须报错而不是任选一份结论。
	ErrIdempotencyConflict = errors.New("coin: request_id conflicts with stored flow, parameters differ")
	// ErrAccountNotFound 已 EnsureAccountTx 之后仍读不到账户行（事务内顺序被改坏）。
	ErrAccountNotFound = errors.New("coin: coin account not found")

	// ErrInsufficientBalance 条件扣减未命中：余额不足（转成 reason=INSUFFICIENT_BALANCE）。
	ErrInsufficientBalance = errors.New("coin: insufficient coin balance")
	// ErrDailyLimitExceeded 今日可投额度已用尽（转成 reason=DAILY_LIMIT）。
	ErrDailyLimitExceeded = errors.New("coin: daily toss limit exceeded")
	// ErrTargetLimitExceeded 对同一内容的累计投币已达上限（转成 reason=TARGET_LIMIT）。
	ErrTargetLimitExceeded = errors.New("coin: per-target toss limit exceeded")
	// ErrTossNotFound 找不到对应的投币记录（取消/查询无命中）。
	ErrTossNotFound = errors.New("coin: toss record not found")
	// ErrTossNotActive 投币记录已取消，不能再次取消。
	ErrTossNotActive = errors.New("coin: toss record is not active")
	// ErrCancelWindowExpired 已超出取消窗口（proto 无专用 reason 枚举，
	// 本轮复用 reason=TARGET_LIMIT 表达，见 README「契约缺口」）。
	ErrCancelWindowExpired = errors.New("coin: cancel window expired")
	// ErrConcurrentUpdate 条件更新在并发下未命中，调用方重读后重试即可（不重复扣币）。
	ErrConcurrentUpdate = errors.New("coin: concurrent balance update, retry")

	// ErrGrantTypeInvalid GrantCoin 的 flow_type 不在 ORDER_PACK/ADMIN_GRANT 内。
	ErrGrantTypeInvalid = errors.New("coin: grant flow_type must be ORDER_PACK or ADMIN_GRANT")
	// ErrGrantDeltaInvalid delta 为 0，或绝对值超过 Coin.MaxGrantDelta。
	ErrGrantDeltaInvalid = errors.New("coin: grant delta invalid (zero or beyond MaxGrantDelta)")
	// ErrGrantOperatorRequired ADMIN_GRANT 未填操作者（运营扣回必须有工号可追）。
	ErrGrantOperatorRequired = errors.New("coin: admin grant requires operator")
	// ErrGrantReasonRequired ADMIN_GRANT 未填原因。
	ErrGrantReasonRequired = errors.New("coin: admin grant requires reason")
	// ErrGrantBalanceWouldGoNegative 扣回金额超过当前余额（现金币不分叉，不做负余额）。
	ErrGrantBalanceWouldGoNegative = errors.New("coin: grant clawback exceeds current balance")
	// ErrGrantBizNoRequired ORDER_PACK（硬币包履约）必须带订单号，否则无法与运营白送对账。
	ErrGrantBizNoRequired = errors.New("coin: order pack grant requires biz_no")

	// ErrOperatorReasonRequired 非本人自助操作（operator 不是 "user"）却没填原因：
	// 代客退币/代客扣回必须有可追责的凭据，否则运营面就是无凭据的余额改动机。
	ErrOperatorReasonRequired = errors.New("coin: acting on behalf of a user requires a reason")
	// ErrInvalidFlowType 入参给出的流水类型不在 rpc.CoinFlowType 的合法取值内。
	ErrInvalidFlowType = errors.New("coin: invalid coin flow type")

	// ErrPageSizeTooLarge 请求的每页条数超过 Coin.MaxPageSize（不静默截断）。
	ErrPageSizeTooLarge = errors.New("coin: page size exceeds max")
	// ErrInvalidTimeRange 台账查询的时间窗本身非法（负数或 from > to）。
	ErrInvalidTimeRange = errors.New("coin: invalid time range")
	// ErrUnboundedLedgerQuery 跨用户（mid=0）查流水台账却没给时间窗或订单号：
	// 这是全表扫描，必须拒绝而不是慢查询拖垮主库。
	ErrUnboundedLedgerQuery = errors.New("coin: cross-user flow query requires time window or biz_no")
	// ErrInvalidAids BatchGetTargetSummary 未给出任何有效 aid。
	ErrInvalidAids = errors.New("coin: no valid aid provided")
	// ErrInvalidStateFilter ListMyTosses 的 state 取值不在枚举内。
	ErrInvalidStateFilter = errors.New("coin: invalid toss state filter")
)
