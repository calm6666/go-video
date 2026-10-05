package logic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// ============================================================================
// 期望值推导器（独立于被测实现）
// ============================================================================
//
// 本文件不把手抄的候选顺序写进断言：池内容先落成测试数据，
// 再由下面的 expect* 函数按 README 记录的规则独立算一遍期望：
//   去重 = 按 (SourcePriority, pool_key) 顺序首次拥有者胜出；
//   粗排 = 路优先级 -> 该路池内名次 -> aid；跨路不比分数。
// 这样"排序键被改坏"会让用例变红，而"顺序恰好和实现里抄的一样"不会。

// poolSeed 是一个池版本的测试数据（同时就是播种数据，避免期望与库用两份事实）。
type poolSeed struct {
	source  int32
	poolKey string
	version int64
	batch   string
	entries []item
}

// orderedEntries 按 recall_pool 的真实读取口径（score DESC, aid ASC）给出条目与名次。
func (p poolSeed) orderedEntries() []model.RecallPool {
	rows := make([]model.RecallPool, 0, len(p.entries))
	for _, it := range p.entries {
		rows = append(rows, model.RecallPool{Aid: it.aid, Score: it.score})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score != rows[j].Score {
			return rows[i].Score > rows[j].Score
		}
		return rows[i].Aid < rows[j].Aid
	})
	return rows
}

// seedTo 把同一份数据播种进内存库（指针 + 条目）。
func (s *store) seedTo(p poolSeed, publishedAt int64) {
	t := s.seedVersion(p.source, p.poolKey, p.version, p.batch, model.VersionStateCurrent,
		int64(len(p.entries)))
	t.PublishedAt = publishedAt
	s.seedPointer(p.source, p.poolKey, p.version, p.batch, publishedAt)
	s.seedItems(p.source, p.poolKey, p.version, p.entries...)
}

// wantCandidate 是期望回复里的一条候选。
type wantCandidate struct {
	aid      int64
	source   int32
	rank     int32
	version  int64
	batch    string
	alsoFrom []int32
}

// expectCandidates 独立推导去重合并 + 粗排 + 截断后的候选。
// visible==nil 表示"没有可用过滤依据（一条都不剔除）"。
func expectCandidates(pools []poolSeed, exclude, visible map[int64]struct{}, limit int) []wantCandidate {
	ordered := append([]poolSeed(nil), pools...)
	sort.SliceStable(ordered, func(i, j int) bool {
		pi, pj := model.SourcePriority(ordered[i].source), model.SourcePriority(ordered[j].source)
		if pi != pj {
			return pi < pj
		}
		return ordered[i].poolKey < ordered[j].poolKey
	})
	var merged []wantCandidate
	owner := make(map[int64]int, 16)
	for _, p := range ordered {
		for i, row := range p.orderedEntries() {
			if _, bad := exclude[row.Aid]; bad {
				continue
			}
			if visible != nil {
				if _, ok := visible[row.Aid]; !ok {
					continue
				}
			}
			if at, seen := owner[row.Aid]; seen {
				cur := merged[at]
				if cur.source != p.source && !containsInt32(cur.alsoFrom, p.source) {
					merged[at].alsoFrom = append(merged[at].alsoFrom, p.source)
				}
				continue
			}
			owner[row.Aid] = len(merged)
			merged = append(merged, wantCandidate{
				aid: row.Aid, source: p.source, rank: int32(i), version: p.version, batch: p.batch,
			})
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		pi, pj := model.SourcePriority(merged[i].source), model.SourcePriority(merged[j].source)
		if pi != pj {
			return pi < pj
		}
		if merged[i].rank != merged[j].rank {
			return merged[i].rank < merged[j].rank
		}
		return merged[i].aid < merged[j].aid
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

func containsInt32(in []int32, v int32) bool {
	for _, x := range in {
		if x == v {
			return true
		}
	}
	return false
}

// expectDigest 独立算 versions_digest：sha256(升序 "source:pool_key:version" 以 "|" 连接)。
// 入参是"本用例声明应当被读到的池"，不是从响应里回抄的。
func expectDigest(pools []poolSeed) string {
	parts := make([]string, 0, len(pools))
	for _, p := range pools {
		parts = append(parts, fmt.Sprintf("%d:%s:%d", p.source, p.poolKey, p.version))
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])
}

// assertCandidates 逐条比对候选（顺序、来源、名次、池版本、批次、also_from）。
func assertCandidates(t *testing.T, got []*rpc.Candidate, want []wantCandidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("候选条数=%d got=%s，期望 %d 条 want=%s", len(got), aidText(got), len(want), wantAidText(want))
	}
	for i := range want {
		c := got[i]
		if c.GetAid() != want[i].aid {
			t.Fatalf("第 %d 条 aid=%d，期望 %d（全序列 %s）", i, c.GetAid(), want[i].aid, aidText(got))
		}
		if int32(c.GetSource()) != want[i].source {
			t.Fatalf("候选 aid=%d 的来源=%v，期望 %d（去重时首次拥有者胜出）", want[i].aid, c.GetSource(), want[i].source)
		}
		if c.GetRankInSource() != want[i].rank {
			t.Fatalf("候选 aid=%d 的 rank_in_source=%d，期望 %d", want[i].aid, c.GetRankInSource(), want[i].rank)
		}
		if c.GetPoolVersion() != want[i].version {
			t.Fatalf("候选 aid=%d 的 pool_version=%d，期望 %d", want[i].aid, c.GetPoolVersion(), want[i].version)
		}
		if c.GetBatchId() != want[i].batch {
			t.Fatalf("候选 aid=%d 的 batch_id=%q，期望 %q", want[i].aid, c.GetBatchId(), want[i].batch)
		}
		if len(c.GetAlsoFrom()) != len(want[i].alsoFrom) {
			t.Fatalf("候选 aid=%d 的 also_from=%v，期望 %v", want[i].aid, c.GetAlsoFrom(), want[i].alsoFrom)
		}
		for j := range want[i].alsoFrom {
			if int32(c.GetAlsoFrom()[j]) != want[i].alsoFrom[j] {
				t.Fatalf("候选 aid=%d 的 also_from[%d]=%v，期望 %v", want[i].aid, j, c.GetAlsoFrom()[j], want[i].alsoFrom[j])
			}
		}
	}
}

func aidText(in []*rpc.Candidate) string {
	parts := make([]string, 0, len(in))
	for _, c := range in {
		parts = append(parts, fmt.Sprintf("%d@%d", c.GetAid(), c.GetSource()))
	}
	return strings.Join(parts, ",")
}

func wantAidText(in []wantCandidate) string {
	parts := make([]string, 0, len(in))
	for _, c := range in {
		parts = append(parts, fmt.Sprintf("%d@%d", c.aid, c.source))
	}
	return strings.Join(parts, ",")
}

func reqCtx(mid int64, platform rpc.Platform) *rpc.RequestContext {
	return &rpc.RequestContext{
		Mid:       mid,
		Platform:  platform,
		Scene:     "home.feed",
		RequestId: "req-fake-0001",
	}
}

// recallReq 构造一次"允许降级"的召回请求：契约里 allow_degrade 的 proto 默认值是 false，
// 不带它就等于要求"任何降级都报错"（该默认值本身由 TestRecallCandidatesAllowDegradeDefaultsToFalse 钉住）。
func recallReq(mid int64, platform rpc.Platform, sources ...rpc.Source) *rpc.RecallCandidatesReq {
	return &rpc.RecallCandidatesReq{
		Context:      reqCtx(mid, platform),
		Sources:      sources,
		AllowDegrade: true,
	}
}

func statFor(t *testing.T, reply *rpc.RecallCandidatesReply, source rpc.Source) *rpc.SourceStat {
	t.Helper()
	for _, s := range reply.GetPerSource() {
		if s.GetSource() == source {
			return s
		}
	}
	t.Fatalf("响应里没有路 %v 的统计，实际 %d 条", source, len(reply.GetPerSource()))
	return nil
}

func insertedLog(t *testing.T, e *env) *model.RecallRequestLog {
	t.Helper()
	if len(e.st.logs) != 1 {
		t.Fatalf("审计行数=%d，期望恰好 1 行", len(e.st.logs))
	}
	return e.st.logs[0]
}

// ============================================================================
// 1. 入参门禁：拒绝时一次依赖都不碰，且门禁之间有可判别的先后顺序
// ============================================================================

const defaultMaxExclude = 500 // 与 etc/recommendrecall.v1.yaml 的 MaxExcludeAids 对应

func aidRange(from int64, n int) []int64 {
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, from+int64(i))
	}
	return out
}

