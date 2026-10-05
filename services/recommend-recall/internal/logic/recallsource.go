// 本文件是 logic 包的手写扩展（在线召回的选路、取数、过滤与合并机制），
// 不是 goctl 生成产物。
//
// 与 recallcandidateslogic.go 的分工：那里是"一次请求的编排"（入参归一、预算、
// 降级判定、审计、回复），这里是可独立推理的机制件——
// 降级汇总状态机、按池展开的读取、指针批量解析、版本粒度读透缓存、去重合并与粗排。
// 这里不出现任何 SQL，也不出现任何 gRPC 依赖错误码，方便逐条对照 README 的降级矩阵。
package logic

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// 降级主因的选取顺序（越靠前越严重）。
//
// 依据是"调用方拿到这个响应该做什么"：
//  1. all_sources_empty     完全没有候选，feed 必须切端到端兜底；
//  2. store_unavailable     MySQL/Redis 异常，整服务健康都可能受影响；
//  3. pool_not_ready        这一路根本没上线，是离线作业问题，不修就一直空；
//  4. downstream_timeout    依赖抖动，重试可能就好了；
//  5. feature_unavailable   依赖未接线/不可读，是个稳定的"能力缺失"而非故障；
//  6. budget_exhausted      本服务自己裁剪了路，属可解释的性能降级；
//  7. cold_start            非故障，只是必须显式声明"这是冷启动结果"。
var degradeSeverity = map[string]int{
	model.DegradeReasonAllSourcesEmpty:    1,
	model.DegradeReasonStoreUnavailable:   2,
	model.DegradeReasonPoolNotReady:       3,
	model.DegradeReasonDownstreamTimeout:  4,
	model.DegradeReasonFeatureUnavailable: 5,
	model.DegradeReasonBudgetExhausted:    6,
	model.DegradeReasonColdStart:          7,
}

// degradeDetailMaxLen 是 DegradationInfo.detail 的文本上限（不是 DB 列，但没有上限
// 就等于允许一次响应带上无界文本）。
const degradeDetailMaxLen = 512

// degradeTracker 汇总一次召回的降级状态。
//
// 它是"降级不能吞"的落点：每一处被丢弃的路、每一次依赖故障都必须 add/drop 进来，
// 最后一次性变成 DegradationInfo + recall_request_log 的字段，
// 而不是在某个分支里 log 一下就继续当没事发生。
type degradeTracker struct {
	reasons []string          // 已记录的降级 key（不含 DegradeReasonNone），按发生顺序
	details map[string]string // key -> 首条排障文本（同 key 只留第一条，避免文本膨胀）
	dropped []int32           // 被丢弃/未出数的召回路（去重）
	served  []int32           // 实际出数的召回路（去重）
}

func newDegradeTracker() *degradeTracker {
	return &degradeTracker{details: make(map[string]string, 4)}
}

// add 记录一个降级原因（detail 用 fmt 格式化，避免调用点拼串把 % 当字面量）。
// key 必须是 model.DegradeReason* 受控集合内的值；DegradeReasonNone（空串）表示"没有降级"，
// 传进来直接忽略——静默记成未知原因会让"未降级"的响应带上噪声文本。
func (t *degradeTracker) add(key, format string, args ...interface{}) {
	if key == model.DegradeReasonNone {
		return
	}
	if !model.ValidDegradeReason(key) {
		// 未知 key 不进 reasons：rpc 侧无法映射成枚举，落库也会被 Validate 拒绝。
		// 这里是编码错误，必须在日志里留下原文，不能悄悄丢掉。
		logx.Errorf("recommend-recall: unknown degrade reason %q (dropped from response marker)", key)
		return
	}
	detail := ""
	if format != "" {
		detail = fmt.Sprintf(format, args...)
	}
	for _, exist := range t.reasons {
		if exist == key {
			return
		}
	}
	t.reasons = append(t.reasons, key)
	if detail != "" {
		t.details[key] = detail
	}
}

