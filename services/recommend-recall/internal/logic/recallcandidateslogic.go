package logic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/common/idgen"
	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

type RecallCandidatesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRecallCandidatesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RecallCandidatesLogic {
	return &RecallCandidatesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 服务端生成的审计 ID 前缀（列宽 VARCHAR(64)；"<prefix>_<ULID>" 远小于 64）。
const (
	requestIDPrefix  = "recall"
	snapshotIDPrefix = "snap"
	// visibleQueryChunk 是一次 FilterVisible 的 aid 数上限：候选总量在极端配置下可以远大于
	// 一个安全的 IN 列表，分片既不放宽合规过滤（每片都必须通过），也不会拼出无界 SQL。
	visibleQueryChunk = model.MaxPoolQueryLimit
)

// 多路召回主入口：按路取数 -> 过滤 -> 去重合并 -> 显式降级声明
//
// 本文件只做"一次请求的编排"（入参归一、预算、降级判定、审计、回复），
// 取数/过滤/合并的机制件在 recallsource.go，这里不出现任何 SQL。
//
// 三条不可让的线：
//  1. 空候选绝不以"正常结果"的面目返回：全空必然带 DEGRADE_REASON_ALL_SOURCES_EMPTY，
//     调用方禁止不允许降级时（allow_degrade=false）直接上抛 model.ErrAllSourcesEmpty；
//  2. 依赖不可读不冒充"这个用户没有内容"：nil 依赖以 repository.ErrSourceNotConfigured
//     的文本进降级声明（feature_unavailable），并按 README 走冷启动/兜底路；
//  3. 降级结果 ttl_seconds 恒为 0，禁止网关把故障快照缓存给之后的正常请求。
//
// 已知能力缺口（不静默、写在注释里给接线的人）：契约的 VisibilitySource.BlockedUps 给出的是
// 被拉黑的 UP mid 列表，而 aid -> UP 的归属是 video 服务的事实，本服务没有该投影，
// 因此"按拉黑 UP 过滤候选"在接线时必须由 video 侧的可见性判定（FilterVisible）承担，
// 不在这里用假映射冒充合规过滤。
func (l *RecallCandidatesLogic) RecallCandidates(in *rpc.RecallCandidatesReq) (*rpc.RecallCandidatesReply, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: RecallCandidatesReq", model.ErrRequestRequired)
	}
	repo := l.svcCtx.Repository
	if repo == nil {
		// 必需依赖缺失是装配缺陷，不是可降级状态：回一整套"降级声明"照样是假响应。
		return nil, fmt.Errorf("%w: repository is not assembled", repository.ErrSourceNotConfigured)
	}
	opts := repo.Options()
	rc := l.svcCtx.Config.Recall

	tracker := newDegradeTracker()
	p, err := l.buildPlan(in, opts, rc, tracker)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	// 预算只约束"取候选"这段：审计行落在预算之外（丢了审计的召回等于不可回放，
	// 而超预算 20ms 写一行 INSERT 不会让谁更不可用）。
	ctx, cancel := budgetContext(l.ctx, rc.BudgetMillis)
	defer cancel()

	runner := &recallRunner{repo: repo, rc: rc, opts: opts, logger: l.Logger, limit: p.limit}
	reads, merged, counts := runner.collect(ctx, p, p.sources, tracker)

	attempted := p.sources
	if len(merged) == 0 && p.allowDegrade {
		// 主路全空才要兜底：不允许降级的请求（压测/回放）必须看到"主路真的没出数"，
		// 这里偷偷兜一把再报错等于把现场改写过。
		if fb := runner.fallbackSources(p); len(fb) > 0 {
			tracker.add(model.DegradeReasonColdStart, "requested sources served nothing, retry on fallback sources %s",
				encodeSourcesCSV(fb))
			fReads, fMerged, fCounts := runner.collect(ctx, p, fb, tracker)
			reads = append(reads, fReads...)
			for key, n := range fCounts {
				counts[key] = n
			}
			merged = fMerged
			attempted = append(append(make([]int32, 0, len(p.sources)+len(fb)), p.sources...), fb...)
		}
	}
	if len(merged) == 0 {
		// 到这里就是"没有任何可下发候选"：这必须是响应里的显式主原因，
		// 绝不能是一个空数组 + nil error。
		tracker.add(model.DegradeReasonAllSourcesEmpty, "no candidate after %d pool read(s)", len(reads))
	}

	served := truncateMerged(merged, p.limit)
	candidates := candidateList(served)
	if len(candidates) > opts.MaxCandidates {
		// 不变量自证：limit 已在归一时被 MaxCandidates 卡住，这里超了说明裁剪环节被改坏，
		// 宁可报错也不能把一次无界放大交给网关。
		return nil, fmt.Errorf("%w: served %d > max_candidates %d", model.ErrLimitTooLarge,
			len(candidates), opts.MaxCandidates)
	}

	cost := costMillis(start)
	stats := statsFor(attempted, reads, counts)

	row, lerr := l.buildRequestLog(p, reads, stats, len(merged), len(candidates), tracker, cost)
	if lerr != nil {
		// 审计行构造失败不改写候选（候选是真算出来的），但必须留下错误：
		// 这次召回从此无法回放，是比慢 10ms 更严重的事。
		l.Logger.Errorf("recommend-recall: build recall audit row request_id=%s: %v", p.requestID, lerr)
	} else {
		l.insertRequestLog(repo, row)
	}

	if !p.allowDegrade && tracker.degraded() {
		// 不允许降级：把已经如实记录的降级状态翻成错误。审计行已经落库，压测/回放看得到现场。
		// 全空用专门的 ErrAllSourcesEmpty（它就是为这个场景存在的哨兵），其余用降级哨兵。
		if len(candidates) == 0 {
			return nil, fmt.Errorf("%w: reason=%s %s", model.ErrAllSourcesEmpty,
				tracker.primary(), tracker.detailText())
		}
		return nil, tracker.errDisabled()
	}

	ttl := int64(0)
	if !tracker.degraded() {
		ttl = rc.TTLSeconds
		if ttl < 0 {
			ttl = 0
		}
	}
	return &rpc.RecallCandidatesReply{
		RequestId:   p.requestID,
		SnapshotId:  p.snapshotID,
		Candidates:  candidates,
		PerSource:   stats,
		Degradation: tracker.info(cost),
		TtlSeconds:  ttl,
		ServerTime:  nowUnix(),
	}, nil
}

