package logic

import (
	"context"
	"sort"

	"go-video/services/open-platform/internal/svc"
	"go-video/services/open-platform/model"
	"go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RecomputeQuotaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRecomputeQuotaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RecomputeQuotaLogic {
	return &RecomputeQuotaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

const (
	// quotaRuleScanLimit 重算读取规则目录时**每一批**的条数上界（见 quotaRuleBatches：
	// 全部接口模式 2 批、精确接口模式 4 批）。规则由运营手配，单批量级在几条到几十条；
	// 用固定上界而不是配置值，是为了让「扫描规模」与 MaxPageSize 解耦——
	// 分页上限改动不该悄悄放大一次重算的读量。
	quotaRuleScanLimit = 100
	// recomputeWindowCap 单次重算处理的窗口数上界：超过即拒绝并要求分段区间，
	// 防止「一次 RPC 打几十万条 upsert」把投影表写成热点（AGENTS.md §9 的超时/故障场景）。
	recomputeWindowCap = 2000
	// recomputeRangeSpanFactor 允许区间长度 = QuotaRecomputeLookbackSeconds 的这个倍数。
	recomputeRangeSpanFactor = 4
	// defaultRecomputeLookbackSeconds 配置缺省时的回溯基准（与 config 的 default=7200 一致）。
	defaultRecomputeLookbackSeconds = 7200
)

// 从调用流水重算配额投影。
//
// 真值是 op_api_call_log（append-only），本方法把 op_quota_usage 拉回真值附近：
// 只覆盖窗口计数，不清空重刷（清空会让限额瞬间失效，属安全事故），也不加全局锁
// （在线扣减仍在 Add 累加，语义是「下一次重算继续收敛」）。
//
// 错误映射：ErrOperatorRequired→PermissionDenied；ErrInvalidAppID/ErrAppNotFound/
// ErrWindowInvalid/errAPICodeRequired/ErrScopeUnknown/errRecomputeRangeTooWide/
// errRecomputeTooManyWindows→InvalidArgument 或 FailedPrecondition（区间过宽属调用方可改）；
// SQL 失败→Internal。重算失败不回滚在线扣减（两者无事务耦合）。
//
// 窗口枚举不生成 [from,to) 的起点序列，而是取「两侧已出现的窗口」并集：投影行的 window_start
// 与流水按 FLOOR(ctime/w)*w 分组的 window_start（model 里这条 SQL 与 model.AlignWindow 同口径，
// Go 侧不再二次取整）。两边都没有的窗口既没有计数也没有行，写它只会凭空新增无意义记录。
func (l *RecomputeQuotaLogic) RecomputeQuota(in *rpc.RecomputeQuotaReq) (*rpc.RecomputeQuotaReply, error) {
	ctx, s := l.ctx, l.svcCtx

	// 1. 身份：高危写面只接受自然人运营主体（proto:496「operator_mid」+ 仓库约定
	//    operator=0 只用于无自然人主体的内部回写）。cron 必须以当初授权的运营 mid 触发，
	//    否则「谁改写了限额用量」在审计上不可追溯。
	if err := requireOperator(in.OperatorMid); err != nil {
		return nil, err
	}

	// 2. 范围：app_id 必须指定真实应用。
	//    为什么不支持 proto 注释里的「0 = 全部应用」：冻结的 model 读法
	//    （ListWindowTotals / ListWindowsInRange 在 appID<=0 时不加 app_id 条件）会把
	//    **所有应用**的流水并进同一个窗口桶，拿这个合并计数去覆盖单应用行会把限额语义直接打穿
	//    （比不重算危险）。要覆盖全部应用只能逐应用调用本方法，这恰好也是可观测、可断点续跑的做法；
	//    跨应用聚合属 SPM/运营报表口径，不在配额投影这一层。
	if in.AppId <= 0 {
		return nil, model.ErrInvalidAppID
	}
	if _, err := findApp(ctx, s, in.AppId); err != nil {
		return nil, err
	}

	// 3. 区间：[window_start, window_end) 必须正向，且长度受限——重算区间过大会把流水表扫成慢查询。
	if in.WindowStart < 0 || in.WindowEnd <= in.WindowStart {
		return nil, model.ErrWindowInvalid
	}
	lookback := s.Config.OpenPlatform.QuotaRecomputeLookbackSeconds
	if lookback <= 0 {
		lookback = defaultRecomputeLookbackSeconds
	}
	if maxSpan := lookback * recomputeRangeSpanFactor; in.WindowEnd-in.WindowStart > maxSpan {
		return nil, errRecomputeRangeTooWide
	}

	// 4. 接口：精确 api_code 只重算该接口；"*"/空 表示全部接口，
	//    枚举源是本服务规则目录（有界），而不是 op_quota_usage 的行
	//    （api_code 不来自白名单目录，用量表的 distinct 接口数不受服务端约束）。
	apiCode, err := optionalRecomputeAPICode(in.ApiCode)
	if err != nil {
		return nil, err
	}

	// 5. 规则目录：枚举「可能作用到目标接口」的规则（含已停用规则——历史窗口可能是它们记的账，
	//    停用后窗口仍需能被解释与重算）。
	//    精确接口只取该接口名与本应用/全局两层的 '*' 通配规则，而不是把整个应用的规则集读回来：
	//    大应用的规则集会撑爆扫描上界，而上界一旦生效就意味着「少算了接口」——那比报错更糟。
	batches, err := quotaRuleBatches(ctx, s, in.AppId, apiCode)
	if err != nil {
		return nil, err
	}

	codes, err := recomputeAPICodes(apiCode, batches...)
	if err != nil {
		return nil, err
	}

	// 6. 非 dry_run 才写库，因此只在真要写时占进程级写令牌（dry run 是纯读，用来给运营看漂移）。
	if !in.DryRun {
		release, err := writePermit(ctx, s)
		if err != nil {
			return nil, err
		}
		defer release()
	}

	var scanned, fixed, maxDelta int64
	for _, code := range codes {
		// 生效限额一次算好（写回 limit_snapshot 用）：按判定链同一口径逐层收敛，
		// 不另写一套排序，否则重算写入的快照会与在线判定用的限额漂移。
		limits := quotaEffectiveLimits(in.AppId, code, batches...)
		for _, w := range recomputeWindowLengths(code, batches...) {
			existing, err := s.QuotaUsages.ListWindowsInRange(ctx, in.AppId, code, w, in.WindowStart, in.WindowEnd)
			if err != nil {
				return nil, err
			}
			totals, err := s.CallLogs.ListWindowTotals(ctx, in.AppId, code, w, in.WindowStart, in.WindowEnd)
			if err != nil {
				return nil, err
			}

			// 处理窗口 = 投影已有 ∪ 流水已有：后者能补回被清理任务删掉的投影行（proto:27-28
			// 说明投影可重建），前者能把「流水已超出保留期被清掉」的虚高计数拉下来。
			oldUsed := make(map[int64]int64, len(existing))
			oldSnapshot := make(map[int64]int64, len(existing))
			starts := make([]int64, 0, len(existing)+len(totals))
			for _, u := range existing {
				if u == nil {
					continue
				}
				if _, ok := oldUsed[u.WindowStart]; !ok {
					starts = append(starts, u.WindowStart)
				}
				oldUsed[u.WindowStart] = u.Used
				oldSnapshot[u.WindowStart] = u.LimitSnapshot
			}
			totalUsed := make(map[int64]int64, len(totals))
			for _, t := range totals {
				if t == nil {
					continue
				}
				if _, ok := oldUsed[t.WindowStart]; !ok {
					if _, dup := totalUsed[t.WindowStart]; !dup {
						starts = append(starts, t.WindowStart)
					}
				}
				totalUsed[t.WindowStart] += t.Total
			}
			// 计数来源：完整落在区间里的窗口直接用上面那次分组统计的结果；
			// 被区间切断的跨界窗口（含 cron 只回溯 2 小时却撞上 86400s 日窗口的情况）
			// 单独按 [ws, ws+w) 再取一次——拿被切断的计数覆盖投影会把一个正常窗口的用量抹小，
			// 那是「越重算越偏」的事故，比多一条查询贵得多。
			totalFor := func(ws int64) (int64, error) {
				if ws >= in.WindowStart && ws+w <= in.WindowEnd {
					return totalUsed[ws], nil
				}
				rows, err := s.CallLogs.ListWindowTotals(ctx, in.AppId, code, w, ws, ws+w)
				if err != nil {
					return 0, err
				}
				want := int64(0)
				for _, t := range rows {
					if t != nil && t.WindowStart == ws {
						want += t.Total
					}
				}
				return want, nil
			}
			sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })

			for _, ws := range starts {
				scanned++
				if scanned > recomputeWindowCap {
					return nil, errRecomputeTooManyWindows
				}
				want, err := totalFor(ws)
				if err != nil {
					return nil, err
				}
				delta := want - oldUsed[ws]
				if delta == 0 {
					continue // 已收敛：不写库，避免把 updated_at 刷成噪声
				}
				if !in.DryRun {
					// 快照优先级：当前生效限额 > 该行原快照 > 本次计数（至少 1）。
					// 绝不写 0：quota_limit=0 在判定里等价「禁用该接口」，重算不该顺手把窗口判死。
					snapshot := limits[w]
					if snapshot <= 0 {
						snapshot = oldSnapshot[ws]
					}
					if snapshot <= 0 {
						snapshot = nonNeg(want)
						if snapshot < 1 {
							snapshot = 1
						}
					}
					// 以写入时刻的差值为准：在线扣减可能刚改过 used，重算只保证「拉回真值附近」。
					real, err := s.QuotaUsages.RecomputeOverwrite(ctx, in.AppId, code, w, ws, want, snapshot)
					if err != nil {
						return nil, err
					}
					delta = real
					if real == 0 {
						continue
					}
				}
				fixed++
				if absInt64(delta) > absInt64(maxDelta) {
					maxDelta = delta
				}
			}
		}
	}

	logx.WithContext(ctx).Infof("open-platform: 配额重算 app_id=%d api_code=%s range=[%d,%d) dry_run=%t "+
		"operator_mid=%d scanned=%d fixed=%d max_delta=%d", in.AppId, in.ApiCode, in.WindowStart, in.WindowEnd,
		in.DryRun, in.OperatorMid, scanned, fixed, maxDelta)

	return &rpc.RecomputeQuotaReply{
		WindowsScanned: scanned,
		WindowsFixed:   fixed,
		MaxDelta:       maxDelta,
	}, nil
}