// drop 记录"这一路没出数"：同时登记原因与丢弃的路。
func (t *degradeTracker) drop(source int32, key, format string, args ...interface{}) {
	t.add(key, format, args...)
	t.appendSources(&t.dropped, source)
}

// serve 记录"这一路出了数"。
func (t *degradeTracker) serve(source int32) {
	t.appendSources(&t.served, source)
}

func (t *degradeTracker) appendSources(dst *[]int32, source int32) {
	for _, s := range *dst {
		if s == source {
			return
		}
	}
	*dst = append(*dst, source)
}

func (t *degradeTracker) degraded() bool { return len(t.reasons) > 0 || len(t.dropped) > 0 }

// primary 返回主原因（按 degradeSeverity 选最严重的一个）。
func (t *degradeTracker) primary() string {
	best := model.DegradeReasonNone
	bestRank := 0
	for _, key := range t.reasons {
		rank, ok := degradeSeverity[key]
		if !ok {
			continue
		}
		if bestRank == 0 || rank < bestRank {
			best, bestRank = key, rank
		}
	}
	return best
}

// detailText 拼接排障文本（"key=detail" 分号连接，总长有上限）。
func (t *degradeTracker) detailText() string {
	parts := make([]string, 0, len(t.reasons))
	total := 0
	for _, key := range t.reasons {
		part := key
		if d := t.details[key]; d != "" {
			part += "=" + d
		}
		if total+len(part)+2 > degradeDetailMaxLen {
			part = part[:max(0, degradeDetailMaxLen-total-2)]
			parts = append(parts, part)
			break
		}
		parts = append(parts, part)
		total += len(part) + 2
	}
	return strings.Join(parts, "; ")
}

// info 构造降级声明。未降级时 reason 恒为 UNSPECIFIED、detail 为空，
// 避免"degraded=false 但带着一堆文本"的自相矛盾响应。
func (t *degradeTracker) info(costMs int32) *rpc.DegradationInfo {
	out := &rpc.DegradationInfo{
		Degraded:       t.degraded(),
		CostMs:         costMs,
		DroppedSources: rpcSourceList(t.dropped),
		ServedSources:  rpcSourceList(t.served),
	}
	if out.Degraded {
		out.Reason = degradeReasonFromKey(t.primary())
		out.Detail = t.detailText()
	}
	return out
}

// err 把降级状态翻译成"不允许降级"时的错误（in.AllowDegrade=false 或全局关闭降级）。
// 包装主原因的文本，让调用方在 gRPC 错误里就能看到失败落点，而不是只有一个哨兵。
func (t *degradeTracker) errDisabled() error {
	key := t.primary()
	if key == model.DegradeReasonNone && len(t.dropped) > 0 {
		key = model.DegradeReasonPoolNotReady
	}
	return fmt.Errorf("%w: reason=%s %s", model.ErrDegradationDisabled, key, t.detailText())
}

// poolRead 是「读某个池的某个版本 TopN」的一次取数。
//
// 粒度是池而不是路：标签路与协同路一个请求可能读 N 个种子池
// （tag:<id> / aid:<seed>），按池展开才能做指针批量解析与版本粒度缓存。
// 一条路的 rpc.SourceStat 由它名下的所有 poolRead 聚合而成（见 statOfSource）。
type poolRead struct {
	source  int32
	poolKey string
	// planned 是本次计划取数上限（已按路内池数做过公平分配）。
	planned int
	// version/batchID 由 resolvePointers 填入；version==0 表示该池没有可用的 CURRENT。
	version int64
	// batchID 是 recall_pool_current 上记录的生效批次（候选的 batch_id 出处）。
	batchID string
	// entries 是实际取到的候选（score DESC, aid ASC），rank_in_source 即下标。
	entries []poolEntry
	// errKey 非空表示这次读取降级了（pool_not_ready / feature_unavailable / ...）。
	errKey string
	// live 表示候选不是从池读出来的（向量近邻这类在线实时路），此时 pool_version=0。
	live bool
}