// buildPlan 归一入参（README 的"条数上限都在配置里，代码不写死"）。
//
// 每一步都是"拒绝或如实标记"，没有静默裁剪：
//   - limit 超上限报 ErrLimitTooLarge，而不是按上限悄悄少给（调用方以为拿到了 400 条）；
//   - 未启用的路直接 ErrSourceDisabled，配置里没开的路跑起来只会稳定降级；
//   - 游客请求里超出游客能力的路会被裁掉，并被裁掉这件事本身记进降级声明；
//   - device_id_hash 只做隐私闸门（sha256 摘要），本服务任何表与日志都不落它。
func (l *RecallCandidatesLogic) buildPlan(in *rpc.RecallCandidatesReq, opts repository.Options,
	rc config.RecallConf, tr *degradeTracker) (*recallPlan, error) {
	limit := int(in.GetLimit())
	switch {
	case limit < 0:
		return nil, fmt.Errorf("%w: limit=%d", model.ErrInvalidLimit, limit)
	case limit == 0:
		limit = opts.DefaultLimit
	}
	if limit > opts.MaxCandidates {
		return nil, fmt.Errorf("%w: limit=%d > %d", model.ErrLimitTooLarge, limit, opts.MaxCandidates)
	}

	reqCtx := in.GetContext()
	mid := reqCtx.GetMid()
	if mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d", model.ErrRequestLogRequired, mid)
	}
	if err := checkDeviceIDHash(reqCtx.GetDeviceIdHash()); err != nil {
		return nil, err
	}
	scene, err := optionalText("scene", reqCtx.GetScene(), colScene)
	if err != nil {
		return nil, err
	}
	appVersion, err := optionalText("app_version", reqCtx.GetAppVersion(), colAppVersion)
	if err != nil {
		return nil, err
	}
	region, err := optionalText("region", reqCtx.GetRegion(), colRegion)
	if err != nil {
		return nil, err
	}
	requestID, err := recallRequestID(reqCtx.GetRequestId())
	if err != nil {
		return nil, err
	}
	snapshotID, err := idgen.Prefixed(snapshotIDPrefix)
	if err != nil {
		return nil, fmt.Errorf("recommend-recall: generate snapshot_id: %w", err)
	}

	requested, err := sourcesFromRPC(in.GetSources())
	if err != nil {
		return nil, err
	}
	if len(requested) == 0 {
		// 没有显式路就用配置的默认组合（DefaultSources 由 RecallConf.Validate 保证是
		// EnabledSources 的非空子集，因此这里不必再判一遍启用）。
		requested = sourceList(rc.DefaultSources)
	}
	enabled := enabledSet(rc.EnabledSources)
	for _, source := range requested {
		if _, ok := enabled[source]; !ok {
			return nil, fmt.Errorf("%w: source=%d", model.ErrSourceDisabled, source)
		}
	}
	sources := requested
	if mid == 0 {
		kept, trimmed := trimGuestSources(requested, guestSources(rc))
		sources = kept
		for _, source := range trimmed {
			// 游客没有 mid，也就没有关注列表与行为种子。这一路现在读只会掉进降级，
			// 提前裁掉并把"裁了哪一路"如实记进 dropped_sources，比让它打一轮依赖更好解释。
			tr.drop(source, model.DegradeReasonColdStart, "guest request (mid=0): source trimmed")
		}
	}

	seedAids, err := checkAidList("seed_aids", in.GetSeedAids(), opts.MaxSeedAids)
	if err != nil {
		return nil, err
	}
	seedTags, err := checkAidList("seed_tag_ids", in.GetSeedTagIds(), opts.MaxSeedTags)
	if err != nil {
		return nil, err
	}
	excludeAids, err := checkAidList("exclude_aids", in.GetExcludeAids(), opts.MaxExcludeAids)
	if err != nil {
		return nil, err
	}

	return &recallPlan{
		requestID:  requestID,
		snapshotID: snapshotID,
		mid:        mid,
		platform:   platformFromRPC(reqCtx.GetPlatform()),
		scene:      scene,
		appVersion: appVersion,
		region:     region,
		traceID:    sanitizeTraceID(reqCtx.GetTraceId()),
		limit:      limit,
		sources:    sources,
		requested:  requested,
		seedAids:   seedAids,
		seedTags:   seedTags,
		exclude:    int64Set(excludeAids),
		// 请求级与开关级取与：任一关闭都不允许静默降级（压测要看到真实失败）。
		allowDegrade: in.GetAllowDegrade() && rc.DegradeEnabled,
	}, nil
}

