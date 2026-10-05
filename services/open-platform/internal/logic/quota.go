package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"

	"github.com/zeromicro/go-zero/core/logx"
)

// 配额判定与扣减的唯一实现，AuthorizeRequest 与 ListQuotaUsage 共用。
//
// 三条不可商量的语义：
//  1. 生效层级只由 model.NarrowPolicies 决定（app+精确 → app+* → 全局+精确 → 全局+*），
//     读面与扣减面共用同一函数，保证「看到的限额」与「实际扣的限额」不会漂移；
//  2. fail closed：一条规则都匹配不上时返回 ErrQuotaPolicyNotFound，
//     而不是当成「没有上限就放行」——000003 的 app_id=0/api_code='*' 全局兜底就是为此存在；
//  3. 超限只拒本次请求，不回滚已发生的记账：op_quota_usage 是投影，
//     真实次数以 op_api_call_log 为准，回滚投影等于把被拒用量藏起来（运营必须看得见）。
//     「同一 request_id 不重复扣」由 AuthorizeRequest 的 uniq_request_id 重放兜住。

// quotaWindow 单条规则的扣减结果（多窗口并存时逐条记录，回显取最紧的一条）。
type quotaWindow struct {
	windowSeconds int64
	limit         int64
	used          int64
	remaining     int64
	windowEnd     int64
	exceeded      bool
}

// quotaDecision 一次配额判定的结论。
type quotaDecision struct {
	// allowed 全部生效规则都未超限。
	allowed bool
	// windows 生效层级内每条规则的扣减结果（按规则顺序，长度>=1）。
	windows []quotaWindow
}

// tightest 返回剩余额度最小的窗口：调用方要看的「最先撞到的墙」。
func (d *quotaDecision) tightest() (quotaWindow, bool) {
	best := quotaWindow{}
	found := false
	for _, w := range d.windows {
		if !found || w.remaining <= best.remaining {
			best, found = w, true
		}
	}
	return best, found
}

// blocked 返回第一条超限的窗口（未超限时 ok=false）。
func (d *quotaDecision) blocked() (quotaWindow, bool) {
	for _, w := range d.windows {
		if w.exceeded {
			return w, true
		}
	}
	return quotaWindow{}, false
}

// retryAfter 被拒时的建议等待秒数：取所有超限窗口里最早结束的那条，至少 1 秒。
// 「至少 1 秒」是必要的：0 会让客户端在同一窗口内以 CPU 速度重试，等于自己打自己。
func (d *quotaDecision) retryAfter(now int64) int64 {
	best := int64(0)
	for _, w := range d.windows {
		if !w.exceeded {
			continue
		}
		wait := w.windowEnd - now
		if wait < 1 {
			wait = 1
		}
		if best == 0 || wait < best {
			best = wait
		}
	}
	return best
}

// effectiveQuotaPolicies 取当前生效层级的规则集。
// 返回空集时回 ErrQuotaPolicyNotFound（判定链不得因缺规则而放行）。
func effectiveQuotaPolicies(ctx context.Context, s *svc.ServiceContext, appID int64,
	apiCode string) ([]*model.QuotaPolicy, error) {
	if appID < 0 {
		return nil, model.ErrInvalidAppID
	}
	if apiCode == "" {
		return nil, errAPICodeRequired
	}
	cands, err := s.QuotaPolicies.ListCandidates(ctx, appID, apiCode)
	if err != nil {
		return nil, err
	}
	policies := model.NarrowPolicies(cands, appID, apiCode)
	if len(policies) == 0 {
		return nil, model.ErrQuotaPolicyNotFound
	}
	return policies, nil
}