// recallPlan 是 RecallCandidatesReq 归一后的执行参数（校验与裁剪都在 logic 侧做完）。
//
// 单列一个结构体的意义：把"哪些入参经过怎样处理才生效"集中在一处，
// 后面的选路、过滤、合并都只读它，不再回头解释原始请求。
type recallPlan struct {
	// requestID/snapshotID 是审计与回放坐标：客户端没带 request_id 时由服务端生成并回显。
	requestID  string
	snapshotID string
	mid        int64
	platform   int32
	scene      string
	appVersion string
	region     string
	traceID    string
	// limit 是归一后的总候选上限（已保证 0<limit<=MaxCandidates）。
	limit int
	// sources 是实际要读的路（已过启用校验与游客裁剪，保持优先级无关的顺序）。
	sources []int32
	// requested 是调用方原始请求的路（进审计的 requested_sources）。
	requested []int32
	seedAids  []int64
	seedTags  []int64
	exclude   map[int64]struct{}
	// allowDegrade 决定是否允许降级出数（请求级 with 服务级开关的与运算结果）。
	allowDegrade bool
}

// recallRunner 是一次在线召回的运行时依赖集合（每请求构造一个，不跨请求复用）。
type recallRunner struct {
	repo   *repository.Repository
	rc     config.RecallConf
	opts   repository.Options
	logger logx.Logger
	// limit 是本次请求归一后的总候选上限（planPools 之前必须已就绪）。
	limit int
}

// planPools 把「请求的路 + 请求上下文」折算成待读的池列表。
//
// 关键点：
//   - 关注/冷启动/热门池不需要任何下游特征就能寻址（pool_key 直接由 mid / platform 决定），
//     所以这三路在本服务特征依赖未接线时依然能出数，这是降级后仍有结果的根本原因；
//   - 标签/协同路没带种子时才去问特征下游，种子已在请求里就不花这次 RPC；
//   - 依赖读不到时"丢弃这一路并记降级原因"，绝不返回空条目冒充"这个用户没兴趣"。
func (r *recallRunner) planPools(ctx context.Context, p *recallPlan, tr *degradeTracker) []poolRead {
	reads := make([]poolRead, 0, len(p.sources)+2)
	for _, source := range p.sources {
		if !r.enabled(source) {
			// 调用方请求了未启用的路：在入参归一阶段已硬拒绝（ErrSourceDisabled），
			// 走到这里说明 plans 构造有 bug，按降级记录而不是静默丢。
			tr.drop(source, model.DegradeReasonFeatureUnavailable, "source disabled")
			continue
		}
		switch source {
		case model.SourceHot:
			reads = append(reads, poolRead{source: source, poolKey: hotPoolKey(), planned: r.perSourceWant()})
		case model.SourceCold:
			reads = append(reads, poolRead{source: source, poolKey: coldPoolKey(p.platform), planned: r.perSourceWant()})
		case model.SourceFollow:
			if p.mid <= 0 {
				tr.drop(source, model.DegradeReasonColdStart, "follow pool needs mid")
				continue
			}
			reads = append(reads, poolRead{
				source: source, poolKey: "mid:" + strconv.FormatInt(p.mid, 10), planned: r.perSourceWant(),
			})
		case model.SourceTag:
			reads = append(reads, r.planSeedPools(ctx, source, model.SourceTag, p, p.seedTags, "tag:", tr)...)
		case model.SourceCollab:
			reads = append(reads, r.planSeedPools(ctx, source, model.SourceCollab, p, p.seedAids, "aid:", tr)...)
		case model.SourceVector:
			reads = append(reads, r.vectorRead(ctx, p, tr))
		default:
			tr.drop(source, model.DegradeReasonFeatureUnavailable, "unsupported source")
		}
	}
	return reads
}