func TestRecallCandidatesGuardsRejectWithoutTouchingAnyDependency(t *testing.T) {
	hash := strings.Repeat("a", sha256HexLen)
	e := newEnv(t)
	// 边界字面量必须和装配出来的配置一致，否则用例测的是别的配置下的行为。
	if got := e.repo.Options(); got.MaxCandidates != 400 || got.MaxExcludeAids != defaultMaxExclude {
		t.Fatalf("门禁边界值与装配配置漂移: max_candidates=%d max_exclude=%d", got.MaxCandidates, got.MaxExcludeAids)
	}

	cases := []struct {
		name  string
		in    *rpc.RecallCandidatesReq
		want  error
		guard string
	}{
		{
			name:  "limit 为负先于其它一切校验",
			in:    &rpc.RecallCandidatesReq{Context: reqCtx(-5, rpc.Platform_PLATFORM_IOS), Limit: -1},
			want:  model.ErrInvalidLimit,
			guard: "mid<0 同时存在，必须报 limit 而不是 mid",
		},
		{
			name:  "limit 超总上限报 ErrLimitTooLarge 而不是静默裁剪",
			in:    &rpc.RecallCandidatesReq{Context: reqCtx(777, rpc.Platform_PLATFORM_IOS), Limit: 401},
			want:  model.ErrLimitTooLarge,
			guard: "MaxCandidates=400",
		},
		{
			name:  "mid 为负被当作审计必填缺失",
			in:    &rpc.RecallCandidatesReq{Context: reqCtx(-1, rpc.Platform_PLATFORM_IOS)},
			want:  model.ErrRequestLogRequired,
			guard: "游客是 mid=0，负值没有合法解释",
		},
		{
			name: "明文设备号被隐私闸门拒绝",
			in: &rpc.RecallCandidatesReq{Context: &rpc.RequestContext{
				Mid: 777, DeviceIdHash: "imei-1234567890", Scene: strings.Repeat("s", colScene+1),
			}},
			want:  model.ErrRawDeviceID,
			guard: "scene 同时超长，隐私闸门必须在前",
		},
		{
			name: "scene 超列宽拒绝",
			in: &rpc.RecallCandidatesReq{Context: &rpc.RequestContext{
				Mid: 777, DeviceIdHash: hash, Scene: strings.Repeat("s", colScene+1),
			}},
			want:  model.ErrFieldTooLong,
			guard: "列宽溢出必须拒绝而不是截断",
		},
		{
			name: "request_id 超列宽拒绝而不是截断",
			in: &rpc.RecallCandidatesReq{Context: &rpc.RequestContext{
				Mid: 777, RequestId: strings.Repeat("r", colRefID+1),
			}},
			want:  model.ErrFieldTooLong,
			guard: "request_id 是回放锚点，截断后调用方拿到的 ID 与库里那行对不上",
		},
		{
			name: "未定义的召回路编号直接拒绝",
			in: &rpc.RecallCandidatesReq{Context: reqCtx(777, rpc.Platform_PLATFORM_IOS),
				Sources: []rpc.Source{rpc.Source(99)}},
			want:  model.ErrInvalidSource,
			guard: "枚举外的值不当成 0 处理",
		},
		{
			name: "配置里没开的路拒绝（协同/向量未接线）",
			in: &rpc.RecallCandidatesReq{Context: reqCtx(777, rpc.Platform_PLATFORM_IOS),
				Sources: []rpc.Source{rpc.Source_SOURCE_COLLAB}},
			want:  model.ErrSourceDisabled,
			guard: "EnabledSources=[1,2,3,6]，开 4 只会稳定降级",
		},
		{
			name: "种子 aid 含非正数拒绝",
			in: &rpc.RecallCandidatesReq{Context: reqCtx(777, rpc.Platform_PLATFORM_IOS),
				SeedAids: []int64{10, 0}},
			want:  model.ErrInvalidAid,
			guard: "非正数种子不静默丢",
		},
		{
			name: "排除 aid 超上限拒绝而不是静默丢",
			in: &rpc.RecallCandidatesReq{Context: reqCtx(777, rpc.Platform_PLATFORM_IOS),
				ExcludeAids: aidRange(1, defaultMaxExclude+1)},
			want:  model.ErrTooManySeeds,
			guard: "静默丢掉一条等于把已看过的稿件漏放行",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.writes
			reply, err := NewRecallCandidatesLogic(e.ctx(), e.svcCtx).RecallCandidates(tc.in)
			if reply != nil {
				t.Fatalf("被拒绝的请求不得返回响应体，实际 %+v", reply)
			}
			requireErrIs(t, err, tc.want)
			if got := e.calls().total(); got != 0 {
				t.Fatalf("门禁拒绝时依赖调用数=%d（%v），期望 0（%s）", got, e.calls().ops(), tc.guard)
			}
			if e.st.writes != before {
				t.Fatalf("门禁拒绝却产生了 %d 次写入类调用", e.st.writes-before)
			}
		})
	}

	t.Run("空请求体", func(t *testing.T) {
		e := newEnv(t)
		_, err := NewRecallCandidatesLogic(e.ctx(), e.svcCtx).RecallCandidates(nil)
		requireErrIs(t, err, model.ErrRequestRequired)
		if got := e.calls().total(); got != 0 {
			t.Fatalf("依赖调用数=%d，期望 0", got)
		}
	})

	t.Run("limit 恰好等于上限放行（边界不属于拒绝侧）", func(t *testing.T) {
		e := newEnv(t)
		in := recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT)
		in.Limit = int32(e.repo.Options().MaxCandidates)
		if _, err := NewRecallCandidatesLogic(e.ctx(), e.svcCtx).RecallCandidates(in); err != nil {
			t.Fatalf("limit==MaxCandidates 必须放行，实际 %v", err)
		}
	})
}

