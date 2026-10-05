package logic

// 本文件是手写校验与幂等辅助（不是 goctl 生成产物）：
// 集中放「所有写接口共用」的边界判定，避免 16 个 logic 各写一套口径。

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"go-video/services/membership/internal/config"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"
)

// maxRequestIdLen 是 mb_grant.request_id / mb_biz_request.request_id /
// mb_plan_change_log.request_id 的列宽（VARCHAR(64)）。
const maxRequestIdLen = 64

// clampPage 把分页参数裁剪到安全区间，返回实际生效的 page/size 与 offset。
//
// 口径：超限「裁剪而不是报错」，并由 reply 回显实际 page/size（契约里带了这两个字段），
// 这样客户端拿到的是自己请求的那一页，而不是一个 500。
func clampPage(cfg config.MembershipConf, page, size int64) (int64, int64, int64) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = int64(cfg.DefaultPageSize)
	}
	if cfg.MaxPageSize > 0 && size > int64(cfg.MaxPageSize) {
		size = int64(cfg.MaxPageSize)
	}
	if size <= 0 {
		size = 1
	}
	return page, size, (page - 1) * size
}

// clampLimit 把 cron 扫描的单批条数裁剪到 ExpireScanMaxLimit（0 表示用上限）。
func clampLimit(cfg config.MembershipConf, limit int64) int64 {
	max := int64(cfg.ExpireScanMaxLimit)
	if max <= 0 {
		max = 500
	}
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}

// requireMid 校验用户 ID：游客态由调用方自行判定不通过，不打到会员域。
func requireMid(mid int64) error {
	if mid <= 0 {
		return fmt.Errorf("%w: %d", model.ErrInvalidMid, mid)
	}
	return nil
}

// requireVipType 校验档位（0 表示「不指定」的调用方应改用 requireVipTypeOrUnspecified）。
func requireVipType(v rpc.VipType) error {
	if !model.ValidVipType(int32(v)) {
		return fmt.Errorf("%w: %d", model.ErrInvalidVipType, int32(v))
	}
	return nil
}

// requireVipTypeOrUnspecified 允许 UNSPECIFIED（表示「当前生效的最高档」）。
func requireVipTypeOrUnspecified(v rpc.VipType) error {
	if v == rpc.VipType_VIP_TYPE_UNSPECIFIED {
		return nil
	}
	return requireVipType(v)
}

// requireRequestId 校验幂等键：写接口没有幂等键就无法区分重试与二次请求，一律拒。
func requireRequestId(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return model.ErrRequestIdRequired
	}
	return checkLen("request_id", id, maxRequestIdLen)
}

// requireOperator 校验操作者身份（网关按会话渲染，不接受客户端自报）。
// 用户自助动作按契约传 "user"，cron 传 "cron"。
func requireOperator(op string) error {
	op = strings.TrimSpace(op)
	if op == "" {
		return model.ErrOperatorRequired
	}
	return checkLen("operator", op, model.MaxOperatorLength)
}

// requireReason 校验必填理由（收回、上下架、运营手工开通）。
// reason 只写摘要：凭据、手机号等 PII 一律不允许进台账。
func requireReason(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return model.ErrReasonRequired
	}
	return checkLen("reason", reason, model.MaxReasonLength)
}

// optionalReason 允许为空，但同样受列宽约束。
func optionalReason(reason string) error {
	return checkLen("reason", reason, model.MaxReasonLength)
}

// checkLen 按 rune 计数校验列宽：VARCHAR(n) 在 MySQL 里是「字符数」而不是字节数，
// 用 len() 会把中文按 3 倍拒掉或放过。
func checkLen(field, v string, max int) error {
	if utf8.RuneCountInString(v) > max {
		return fmt.Errorf("%w: %s exceeds %d characters", model.ErrFieldTooLong, field, max)
	}
	return nil
}

// checkGrantDelta 校验单次授予/收回的时长边界（绝对值不超过 MaxGrantDeltaDays）。
// 越界返回错误而不是静默裁剪：一次写进几年的会员通常是上游算错了月份，
// 裁一下就等于把错误结论合法化。
func checkGrantDelta(cfg config.MembershipConf, delta int32) error {
	if delta == 0 {
		return fmt.Errorf("%w: delta_days must not be 0", model.ErrInvalidGrantDelta)
	}
	max := cfg.MaxGrantDeltaDays
	if max <= 0 {
		max = 3660
	}
	if delta > max || delta < -max {
		return fmt.Errorf("%w: delta_days=%d exceeds ±%d", model.ErrInvalidGrantDelta, delta, max)
	}
	return nil
}

// grantMatchesRequest 判定「同 request_id 的重放」是否与首次请求关键参数一致。
// 不一致必须返回 ErrRequestIdReused：幂等键只用于重试，不允许借它改口径。
// 台账里的 delta_days 记的是请求意图（带符号），实际影响看 before/after_expire_at。
func grantMatchesRequest(g *model.Grant, mid int64, vipType int32, action string, deltaDays int32,
	planID int64, source int32, bizOrderNo, paymentNo string) bool {
	return g.Mid == mid &&
		g.VipType == vipType &&
		g.Action == action &&
		g.DeltaDays == deltaDays &&
		g.PlanID == planID &&
		g.Source == source &&
		g.BizOrderNo == strings.TrimSpace(bizOrderNo) &&
		g.PaymentNo == strings.TrimSpace(paymentNo)
}

// membershipSubject 生成 mb_biz_request.subject 中会员身份作用的键。
func membershipSubject(mid int64, vipType int32) string {
	return fmt.Sprintf("mid:%d:vip:%d", mid, vipType)
}

// autoRenewOf 把 0/1 列值转成 bool。
func autoRenewOf(v int32) bool { return v == 1 }

// int32FromBool 把 bool 落成 0/1 列值。
func int32FromBool(b bool) int32 {
	if b {
		return 1
	}
	return 0
}

// daysRemoved 把「被扣掉的秒数」折算成台账里记的负数天数（向上取整）。
// 向上取整是保守口径：宁可少记用户已享受的天数，也不要把没扣的记成扣了。
func daysRemoved(seconds int64) int32 {
	if seconds <= 0 {
		return 0
	}
	const maxInt32 = int64(1<<31 - 1)
	d := (seconds + model.SecondsPerDay - 1) / model.SecondsPerDay
	if d > maxInt32 {
		d = maxInt32
	}
	return -int32(d)
}