// planSeedPools 处理"按种子展开成多个池"的路（标签 tag:<id>、协同 aid:<seed>）。
// 没有种子时向特征下游要：标签路要兴趣标签、协同路要正向行为 aid。
func (r *recallRunner) planSeedPools(ctx context.Context, source, seedKind int32, p *recallPlan,
	seeds []int64, prefix string, tr *degradeTracker) []poolRead {
	if len(seeds) == 0 {
		seeds = r.fetchSeeds(ctx, seedKind, p, tr)
	}
	if len(seeds) == 0 {
		// 到这里没有任何可用种子：可能是"用户真的没行为"（冷启动，非故障），也可能是
		// 依赖没接线（fetchSeeds 已记 feature_unavailable）。两种都不能出空数组冒充结果。
		if !tr.hasReason(model.DegradeReasonFeatureUnavailable) {
			tr.drop(source, model.DegradeReasonColdStart, "no seeds for pool addressing")
		} else {
			tr.add(model.DegradeReasonFeatureUnavailable, "source=%d has no seeds", source)
			tr.dropSourceOnly(source)
		}
		return nil
	}
	// 一条路内多池要做公平分配：路内总额仍是 PerSourceMax，避免 20 个标签池各读 120 条
	// 把预算吃满（那会让后面的路全被 budget_exhausted 裁掉）。
	want := fairShare(r.perSourceWant(), len(seeds))
	reads := make([]poolRead, 0, len(seeds))
	for _, seed := range seeds {
		reads = append(reads, poolRead{
			source: source, poolKey: prefix + strconv.FormatInt(seed, 10), planned: want,
		})
	}
	return reads
}

// fetchSeeds 向特征下游要种子；失败只记降级原因、返回空列表。
func (r *recallRunner) fetchSeeds(ctx context.Context, seedKind int32, p *recallPlan, tr *degradeTracker) []int64 {
	switch seedKind {
	case model.SourceTag:
		tags, err := r.repo.Features.GetUserInterest(ctx, p.mid, r.opts.MaxSeedTags)
		if err != nil {
			tr.add(degradeKeyForDependencyErr(err), "GetUserInterest: %s", err.Error())
			return nil
		}
		out := make([]int64, 0, len(tags))
		for _, t := range tags {
			if t.TagID > 0 {
				out = append(out, t.TagID)
			}
			if len(out) >= r.opts.MaxSeedTags {
				break
			}
		}
		return out
	case model.SourceCollab:
		signals, err := r.repo.Features.GetBehaviorSignals(ctx, p.mid, r.seedWindow(), r.opts.MaxSeedAids)
		if err != nil {
			tr.add(degradeKeyForDependencyErr(err), "GetBehaviorSignals: %s", err.Error())
			return nil
		}
		if signals == nil {
			tr.add(model.DegradeReasonFeatureUnavailable, "GetBehaviorSignals returned nil signals")
			return nil
		}
		out := make([]int64, 0, len(signals.PositiveAids))
		for _, aid := range signals.PositiveAids {
			if aid > 0 {
				out = append(out, aid)
			}
			if len(out) >= r.opts.MaxSeedAids {
				break
			}
		}
		return out
	default:
		return nil
	}
}

// vectorRead 处理向量近邻路：候选由特征下游在线给出，没有池快照，
// 因此候选的 pool_version=0、batch_id 为空（与 rpc.Candidate 注释一致）。
func (r *recallRunner) vectorRead(ctx context.Context, p *recallPlan, tr *degradeTracker) poolRead {
	read := poolRead{source: model.SourceVector, poolKey: "", planned: r.perSourceWant(), live: true}
	rows, err := r.repo.Features.GetVectorCandidates(ctx, p.mid, read.planned)
	if err != nil {
		read.errKey = degradeKeyForDependencyErr(err)
		tr.drop(model.SourceVector, read.errKey, "GetVectorCandidates: %s", err.Error())
		return read
	}
	for _, row := range rows {
		if row.Aid <= 0 {
			continue
		}
		read.entries = append(read.entries, poolEntry{Aid: row.Aid, Score: row.Score})
		if len(read.entries) >= read.planned {
			break
		}
	}
	return read
}