// recallRequestID 归一审计/幂等键：客户端没带就由服务端生成并在响应回传（契约要求回显）。
// 带上限校验而不是截断：request_id 是回放锚点，截断后调用方拿到的 ID 与库里那行对不上。
func recallRequestID(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		id, err := idgen.Prefixed(requestIDPrefix)
		if err != nil {
			return "", fmt.Errorf("recommend-recall: generate request_id: %w", err)
		}
		return id, nil
	}
	if err := checkMaxLen("request_id", v, colRefID); err != nil {
		return "", err
	}
	return v, nil
}

// collect 完成一轮"寻路 -> 指针 -> 取数 -> 可见性过滤 -> 合并"。
//
// 允许被调用两次（主路 + 兜底路）：兜底路的候选合并同一套统计与摘要，
// 但降级原因始终汇进同一个 tracker，不会因为"又读了一遍"而丢第一轮的失败现场。
func (r *recallRunner) collect(ctx context.Context, p *recallPlan, sources []int32,
	tr *degradeTracker) ([]poolRead, []mergedCandidate, map[readKey]int32) {
	counts := make(map[readKey]int32, len(sources))
	reads := r.planSources(ctx, p, sources, tr)
	if len(reads) == 0 {
		return nil, nil, counts
	}
	if err := r.resolvePointers(ctx, reads); err != nil {
		// 指针批量读失败 = 这一轮所有池路都无法判定可见版本。整次按 store_unavailable 降级，
		// 并把每个未取数的池读都打上同一原因，避免 statOfSource 把"读不到库"报成"池没上线"。
		tr.add(model.DegradeReasonStoreUnavailable, "%s", err.Error())
		markReadsDropped(reads, tr, model.DegradeReasonStoreUnavailable, "current pointer lookup failed")
	}
	r.fetchPoolEntries(ctx, reads, tr)

	aids := candidateAids(reads, p.exclude)
	visible, visibilityOK := r.filterVisibleChunks(ctx, aids, tr)
	// 依赖不可读时不剔除任何稿件（没有依据就替下游做决定等于凭空抹掉候选），
	// 由 tracker 的 feature_unavailable/store_unavailable 声明这次结果不可信。
	filterSet := visible
	if !visibilityOK {
		filterSet = nil
	}
	if visibilityOK && len(aids) > 0 && len(visible) == 0 {
		// "读得到、但一条都不允许下发"是真的无可下发。mergeReads/countByPool 的空集合短路
		// （len(visible)>0 才筛）在这里必须被绕开，否则合规过滤会被当成"没筛"而放行全部候选。
		for _, read := range reads {
			tr.dropSourceOnly(read.source)
		}
		return reads, nil, counts
	}
	counts = countByPool(reads, filterSet, p.exclude)
	merged := mergeReads(reads, filterSet, p.exclude)
	for _, candidate := range merged {
		// "实际出数"判定的是过滤后仍有贡献的路，与后面的 limit 裁剪无关：
		// 被裁掉的名次仍然是这一路真实出的候选。
		tr.serve(candidate.source)
	}
	sortMerged(merged)
	return reads, merged, counts
}

