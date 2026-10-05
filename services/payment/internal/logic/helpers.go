package logic

import (
	"strings"
	"unicode/utf8"

	"go-video/common/idgen"
	"go-video/services/payment/internal/config"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"
)

// 本文件是 logic 层的手写公共件（AGENTS.md §4：自定义代码只能放在生成结构之外，
// internal/logic 下的手写辅助文件是允许的位置）。

// 单据号前缀：可读、可判来源，唯一性由 common/idgen 的 ULID 部分保证。
const (
	prefixRecharge = "RC"
	prefixPayment  = "PM"
	prefixRefund   = "RF"
	prefixAdjust   = "AJ"
)

// defaultListSize 是调用方未给 size 时的每页条数。
const defaultListSize int64 = 20

// maxTextRunes 是 subject/reason/remark/operator 类字段的长度上限（rune）。
// DB 列宽 255 字节，按 rune 收口比按字节更可控，也避免中文理由被静默截断。
const maxTextRunes = 100

// newDocumentNo 生成带前缀的单据号。不自己拼随机数：唯一性与时间有序性
// 统一由 common/idgen（ULID）保证。取不到熵就失败，不退化成时间戳拼接。
func newDocumentNo(prefix string) (string, error) {
	no, err := idgen.Prefixed(prefix)
	if err != nil {
		return "", model.ErrDocumentNoUnavailable
	}
	return no, nil
}

// resolveCurrency 归一并校验币种：本服务只维护单一币种台账，
// 其他币种直接拒绝，不做隐式换汇（金额语义不容猜）。
func resolveCurrency(cfg config.PaymentConf, currency string) (string, error) {
	normalized := cfg.NormalizeCurrency(currency)
	if normalized != strings.ToUpper(strings.TrimSpace(cfg.DefaultCurrency)) {
		return "", model.ErrUnsupportedCurrency
	}
	return normalized, nil
}

// requireRequestID 写接口必须有幂等键：没有它就无法区分「客户端重试」与「重复下单」，
// 只能拒绝，不能各建一单。
func requireRequestID(requestID string) error {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return model.ErrRequestIDRequired
	}
	return requireMaxLength("request_id", requestID, 64)
}

// requireOperator 资金变更必须有审计主体。
func requireOperator(operator string) error {
	operator = strings.TrimSpace(operator)
	if operator == "" {
		return model.ErrOperatorRequired
	}
	return requireMaxLength("operator", operator, 64)
}

// requireReason Cancel/Refund/Close/Adjust 的理由必填（审计口径要求可追溯）。
func requireReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.ErrReasonRequired
	}
	return requireMaxLength("reason", reason, maxTextRunes)
}

// requireMaxLength 按 rune 计长度，超限拒绝而不是截断入库。
func requireMaxLength(field, value string, limit int) error {
	if utf8.RuneCountInString(value) > limit {
		return model.ErrTextTooLong(field)
	}
	return nil
}

// normalizePage 归一 page/size：page<1 视为 1，size<=0 取默认值，
// 超过 Payment.MaxPageSize 直接拒绝（而不是悄悄改小让调用方以为拿到了全量）。
func normalizePage(cfg config.PaymentConf, page, size int64) (int64, int64, error) {
	maxSize := cfg.MaxPageSize
	if maxSize <= 0 {
		maxSize = 100
	}
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = defaultListSize
	}
	if size > maxSize {
		return page, size, model.ErrPageSizeTooLarge
	}
	maxOffset := cfg.MaxListOffset
	if maxOffset <= 0 {
		maxOffset = 10000
	}
	if (page-1)*size > maxOffset {
		return page, size, model.ErrListOffsetTooDeep
	}
	return page, size, nil
}

// listPage 把归一后的 page/size 转成 SQL 偏移。
func listPage(page, size int64) model.ListPage {
	return model.ListPage{Offset: (page - 1) * size, Limit: size}
}

// requireListBounds 跨用户（mid=0）台账查询的有界性检查：
// 必须给完整时间窗且窗口不超过 Payment.MaxListWindowSeconds。
// 资金表只增不减，无界扫描会把运营页变成拖垮 MySQL 的入口，
// 所以超窗是拒绝，不是悄悄只返一部分。
func requireListBounds(cfg config.PaymentConf, mid, fromTs, toTs int64) error {
	if fromTs > 0 && toTs > 0 && toTs < fromTs {
		return model.ErrInvalidTimeRange
	}
	if mid != 0 {
		return nil
	}
	if fromTs <= 0 || toTs <= 0 {
		return model.ErrListWindowRequired
	}
	if toTs < fromTs {
		return model.ErrInvalidTimeRange
	}
	window := cfg.MaxListWindowSeconds
	if window <= 0 {
		window = 2592000
	}
	if toTs-fromTs > window {
		return model.ErrListWindowTooLarge
	}
	return nil
}