// chargeQuota 在生效层级的每条规则上各扣一次并回读用量。
//
// 每条规则都要扣（不是「第一条超限就短路」）：多窗口是同时约束的，
// 短路会让「60s 窗口已超限」的请求不再累加日窗口，日累计被系统性低估。
func chargeQuota(ctx context.Context, s *svc.ServiceContext, appID int64, apiCode string,
	now int64) (*quotaDecision, error) {
	policies, err := effectiveQuotaPolicies(ctx, s, appID, apiCode)
	if err != nil {
		return nil, err
	}
	dec := &quotaDecision{allowed: true}
	for _, p := range policies {
		if p == nil || p.WindowSeconds <= 0 {
			// 规则行本身异常：保守拒绝并留日志，不做默认窗口兜底（默认值等于放宽）。
			logx.WithContext(ctx).Errorf("open-platform: 配额规则窗口非法 app_id=%d api_code=%s", appID, apiCode)
			dec.allowed = false
			dec.windows = append(dec.windows, quotaWindow{limit: 0, windowEnd: now + 1, exceeded: true})
			continue
		}
		windowStart := model.AlignWindow(now, p.WindowSeconds)
		windowEnd := windowStart + p.WindowSeconds
		if p.Denied() {
			// quota_limit<=0 且 enabled=1 等价「禁用该接口」，是显式配置而不是超限。
			logQuotaDeny(ctx, p)
			dec.allowed = false
			dec.windows = append(dec.windows, quotaWindow{
				windowSeconds: p.WindowSeconds, limit: 0, windowEnd: windowEnd, exceeded: true})
			continue
		}
		usage, err := s.QuotaUsages.Add(ctx, appID, apiCode, p.WindowSeconds, windowStart, 1, p.QuotaLimit)
		if err != nil {
			return nil, err
		}
		used := int64(1)
		if usage != nil {
			used = usage.Used
		} else {
			// 投影回读失败（并发清理等）：按 used=1 的下界处理，既不放宽也不虚增。
			logx.WithContext(ctx).Errorf("open-platform: 配额投影回读为空 app_id=%d api_code=%s w=%d",
				appID, apiCode, p.WindowSeconds)
		}
		w := quotaWindow{
			windowSeconds: p.WindowSeconds,
			limit:         p.QuotaLimit,
			used:          used,
			remaining:     nonNeg(p.QuotaLimit - used),
			windowEnd:     windowEnd,
			exceeded:      used > p.QuotaLimit,
		}
		if w.exceeded {
			logQuotaDeny(ctx, p)
			dec.allowed = false
		}
		dec.windows = append(dec.windows, w)
	}
	return dec, nil
}

// peekQuota 只读地算出生效层级的当前用量（ListQuotaUsage 用，绝不扣减）。
// 缺失的窗口行按 used=0 呈现：投影尚未记账不等于错误。
func peekQuota(ctx context.Context, s *svc.ServiceContext, appID int64, apiCode string,
	now, windowStart int64) ([]quotaWindow, error) {
	policies, err := effectiveQuotaPolicies(ctx, s, appID, apiCode)
	if err != nil {
		return nil, err
	}
	out := make([]quotaWindow, 0, len(policies))
	for _, p := range policies {
		if p == nil || p.WindowSeconds <= 0 {
			continue
		}
		start := windowStart
		if start <= 0 {
			start = model.AlignWindow(now, p.WindowSeconds)
		}
		usage, err := s.QuotaUsages.Peek(ctx, appID, apiCode, p.WindowSeconds, start)
		if err != nil {
			return nil, err
		}
		used := int64(0)
		if usage != nil {
			used = usage.Used
		}
		limit := p.QuotaLimit
		if limit < 0 {
			limit = 0
		}
		out = append(out, quotaWindow{
			windowSeconds: p.WindowSeconds,
			limit:         limit,
			used:          used,
			remaining:     nonNeg(limit - used),
			windowEnd:     start + p.WindowSeconds,
			exceeded:      used > p.QuotaLimit,
		})
	}
	return out, nil
}

func logQuotaDeny(ctx context.Context, p *model.QuotaPolicy) {
	logx.WithContext(ctx).Errorf("open-platform: 配额拒绝 app_id=%d api_code=%s window=%ds limit=%d enabled=%d",
		p.AppID, p.APICode, p.WindowSeconds, p.QuotaLimit, p.Enabled)
}