// planSources 逐路展开池读，并在每一路开始前给预算让路。
//
// 为什么一路一停而不是一次 planPools：预算耗尽要的是"停止开新路"，
// 一次性展开会把协同/标签的种子依赖调用全打出去，那时 budget_exhausted
// 说的就不再是"没来得及读的路"。
// 每路用私有 tracker，才能在"这一路一条池都没展开"时问出真实原因。
func (r *recallRunner) planSources(ctx context.Context, p *recallPlan, sources []int32,
	tr *degradeTracker) []poolRead {
	reads := make([]poolRead, 0, len(sources))
	for _, source := range sources {
		if err := ctx.Err(); err != nil {
			tr.drop(source, model.DegradeReasonBudgetExhausted, "budget exhausted before planning source %d: %s",
				source, err.Error())
			continue
		}
		if gap := dependencyGap(r.repo, source, p); gap != nil {
			// 依赖没接线（nil）：不进 planPools，否则是对 nil 接口发起调用（panic）。
			tr.drop(source, model.DegradeReasonFeatureUnavailable, "%s", gap.Error())
			reads = append(reads, poolRead{source: source, errKey: model.DegradeReasonFeatureUnavailable})
			continue
		}
		single := *p
		single.sources = []int32{source}
		singleTracker := newDegradeTracker()
		got := r.planPools(ctx, &single, singleTracker)
		if len(got) == 0 {
			// 这一路没展开出任何池读（没有种子、依赖未接线、缺 mid）。
			// 留空的话 statOfSource 只会报 pool_not_ready，那是把"能力缺失"说成"池没上线"，
			// 所以合成一条带真实原因的空读，逐路统计才有正确归因。
			got = []poolRead{{source: source, errKey: singleTracker.sourceReason()}}
		}
		adoptTracker(tr, singleTracker)
		reads = append(reads, got...)
	}
	return reads
}