// TestRecallCandidatesGuardOrderIsDiscriminable 用"同时踩两条门禁"的输入钉住判定顺序：
// 期望值来自逐条推演，不是"随便报一个错都行"。
func TestRecallCandidatesGuardOrderIsDiscriminable(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.RecallCandidatesReq
		want error
	}{
		{"limit 早于召回路编号", &rpc.RecallCandidatesReq{
			Context: reqCtx(777, rpc.Platform_PLATFORM_IOS), Limit: 99999,
			Sources: []rpc.Source{rpc.Source(99)}}, model.ErrLimitTooLarge},
		{"设备号闸门早于召回路编号", &rpc.RecallCandidatesReq{
			Context: &rpc.RequestContext{Mid: 777, DeviceIdHash: "plain-text-device-id"},
			Sources: []rpc.Source{rpc.Source_SOURCE_COLLAB}}, model.ErrRawDeviceID},
		{"召回路早于种子列表（路没开就不必看种子）", &rpc.RecallCandidatesReq{
			Context: reqCtx(777, rpc.Platform_PLATFORM_IOS), Sources: []rpc.Source{rpc.Source_SOURCE_VECTOR},
			SeedAids: []int64{-1}}, model.ErrSourceDisabled},
		{"种子校验晚于游客裁剪（游客裁完还剩路）", &rpc.RecallCandidatesReq{
			Context: reqCtx(0, rpc.Platform_PLATFORM_IOS),
			Sources: []rpc.Source{rpc.Source_SOURCE_COLD}, SeedTagIds: []int64{-2}}, model.ErrInvalidAid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, err := NewRecallCandidatesLogic(e.ctx(), e.svcCtx).RecallCandidates(tc.in)
			requireErrIs(t, err, tc.want)
			if got := e.calls().total(); got != 0 {
				t.Fatalf("依赖调用数=%d，期望 0", got)
			}
		})
	}
}

// TestRecallCandidatesAllowDegradeDefaultsToFalse 钉住一个容易踩的契约默认值：
// allow_degrade 不带（proto 默认 false）时，连"缓存缺位"这种非故障降级都会让整个请求失败。
func TestRecallCandidatesAllowDegradeDefaultsToFalse(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)
	in := &rpc.RecallCandidatesReq{
		Context: reqCtx(777, rpc.Platform_PLATFORM_IOS), Sources: []rpc.Source{rpc.Source_SOURCE_HOT},
	}
	_, err := NewRecallCandidatesLogic(e.ctx(), e.svcCtx).RecallCandidates(in)
	requireErrIs(t, err, model.ErrDegradationDisabled)
	if row := insertedLog(t, e); row.Degraded != 1 {
		t.Fatalf("报错请求的审计 degraded=%d，期望 1", row.Degraded)
	}
}

// ============================================================================
// 2. 游客裁剪：被裁的路不发任何依赖调用，且"裁了谁"进审计与响应
// ============================================================================

func TestRecallCandidatesGuestTrimDropsPersonalizedSourcesWithoutCallingFeatures(t *testing.T) {
	pools := []poolSeed{{source: srcCold32, poolKey: "platform:2", version: 3, batch: "b-cold-3",
		entries: []item{{aid: 401, score: 5}, {aid: 402, score: 4}}}}
	e := newEnv(t)
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(&rpc.RecallCandidatesReq{
		Context:      reqCtx(0, rpc.Platform_PLATFORM_IOS),
		Sources:      []rpc.Source{rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_TAG, rpc.Source_SOURCE_COLD},
		AllowDegrade: true,
	})
	if err != nil {
		t.Fatalf("游客召回必须出数（冷启动路），实际 %v", err)
	}

	// 只按冷启动池寻址一次：没有 mid 就没有 mid:<mid> 池，也没有标签池。
	assertOps(t, e.calls(), 0,
		"Current.ListBySources", "Pool.TopByVersion", "Visibility.FilterVisible", "RequestLog.Insert")
	got := e.st.lastListBySourcesPools
	want := []model.PoolKey{{Source: srcCold32, PoolKey: "platform:2"}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("指针批量读问了这些池 %v，期望 %v", got, want)
	}
	for _, p := range got {
		if err := model.ValidatePoolKey(p.Source, p.PoolKey); err != nil {
			t.Fatalf("寻址结果 %v 不是合法池键: %v", p, err)
		}
	}
	if e.calls().has("Features.GetUserInterest") {
		t.Fatal("游客请求却去要了兴趣标签：标签路应在裁剪阶段就被丢掉")
	}

	dropped := reply.GetDegradation().GetDroppedSources()
	if fmt.Sprint(dropped) != fmt.Sprint([]rpc.Source{rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_TAG}) {
		t.Fatalf("dropped_sources=%v，期望 [2 3]（游客拿不到的路必须显式声明）", dropped)
	}
	if r := reply.GetDegradation().GetReason(); r != rpc.DegradeReason_DEGRADE_REASON_STORE_UNAVAILABLE {
		t.Fatalf("主原因=%v，期望 STORE_UNAVAILABLE（缓存缺位的严重度高于 cold_start）", r)
	}
	assertCandidates(t, reply.GetCandidates(), expectCandidates(pools, nil, nil, 0))

	row := insertedLog(t, e)
	if row.RequestedSources != "2,3,6" {
		t.Fatalf("requested_sources=%q，期望保留调用方原始请求 2,3,6", row.RequestedSources)
	}
	if row.DroppedSources != "2,3" {
		t.Fatalf("dropped_sources=%q，期望 2,3（裁剪必须可回放）", row.DroppedSources)
	}
}