// resolvePointers 用一次 Current.ListBySources 批量取回所有待读池的 CURRENT 版本。
//
// 禁止逐池 FindOne：一次请求最多 6 路 x 若干种子池，逐池 round trip 会把
// 80ms 预算全花在指针查询上。ListBySources 的池数上限是 model.MaxPoolRefsPerQuery。
// 返回 error 只用于"MySQL 读本身失败"（整次召回按 store_unavailable 降级），
// 单个池没有指针不是错误，落在 read.errKey 上。
func (r *recallRunner) resolvePointers(ctx context.Context, reads []poolRead) error {
	pools := make([]model.PoolKey, 0, len(reads))
	for _, read := range reads {
		if read.live || read.errKey != "" {
			continue
		}
		pools = append(pools, model.PoolKey{Source: read.source, PoolKey: read.poolKey})
	}
	if len(pools) == 0 {
		return nil
	}
	rows, err := r.repo.Current.ListBySources(ctx, pools)
	if err != nil {
		return fmt.Errorf("recall_pool_current ListBySources: %w", err)
	}
	pointers := make(map[string]*model.RecallPoolCurrent, len(rows))
	for _, row := range rows {
		pointers[pointerMapKey(row.Source, row.PoolKey)] = row
	}
	for i := range reads {
		read := &reads[i]
		if read.live || read.errKey != "" {
			continue
		}
		ptr, ok := pointers[pointerMapKey(read.source, read.poolKey)]
		if !ok || ptr.Version <= 0 {
			// 指针行不存在或 version=0：该池从未上线。这与"池上线了但没条目"是两件事，
			// 前者按 pool_not_ready 降级，后者是 TopByVersion 返回空条目。
			read.errKey = model.DegradeReasonPoolNotReady
			continue
		}
		read.version = ptr.Version
		read.batchID = ptr.BatchID
	}
	return nil
}

// fetchPoolEntries 逐池读 TopN（版本粒度的读透缓存 + 预算裁剪）。
//
// 预算判定在"开新读之前"而不是"读超时之后"：已发出的查询要么让它跑完，
// 要么让它被 ctx 打断，但不再开新路，这样 budget_exhausted 说的是"没来得及读的路"，
// 而不是"读了但不知道有没有读到"。
func (r *recallRunner) fetchPoolEntries(ctx context.Context, reads []poolRead, tr *degradeTracker) {
	for i := range reads {
		read := &reads[i]
		if read.errKey != "" || read.live {
			continue
		}
		if err := ctx.Err(); err != nil {
			read.errKey = model.DegradeReasonBudgetExhausted
			tr.drop(read.source, read.errKey, "budget exhausted before reading %s", poolKeyForLog(read.poolKey))
			continue
		}
		if read.version <= 0 {
			read.errKey = model.DegradeReasonPoolNotReady
			tr.drop(read.source, read.errKey, "pool %s has no current version", poolKeyForLog(read.poolKey))
			continue
		}
		entries, batchID, cacheUsable, err := r.readTopN(ctx, read)
		if !cacheUsable {
			// 缓存层不可用不回源打爆 MySQL 的"静默"版本：照样读 MySQL（否则整个服务不可用），
			// 但必须显式声明 store_unavailable，运维看到的是压力风险而不是"一切正常"。
			tr.add(model.DegradeReasonStoreUnavailable, "cache unavailable")
		}
		if err != nil {
			read.errKey = degradeKeyForDependencyErr(err)
			tr.drop(read.source, read.errKey, "TopByVersion: %s", err.Error())
			continue
		}
		read.entries = entries
		if batchID != "" {
			read.batchID = batchID
		}
	}
}