// fallbackSources 按 README 的兜底顺序（冷启动池 -> 配置的兜底路）给出再取一次的路，
// 跳过本轮已经读过的路：同一个池版本再读一遍不会变出候选，只会把压力翻倍。
func (r *recallRunner) fallbackSources(p *recallPlan) []int32 {
	out := make([]int32, 0, 2)
	seen := make(map[int32]struct{}, len(p.sources)+2)
	for _, source := range p.sources {
		seen[source] = struct{}{}
	}
	for _, configured := range []int64{r.rc.ColdStartSource, r.rc.FallbackSource} {
		source := int32(configured)
		if !model.ValidSource(source) || !r.enabled(source) {
			// 兜底路没开是配置问题（RecallConf.Validate 本应挡住），
			// 这里跳过而不是报错：已经没有别的手段把这次召回救回来。
			continue
		}
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		out = append(out, source)
	}
	return out
}

// filterVisibleChunks 分批做可见性过滤，返回 (可见集合, 依赖是否可用)。
//
// 与 filterInvisible 的分工：那里是"一次调用的错误分类"，这里是"一次请求的分片与 nil 守卫"。
// 任一分片失败即整体判为不可用（宁可带降级返回，也不能只筛了一半就装作筛过）。
func (r *recallRunner) filterVisibleChunks(ctx context.Context, aids []int64, tr *degradeTracker) (map[int64]struct{}, bool) {
	if len(aids) == 0 {
		// 没有候选要判定，过滤这一步天然成立。
		return map[int64]struct{}{}, true
	}
	if r.repo == nil || r.repo.Visibility == nil {
		tr.add(model.DegradeReasonFeatureUnavailable, "%v: VisibilitySource is not configured",
			repository.ErrSourceNotConfigured)
		return nil, false
	}
	if len(aids) <= visibleQueryChunk {
		set, key := r.filterInvisible(ctx, aids, tr)
		if key != model.DegradeReasonNone {
			return nil, false
		}
		return set, true
	}
	visible := make(map[int64]struct{}, len(aids))
	for start := 0; start < len(aids); start += visibleQueryChunk {
		end := start + visibleQueryChunk
		if end > len(aids) {
			end = len(aids)
		}
		set, key := r.filterInvisible(ctx, aids[start:end], tr)
		if key != model.DegradeReasonNone {
			return nil, false
		}
		for aid := range set {
			visible[aid] = struct{}{}
		}
	}
	return visible, true
}

// buildRequestLog 构造 recall_request_log 审计行（候选可回放的唯一依据）。
//
// 隐私：本行没有设备维度（表里也没有），mid/scene/region 是排障最小集；
// 逐路统计里只有池坐标与条数，不含稿件与用户内容字段。
func (l *RecallCandidatesLogic) buildRequestLog(p *recallPlan, reads []poolRead, stats []*rpc.SourceStat,
	candidateCount, returnedCount int, tr *degradeTracker, costMs int32) (*model.RecallRequestLog, error) {
	perSource, err := encodeSourceStats(stats)
	if err != nil {
		return nil, err
	}
	candidates, err := toInt32("candidate_count", int64(candidateCount))
	if err != nil {
		return nil, err
	}
	returned, err := toInt32("returned_count", int64(returnedCount))
	if err != nil {
		return nil, err
	}
	cost, err := toInt32("cost_ms", int64(costMs))
	if err != nil {
		return nil, err
	}
	degraded := int32(0)
	reason := model.DegradeReasonNone
	if tr.degraded() {
		degraded = 1
		reason = tr.primary()
		if reason == model.DegradeReasonNone {
			// 只有"被丢弃的路"没有登记原因（编码漏记）：留空会让回放把降级读成正常，
			// 与 errDisabled 同一口径补一个最保守的 key。
			reason = model.DegradeReasonPoolNotReady
		}
	}
	return &model.RecallRequestLog{
		RequestID:        p.requestID,
		SnapshotID:       p.snapshotID,
		Mid:              p.mid,
		Scene:            p.scene,
		Platform:         p.platform,
		AppVersion:       p.appVersion,
		Region:           p.region,
		RequestedSources: encodeSourcesCSV(p.requested),
		PerSource:        perSource,
		CandidateCount:   candidates,
		ReturnedCount:    returned,
		Degraded:         degraded,
		DegradeReason:    reason,
		DroppedSources:   encodeSourcesCSV(tr.dropped),
		VersionsDigest:   versionsDigest(poolVersionPairs(reads)),
		CostMs:           cost,
		TraceID:          p.traceID,
		Ctime:            nowUnix(),
	}, nil
}