// ============================================================================
// 3. 候选集规模、去重与排序稳定性
// ============================================================================

func TestRecallCandidatesDedupAndCoarseOrderComeFromPriorityNotFromScoreOrInsertionOrder(t *testing.T) {
	// 故意乱序播种，且让"最高分稿件"落在优先级最低的热门上：
	// 若粗排被改成按分数跨路排序，101 会跳到第一位，本用例立即变红。
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11", entries: []item{
			{aid: 103, score: 8.0}, {aid: 101, score: 9.0}, {aid: 104, score: 8.0}, {aid: 102, score: 8.5}}},
		{source: srcFollow3, poolKey: "mid:777", version: 7, batch: "b-follow-7", entries: []item{
			{aid: 105, score: 1.4}, {aid: 102, score: 1.5}}},
		{source: srcCold32, poolKey: "platform:2", version: 3, batch: "b-cold-3", entries: []item{
			{aid: 106, score: 4.0}, {aid: 104, score: 5.0}}},
	}
	e := newEnv(t)
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	exclude := map[int64]struct{}{106: {}}

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	in := recallReq(777, rpc.Platform_PLATFORM_IOS)
	in.Limit = 10
	in.ExcludeAids = []int64{106}
	reply, err := l.RecallCandidates(in)
	if err != nil {
		t.Fatalf("三路都有 CURRENT，应正常出数: %v", err)
	}

	assertCandidates(t, reply.GetCandidates(), expectCandidates(pools, exclude, nil, 10))
	if got := len(reply.GetCandidates()); got != 5 {
		t.Fatalf("去重后候选数=%d，期望 5（4+2+2 条去重成 101..105，再排除 106）", got)
	}
	// 三条具体事实单独钉住（不只是"和推导器一致"）。
	if first := reply.GetCandidates()[0]; first.GetAid() != 102 || int32(first.GetSource()) != srcFollow3 {
		t.Fatalf("首位候选=%d@%v，期望 102@2（关注路名次 0 优先于热门路任何名次）",
			first.GetAid(), first.GetSource())
	}
	for _, c := range reply.GetCandidates() {
		if c.GetAid() == 101 && c.GetRankInSource() == 0 && reply.GetCandidates()[0].GetAid() == 101 {
			t.Fatal("全局分数最高的 101 排到了第一：粗排不得跨路比分数")
		}
		if c.GetAid() == 106 {
			t.Fatal("exclude_aids 里的 106 被召回了")
		}
	}

	// 逐路统计：Returned 是"这一路的池里活下来几条"，与被谁拥有无关。
	for _, w := range []struct {
		source            rpc.Source
		planned, returned int32
		version           int64
		batch             string
	}{
		{rpc.Source_SOURCE_HOT, 10, 4, 11, "b-hot-11"},
		{rpc.Source_SOURCE_FOLLOW, 10, 2, 7, "b-follow-7"},
		{rpc.Source_SOURCE_COLD, 10, 1, 3, "b-cold-3"},
	} {
		s := statFor(t, reply, w.source)
		if s.GetPlanned() != w.planned {
			t.Fatalf("路 %v planned=%d，期望 %d（per_source_want=min(PerSourceMax, limit)）",
				w.source, s.GetPlanned(), w.planned)
		}
		if s.GetReturned() != w.returned {
			t.Fatalf("路 %v returned=%d，期望 %d", w.source, s.GetReturned(), w.returned)
		}
		if s.GetPoolVersion() != w.version || s.GetBatchId() != w.batch {
			t.Fatalf("路 %v 的池坐标=%d/%q，期望 %d/%q", w.source, s.GetPoolVersion(), s.GetBatchId(), w.version, w.batch)
		}
		if s.GetDegraded() {
			t.Fatalf("路 %v 被标成 degraded（error_code=%q），但这三路都正常出了数", w.source, s.GetErrorCode())
		}
	}

	// 全序列：一次指针批量读、三次池读（顺序 = 请求路顺序）、一次可见性判定、一次审计写入。
	assertOps(t, e.calls(), 0,
		"Current.ListBySources",
		"Pool.TopByVersion", "Pool.TopByVersion", "Pool.TopByVersion",
		"Visibility.FilterVisible", "RequestLog.Insert")
	if n := len(e.st.lastListBySourcesPools); n != 3 {
		t.Fatalf("指针批量读一次问了 %d 个池，期望 3（禁止逐池 FindOne）", n)
	}
	if fmt.Sprint(sortedAids(e.vis.lastAids)) != fmt.Sprint([]int64{101, 102, 103, 104, 105}) {
		t.Fatalf("送去可见性判定的 aid=%v，期望 [101 102 103 104 105]（排除项与重复项不进 IN 列表）", e.vis.lastAids)
	}
	// AGENTS.md §5：在线读路径只允许写自己那行审计，不得改任何计数或他域主数据。
	if delta := e.st.writes; delta != 1 {
		t.Fatalf("写入类调用共 %d 次，期望恰好 1（只有 recall_request_log 一行审计）", delta)
	}

	row := insertedLog(t, e)
	if row.CandidateCount != 5 || row.ReturnedCount != 5 {
		t.Fatalf("审计 candidate/returned=%d/%d，期望 5/5", row.CandidateCount, row.ReturnedCount)
	}
	if row.VersionsDigest != expectDigest(pools) {
		t.Fatalf("versions_digest=%s，与按实际读取集合独立算出的 %s 不一致", row.VersionsDigest, expectDigest(pools))
	}
	if row.Degraded != 1 || row.DegradeReason != model.DegradeReasonStoreUnavailable {
		t.Fatalf("降级标记=%d/%q，期望 1/store_unavailable（Cache 为 nil 是必然降级，见 README 覆盖边界）",
			row.Degraded, row.DegradeReason)
	}
	if reply.GetTtlSeconds() != 0 {
		t.Fatalf("降级结果的 ttl_seconds=%d，必须恒为 0", reply.GetTtlSeconds())
	}
}