// readTopN 读一个池版本的 TopN，带 (source, pool_key, version, limit) 粒度的读透缓存。
//
// 缓存键里带 version 是本服务不需要任何"失效缓存"动作的原因：
// 已发布版本不可变 -> 键内容不会变错，切版本后读的是新键，旧键随 TTL 自然过期。
func (r *recallRunner) readTopN(ctx context.Context, read *poolRead) ([]poolEntry, string, bool, error) {
	key := topNCacheKey(read.source, read.poolKey, read.version, read.planned)
	cached, usable := cacheGetTopN(ctx, r.repo.Cache, key, read.planned)
	if len(cached) > 0 {
		return cached, read.batchID, usable, nil
	}
	// 缓存不可用（未配置或连接类错误）时照样回源：真值在 MySQL，
	// 不回源等于把整路候选丢掉；不可用的事实由调用方记成 store_unavailable。
	rows, err := r.repo.Pool.TopByVersion(ctx, read.source, read.poolKey, read.version, read.planned)
	if err != nil {
		return nil, read.batchID, usable, err
	}
	entries := make([]poolEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, poolEntry{Aid: row.Aid, Score: row.Score})
	}
	if !usable {
		return entries, read.batchID, false, nil
	}
	if werr := cacheSetTopN(ctx, r.repo.Cache, key, entries, r.rc.TTLSeconds); werr != nil {
		// 缓存写失败不影响本次响应（真值在 MySQL），但必须留下日志：
		// 持续写不进去意味着所有请求都在打 MySQL，这是容量风险而不是"缓存没命中"。
		r.logger.Errorf("recommend-recall: warm pool topn cache key=%s: %v", key, werr)
	}
	return entries, read.batchID, true, nil
}

// filterInvisible 用 VisibilitySource 把不可下发的稿件剔除。
//
// 返回 (visible 集合, errKey)：
//   - 依赖未接线/报错 -> errKey 非空并就地记入降级原因，此时**不剔除任何稿件**
//     （没有依据就断言它不可见，等于替下游做决定），由调用方按降级路径处理
//     （degraded + ttl=0，或不允许降级时直接报错）；
//   - 正常返回 -> 只保留下游明确判定可见的 aid。
//
// 未配置与读失败都走 errKey 而不是静默跳过：跳过等于把合规过滤整段抹掉。
func (r *recallRunner) filterInvisible(ctx context.Context, aids []int64, tr *degradeTracker) (map[int64]struct{}, string) {
	if len(aids) == 0 {
		return map[int64]struct{}{}, model.DegradeReasonNone
	}
	visible, err := r.repo.Visibility.FilterVisible(ctx, aids)
	if err != nil {
		key := degradeKeyForDependencyErr(err)
		tr.add(key, "FilterVisible: %s", err.Error())
		return nil, key
	}
	set := make(map[int64]struct{}, len(visible))
	for _, aid := range visible {
		if aid > 0 {
			set[aid] = struct{}{}
		}
	}
	return set, model.DegradeReasonNone
}

// perSourceWant 是一条召回路的取数上限（路内多池再按池数摊）。
// 取「配置的单路上限」与「本次请求总上限」的较小值：请求只要 10 条时不该按 120 条去捞池。
func (r *recallRunner) perSourceWant() int {
	if r.opts.PerSourceMax > r.limit {
		return r.limit
	}
	return r.opts.PerSourceMax
}

// seedWindow 是行为信号的时间窗口：与池新鲜度同源（PoolStaleSeconds），
// 超过这个窗口没看过的稿件不算"近期"。
func (r *recallRunner) seedWindow() time.Duration {
	return time.Duration(r.opts.PoolStaleSeconds) * time.Second
}

func (r *recallRunner) enabled(source int32) bool {
	for _, s := range r.rc.EnabledSources {
		if int32(s) == source {
			return true
		}
	}
	return false
}

func (t *degradeTracker) hasReason(key string) bool {
	for _, exist := range t.reasons {
		if exist == key {
			return true
		}
	}
	return false
}

// dropSourceOnly 只把路记进 dropped（原因已经记过，不重复写文本）。
func (t *degradeTracker) dropSourceOnly(source int32) {
	t.appendSources(&t.dropped, source)
}

// --- 合并与粗排 ---

// mergedCandidate 是去重合并后的一条候选（跨路只按 aid 去重）。
type mergedCandidate struct {
	aid      int64
	source   int32
	rank     int32
	score    float64
	version  int64
	batchID  string
	alsoFrom []int32
}

