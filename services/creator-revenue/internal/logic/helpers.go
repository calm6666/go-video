// 本文件是 logic 包的手写扩展（入参校验、行→RPC 投影、事务内 model 装配），
// 不是 goctl 生成产物。SQL 与库表读写一律留在 model（AGENTS.md §4）。

package logic

import (
	"fmt"
	"strings"

	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"
)

// selfOperator 是创作者自助动作的固定 operator 值（proto: EnrollCreatorReq.operator 注释）。
const selfOperator = "user"

// systemOperators 是回填来源的系统身份：它们不需要 reason（事实搬运），
// 但运营工号做更正/激励时必须给 reason（人为判断，必须有据可查）。
var systemOperators = map[string]bool{"cron": true, "spm": true, "creatorrevenue": true}

// IsSystemOperator 判定发起方是否系统回填身份。
func IsSystemOperator(op string) bool { return systemOperators[strings.TrimSpace(op)] }

// checkText 按列宽拒掉超长入参；field 只出现在错误里，便于网关直接回给运营。
func checkText(field, v string, max int) error {
	if len(strings.TrimSpace(v)) == 0 {
		return nil // 是否必填由各自的判定决定，这里只管长度
	}
	if len(v) > max {
		return fmt.Errorf("%w: %s 长度 %d 超过列宽 %d", model.ErrTextTooLong, field, len(v), max)
	}
	return nil
}

// requireOperator 要求写接口带身份（网关按会话渲染；缺失即拒，不记「匿名变更」）。
func requireOperator(op string) (string, error) {
	v := strings.TrimSpace(op)
	if v == "" {
		return "", model.ErrOperatorRequired
	}
	if err := checkText("operator", v, model.MaxOperatorBytes); err != nil {
		return "", err
	}
	return v, nil
}

// requireReason 要求「有后果的动作」留下原因。
func requireReason(reason string) (string, error) {
	v := strings.TrimSpace(reason)
	if v == "" {
		return "", model.ErrReasonRequired
	}
	if err := checkText("reason", v, model.MaxReasonBytes); err != nil {
		return "", err
	}
	return v, nil
}

// requireRequestID 要求写接口带幂等键（AGENTS.md §5）。
// max 由调用方给出：规则台账要求整条 request_id 落库；结算单要留出
// `#<period>#<mid>` 后缀的空间，所以那边给的是更小的上限。
func requireRequestID(requestID string, max int) (string, error) {
	v := strings.TrimSpace(requestID)
	if v == "" {
		return "", model.ErrRequestIDRequired
	}
	if err := checkText("request_id", v, max); err != nil {
		return "", err
	}
	return v, nil
}

// requireScopedRequestID 用于「落库幂等键由父 request_id 拼后缀」的写接口
// （SetRevenueRuleState 的自动归档子键、GenerateSettlement 的行级键）。
//
// suffixMax 是后缀的固定最大开销（数字按 int64 最坏 19 位算，再加分隔符）。
// 必须在这里拒掉超长父键：否则 INSERT 才在 VARCHAR(64) 上失败，
// 要么报截断、要么把幂等键悄悄改短，两种都会让「同一 request_id 重放」判不出来。
func requireScopedRequestID(requestID string, suffixMax int) (string, error) {
	max := model.MaxRequestIDBytes - suffixMax
	if max < 8 {
		max = 8
	}
	return requireRequestID(requestID, max)
}

// autoArchiveKey 是「切换规则生效」时附带归档旧 ACTIVE 规则所用的台账子键。
// 数字 rule_id 做后缀，宽度可预算（见 requireScopedRequestID 的 suffixMax）。
func autoArchiveKey(requestID string, ruleID int64) string {
	return fmt.Sprintf("%s#a%d", requestID, ruleID)
}

// validSettlementStateFilter 判定结算单列表的状态过滤值合法（0 表示不过滤）。
// 越界状态回错误而不是空名单：运营面板拿「查不到」去催确认，会催错方向。
func validSettlementStateFilter(v int32) error {
	if v < model.SettlementStateUnspecified || v > model.SettlementStateVoided {
		return fmt.Errorf("%w: %d", model.ErrInvalidRuleState, v)
	}
	return nil
}

// validSourceType 判定来源类型落在枚举内。
func validSourceType(v int32) error {
	if !model.SourceTypeValid(v) {
		return fmt.Errorf("%w: %d", model.ErrInvalidSourceType, v)
	}
	return nil
}

// normalizeMid 校验作者 ID：分成人必须是真实账号，0/负数一律拒。
func normalizeMid(mid int64) (int64, error) {
	if mid <= 0 {
		return 0, fmt.Errorf("%w: mid=%d", model.ErrInvalidMid, mid)
	}
	return mid, nil
}

// normalizeAid 校验内容 ID：允许 0（运营活动激励不挂具体内容），负数拒。
func normalizeAid(aid int64) (int64, error) {
	if aid < 0 {
		return 0, fmt.Errorf("%w: aid=%d（0 表示不挂具体内容，负数非法）", model.ErrInvalidAid, aid)
	}
	return aid, nil
}