func sortedAids(in []int64) []int64 {
	out := append([]int64(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func TestRecallCandidatesLimitTruncatesAfterMergingAndCountsBeforeTruncating(t *testing.T) {
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11", entries: []item{
			{aid: 101, score: 9}, {aid: 102, score: 8}}},
		{source: srcFollow3, poolKey: "mid:777", version: 7, batch: "b-follow-7", entries: []item{
			{aid: 103, score: 1.5}, {aid: 104, score: 1.4}}},
	}
	e := newEnv(t)
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	in := recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_HOT)
	in.Limit = 2
	reply, err := l.RecallCandidates(in)
	if err != nil {
		t.Fatalf("出数失败: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(), expectCandidates(pools, nil, nil, 2))
	row := insertedLog(t, e)
	if row.CandidateCount != 4 {
		t.Fatalf("candidate_count=%d，期望 4：它是合并后的总候选数，与 limit 裁剪无关", row.CandidateCount)
	}
	if row.ReturnedCount != 2 {
		t.Fatalf("returned_count=%d，期望 2（实发条数）", row.ReturnedCount)
	}
	// 被裁掉的名次仍算"这一路真实出的候选"：served_sources 含两路，顺序按合并顺序。
	if fmt.Sprint(reply.GetDegradation().GetServedSources()) !=
		fmt.Sprint([]rpc.Source{rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_HOT}) {
		t.Fatalf("served_sources=%v，期望两路都算出过数", reply.GetDegradation().GetServedSources())
	}
}

// ============================================================================
// 4. 过滤器到底生不生效
// ============================================================================

func TestRecallCandidatesVisibilityFilterRemovesWhatDownstreamRefuses(t *testing.T) {
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11", entries: []item{
			{aid: 101, score: 9}, {aid: 102, score: 8}, {aid: 103, score: 7}}},
	}
	e := newEnv(t)
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	e.vis.visible = []int64{101, 103} // 下游只放行两条

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
	if err != nil {
		t.Fatalf("出数失败: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(),
		expectCandidates(pools, nil, map[int64]struct{}{101: {}, 103: {}}, 0))
	if len(reply.GetCandidates()) != 2 {
		t.Fatalf("候选数=%d，期望 2（aid=102 必须被下游拒掉）", len(reply.GetCandidates()))
	}
	if s := statFor(t, reply, rpc.Source_SOURCE_HOT); s.GetReturned() != 2 {
		t.Fatalf("路 HOT returned=%d，期望 2（过滤后计数才算出数）", s.GetReturned())
	}
}

// TestRecallCandidatesWithProductionStubVisibilityServesEverything 是对"未过审/已删除稿件
// 会不会被召回"的正面回答：会。
//
// 生产口径的 Repository 装的是 repository.UnconfiguredVisibilitySource（video 侧投影未接线），
// FilterVisible 稳定报 ErrSourceNotConfigured；logic 的取舍是"没有依据就不替下游做决定"，
// 于是一条都不剔除、只把这次结果标成 feature_unavailable + ttl=0。
// 本用例按现状钉住，不改生产代码；缺口登记在 README「已知缺口」。
func TestRecallCandidatesWithProductionStubVisibilityServesEverything(t *testing.T) {
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11", entries: []item{
			{aid: 101, score: 9}, {aid: 999, score: 8}}}, // 999 = 假想的"已下架稿件"
	}
	e := newEnvWith(t, repository.UnconfiguredFeatureSource{}, repository.UnconfiguredVisibilitySource{})
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
	if err != nil {
		t.Fatalf("现状是照出候选（而不是报错）: %v", err)
	}
	var served []int64
	for _, c := range reply.GetCandidates() {
		served = append(served, c.GetAid())
	}
	if fmt.Sprint(served) != fmt.Sprint([]int64{101, 999}) {
		t.Fatalf("候选 aid=%v，现状必须包含 999（合规过滤在依赖未接线时一条都不剔）", served)
	}
	d := reply.GetDegradation()
	if !d.GetDegraded() {
		t.Fatal("合规过滤没做成却不降级：响应会把不可信结果读成正常")
	}
	if r := d.GetReason(); r != rpc.DegradeReason_DEGRADE_REASON_STORE_UNAVAILABLE {
		t.Fatalf("主原因=%v，期望 STORE_UNAVAILABLE（严重度高于 feature_unavailable）", r)
	}
	if !strings.Contains(d.GetDetail(), model.DegradeReasonFeatureUnavailable) {
		t.Fatalf("detail=%q 未包含 feature_unavailable，缺依赖的事实会被丢掉", d.GetDetail())
	}
	if reply.GetTtlSeconds() != 0 {
		t.Fatalf("ttl=%d，不可信结果不许被网关缓存", reply.GetTtlSeconds())
	}
}

// TestRecallCandidatesBlacklistFilterIsNeverInvoked 钉住"黑名单/关注关系过滤器不存在"：
// 契约里有 VisibilitySource.BlockedUps 与 FollowingUps，整条召回链路一次都没调用它们。
func TestRecallCandidatesBlacklistFilterIsNeverInvoked(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	if _, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT)); err != nil {
		t.Fatalf("出数失败: %v", err)
	}
	for _, op := range []string{"Visibility.BlockedUps", "Visibility.FollowingUps"} {
		if e.calls().has(op) {
			t.Fatalf("%s 竟被调用了：本用例钉的是该过滤器在召回链路里从未被调用", op)
		}
	}
	if !e.calls().has("Visibility.FilterVisible") {
		t.Fatal("唯一的可见性入口 FilterVisible 没被调用，说明整段合规过滤被跳过了")
	}
}