// insertRequestLog 落审计行，失败只记日志。
//
// 候选是真算出来的，因为审计写不进去就把结果丢掉，等于让"可追溯"变成"更不可用"。
// ErrRequestLogExists 同样不致命，但要说清代价：本表只存摘要不存候选原文（AGENTS.md §5
// 不复制主数据），所以同一 request_id 的第二次执行无法回放第一次的结果，
// 这里如实返回新算的候选，并把"同一 ID 留了两份现场"的错误留在日志里。
func (l *RecallCandidatesLogic) insertRequestLog(repo *repository.Repository, row *model.RecallRequestLog) {
	if row == nil {
		return
	}
	if err := repo.RequestLog.Insert(l.ctx, row); err != nil {
		if errors.Is(err, model.ErrRequestLogExists) {
			l.Logger.Errorf("recommend-recall: request_id %s already audited; served candidates were recomputed "+
				"and are NOT the stored snapshot (recall_request_log keeps no candidate list): %v",
				row.RequestID, err)
			return
		}
		l.Logger.Errorf("recommend-recall: insert recall_request_log request_id=%s snapshot_id=%s: %v",
			row.RequestID, row.SnapshotID, err)
	}
}

// --- 与 recallsource.go 配套的机制件（同包，不放那里是为了不改动已冻结的机制文件）---

// budgetContext 取「调用方 ctx 剩余时间」与「Config.Recall.BudgetMillis」的较小值。
//
// 外层已经到点时不再包一层（立刻过期的 ctx 会把所有读判成 budget_exhausted，
// 而真实原因是上游超时，透传原 ctx 让 degradeKeyForDependencyErr 去分类）。
func budgetContext(ctx context.Context, budgetMillis int) (context.Context, context.CancelFunc) {
	noop := func() {}
	if budgetMillis <= 0 {
		return ctx, noop
	}
	timeout := time.Duration(budgetMillis) * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return ctx, noop
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	return context.WithTimeout(ctx, timeout)
}

// guestSources 是游客（mid==0）唯一可走的路：冷启动池 + 配置的兜底路。
//
// RecallCandidates 用它裁剪入参、GetRecallConfig 用它裁剪下发列表，
// 两处共用同一份判定才不会漂移（下发全集等于诱导客户端发出必然降级的请求）。
func guestSources(rc config.RecallConf) map[int32]struct{} {
	allowed := make(map[int32]struct{}, 2)
	for _, source := range []int64{rc.ColdStartSource, rc.FallbackSource} {
		if s := int32(source); model.ValidSource(s) {
			allowed[s] = struct{}{}
		}
	}
	return allowed
}

// trimGuestSources 裁掉游客取不到的路，返回 (保留路, 被裁路)，两边都保持入参顺序。
func trimGuestSources(in []int32, allowed map[int32]struct{}) (kept, trimmed []int32) {
	kept = make([]int32, 0, len(in))
	for _, source := range in {
		if _, ok := allowed[source]; ok {
			kept = append(kept, source)
			continue
		}
		trimmed = append(trimmed, source)
	}
	return kept, trimmed
}

// dependencyGap 判定"这一路必需的下游依赖是否压根没装配"。
// 返回的 error 包 repository.ErrSourceNotConfigured，由调用方映射成 feature_unavailable。
//
// 判定要贴着 planPools 的实际用法：标签/协同路带了种子就不问特征下游，
// 只有真的要现取种子/现取向量候选时才需要 Features。
func dependencyGap(repo *repository.Repository, source int32, p *recallPlan) error {
	if repo == nil {
		return fmt.Errorf("%w: repository", repository.ErrSourceNotConfigured)
	}
	if repo.Features != nil {
		return nil
	}
	// 向量路的候选完全由下游在线给出，没有 Features 就没有这一路。
	if source == model.SourceVector {
		return fmt.Errorf("%w: FeatureSource.GetVectorCandidates", repository.ErrSourceNotConfigured)
	}
	// 标签/协同路只在"请求没带种子、要现取种子"时依赖 Features：
	// 种子已在请求里就不花这次下游调用，这时候缺依赖不该把能出的路判死。
	if source == model.SourceTag && len(p.seedTags) == 0 {
		return fmt.Errorf("%w: FeatureSource.GetUserInterest", repository.ErrSourceNotConfigured)
	}
	if source == model.SourceCollab && len(p.seedAids) == 0 {
		return fmt.Errorf("%w: FeatureSource.GetBehaviorSignals", repository.ErrSourceNotConfigured)
	}
	return nil
}