// optionalRecomputeAPICode 归一重算的接口过滤：返回空串表示「全部接口」。
// "*" 与空串同义（proto:491）；其余取值必须是合法标识，否则参数错。
func optionalRecomputeAPICode(raw string) (string, error) {
	code, err := optionalLen(raw, maxAPICodeRunes, errAPICodeRequired)
	if err != nil {
		return "", err
	}
	if code == model.AnyAPICode {
		return "", nil
	}
	if code != "" {
		if err := validScopeToken(code); err != nil {
			return "", err
		}
	}
	return code, nil
}

// quotaRuleBatches 读回可能作用目标接口的规则批次（含已停用规则）。
//
// apiCode 为空（全部接口）时按应用层与全局层各扫一遍；给定精确接口时只扫「该接口名」与
// 两层的 '*' 通配规则——同一接口同一层每种窗口长度只有一行（uniq_app_api_window），
// 因此这四批天然远小于扫描上界。
// 任一批次读满上界就说明枚举被截断（重算会「少算接口却报已收敛」），一律报错：
// 全部接口模式提示显式传 api_code，精确模式提示窗口数超限。
func quotaRuleBatches(ctx context.Context, s *svc.ServiceContext,
	appID int64, apiCode string) ([][]*model.QuotaPolicy, error) {
	type ruleScan struct {
		appID   int64
		apiCode string
	}
	scans := []ruleScan{{appID, ""}, {model.GlobalAppID, ""}}
	if apiCode != "" {
		scans = []ruleScan{
			{appID, apiCode},
			{appID, model.AnyAPICode},
			{model.GlobalAppID, apiCode},
			{model.GlobalAppID, model.AnyAPICode},
		}
	}
	batches := make([][]*model.QuotaPolicy, 0, len(scans))
	for _, q := range scans {
		rows, err := s.QuotaPolicies.ListByApp(ctx, q.appID, q.apiCode, 0, 0, quotaRuleScanLimit)
		if err != nil {
			return nil, err
		}
		if len(rows) >= quotaRuleScanLimit {
			if apiCode == "" {
				return nil, errTooManyQuotaAPIs
			}
			return nil, errRecomputeTooManyWindows
		}
		batches = append(batches, rows)
	}
	return batches, nil
}