// TestRecallCandidatesComplianceFilterToEmptyKeepsSourceStatsLookingNormal 钉住归因缺陷：
// 下游明确回答"一条都不许下发"时，候选为空、路被记进 dropped_sources，
// 但每路 SourceStat 既不 degraded 也没有 error_code —— 与"池上线了但没条目"无法区分。
func TestRecallCandidatesComplianceFilterToEmptyKeepsSourceStatsLookingNormal(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}, {aid: 102, score: 8}}}, 1)
	e.vis.visible = []int64{} // 非 nil 空集：读得到，但一条都不允许下发

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
	if err != nil {
		t.Fatalf("allow_degrade=true 时按现状返回空候选: %v", err)
	}
	if len(reply.GetCandidates()) != 0 {
		t.Fatalf("候选=%s，期望空", aidText(reply.GetCandidates()))
	}
	s := statFor(t, reply, rpc.Source_SOURCE_HOT)
	if s.GetDegraded() || s.GetErrorCode() != "" {
		t.Fatalf("SourceStat 现状=%v/%q，按代码它必须看起来正常（本用例钉的是归因缺陷）",
			s.GetDegraded(), s.GetErrorCode())
	}
	if s.GetReturned() != 0 || s.GetPoolVersion() != 11 {
		t.Fatalf("SourceStat returned=%d version=%d，期望 0/11：合规剔空后仍报出池版本，如同正常出数",
			s.GetReturned(), s.GetPoolVersion())
	}
	if r := reply.GetDegradation().GetReason(); r != rpc.DegradeReason_DEGRADE_REASON_ALL_SOURCES_EMPTY {
		t.Fatalf("主原因=%v，期望 ALL_SOURCES_EMPTY", r)
	}
	if fmt.Sprint(reply.GetDegradation().GetDroppedSources()) != fmt.Sprint([]rpc.Source{rpc.Source_SOURCE_HOT}) {
		t.Fatalf("dropped_sources=%v，期望 [1]（dropSourceOnly 只记路不记原因）",
			reply.GetDegradation().GetDroppedSources())
	}
	// 空候选仍是一次完整判定：可见性下游被调用过。
	if !e.calls().has("Visibility.FilterVisible") {
		t.Fatal("合规过滤到空却没问过可见性下游，说明空集来自别处")
	}
}

// ============================================================================
// 5. 超时与降级：一路失败 / 全路失败分别返回什么
// ============================================================================

func TestRecallCandidatesPoolReadFailureKeepsRawErrorAndMapsToDownstreamTimeout(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)
	e.st.seedTo(poolSeed{source: srcFollow3, poolKey: "mid:777", version: 7, batch: "b-follow-7",
		entries: []item{{aid: 105, score: 1.5}}}, 1)
	// 只让热门池所在的读失败：真实故障总是先打在某个池上。
	e.st.errTopByKey = map[readKey]error{{source: srcHot32, poolKey: "global"}: sql.ErrConnDone}

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS,
		rpc.Source_SOURCE_HOT, rpc.Source_SOURCE_FOLLOW))
	if err != nil {
		t.Fatalf("allow_degrade=true 时故障路被丢掉而不是整请求失败: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(), []wantCandidate{
		{aid: 105, source: srcFollow3, rank: 0, version: 7, batch: "b-follow-7"}})

	if s := statFor(t, reply, rpc.Source_SOURCE_HOT); s.GetErrorCode() != model.DegradeReasonDownstreamTimeout {
		t.Fatalf("失败路 error_code=%q，期望 downstream_timeout", s.GetErrorCode())
	} else if !s.GetDegraded() {
		t.Fatal("失败路没被标 degraded")
	}
	if s := statFor(t, reply, rpc.Source_SOURCE_FOLLOW); s.GetDegraded() {
		t.Fatalf("健康路被标成 degraded（error_code=%q）", s.GetErrorCode())
	}
	d := reply.GetDegradation()
	if !strings.Contains(d.GetDetail(), sql.ErrConnDone.Error()) {
		t.Fatalf("detail=%q 未原样带上游错误文本 %q：错误被换成了自造哨兵", d.GetDetail(), sql.ErrConnDone.Error())
	}
	if fmt.Sprint(d.GetDroppedSources()) != fmt.Sprint([]rpc.Source{rpc.Source_SOURCE_HOT}) {
		t.Fatalf("dropped_sources=%v，期望只丢掉失败的那一路", d.GetDroppedSources())
	}
	if reply.GetTtlSeconds() != 0 {
		t.Fatalf("ttl=%d，故障结果必须 0", reply.GetTtlSeconds())
	}
}

func TestRecallCandidatesPointerReadFailureSkipsAllPoolReadsAndReportsStoreUnavailable(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcFollow3, poolKey: "mid:777", version: 7, batch: "b-follow-7",
		entries: []item{{aid: 105, score: 1.5}}}, 1)
	e.st.errPointerList = errors.New("dial tcp 127.0.0.1:3306: connect refused")

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_FOLLOW))
	if err != nil {
		t.Fatalf("允许降级时返回声明过的空候选而不是错误: %v", err)
	}
	// 指针批量读失败 = 无法判定任何池的生效版本：一条池读都不该发出。
	// 两轮 ListBySources 是主路 + 兜底路各一次（兜底路同样卡在指针上）。
	assertOps(t, e.calls(), 0,
		"Current.ListBySources", "Current.ListBySources", "RequestLog.Insert")
	if r := reply.GetDegradation().GetReason(); r != rpc.DegradeReason_DEGRADE_REASON_ALL_SOURCES_EMPTY {
		t.Fatalf("主原因=%v，期望 ALL_SOURCES_EMPTY（空候选必须是显式主原因）", r)
	}
	if !strings.Contains(reply.GetDegradation().GetDetail(), "connect refused") {
		t.Fatalf("detail=%q 未带上原始错误", reply.GetDegradation().GetDetail())
	}
	for _, src := range []rpc.Source{rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_COLD, rpc.Source_SOURCE_HOT} {
		if s := statFor(t, reply, src); s.GetErrorCode() != model.DegradeReasonStoreUnavailable {
			t.Fatalf("路 %v error_code=%q，期望 store_unavailable（不能把读不到库报成池没上线）",
				src, s.GetErrorCode())
		}
	}
}