// markReadsDropped 给"还没来得及读就先失败"的池读统一打降级标记。
// 保留 poolKey 进文本（mid 池键会被 poolKeyForLog 遮蔽），逐路统计才归因正确。
func markReadsDropped(reads []poolRead, tr *degradeTracker, key, detail string) {
	for i := range reads {
		read := &reads[i]
		if read.live || read.errKey != "" {
			continue
		}
		read.errKey = key
		tr.drop(read.source, key, "%s: pool %s", detail, poolKeyForLog(read.poolKey))
	}
}

// candidateAids 汇总待判可见性的 aid：跳过已降级/空的读，先剔 exclude，再按 aid 去重。
func candidateAids(reads []poolRead, exclude map[int64]struct{}) []int64 {
	var out []int64
	seen := make(map[int64]struct{}, 256)
	for _, read := range reads {
		if read.errKey != "" {
			continue
		}
		for _, entry := range read.entries {
			if entry.Aid <= 0 {
				continue
			}
			if _, bad := exclude[entry.Aid]; bad {
				continue
			}
			if _, ok := seen[entry.Aid]; ok {
				continue
			}
			seen[entry.Aid] = struct{}{}
			out = append(out, entry.Aid)
		}
	}
	return out
}

// poolVersionPairs 汇总本次实际读到的池版本（在线实时路没有池快照，不进摘要）。
func poolVersionPairs(reads []poolRead) []versionPair {
	out := make([]versionPair, 0, len(reads))
	for _, read := range reads {
		if read.live || read.errKey != "" || read.version <= 0 {
			continue
		}
		out = append(out, versionPair{Source: read.source, PoolKey: read.poolKey, Version: read.version})
	}
	return out
}

// statsFor 按"请求路 -> 兜底路"的顺序给出逐路统计（顺序稳定，回放比对才有意义）。
func statsFor(sources []int32, reads []poolRead, counts map[readKey]int32) []*rpc.SourceStat {
	stats := make([]*rpc.SourceStat, 0, len(sources))
	seen := make(map[int32]struct{}, len(sources))
	for _, source := range sources {
		if _, ok := seen[source]; ok {
			continue
		}
		seen[source] = struct{}{}
		stats = append(stats, statOfSource(source, reads, counts))
	}
	return stats
}

// adoptTracker 把单路的降级状态并进请求级 tracker（原因、文本、丢弃/出数路全部保留）。
func adoptTracker(dst, src *degradeTracker) {
	if dst == nil || src == nil {
		return
	}
	for _, key := range src.reasons {
		dst.add(key, "%s", src.details[key])
	}
	for _, source := range src.dropped {
		dst.appendSources(&dst.dropped, source)
	}
	for _, source := range src.served {
		dst.appendSources(&dst.served, source)
	}
}

// sourceReason 给"这一路为什么没出数"一个可落 SourceStat.error_code 的 key。
//
// 它只在单路私有 tracker 上有意义（一个 tracker 只承载一路的原因）：
// 完全没有登记原因时才是 pool_not_ready —— 那才是"池真没上线"的字面意思。
func (t *degradeTracker) sourceReason() string {
	if key := t.primary(); key != model.DegradeReasonNone {
		return key
	}
	return model.DegradeReasonPoolNotReady
}

// costMillis 把耗时收成 int32（cost_ms 是排障字段，饱和而不是报错：
// 一次 25 天的挂起本身就一定会在别处被暴露）。
func costMillis(start time.Time) int32 {
	ms := time.Since(start).Milliseconds()
	if ms <= 0 {
		return 0
	}
	if ms > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(ms)
}
