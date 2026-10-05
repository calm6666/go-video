// 手写文件（不属于 goctl 生成产物）：/admin/payment 八条路由共用的身份渲染、
// 传输层门槛与 RPC→后台投影。
//
// 放在这里而不是各 logic 里重复一遍（AGENTS.md §4/§5）：
//   - operator 只能来自会话身份，且要满足 payment 侧 operator 列宽；
//   - 金额一律 int64 最小货币单位（分）+ 显式币种原样透传，网关不做单位换算、
//     不比较金额大小、不判「够不够扣」（§6 禁止浮点，§5 资金台账归 payment）；
//   - 状态机（充值 PENDING→SUCCESS、支付可否退款、余额不得为负、单次调整上限、
//     币种是否支持、渠道是否被运营关掉）全部由 services/payment 判定；
//   - 分页/时间窗只挡形状问题（负数、from>to），窗口有界性与页大小由服务判定并
//     回显实际值，网关照抄 reply 里的 page/size，绝不本地按自己的上限重算一遍。
//
// 本域是**沙箱台账**（AGENTS.md §1）：「充值结算 → 余额到账」是台账内的真实推进，
// 因此读侧一条都没配下游时也必须报错，不能回空台账——那等于让后台把
// 「payment 没接」读成「这个用户一分钱没有」，进而用 /balance/adjust 去「补数」。

package logic

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	paymentrpc "go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// paymentOperatorPrefix 与 membership/cron/collector 同口径：operator 前缀说明
// 「哪个入口提交的」（服务名，不是实例地址），后面接会话 admin_id，让 pm_flow /
// pm_recharge / pm_payment 的经办人列能回溯到人。
const paymentOperatorPrefix = "gateway/admin:"

// paymentMaxOperatorLength 是 pm_recharge/pm_payment/pm_refund/pm_flow 的 operator
// 列宽（VARCHAR(64)；服务侧收口在 services/payment/internal/logic/helpers.go 的
// requireOperator，同样是 64 个 rune）。
// 前缀 14 + int64 最多 19 位，正常永远碰不到；留着是因为一旦超限，服务报的是
// "payment: operator too long"，从后台错误里看不出这串是网关拼出来的。
// 注意：payment 没有像 membership 的 model.MaxOperatorLength 那样导出这个常量，
// 只在 helpers.go 里写死 64，所以这里是本仓库第二处副本（已作为契约缺口上报）。
const paymentMaxOperatorLength = 64

// errPaymentServiceNotConfigured 未配 PaymentRPC 时本域 8 条路由一律返回它。
// 不退化成空台账或 0 余额：见文件头「沙箱台账」段——资金读侧的「查不到」必须是错误，
// 而不是一个看起来像结论的数字。
var errPaymentServiceNotConfigured = errors.New("gateway/admin: payment service client not configured")

// errPaymentRequestMissing 请求体缺失。goctl 生成的 handler 永远传非 nil 指针，
// 该分支只覆盖 logic 被直接复用的场景。
var errPaymentRequestMissing = errors.New("gateway/admin: request body required")

// errPaymentSessionRequired 受 AdminPermission 保护的写路由拿不到会话身份。
// 此时说明这条路由没被中间件保护（权限表/挂载漂移），一律 fail-closed：
// /balance/adjust 是全后台唯一能凭空改写资金台账的口，没有主体就一个字都不写。
var errPaymentSessionRequired = errors.New("gateway/admin: admin session identity required")

// paymentOperator 渲染下传给 payment 的 operator。
//
// claimed 是请求体里的 operator 位（.api 注释「必须 > 0」）：它**只**用于两件事——
// 按契约确认表单确实填了审计主体，以及在与会话不一致时留一条越权线索日志。
// 真正进台账的永远是会话 admin_id 渲染出的 gateway/admin:<id>，否则请求体就等于
// 能自称是任意后台账号（AGENTS.md §5：台账归 payment 持有，网关不能投喂未证实主体）。
//
// 日志不打 reason 正文与 idempotency_key：前者是人读文案、后者可被重放（§7 脱敏口径）。
func paymentOperator(ctx context.Context, route string, claimed int64) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errPaymentSessionRequired
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
	operator := fmt.Sprintf("%s%d", paymentOperatorPrefix, id.AdminID)
	if utf8.RuneCountInString(operator) > paymentMaxOperatorLength {
		return "", fmt.Errorf("gateway/admin: rendered operator exceeds %d characters", paymentMaxOperatorLength)
	}
	logx.WithContext(ctx).Infof("gateway/admin/%s: admin_id=%d operator=%s", route, id.AdminID, operator)
	return operator, nil
}

// --- 传输层门槛 ---

// paymentNonNeg 只挡负数：0 在本域普遍是合法哨兵（page/size=0 用服务默认页、
// from_ts/to_ts=0 该侧不设界、mid=0 跨用户查台账、currency="" 用服务默认币种、
// state/method/biz_type=0 表示不按该位过滤），
// 负数没有任何对应语义，透传只会换来一次无意义往返。
// 上限（MaxPageSize、MaxListOffset、MaxListWindowSeconds、MaxAdjustMinor、列宽）
// 一律由服务拒绝或夹取并回显，网关不复算。
func paymentNonNeg(field string, v int64) error {
	if v < 0 {
		return fmt.Errorf("gateway/admin: %s must be >= 0, got %d", field, v)
	}
	return nil
}