func TestRecallCandidatesMissingPointerIsPoolNotReadyAndFallsBackToUnreadSources(t *testing.T) {
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11", entries: []item{{aid: 101, score: 9}}},
	}
	e := newEnv(t)
	for _, p := range pools {
		e.st.seedTo(p, 1)
	}
	// 关注池与冷启动池从未上线（没有指针行）；请求只要关注路 -> 主路全空 -> 兜底 cold_start -> fallback。
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_FOLLOW))
	if err != nil {
		t.Fatalf("兜底路应当出数: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(), expectCandidates(pools, nil, nil, 0))

	// 统计顺序是"请求路 -> 兜底路"，回放比对才稳定。
	per := reply.GetPerSource()
	if len(per) != 3 || per[0].GetSource() != rpc.Source_SOURCE_FOLLOW ||
		per[1].GetSource() != rpc.Source_SOURCE_COLD || per[2].GetSource() != rpc.Source_SOURCE_HOT {
		t.Fatalf("per_source 顺序=%d 条，期望 [2 6 1]（请求路在前，兜底路按 cold_start -> fallback 追加）", len(per))
	}
	for i, src := range []rpc.Source{rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_COLD} {
		if code := per[i].GetErrorCode(); code != model.DegradeReasonPoolNotReady {
			t.Fatalf("路 %v error_code=%q，期望 pool_not_ready", src, code)
		}
	}
	if per[2].GetReturned() != 1 {
		t.Fatalf("兜底热门路 returned=%d，期望 1", per[2].GetReturned())
	}
	// 兜底只读没读过的路：池读发生一次（关注/冷启动根本没指针，不发起池读）。
	if n := e.calls().countOf("Pool.TopByVersion"); n != 1 {
		t.Fatalf("池读发生 %d 次，期望 1（同一个池版本再读一遍不会变出候选）", n)
	}
	if !strings.Contains(reply.GetDegradation().GetDetail(), model.DegradeReasonColdStart) {
		t.Fatalf("detail=%q 未声明走了兜底路", reply.GetDegradation().GetDetail())
	}
	// 归因缺陷按现状钉住：池没有指针导致整路不可用，却进不了 dropped_sources
	// （resolvePointers 只写 read.errKey，不调用 tr.drop）。
	if n := len(reply.GetDegradation().GetDroppedSources()); n != 0 {
		t.Fatalf("dropped_sources 有 %d 项，现状应为 0（本用例钉的是缺失归因）", n)
	}
}

func TestRecallCandidatesCancelledBudgetDropsAllSourcesButStillAudits(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 预算 ctx 由已取消的父 ctx 派生 -> 每条路在"开新读之前"就被裁掉
	l := NewRecallCandidatesLogic(ctx, e.svcCtx)
	reply, err := l.RecallCandidates(&rpc.RecallCandidatesReq{
		Context:      reqCtx(777, rpc.Platform_PLATFORM_IOS),
		Sources:      []rpc.Source{rpc.Source_SOURCE_HOT, rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_COLD},
		AllowDegrade: true,
	})
	if err != nil {
		t.Fatalf("允许降级的请求即使超预算也返回声明过的空结果: %v", err)
	}
	if len(reply.GetCandidates()) != 0 {
		t.Fatalf("候选=%s，期望空", aidText(reply.GetCandidates()))
	}
	// 一次存储读都没有（裁剪发生在开新读之前），但审计动作照样被尝试（审计不吃预算 ctx）。
	assertOps(t, e.calls(), 0, "RequestLog.Insert")
	if fmt.Sprint(reply.GetDegradation().GetDroppedSources()) !=
		fmt.Sprint([]rpc.Source{rpc.Source_SOURCE_HOT, rpc.Source_SOURCE_FOLLOW, rpc.Source_SOURCE_COLD}) {
		t.Fatalf("dropped_sources=%v，期望三路全被裁掉", reply.GetDegradation().GetDroppedSources())
	}
	// 归因缺陷按现状钉住：请求级说 budget_exhausted，逐路却因为没有池读而报 pool_not_ready。
	if s := statFor(t, reply, rpc.Source_SOURCE_HOT); s.GetErrorCode() != model.DegradeReasonPoolNotReady {
		t.Fatalf("路 HOT error_code=%q，现状是 pool_not_ready（被裁掉的路没有专属归因）", s.GetErrorCode())
	}
	if !strings.Contains(reply.GetDegradation().GetDetail(), model.DegradeReasonBudgetExhausted) {
		t.Fatalf("detail=%q 未声明 budget_exhausted", reply.GetDegradation().GetDetail())
	}
}

// ============================================================================
// 6. 不允许降级：报错但现场照记
// ============================================================================

func TestRecallCandidatesDegradeDisabledReturnsRealFailureAfterAuditing(t *testing.T) {
	pools := []poolSeed{
		{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
			entries: []item{{aid: 101, score: 9}, {aid: 102, score: 8}}},
	}
	t.Run("部分出数但带降级 -> ErrDegradationDisabled 且不回候选", func(t *testing.T) {
		e := newEnv(t)
		for _, p := range pools {
			e.st.seedTo(p, 1)
		}
		// 关注路有指针但池里没条目 -> 该路 returned=0，整次请求仍是降级状态。
		e.st.seedPointer(srcFollow3, "mid:777", 7, "b-follow-7", 1)
		l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
		reply, err := l.RecallCandidates(&rpc.RecallCandidatesReq{
			Context: reqCtx(777, rpc.Platform_PLATFORM_IOS),
			Sources: []rpc.Source{rpc.Source_SOURCE_HOT, rpc.Source_SOURCE_FOLLOW},
		})
		requireErrIs(t, err, model.ErrDegradationDisabled)
		if reply != nil {
			t.Fatalf("不允许降级却回了响应体（%d 条候选），等于把现场改写一遍", len(reply.GetCandidates()))
		}
		if !strings.Contains(err.Error(), model.DegradeReasonStoreUnavailable) {
			t.Fatalf("错误文本 %q 未带主原因，调用方无法定位落点", err.Error())
		}
		row := insertedLog(t, e)
		if row.Degraded != 1 {
			t.Fatalf("报错请求的审计 degraded=%d，期望 1（压测/回放要看得到现场）", row.Degraded)
		}
		// 报错前已完成取数 + 审计：审计写不早于判定，也不被错误跳过。
		assertOps(t, e.calls(), 0,
			"Current.ListBySources", "Pool.TopByVersion", "Pool.TopByVersion",
			"Visibility.FilterVisible", "RequestLog.Insert")
	})

	t.Run("全空 -> ErrAllSourcesEmpty 而不是通用降级哨兵", func(t *testing.T) {
		e := newEnv(t)
		e.st.seedPointer(srcHot32, "global", 11, "b-hot-11", 1) // 池上线了但没条目
		l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
		_, err := l.RecallCandidates(&rpc.RecallCandidatesReq{
			Context: reqCtx(777, rpc.Platform_PLATFORM_IOS), Sources: []rpc.Source{rpc.Source_SOURCE_HOT},
		})
		requireErrIs(t, err, model.ErrAllSourcesEmpty)
		if !strings.Contains(err.Error(), model.DegradeReasonAllSourcesEmpty) {
			t.Fatalf("错误文本 %q 未带 all_sources_empty", err.Error())
		}
	})

	t.Run("请求允许但全局开关关闭同样按不允许处理", func(t *testing.T) {
		e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.DegradeEnabled = false })
		for _, p := range pools {
			e.st.seedTo(p, 1)
		}
		l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
		_, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
		requireErrIs(t, err, model.ErrDegradationDisabled)
	})
}

