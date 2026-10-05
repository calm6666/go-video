// 本文件是 model 包的手写扩展，不是 goctl 生成产物。
//
// 这里只放「与时间/周期有关的纯函数」：period 是台账与结算单的主键成分，
// 它的格式判定必须在 logic（入参校验）与 model（聚合窗口）里保持同一套口径，
// 所以收敛到一处，不允许各自 regexp。

package model

import (
	"fmt"
	"strconv"
	"time"
)

// nowUnix 返回当前 Unix 秒时间戳，供各 model 统一取时间。
func nowUnix() int64 { return time.Now().Unix() }

// NowUnix 是 logic 侧取时间戳的唯一入口：与 model 内部同一个时钟口径，
// 避免「logic 算一个时间、model 又算一个时间」导致同一行里时间戳互相矛盾。
func NowUnix() int64 { return nowUnix() }

// CurrentPeriod 返回服务端当前周期（YYYYMM，本地时区）。
// 周期归属以服务端时钟为准：客户端传的 period 只作为查询条件，不作为「现在」的定义。
func CurrentPeriod() string { return FormatPeriod(time.Now()) }

// FormatPeriod 把时间点渲染成 YYYYMM 周期码。
func FormatPeriod(t time.Time) string { return fmt.Sprintf("%04d%02d", t.Year(), int(t.Month())) }

// ValidatePeriod 校验 period 必须是 6 位 YYYYMM，且 0 < month <= 12。
// 返回归一化（去空格）后的周期码；非法即报 ErrInvalidPeriod，不做「猜测式纠正」，
// 因为把 2026-1 当成 202601 或 202610 都是资金口径上的赌博。
func ValidatePeriod(period string) (string, error) {
	p := ""
	for _, r := range period {
		if r == ' ' || r == '-' || r == '_' || r == '\t' {
			continue
		}
		p += string(r)
	}
	if len(p) != 6 {
		return "", fmt.Errorf("%w: %q 不是 6 位数字", ErrInvalidPeriod, period)
	}
	for _, r := range p {
		if r < '0' || r > '9' {
			return "", fmt.Errorf("%w: %q 含非数字字符", ErrInvalidPeriod, period)
		}
	}
	year, err := strconv.Atoi(p[:4])
	if err != nil || year <= 0 {
		return "", fmt.Errorf("%w: %q 年份非法", ErrInvalidPeriod, period)
	}
	month, err := strconv.Atoi(p[4:])
	if err != nil || month < 1 || month > 12 {
		return "", fmt.Errorf("%w: %q 月份必须在 1-12 之间", ErrInvalidPeriod, period)
	}
	return p, nil
}

// PeriodStartUnix 返回该周期首秒（本地时区当月 1 日 00:00:00）的 Unix 秒。
// 用于判定规则 effective_from：规则注释口径是「晚于生效起点的周期才用本规则」，
// 因此比较对象是周期起点，而不是「现在几点」。
func PeriodStartUnix(period string) (int64, error) {
	p, err := ValidatePeriod(period)
	if err != nil {
		return 0, err
	}
	year, _ := strconv.Atoi(p[:4])
	month, _ := strconv.Atoi(p[4:])
	return time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.Local).Unix(), nil
}

// PeriodBeforeCurrent 判断某周期是否严格早于当前周期（即「已收官的整月」）。
// 用于：拒绝为未来周期出单；给「本月未收官就确认结算」留 Warn 日志。
func PeriodBeforeCurrent(period string) (bool, error) {
	p, err := ValidatePeriod(period)
	if err != nil {
		return false, err
	}
	return p < CurrentPeriod(), nil
}

// PeriodCutoff 返回「最近 back 个周期」的起始周期码（含当前周期）。
//
// back <= 0 时返回空串，表示不限制窗口（全历史）；聚合查询据此决定是否加
// `period >= ?` 条件。周期码是定长 YYYYMM，字典序与时间序一致，所以字符串比较即可用。
func PeriodCutoff(current string, back int64) (string, error) {
	if back <= 0 {
		return "", nil
	}
	p, err := ValidatePeriod(current)
	if err != nil {
		return "", err
	}
	year, _ := strconv.Atoi(p[:4])
	month, _ := strconv.Atoi(p[4:])
	// 往前推 back-1 个月：窗口含当前周期，共 back 个周期。
	idx := year*12 + (month - 1) - int(back-1)
	if idx < 0 {
		return "", nil // 已经早于公元 1 年，等价于「不设窗口」
	}
	y, m := idx/12, idx%12+1
	out := fmt.Sprintf("%04d%02d", y, m)
	if _, err := ValidatePeriod(out); err != nil {
		return "", err
	}
	return out, nil
}