// paymentPositive 给「没有 0 语义」的主体位设下界：/wallet/get 与 /balance/adjust 的
// mid=0 在服务侧都是硬错误（GetWallet/AdjustBalance 对 mid<=0 回 ErrInvalidMid，
// 资金接口没有游客也没有 0 号账号），因此 0 与负数都在下传前拒掉。
func paymentPositive(field string, v int64) error {
	if v <= 0 {
		return fmt.Errorf("gateway/admin: %s required (must be > 0, got %d)", field, v)
	}
	return nil
}

// paymentDeltaNonZero 只管 delta_minor 的「0 位」：本域唯一一处 0 非法但正负都合法
// 的入参（.api 写明「正入负出，不允许 0」，服务侧 ErrAdjustDeltaZero 同样只拒 0）。
// 绝对值上限 Payment.MaxAdjustMinor、扣成负余额、币种不匹配全部由服务判定，
// 网关一个都不复算——尤其不做「负数看着不合理就拒掉」这种兜底。
func paymentDeltaNonZero(v int64) error {
	if v == 0 {
		return errors.New("gateway/admin: delta_minor must not be 0 (positive credits, negative debits)")
	}
	return nil
}

// paymentTimeWindow 只挡「负数」与「倒着给」的窗口：服务侧 requireListBounds 对
// from_ts>to_ts（两者都为正）返回 ErrInvalidTimeRange，单边的 0 表示该侧不设界。
// 跨用户（mid=0）必须给完整窗口、窗口跨度不得超 Payment.MaxListWindowSeconds——
// 这两条依赖服务的配置值与「哪个查询位能替代时间窗」的判断（ListRefunds 有
// payment_no 时可省窗），网关自己复算只会和服务口径漂移，因此不在这里挡。
func paymentTimeWindow(from, to int64) error {
	if err := paymentNonNeg("from_ts", from); err != nil {
		return err
	}
	if err := paymentNonNeg("to_ts", to); err != nil {
		return err
	}
	if from > 0 && to > 0 && from > to {
		return errors.New("gateway/admin: from_ts must be <= to_ts")
	}
	return nil
}

// --- rpc → 后台 types ---

// paymentWalletToAPI 投影余额行。
//
// 契约缺口：GetWalletReply 只有 wallet 一位，**没有 found/present 之类的判定位**，
// 而服务侧 GetWallet 明确「只读不建行，账户不存在就是 0 余额」。因此后台拿到的
// balance_minor=0 无法区分「从未开过户」与「真的花光了」。这里既不补一个 found=true
// 暗示查到了账户，也不把 0 余额说成未开通——只转达服务给的那一行，缺口已上报待
// 改 proto（同域 /recharge/list、/flow/list 的台账可作旁证，但那不是本路由的结论）。
// frozen_minor 原样转达（本项目恒为 0），不并进可用余额。
func paymentWalletToAPI(w *paymentrpc.WalletInfo) types.PaymentWalletItem {
	if w == nil {
		return types.PaymentWalletItem{}
	}
	return types.PaymentWalletItem{
		Mid:          w.GetMid(),
		BalanceMinor: w.GetBalanceMinor(),
		FrozenMinor:  w.GetFrozenMinor(),
		Currency:     w.GetCurrency(),
		Version:      w.GetVersion(),
		Ctime:        w.GetCtime(),
		Mtime:        w.GetMtime(),
	}
}

// paymentRechargeToAPI 投影充值单。operator/request_id/reason 是审计三要素，
// 一个都不能裁：后台靠它区分「谁在什么时候为哪张单动的钱」。
func paymentRechargeToAPI(r *paymentrpc.RechargeInfo) types.PaymentRechargeItem {
	if r == nil {
		return types.PaymentRechargeItem{}
	}
	return types.PaymentRechargeItem{
		RechargeNo:  r.GetRechargeNo(),
		Mid:         r.GetMid(),
		AmountMinor: r.GetAmountMinor(),
		Currency:    r.GetCurrency(),
		Channel:     int32(r.GetChannel()),
		State:       int32(r.GetState()),
		Operator:    r.GetOperator(),
		RequestId:   r.GetRequestId(),
		Reason:      r.GetReason(),
		SettledAt:   r.GetSettledAt(),
		Ctime:       r.GetCtime(),
		Mtime:       r.GetMtime(),
	}
}

func paymentRechargesToAPI(list []*paymentrpc.RechargeInfo) []types.PaymentRechargeItem {
	out := make([]types.PaymentRechargeItem, 0, len(list))
	for _, r := range list {
		out = append(out, paymentRechargeToAPI(r))
	}
	return out
}