// ============================================================================
// 7. 审计行：隐私、可回放，以及写失败只记日志
// ============================================================================

func TestRecallCandidatesAuditRowIsReplayableAndCarriesNoDeviceIdentity(t *testing.T) {
	const mid int64 = 987654321
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcFollow3, poolKey: "mid:987654321", version: 7, batch: "b-follow-7",
		entries: []item{{aid: 201, score: 2}}}, 1)
	rawDevice := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	longTrace := strings.Repeat("t", colRefID+40)

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	in := recallReq(mid, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_FOLLOW)
	in.Context.DeviceIdHash = rawDevice
	in.Context.AppVersion = "9.9.9"
	in.Context.Region = "CN"
	in.Context.RequestId = "req-fake-audit-1"
	in.Context.TraceId = longTrace
	reply, err := l.RecallCandidates(in)
	if err != nil {
		t.Fatalf("出数失败: %v", err)
	}
	if reply.GetRequestId() != "req-fake-audit-1" {
		t.Fatalf("request_id=%q，必须回显调用方给的锚点", reply.GetRequestId())
	}
	row := insertedLog(t, e)
	if row.RequestID != "req-fake-audit-1" {
		t.Fatalf("审计 request_id=%q 与回显不一致", row.RequestID)
	}
	if !strings.HasPrefix(row.SnapshotID, snapshotIDPrefix+"_") {
		t.Fatalf("snapshot_id=%q 少了服务端生成前缀", row.SnapshotID)
	}
	if row.Mid != mid || row.Scene != "home.feed" || row.AppVersion != "9.9.9" || row.Region != "CN" {
		t.Fatalf("审计维度字段=%+v", row)
	}
	if row.Platform != model.PlatformIOS {
		t.Fatalf("platform=%d，期望 2（iOS）", row.Platform)
	}
	if len(row.TraceID) != colRefID {
		t.Fatalf("trace_id 长度=%d，期望被裁到列宽 %d（其它字段是拒绝、它是截断）", len(row.TraceID), colRefID)
	}
	if row.PerSource == "" {
		t.Fatal("per_source 为空：逐路统计是排障唯一依据")
	}
	// 隐私红线：设备摘要不进任何一列，也不出现在序列化出来的整行文本里。
	if strings.Contains(fmt.Sprintf("%+v", row), rawDevice) {
		t.Fatal("审计行里带着设备号摘要原文")
	}
	// mid 是审计必填维度（可以出现），但响应文本不得回显 mid 池键原文。
	if strings.Contains(fmt.Sprintf("%+v", reply), "mid:987654321") {
		t.Fatal("响应里回显了 mid 池键原文，降级文本必须遮蔽")
	}
	if got := poolKeyForLog("mid:987654321"); got != "mid:***" {
		t.Fatalf("poolKeyForLog=%q，期望 mid:***", got)
	}
}

func TestRecallCandidatesAuditWriteFailureIsLoggedNotPropagated(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)
	e.st.errLogInsert = errors.New("audit insert exploded")

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
	if err != nil {
		t.Fatalf("审计写失败不得影响候选: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(), []wantCandidate{
		{aid: 101, source: srcHot32, rank: 0, version: 11, batch: "b-hot-11"}})
	if strings.Contains(fmt.Sprintf("%+v", reply), "exploded") {
		t.Fatal("审计库错误文本泄进了响应")
	}
	if n := e.calls().countOf("RequestLog.Insert"); n != 1 {
		t.Fatalf("审计写入尝试了 %d 次，现状应只试一次（写失败不重试）", n)
	}
}

func TestRecallCandidatesDuplicateAuditKeyStillServesFreshCandidates(t *testing.T) {
	e := newEnv(t)
	e.st.seedTo(poolSeed{source: srcHot32, poolKey: "global", version: 11, batch: "b-hot-11",
		entries: []item{{aid: 101, score: 9}}}, 1)
	e.st.seedRequestLog(&model.RecallRequestLog{
		RequestID: "req-fake-dup", SnapshotID: "snap-old", Mid: 777, Ctime: 1600000000,
	})

	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	in := recallReq(777, rpc.Platform_PLATFORM_UNSPECIFIED, rpc.Source_SOURCE_HOT)
	in.Context.RequestId = "req-fake-dup"
	reply, err := l.RecallCandidates(in)
	if err != nil {
		t.Fatalf("同一 request_id 重放按现状回新算的候选: %v", err)
	}
	assertCandidates(t, reply.GetCandidates(), []wantCandidate{
		{aid: 101, source: srcHot32, rank: 0, version: 11, batch: "b-hot-11"}})
	if len(e.st.logs) != 1 {
		t.Fatalf("库里审计行数=%d，期望旧行未被覆盖（唯一键冲突只记日志）", len(e.st.logs))
	}
	if e.st.logs[0].SnapshotID != "snap-old" {
		t.Fatal("旧审计行被改写：本服务承诺只存摘要、不覆盖历史现场")
	}
}

// ============================================================================
// 8. 装配缺陷不是可降级状态
// ============================================================================

func TestRecallCandidatesWithoutRepositoryFailsLoudly(t *testing.T) {
	e := newEnv(t)
	e.svcCtx.Repository = nil
	l := NewRecallCandidatesLogic(e.ctx(), e.svcCtx)
	reply, err := l.RecallCandidates(recallReq(777, rpc.Platform_PLATFORM_IOS, rpc.Source_SOURCE_HOT))
	requireErrIs(t, err, repository.ErrSourceNotConfigured)
	if reply != nil {
		t.Fatal("缺必需依赖却回了响应体")
	}
	if !strings.Contains(err.Error(), "repository is not assembled") {
		t.Fatalf("错误文本 %q 没说明是装配缺陷", err.Error())
	}
	if got := e.calls().total(); got != 0 {
		t.Fatalf("依赖调用数=%d，期望 0", got)
	}
}