// recomputeAPICodes 重算要覆盖的接口集合：精确请求就一个；通配按规则目录枚举。
//
// '*' 通配规则本身不作为重算目标：op_quota_usage 与 op_api_call_log 的 api_code 列存的
// 都是**请求里的真实接口名**（通配规则只是给它限额），而 model 的跨接口统计在 api_code='*'
// 时会退化成「不加过滤 = 全应用合并计数」，拿它覆盖单接口行等于伪造用量。
// 因此通配模式只重算目录里出现过的具体接口名；未出现在目录里的接口（只被全局 '*' 规则约束）
// 需要显式传 api_code 逐个重算。
func recomputeAPICodes(apiCode string, batches ...[]*model.QuotaPolicy) ([]string, error) {
	if apiCode != "" {
		return []string{apiCode}, nil
	}
	seen := make(map[string]struct{}, 16)
	out := make([]string, 0, 16)
	for _, batch := range batches {
		for _, p := range batch {
			if p == nil || p.APICode == "" || p.APICode == model.AnyAPICode {
				continue
			}
			if _, ok := seen[p.APICode]; ok {
				continue
			}
			seen[p.APICode] = struct{}{}
			out = append(out, p.APICode)
		}
	}
	if len(out) > quotaUsageAPICodeLimit {
		return nil, errTooManyQuotaAPIs
	}
	sortStrings(out)
	return out, nil
}