// channelName 把 rpc.PayChannel 映射成配置里的渠道名（去掉 PAY_CHANNEL_ 前缀）。
func channelName(ch rpc.PayChannel) string {
	return strings.TrimPrefix(strings.ToUpper(ch.String()), "PAY_CHANNEL_")
}

// --- 台账行 → 契约消息投影 ---

// walletInfo 把余额行投影为 WalletInfo；nil 表示账户不存在，按 0 余额语义返回。
// frozen_minor 恒为 0（本项目无预授权），读侧不要拿它当可用余额。
func walletInfo(w *model.Wallet, mid int64, currency string) *rpc.WalletInfo {
	if w == nil {
		return &rpc.WalletInfo{Mid: mid, Currency: currency}
	}
	return &rpc.WalletInfo{
		Mid:          w.Mid,
		BalanceMinor: w.BalanceMinor,
		FrozenMinor:  w.FrozenMinor,
		Currency:     w.Currency,
		Version:      w.Version,
		Ctime:        w.Ctime,
		Mtime:        w.Mtime,
	}
}

func rechargeInfo(r *model.Recharge) *rpc.RechargeInfo {
	if r == nil {
		return nil
	}
	return &rpc.RechargeInfo{
		RechargeNo:  r.RechargeNo,
		Mid:         r.Mid,
		AmountMinor: r.AmountMinor,
		Currency:    r.Currency,
		Channel:     rpc.PayChannel(r.Channel),
		State:       rpc.RechargeState(r.State),
		Operator:    r.Operator,
		RequestId:   r.RequestId,
		Reason:      r.Reason,
		SettledAt:   r.SettledAt,
		Ctime:       r.Ctime,
		Mtime:       r.Mtime,
	}
}

func paymentInfo(p *model.Payment) *rpc.PaymentInfo {
	if p == nil {
		return nil
	}
	return &rpc.PaymentInfo{
		PaymentNo:     p.PaymentNo,
		BizOrderNo:    p.BizOrderNo,
		Mid:           p.Mid,
		AmountMinor:   p.AmountMinor,
		RefundedMinor: p.RefundedMinor,
		Currency:      p.Currency,
		Method:        rpc.PayMethod(p.Method),
		State:         rpc.PaymentState(p.State),
		Subject:       p.Subject,
		PaidAt:        p.PaidAt,
		ExpireAt:      p.ExpireAt,
		RequestId:     p.RequestId,
		Ctime:         p.Ctime,
		Mtime:         p.Mtime,
		Operator:      p.Operator,
		Remark:        p.Remark,
	}
}

func refundInfo(r *model.Refund) *rpc.RefundInfo {
	if r == nil {
		return nil
	}
	return &rpc.RefundInfo{
		RefundNo:    r.RefundNo,
		PaymentNo:   r.PaymentNo,
		BizOrderNo:  r.BizOrderNo,
		Mid:         r.Mid,
		AmountMinor: r.AmountMinor,
		Currency:    r.Currency,
		State:       rpc.RefundState(r.State),
		Destination: r.Destination,
		Operator:    r.Operator,
		RequestId:   r.RequestId,
		Reason:      r.Reason,
		Ctime:       r.Ctime,
	}
}

func flowInfo(f *model.Flow) *rpc.FlowInfo {
	if f == nil {
		return nil
	}
	return &rpc.FlowInfo{
		FlowId:            f.FlowId,
		Mid:               f.Mid,
		BizType:           rpc.FlowBizType(f.BizType),
		BizNo:             f.BizNo,
		DeltaMinor:        f.DeltaMinor,
		BalanceAfterMinor: f.BalanceAfterMinor,
		Currency:          f.Currency,
		Remark:            f.Remark,
		Operator:          f.Operator,
		RequestId:         f.RequestId,
		Ctime:             f.Ctime,
	}
}

func rechargeInfos(rows []*model.Recharge) []*rpc.RechargeInfo {
	out := make([]*rpc.RechargeInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, rechargeInfo(row))
	}
	return out
}

func paymentInfos(rows []*model.Payment) []*rpc.PaymentInfo {
	out := make([]*rpc.PaymentInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, paymentInfo(row))
	}
	return out
}

func refundInfos(rows []*model.Refund) []*rpc.RefundInfo {
	out := make([]*rpc.RefundInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, refundInfo(row))
	}
	return out
}

func flowInfos(rows []*model.Flow) []*rpc.FlowInfo {
	out := make([]*rpc.FlowInfo, 0, len(rows))
	for _, row := range rows {
		out = append(out, flowInfo(row))
	}
	return out
}