// ruleInfo 把规则行投影成 RPC 结构。枚举位靠 int32 直转，取值与 proto 严格一致。
func ruleInfo(r *model.RevenueRule) *rpc.RevenueRuleInfo {
	if r == nil {
		return nil
	}
	return &rpc.RevenueRuleInfo{
		RuleId:                 r.RuleId,
		RuleCode:               r.RuleCode,
		SourceType:             rpc.RevenueSourceType(r.SourceType),
		Name:                   r.Name,
		Description:            r.Description,
		UnitPricePer_1000Minor: r.UnitPricePer1000,
		Currency:               r.Currency,
		Unit:                   r.Unit,
		MinQuantity:            r.MinQuantity,
		MonthlyCapMinor:        r.MonthlyCapMinor,
		State:                  rpc.RuleState(r.State),
		EffectiveFrom:          r.EffectiveFrom,
		Version:                r.Version,
		Ctime:                  r.Ctime,
		Mtime:                  r.Mtime,
		CreatedBy:              r.CreatedBy,
		UpdatedBy:              r.UpdatedBy,
	}
}

// ruleInfos 投影规则列表；空结果投影成非 nil 空数组，让网关能区分「没有」与「没查」。
func ruleInfos(rows []*model.RevenueRule) []*rpc.RevenueRuleInfo {
	out := make([]*rpc.RevenueRuleInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, ruleInfo(r))
	}
	return out
}

func enrollmentInfo(e *model.Enrollment) *rpc.EnrollmentInfo {
	if e == nil {
		return nil
	}
	return &rpc.EnrollmentInfo{
		Mid:               e.Mid,
		State:             rpc.EnrollmentState(e.State),
		AgreedRuleVersion: e.AgreedRuleVersion,
		EnrolledAt:        e.EnrolledAt,
		LeftAt:            e.LeftAt,
		UpdatedAt:         e.Mtime,
		Operator:          e.Operator,
		Remark:            e.Remark,
	}
}

func enrollmentInfos(rows []*model.Enrollment) []*rpc.EnrollmentInfo {
	out := make([]*rpc.EnrollmentInfo, 0, len(rows))
	for _, e := range rows {
		out = append(out, enrollmentInfo(e))
	}
	return out
}

func metricInfo(m *model.RevenueMetric) *rpc.RevenueMetricInfo {
	if m == nil {
		return nil
	}
	return &rpc.RevenueMetricInfo{
		MetricId:          m.MetricId,
		Period:            m.Period,
		Mid:               m.Mid,
		Aid:               m.Aid,
		SourceType:        rpc.RevenueSourceType(m.SourceType),
		RuleCode:          m.RuleCode,
		RuleVersion:       m.RuleVersion,
		Quantity:          m.Quantity,
		Unit:              m.Unit,
		AmountMinor:       m.AmountMinor,
		CappedAmountMinor: m.CappedAmountMinor,
		SourceDetail:      m.SourceDetail,
		Ctime:             m.Ctime,
		Mtime:             m.Mtime,
	}
}

func metricInfos(rows []*model.RevenueMetric) []*rpc.RevenueMetricInfo {
	out := make([]*rpc.RevenueMetricInfo, 0, len(rows))
	for _, m := range rows {
		out = append(out, metricInfo(m))
	}
	return out
}

func settlementInfo(s *model.Settlement) *rpc.SettlementInfo {
	if s == nil {
		return nil
	}
	return &rpc.SettlementInfo{
		SettlementNo:    s.SettlementNo,
		Period:          s.Period,
		Mid:             s.Mid,
		AmountMinor:     s.AmountMinor,
		CapAppliedMinor: s.CapAppliedMinor,
		Currency:        s.Currency,
		MetricCount:     s.MetricCount,
		State:           rpc.SettlementState(s.State),
		PayoutState:     rpc.PayoutState(s.PayoutState),
		ConfirmedAt:     s.ConfirmedAt,
		ConfirmedBy:     s.ConfirmedBy,
		VoidReason:      s.VoidReason,
		Ctime:           s.Ctime,
		Mtime:           s.Mtime,
	}
}

func settlementInfos(rows []*model.Settlement) []*rpc.SettlementInfo {
	out := make([]*rpc.SettlementInfo, 0, len(rows))
	for _, s := range rows {
		out = append(out, settlementInfo(s))
	}
	return out
}

func settlementItemInfos(rows []*model.SettlementItem) []*rpc.SettlementItem {
	out := make([]*rpc.SettlementItem, 0, len(rows))
	for _, it := range rows {
		out = append(out, &rpc.SettlementItem{
			SourceType:  rpc.RevenueSourceType(it.SourceType),
			RuleCode:    it.RuleCode,
			Quantity:    it.Quantity,
			AmountMinor: it.AmountMinor,
		})
	}
	return out
}

// periodToNumber 把 YYYYMM 周期码数值化（rpc.GetRevenueSummaryReply.last_settled_period 用数值）。
// 非法/空串回 0，语义即「没有出过单」；调用方已保证 period 来自库内的合法行。
// 必须按 ValidatePeriod 归一后的结果数值化：它容忍 "2026-01" 这类带分隔符写法，
// 直接遍历原始串会把 '-' 也当成数字算出一个「看起来合法但谁都不认识的周期号」。
func periodToNumber(period string) int64 {
	p, err := model.ValidatePeriod(period)
	if err != nil {
		return 0
	}
	var n int64
	for _, r := range p {
		n = n*10 + int64(r-'0')
	}
	return n
}

// trunc 把字符串裁到列宽以内（用于 remark/source_detail 这类「拼装出来的说明文本」，
// 裁断只丢尾部说明，不会丢金额，所以允许；关键判定字段一律用 checkText 直接拒）。
//
// 裁点必须退到 UTF-8 字符边界：这些文本绝大多数是中文说明，按字节硬裁会在尾部留下
// 半个字符，写进 utf8mb4 列会被 MySQL 判成 Incorrect string value 而让整条 INSERT 失败
// ——报错点离「一句备注太长」这个根因极远，且失败的是自动归档/更正这类低频但必须成功的写。
func trunc(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