// recomputeWindowLengths 某个接口在本方法可见的规则集里出现过的窗口长度（升序、去重、剔除非法值）。
//
// 取「本应用 + 全局」两层里能作用到该接口的规则（精确同名或 '*'），包含已停用规则：
// 重算解释的是历史窗口，当时生效的规则现在可能已被停用或遮蔽，跳过它会让那一层永远算不到。
// window_seconds<=0 的规则行本身异常：跳过并记 Error，不做默认窗口兜底（默认值等于放宽）。
func recomputeWindowLengths(apiCode string, batches ...[]*model.QuotaPolicy) []int64 {
	seen := make(map[int64]struct{})
	out := make([]int64, 0, 4)
	for _, batch := range batches {
		for _, p := range batch {
			if p == nil || !quotaRuleApplies(p, apiCode) {
				continue
			}
			if p.WindowSeconds <= 0 {
				logx.Errorf("open-platform: 配额规则窗口长度非法，重算跳过 app_id=%d api_code=%s window=%ds",
					p.AppID, p.APICode, p.WindowSeconds)
				continue
			}
			if _, ok := seen[p.WindowSeconds]; ok {
				continue
			}
			seen[p.WindowSeconds] = struct{}{}
			out = append(out, p.WindowSeconds)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// quotaEffectiveLimits 给出「当前生效层级」里每个窗口长度对应的限额。
//
// 候选集与 QuotaPolicyModel.ListCandidates 同口径（本应用/全局 × 精确/'*'，只取 enabled=1），
// 层级判定直接调用 model.NarrowPolicies 本身：重算与在线判定必须共用同一个纯函数，
// 否则重算写回的快照会与自己算出的结论不一致。
// 没有匹配长度的生效规则时不回该键（调用方回退旧快照）。
func quotaEffectiveLimits(appID int64, apiCode string,
	batches ...[]*model.QuotaPolicy) map[int64]int64 {
	cands := make([]*model.QuotaPolicy, 0, 8)
	for _, batch := range batches {
		for _, p := range batch {
			if p == nil || p.Enabled != 1 || !quotaRuleApplies(p, apiCode) {
				continue
			}
			cands = append(cands, p)
		}
	}
	out := make(map[int64]int64, len(cands))
	for _, p := range model.NarrowPolicies(cands, appID, apiCode) {
		if p == nil || p.WindowSeconds <= 0 {
			continue
		}
		if _, ok := out[p.WindowSeconds]; !ok {
			out[p.WindowSeconds] = nonNeg(p.QuotaLimit)
		}
	}
	return out
}

// quotaRuleApplies 判断一条规则是否可能作用到该接口（精确同名或 '*' 通配）。
func quotaRuleApplies(p *model.QuotaPolicy, apiCode string) bool {
	if apiCode == "" {
		return true
	}
	return p.APICode == apiCode || p.APICode == model.AnyAPICode
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
