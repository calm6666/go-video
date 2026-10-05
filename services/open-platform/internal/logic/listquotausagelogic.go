package logic

import (
	"context"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListQuotaUsageLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListQuotaUsageLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListQuotaUsageLogic {
	return &ListQuotaUsageLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// quotaUsageAPICodeLimit 汇总视图一次最多展开的接口数（每接口再按生效窗口数展开条目）。
// 取值与 op_quota_policy 的实际规模同量级：一个应用配置的接口限额是十几条的量，
// 超过本上限说明规则集异常，此时要求调用方显式指定 api_code，而不是服务端无界展开。
const quotaUsageAPICodeLimit = 32

// 配额用量查询（投影）。
//
// 本方法只读：绝不触发 QuotaUsages.Add（那是鉴权路径的副作用），也不写任何缓存。
//
// 错误映射：ErrInvalidAppID/ErrAppNotFound/errAPICodeRequired/ErrScopeUnknown/
// ErrWindowInvalid/errTooManyQuotaAPIs→InvalidArgument；ErrQuotaPolicyNotFound→FailedPrecondition
// （连 000003 seed 的全局兜底规则都没有，属运营配置事故，回空数组等于骗人）；SQL 失败→Internal。
func (l *ListQuotaUsageLogic) ListQuotaUsage(in *rpc.ListQuotaUsageReq) (*rpc.ListQuotaUsageReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 范围：用量永远是「某个应用」的，app_id=0 在全局规则层表示「兜底层级」，
	//    但记账（op_quota_usage）与流水都以真实 app_id 为键，所以这里不接受 0，
	//    也不提供任何跨应用汇总（跨应用口径属于 SPM/运营报表，不是配额投影）。
	if in.AppId <= 0 {
		return nil, model.ErrInvalidAppID
	}
	if _, err := findApp(ctx, s, in.AppId); err != nil {
		return nil, err
	}
	// 身份口径：operator_mid==0 表示应用 owner 自查（网关已注入 owner 身份并校验归属，
	// 本服务能保证的是「响应里只有 in.app_id 这一个应用的数据」）；
	// operator_mid>0 表示运营。两者读到的内容相同，区别只进日志（审计要能看出是谁在查）。
	if in.OperatorMid < 0 {
		return nil, model.ErrOperatorRequired
	}

	apiCode, err := optionalLen(in.ApiCode, maxAPICodeRunes, errAPICodeRequired)
	if err != nil {
		return nil, err
	}
	if apiCode != "" {
		if err := validScopeToken(apiCode); err != nil {
			return nil, err
		}
	}
	if in.WindowStart < 0 {
		return nil, model.ErrWindowInvalid
	}

	now := nowUnix()
	var codes []string
	if apiCode != "" {
		codes = []string{apiCode}
	} else {
		// 2. 汇总视图（api_code 为空）：枚举范围由「规则数」收敛，而不是由用量行数收敛。
		//    这一步不能用 op_quota_usage 反查：api_code 只校验字符集、不来自白名单目录，
		//    一个应用理论上能造出任意多的 distinct api_code 行，用它当枚举源等于放开无界扫描。
		//    应用自己没有规则时退回全局默认层（app_id=0）的规则集——那正是它实际撞的限额。
		codes, err = quotaUsageAPICodes(ctx, s, in.AppId)
		if err != nil {
			return nil, err
		}
	}

	// 3. 逐接口按生效层级取窗口：层级判定复用 effectiveQuotaPolicies
	//    （ListCandidates + model.NarrowPolicies），与 AuthorizeRequest 的扣减面同一函数，
	//    保证「看到的限额」与「实际扣的限额」不会漂移。
	list := make([]*rpc.QuotaUsageInfo, 0, len(codes))
	for _, code := range codes {
		entries, err := quotaUsageEntries(ctx, s, in.AppId, code, in.WindowStart, now)
		if err != nil {
			return nil, err
		}
		list = append(list, entries...)
	}
	logx.WithContext(ctx).Infof("open-platform: 配额用量查询 app_id=%d api_code=%s window_start=%d "+
		"operator_mid=%d n=%d", in.AppId, apiCode, in.WindowStart, in.OperatorMid, len(list))

	return &rpc.ListQuotaUsageReply{List: list}, nil
}

// quotaUsageAPICodes 汇总视图的接口枚举：本应用规则集优先，为空时退回全局默认层规则集。
//
// 上限 quotaUsageAPICodeLimit 是「一次汇总最多展开多少个接口」的硬护栏：
// 超出时报错而不是截断——截断会让运营误判「这应用就这些接口有用量」。
// 规则集按 api_code 升序返回，保证同样输入得到同样输出（结果会被上游短缓存）。
func quotaUsageAPICodes(ctx context.Context, s *svc.ServiceContext, appID int64) ([]string, error) {
	codes, truncated, err := scanQuotaRuleAPICodes(ctx, s, appID)
	if err != nil {
		return nil, err
	}
	if len(codes) == 0 {
		if codes, truncated, err = scanQuotaRuleAPICodes(ctx, s, model.GlobalAppID); err != nil {
			return nil, err
		}
	}
	if truncated || len(codes) > quotaUsageAPICodeLimit {
		return nil, errTooManyQuotaAPIs
	}
	sortStrings(codes)
	return codes, nil
}

// scanQuotaRuleAPICodes 取某一层级（appID 可为 0=全局）配置过的 distinct api_code。
// 走 QuotaPolicyModel.ListByApp（含已停用规则：停用的限额仍解释着历史窗口），
// 多取一条用于判断「规则集是否已被上限截断」。
//
// '*' 通配规则也作为一个条目返回（它确实是运营配置的一档限额），但它的 used 恒为 0：
// 通配规则给真实接口记账时窗口行的 api_code 列写的是请求里的接口名，不是 '*'。
// 要看通配层实际压住了哪些接口，用带 api_code 的精确查询或 RecomputeQuota 的流水统计。
func scanQuotaRuleAPICodes(ctx context.Context, s *svc.ServiceContext,
	appID int64) (codes []string, truncated bool, err error) {
	rows, err := s.QuotaPolicies.ListByApp(ctx, appID, "", 0, 0, quotaUsageAPICodeLimit+1)
	if err != nil {
		return nil, false, err
	}
	if len(rows) > quotaUsageAPICodeLimit {
		truncated = true
		rows = rows[:quotaUsageAPICodeLimit]
	}
	seen := make(map[string]struct{}, len(rows))
	codes = make([]string, 0, len(rows))
	for _, p := range rows {
		if p == nil || p.APICode == "" {
			continue
		}
		if _, ok := seen[p.APICode]; ok {
			continue
		}
		seen[p.APICode] = struct{}{}
		codes = append(codes, p.APICode)
	}
	return codes, truncated, nil
}

// quotaUsageEntries 算出单个 api_code 在指定窗口起点上各生效窗口的用量条目。
//
// window_start 口径与 peekQuota 一致：<=0 表示「当前窗口」，按每条规则的 window_seconds
// 用 model.AlignWindow 取齐（与扣减路径同一个取齐函数，绝不另写一套取整，否则窗口边界错一格）；
// 显式传值时按传入值精确定位（响应的 window_start 就是它，用于回看刚过去的那个窗口）。
// 缺失的窗口行按 used=0 呈现：投影还没记账不是错误。
func quotaUsageEntries(ctx context.Context, s *svc.ServiceContext, appID int64, apiCode string,
	windowStart, now int64) ([]*rpc.QuotaUsageInfo, error) {
	policies, err := effectiveQuotaPolicies(ctx, s, appID, apiCode)
	if err != nil {
		return nil, err
	}
	out := make([]*rpc.QuotaUsageInfo, 0, len(policies))
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
		if usage == nil {
			usage = &model.QuotaUsage{
				AppID:         appID,
				APICode:       apiCode,
				WindowSeconds: p.WindowSeconds,
				WindowStart:   start,
				WindowEnd:     start + p.WindowSeconds,
				Used:          0,
			}
		}
		// limit 回显当前生效限额（不是 limit_snapshot）：运营要看的是「现在还能打多少次」。
		// 快照列只解释「当时为什么被拒」，两者不同就意味着限额漂移，这本身要能被看出来。
		if info := projectQuotaUsage(usage, apiCode, nonNeg(p.QuotaLimit)); info != nil {
			out = append(out, info)
		}
	}
	return out, nil
}