// paymentLedgerToAPI 投影支付单。
//
// 契约缺口：PaymentInfo 有 operator(15)/remark(16) 两个审计回显位，proto 注释写明
// 留着它就是为了「运营面必须能看到经办人」，但 admin.api 的 PaymentPaymentItem 没有
// 对应字段（本轮禁止改 .api），因此后台 /payment/list 看不到是谁把一张单推到当前态，
// 只能靠 /flow/list 的流水 operator 旁证。这里不偷偷把 operator 塞进 Subject（那是
// 建单摘要，不是经办人），缺口已上报。
func paymentLedgerToAPI(p *paymentrpc.PaymentInfo) types.PaymentPaymentItem {
	if p == nil {
		return types.PaymentPaymentItem{}
	}
	return types.PaymentPaymentItem{
		PaymentNo:     p.GetPaymentNo(),
		BizOrderNo:    p.GetBizOrderNo(),
		Mid:           p.GetMid(),
		AmountMinor:   p.GetAmountMinor(),
		RefundedMinor: p.GetRefundedMinor(),
		Currency:      p.GetCurrency(),
		Method:        int32(p.GetMethod()),
		State:         int32(p.GetState()),
		Subject:       p.GetSubject(),
		PaidAt:        p.GetPaidAt(),
		ExpireAt:      p.GetExpireAt(),
		RequestId:     p.GetRequestId(),
		Ctime:         p.GetCtime(),
		Mtime:         p.GetMtime(),
	}
}

func paymentLedgersToAPI(list []*paymentrpc.PaymentInfo) []types.PaymentPaymentItem {
	out := make([]types.PaymentPaymentItem, 0, len(list))
	for _, p := range list {
		out = append(out, paymentLedgerToAPI(p))
	}
	return out
}

// paymentRefundToAPI 投影退款单。destination 原样转达：沙箱只有 BALANCE 一种落点，
// 「原路退回渠道」在服务侧是 not-configured，网关不得改写成看起来能用的值。
func paymentRefundToAPI(r *paymentrpc.RefundInfo) types.PaymentRefundItem {
	if r == nil {
		return types.PaymentRefundItem{}
	}
	return types.PaymentRefundItem{
		RefundNo:    r.GetRefundNo(),
		PaymentNo:   r.GetPaymentNo(),
		BizOrderNo:  r.GetBizOrderNo(),
		Mid:         r.GetMid(),
		AmountMinor: r.GetAmountMinor(),
		Currency:    r.GetCurrency(),
		State:       int32(r.GetState()),
		Destination: r.GetDestination(),
		Operator:    r.GetOperator(),
		RequestId:   r.GetRequestId(),
		Reason:      r.GetReason(),
		Ctime:       r.GetCtime(),
	}
}

func paymentRefundsToAPI(list []*paymentrpc.RefundInfo) []types.PaymentRefundItem {
	out := make([]types.PaymentRefundItem, 0, len(list))
	for _, r := range list {
		out = append(out, paymentRefundToAPI(r))
	}
	return out
}

// paymentFlowToAPI 投影资金流水。delta_minor 的正负（正入负出）与 balance_after_minor
// 是台账自身的结论，网关不取绝对值、不校验前后余额是否自洽。
func paymentFlowToAPI(f *paymentrpc.FlowInfo) types.PaymentFlowItem {
	if f == nil {
		return types.PaymentFlowItem{}
	}
	return types.PaymentFlowItem{
		FlowId:            f.GetFlowId(),
		Mid:               f.GetMid(),
		BizType:           int32(f.GetBizType()),
		BizNo:             f.GetBizNo(),
		DeltaMinor:        f.GetDeltaMinor(),
		BalanceAfterMinor: f.GetBalanceAfterMinor(),
		Currency:          f.GetCurrency(),
		Remark:            f.GetRemark(),
		Operator:          f.GetOperator(),
		RequestId:         f.GetRequestId(),
		Ctime:             f.GetCtime(),
	}
}

func paymentFlowsToAPI(list []*paymentrpc.FlowInfo) []types.PaymentFlowItem {
	out := make([]types.PaymentFlowItem, 0, len(list))
	for _, f := range list {
		out = append(out, paymentFlowToAPI(f))
	}
	return out
}

// paymentChannelsToAPI 投影渠道自述。real_money/enabled/note 一律逐字转达：
// 「有没有真实资金渠道」只能由 payment 的配置说话，网关不得把未配置美化成
// 「可用渠道列表为空即正常」，也不得替它补一个沙箱条目。
func paymentChannelsToAPI(list []*paymentrpc.ChannelState) []types.PaymentChannelItem {
	out := make([]types.PaymentChannelItem, 0, len(list))
	for _, c := range list {
		out = append(out, paymentChannelToAPI(c))
	}
	return out
}

func paymentChannelToAPI(c *paymentrpc.ChannelState) types.PaymentChannelItem {
	if c == nil {
		return types.PaymentChannelItem{}
	}
	return types.PaymentChannelItem{
		Channel:   int32(c.GetChannel()),
		Enabled:   c.GetEnabled(),
		RealMoney: c.GetRealMoney(),
		Note:      c.GetNote(),
	}
}