// mergeReads 按 aid 去重合并多路候选，保留 model.SourcePriority 最小的一路，
// 被合并掉的来源写进 also_from（可解释性）。
//
// 合并顺序决定 also_from 的稳定性，因此这里对路做了一次确定性排序：
// 先按 SourcePriority、再按 pool_key，同一个请求两次合并结果完全一致。
func mergeReads(reads []poolRead, visible map[int64]struct{}, exclude map[int64]struct{}) []mergedCandidate {
	ordered := make([]poolRead, 0, len(reads))
	for _, read := range reads {
		if read.errKey != "" || len(read.entries) == 0 {
			continue
		}
		ordered = append(ordered, read)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := model.SourcePriority(ordered[i].source), model.SourcePriority(ordered[j].source)
		if pi != pj {
			return pi < pj
		}
		return ordered[i].poolKey < ordered[j].poolKey
	})
	merged := make([]mergedCandidate, 0, len(ordered)*8)
	owner := make(map[int64]int, len(merged))
	for _, read := range ordered {
		for i, entry := range read.entries {
			if _, bad := exclude[entry.Aid]; bad {
				continue
			}
			if len(visible) > 0 {
				if _, ok := visible[entry.Aid]; !ok {
					continue
				}
			}
			rank := int32(i)
			if at, seen := owner[entry.Aid]; seen {
				// 同一 aid 已在更优先的路上：只合并来源标记，不覆盖分数与名次。
				if !sameSource(merged[at].alsoFrom, read.source) && merged[at].source != read.source {
					merged[at].alsoFrom = append(merged[at].alsoFrom, read.source)
				}
				continue
			}
			owner[entry.Aid] = len(merged)
			merged = append(merged, mergedCandidate{
				aid: entry.Aid, source: read.source, rank: rank, score: entry.Score,
				version: read.version, batchID: read.batchID,
			})
		}
	}
	return merged
}

// sortMerged 给出粗排顺序：先按路的 SourcePriority，再按该路内的 rank。
//
// 这里刻意不用 score 跨路排序：池内分数只在同路可比（README 的分数口径约定），
// 跨路比分数就是"发明一个排序模型"，属 recommend-rank 的职责。
func sortMerged(in []mergedCandidate) {
	sort.SliceStable(in, func(i, j int) bool {
		pi, pj := model.SourcePriority(in[i].source), model.SourcePriority(in[j].source)
		if pi != pj {
			return pi < pj
		}
		if in[i].rank != in[j].rank {
			return in[i].rank < in[j].rank
		}
		return in[i].aid < in[j].aid
	})
}

// truncateMerged 裁到 limit 条（limit<=0 表示不裁）。
func truncateMerged(in []mergedCandidate, limit int) []mergedCandidate {
	if limit > 0 && len(in) > limit {
		return in[:limit]
	}
	return in
}

// statOfSource 聚合某条召回路的取数统计。
//
// pool_version/batch_id 在多池时取"贡献条目最多的那个池"作为代表值（并列取版本号小的），
// 完整坐标在每条候选的 pool_version/batch_id 与审计行的 versions_digest 里，
// 契约的 SourceStat 是逐路的、无法表达逐池，这里选择不谎报：
// 一条都没出数且有降级原因时 pool_version=0 并给出 error_code。
func statOfSource(source int32, reads []poolRead, counts map[readKey]int32) *rpc.SourceStat {
	stat := &rpc.SourceStat{Source: toRPCSource(source)}
	type contrib struct {
		version int64
		batchID string
		n       int
	}
	var best *contrib
	var errs []string
	pools := 0
	for _, read := range reads {
		if read.source != source {
			continue
		}
		stat.Planned += int32(read.planned)
		if read.errKey != "" {
			errs = append(errs, read.errKey)
			continue
		}
		pools++
		n := int(counts[readKey{source: source, poolKey: read.poolKey}])
		stat.Returned += int32(n)
		if read.live {
			// 在线实时路没有池快照，版本恒为 0（契约里 0 的语义就是"无池快照"）。
			continue
		}
		cur := contrib{version: read.version, batchID: read.batchID, n: n}
		switch {
		case best == nil:
			b := cur
			best = &b
		case cur.n > best.n:
			b := cur
			best = &b
		case cur.n == best.n && cur.version < best.version:
			b := cur
			best = &b
		}
	}
	if best != nil {
		stat.PoolVersion = best.version
		stat.BatchId = best.batchID
	}
	if len(errs) > 0 {
		stat.Degraded = true
		stat.ErrorCode = errs[0]
		for _, key := range errs[1:] {
			if degradeSeverity[key] < degradeSeverity[stat.ErrorCode] {
				stat.ErrorCode = key
			}
		}
	} else if pools == 0 {
		stat.Degraded = true
		stat.ErrorCode = model.DegradeReasonPoolNotReady
	}
	return stat
}

// readKey 是 (source, pool_key) 的计数键（过滤后每条还剩多少）。
type readKey struct {
	source  int32
	poolKey string
}

// countByPool 统计过滤后每个池实际存活的条数（供 statOfSource 用）。
func countByPool(reads []poolRead, visible map[int64]struct{}, exclude map[int64]struct{}) map[readKey]int32 {
	counts := make(map[readKey]int32, len(reads))
	for _, read := range reads {
		if read.errKey != "" {
			continue
		}
		key := readKey{source: read.source, poolKey: read.poolKey}
		var n int32
		for _, entry := range read.entries {
			if _, bad := exclude[entry.Aid]; bad {
				continue
			}
			if len(visible) > 0 {
				if _, ok := visible[entry.Aid]; !ok {
					continue
				}
			}
			n++
		}
		counts[key] = n
	}
	return counts
}

// candidateList 把合并裁剪后的候选转成 rpc 结构（顺序即粗排结果）。
func candidateList(in []mergedCandidate) []*rpc.Candidate {
	out := make([]*rpc.Candidate, 0, len(in))
	for _, c := range in {
		out = append(out, &rpc.Candidate{
			Aid:          c.aid,
			Source:       toRPCSource(c.source),
			RankInSource: c.rank,
			Score:        c.score,
			PoolVersion:  c.version,
			BatchId:      c.batchID,
			AlsoFrom:     rpcSourceList(c.alsoFrom),
		})
	}
	return out
}

// --- 池键与文本归一 ---

// hotPoolKey 热门池寻址：本契约没有分区维度入参（RequestContext 无 zone 字段），
// 因此在线侧固定读 global 池；分区池（zone:<typeid>）由离线作业自己发布。
func hotPoolKey() string { return "global" }

// coldPoolKey 冷启动池寻址：平台明确走 platform:<p>，否则 global。
func coldPoolKey(platform int32) string {
	if model.ValidPlatform(platform) {
		return "platform:" + strconv.FormatInt(int64(platform), 10)
	}
	return "global"
}

// poolKeyForLog 把池键里可能含用户标识的值隐去（mid:<mid> -> mid:***）。
// 降级文本会进响应与日志，隐私红线不因"只是排障字段"而放宽。
func poolKeyForLog(key string) string {
	if strings.HasPrefix(key, "mid:") {
		return "mid:***"
	}
	return key
}

func pointerMapKey(source int32, poolKey string) string {
	return strconv.FormatInt(int64(source), 10) + "|" + poolKey
}

// sameSource 判定 also_from 里是否已有该路（避免同路多池重复标记）。
func sameSource(in []int32, source int32) bool {
	for _, s := range in {
		if s == source {
			return true
		}
	}
	return false
}

// fairShare 把路内额度摊到 N 个池（至少 1 条），保证 sum 不超过路额度。
func fairShare(perSource, pools int) int {
	if pools <= 1 {
		return perSource
	}
	share := (perSource + pools - 1) / pools
	if share < 1 {
		return 1
	}
	if share > perSource {
		return perSource
	}
	return share
}
