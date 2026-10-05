// 本文件覆盖 GetRecallConfig（在线参数面下发）。
//
// 它断言的核心不是"返回了一堆数字"，而是三条本契约的红线：
//  1. 数字必须与 Repository.Options()/Config.Recall 同源（用例改配置，响应必须跟着改；
//     任何写死在 logic 里的字面量都会在这里变红）；
//  2. 游客裁剪必须与 RecallCandidates 用同一份判定（下发全集=诱导客户端发必然降级的请求）；
//  3. ready_pools 只描述"指针指向且版本行确实存在"的池：指针在而行没了必须显式失败，
//     一个上线池都没有必须回空列表而不是错误。
package logic

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go-video/services/recommend-recall/internal/config"
	"go-video/services/recommend-recall/internal/repository"
	"go-video/services/recommend-recall/internal/svc"
	"go-video/services/recommend-recall/model"
	"go-video/services/recommend-recall/rpc"
)

// fmtSources 把 repeated Source 渲染成可比对的字符串（不断言切片内部表示）。
func fmtSources(in []rpc.Source) string {
	names := make([]string, 0, len(in))
	for _, s := range in {
		names = append(names, s.String())
	}
	return strings.Join(names, ",")
}

// 参数门禁必须一个存储调用都不发起：这是"拒绝在进事务之前"的口径。
func TestGetRecallConfigRejectsBeforeTouchingStorage(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.GetRecallConfigReq
		want error
	}{
		{"nil 请求", nil, model.ErrRequestRequired},
		{"scene 超 VARCHAR(64)", &rpc.GetRecallConfigReq{Scene: strings.Repeat("s", colScene+1), Mid: 1},
			model.ErrFieldTooLong},
		{"mid 负值（0 才是游客）", &rpc.GetRecallConfigReq{Mid: -1}, model.ErrRequestLogRequired},
	}
	for _, tc := range cases {
		e := newEnv(t)
		reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(tc.req)
		requireErrIs(t, err, tc.want)
		if reply != nil {
			t.Fatalf("%s：拒绝却回了响应 %+v", tc.name, reply)
		}
		if got := e.calls().total(); got != 0 {
			t.Fatalf("%s：拒绝前已触库 %d 次：%v", tc.name, got, e.calls().ops())
		}
		// scene 恰好 64 必须放行（列宽边界只能拒在 >64 一侧）。
		if tc.want == model.ErrFieldTooLong {
			e2 := newEnv(t)
			if _, err := NewGetRecallConfigLogic(e2.ctx(), e2.svcCtx).GetRecallConfig(
				&rpc.GetRecallConfigReq{Scene: strings.Repeat("s", colScene), Mid: 1}); err != nil {
				t.Fatalf("scene 长度恰为列宽 %d 时被拒：%v", colScene, err)
			}
		}
	}
}

// 响应里的每个数字都必须来自同一份配置：改一处、响应跟着改一处。
// 只要 logic 里还有第二份字面量，这条用例就会红。
func TestGetRecallConfigServesTheSameNumbersTheValidatorUses(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) {
		rc.MaxCandidates = 250
		rc.DefaultLimit = 60
		rc.PerSourceMax = 45
		rc.MaxSeedAids = 11
		rc.MaxSeedTags = 12
		rc.MaxExcludeAids = 77
		rc.DegradeEnabled = false
		rc.FallbackSource = srcFollow
		rc.TTLSeconds = 5
	})
	opts := e.repo.Options()
	reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{
		Scene: "home.feed", Mid: 900000001,
	})
	if err != nil {
		t.Fatalf("已登录用户取参数面失败：%v", err)
	}
	pairs := []struct {
		field string
		got   int32
		want  int
	}{
		{"max_candidates", reply.GetMaxCandidates(), opts.MaxCandidates},
		{"default_limit", reply.GetDefaultLimit(), opts.DefaultLimit},
		{"per_source_max", reply.GetPerSourceMax(), opts.PerSourceMax},
		{"max_seed_aids", reply.GetMaxSeedAids(), opts.MaxSeedAids},
		{"max_seed_tags", reply.GetMaxSeedTags(), opts.MaxSeedTags},
		{"max_exclude_aids", reply.GetMaxExcludeAids(), opts.MaxExcludeAids},
	}
	for _, p := range pairs {
		if int(p.got) != p.want {
			t.Errorf("%s=%d，与 Options() 的 %d 不同源（logic 里存在第二份字面量？）", p.field, p.got, p.want)
		}
	}
	if reply.GetDegradeEnabled() != false {
		t.Errorf("degrade_enabled=%t，配置已关，响应仍为真", reply.GetDegradeEnabled())
	}
	if reply.GetFallbackSource() != rpc.Source_SOURCE_FOLLOW {
		t.Errorf("fallback_source=%v，期望配置改成了 SOURCE_FOLLOW", reply.GetFallbackSource())
	}
	if reply.GetTtlSeconds() != 5 {
		t.Errorf("ttl_seconds=%d，期望 5", reply.GetTtlSeconds())
	}
	// mid>0 不裁剪：列表原样下发，且顺序就是配置顺序（客户端按它排默认组合）。
	if got := fmtSources(reply.GetEnabledSources()); got !=
		"SOURCE_HOT,SOURCE_FOLLOW,SOURCE_TAG,SOURCE_COLD" {
		t.Errorf("已登录 enabled_sources=%q，期望配置全集与顺序", got)
	}
	if got := fmtSources(reply.GetDefaultSources()); got != "SOURCE_HOT,SOURCE_FOLLOW,SOURCE_COLD" {
		t.Errorf("已登录 default_sources=%q", got)
	}
	// 没有任何池上线：空列表 + 参数面照常（客户端仍需知道上限），且明细批量读根本不发起。
	if len(reply.GetReadyPools()) != 0 {
		t.Fatalf("recall_pool_current 为空却回了 %d 个池", len(reply.GetReadyPools()))
	}
	assertOps(t, e.calls(), 0, "Current.ListCurrent")
	if !strings.Contains(entryAt(e.calls(), 0), fmt.Sprintf("(%d)", e.rc.MaxReadyPools)) {
		t.Fatalf("ListCurrent 的 limit 没把 MaxReadyPools 传下去：%q", entryAt(e.calls(), 0))
	}
}

// 游客裁剪与 RecallCandidates 共用 guestSources/trimGuestSources：
// 下发的游客列表必须只含冷启动路 + 兜底路，且被裁掉的路不能出现在默认组合里。
func TestGetRecallConfigTrimsGuestSourcesLikeTheRecallPathDoes(t *testing.T) {
	e := newEnv(t)
	reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 0})
	if err != nil {
		t.Fatalf("游客取参数面失败：%v", err)
	}
	if got := fmtSources(reply.GetEnabledSources()); got != "SOURCE_HOT,SOURCE_COLD" {
		t.Errorf("游客 enabled_sources=%q，期望只留冷启动(6)+兜底(1)", got)
	}
	if got := fmtSources(reply.GetDefaultSources()); got != "SOURCE_HOT,SOURCE_COLD" {
		t.Errorf("游客 default_sources=%q", got)
	}
	// 与召回路的同一份判定：同一配置下 trimGuestSources 给出同样的保留集。
	kept, trimmed := trimGuestSources(sourceList(e.rc.EnabledSources), guestSources(e.rc))
	if fmtSources(rpcSourceList(kept)) != fmtSources(reply.GetEnabledSources()) {
		t.Fatalf("参数面与召回路的游客裁剪结果漂移：logic kept=%v，响应 %v", kept, reply.GetEnabledSources())
	}
	if fmtSources(rpcSourceList(trimmed)) != "SOURCE_FOLLOW,SOURCE_TAG" {
		t.Errorf("被裁路=%v，期望关注+标签", trimmed)
	}

	// 冷启动路与兜底路重合时（配置把 ColdStartSource 也设成热门），裁剪结果只剩一条。
	e2 := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.ColdStartSource = srcHot })
	r2, err := NewGetRecallConfigLogic(e2.ctx(), e2.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 0})
	if err != nil {
		t.Fatalf("兜底即冷启动时取参数面失败：%v", err)
	}
	if got := fmtSources(r2.GetEnabledSources()); got != "SOURCE_HOT" {
		t.Errorf("allowed 只剩 {1} 时 enabled_sources=%q", got)
	}
	// 标签路整条不可用时必须清空而不是回一个 nil 让客户端以为"没路可走=全部可走"。
	e3 := newEnvWithRecall(t, func(rc *config.RecallConf) {
		rc.EnabledSources = []int64{srcFollow, srcTag}
		rc.DefaultSources = []int64{srcFollow}
	})
	r3, err := NewGetRecallConfigLogic(e3.ctx(), e3.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 0})
	if err != nil {
		t.Fatalf("全非游客路配置下取参数面失败：%v", err)
	}
	if len(r3.GetEnabledSources()) != 0 || len(r3.GetDefaultSources()) != 0 {
		t.Fatalf("游客拿不到任何路却回了非空列表：%q / %q",
			fmtSources(r3.GetEnabledSources()), fmtSources(r3.GetDefaultSources()))
	}
}

// 负缓存期钳到 0 且不报错：缓存提示不是业务事实，不能让整个参数面不可用。
func TestGetRecallConfigClampsNegativeTTLInsteadOfFailing(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.TTLSeconds = -5 })
	reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	if err != nil {
		t.Fatalf("TTL 为负时不该失败：%v", err)
	}
	if reply.GetTtlSeconds() != 0 {
		t.Fatalf("ttl_seconds=%d，期望钳到 0（负值会被 cacheSetTopN 当不缓存，下发负数只会误导网关）",
			reply.GetTtlSeconds())
	}
	// ttl=0 是合法配置（关掉建议缓存），必须原样下发而不是偷偷换成默认值。
	e0 := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.TTLSeconds = 0 })
	r0, err := NewGetRecallConfigLogic(e0.ctx(), e0.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	if err != nil {
		t.Fatalf("TTL=0 时失败：%v", err)
	}
	if r0.GetTtlSeconds() != 0 {
		t.Fatalf("ttl_seconds=%d，配置是 0", r0.GetTtlSeconds())
	}
}

// ready_pools 的可证事实：批次以指针为准、item_count 以版本行为准、
// state 镜像与 batch 漂移只告警不改结论、新鲜度按指针的 published_at 判。
func TestGetRecallConfigReadyPoolsTakesThePointerAsTruth(t *testing.T) {
	e := newEnv(t)
	now := nowUnix()
	// 池 A：指针 v6（新鲜、published_at=now），版本行状态被写成 RETIRED（镜像落后）、
	// 且 batch_id 与指针不一致（漂移）。两条都只该告警，不该改响应结论。
	e.st.seedPointer(srcHot32, "global", 6, "batch-from-pointer", now)
	e.st.seedVersion(srcHot32, "global", 6, "batch-in-version-row", model.VersionStateRetired, 512)
	// 池 B：指针 v3 的 published_at 超出 PoolStaleSeconds(1800) -> stale=true。
	e.st.seedPointer(srcCold32, "platform:1", 3, "b-cold", now-e.rc.PoolStaleSeconds-1)
	e.st.seedVersion(srcCold32, "platform:1", 3, "b-cold", model.VersionStateCurrent, 40)
	// 池 C：指针 published_at=0（手工修出来的行）-> 一并判 stale。
	e.st.seedPointer(srcTag32, "zone:7", 2, "b-tag", 0)
	e.st.seedVersion(srcTag32, "zone:7", 2, "b-tag", model.VersionStateCurrent, 9)

	writesBefore := e.st.writes
	reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	if err != nil {
		t.Fatalf("镜像漂移/批次漂移不该失败：%v", err)
	}
	pools := reply.GetReadyPools()
	if len(pools) != 3 {
		t.Fatalf("ready_pools=%d 个，期望 3：%+v", len(pools), pools)
	}
	// 顺序由 SQL 决定（published_at DESC）：A(now) > B(now-1801) > C(0)。
	if got := pools[0].GetPool().GetSource(); got != rpc.Source_SOURCE_HOT {
		t.Fatalf("第 1 个池=%v，期望指针按 published_at DESC 排在最前", got)
	}
	a := pools[0]
	if a.GetCurrentVersion() != 6 || a.GetBatchId() != "batch-from-pointer" {
		t.Errorf("池 A 回显 version=%d batch=%q，批次必须以指针为准（版本行的 batch 只用于发现漂移）",
			a.GetCurrentVersion(), a.GetBatchId())
	}
	if a.GetItemCount() != 512 {
		t.Errorf("池 A item_count=%d，期望取版本行登记值 512", a.GetItemCount())
	}
	if a.GetStale() {
		t.Errorf("池 A published_at=now 却判 stale（PoolStaleSeconds=%d）", e.rc.PoolStaleSeconds)
	}
	if pools[1].GetStale() != true || pools[2].GetStale() != true {
		t.Errorf("超窗池 stale=%t / 无 published_at 池 stale=%t，两者都该为真",
			pools[1].GetStale(), pools[2].GetStale())
	}
	if pools[2].GetPool().GetPoolKey() != "zone:7" || pools[2].GetCurrentVersion() != 2 {
		t.Errorf("池 C 寻址回显错：%+v", pools[2])
	}
	// 明细必须一次批量取（逐池 FindByID 是 N+1），且 refs 与指针一一对应。
	assertOps(t, e.calls(), 0, "Current.ListCurrent", "PoolVersion.ListByRefs")
	if len(e.st.lastListByRefs) != 3 {
		t.Fatalf("ListByRefs 收到 %d 个 ref，期望 3：%v", len(e.st.lastListByRefs), e.st.lastListByRefs)
	}
	if got := e.st.lastListByRefs[0]; got.Source != srcHot32 || got.PoolKey != "global" || got.Version != 6 {
		t.Errorf("第 1 个 ref=%+v，未按指针 (1,global,6) 构造", got)
	}
	// 读路径不得改任何计数（AGENTS.md §5）。
	if e.st.writes != writesBefore {
		t.Fatalf("参数面下发写了库：writes %d -> %d", writesBefore, e.st.writes)
	}
}

// 指针在而版本行没了 = 数据撕裂，必须显式失败：
// 回一条 version=7、item_count=0 的记录等于把"登记行被删了"读成"这个池空了"。
func TestGetRecallConfigFailsWhenPointerHasNoVersionRow(t *testing.T) {
	e := newEnv(t)
	e.st.seedPointer(srcHot32, "global", 7, "b-gone", nowUnix())
	// 故意不 seedVersion(…, 7, …)。
	reply, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	requireErrIs(t, err, model.ErrVersionNotFound)
	if reply != nil {
		t.Fatalf("数据撕裂却回了响应（调用方会把 0 条读成空池）：%+v", reply)
	}
	// 失败也必须只读：一次指针 + 一次批量明细，没有第三次调用。
	assertOps(t, e.calls(), 0, "Current.ListCurrent", "PoolVersion.ListByRefs")
}

// MaxReadyPools 的两道闸门都来自配置：<=0 与 >200 各自报对哨兵，200 本身放行。
func TestGetRecallConfigGuardsTheReadyPoolsScanSize(t *testing.T) {
	e := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxReadyPools = 0 })
	_, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	requireErrIs(t, err, model.ErrInvalidLimit)
	if e.calls().total() != 0 {
		t.Fatalf("水位线非法时已触库：%v", e.calls().ops())
	}

	e2 := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxReadyPools = model.MaxPoolRefsPerQuery + 1 })
	_, err = NewGetRecallConfigLogic(e2.ctx(), e2.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	requireErrIs(t, err, model.ErrLimitTooLarge)
	if e2.calls().total() != 0 {
		t.Fatalf("超 model 上限时已触库（本该在 logic 先拒，避免把整个参数面变成一次失败）：%v", e2.calls().ops())
	}

	e3 := newEnvWithRecall(t, func(rc *config.RecallConf) { rc.MaxReadyPools = model.MaxPoolRefsPerQuery })
	if _, err := NewGetRecallConfigLogic(e3.ctx(), e3.svcCtx).GetRecallConfig(
		&rpc.GetRecallConfigReq{Mid: 1}); err != nil {
		t.Fatalf("MaxReadyPools 恰为上限 %d 时被拒：%v", model.MaxPoolRefsPerQuery, err)
	}
}

// item_count 落到 int32 字段：登记值超出 int32 必须报错，不能回绕成负数或小数。
func TestGetRecallConfigRejectsItemCountBeyondInt32(t *testing.T) {
	e := newEnv(t)
	e.st.seedPointer(srcHot32, "global", 4, "b4", nowUnix())
	e.st.seedVersion(srcHot32, "global", 4, "b4", model.VersionStateCurrent, 1<<40)
	_, err := NewGetRecallConfigLogic(e.ctx(), e.svcCtx).GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	if err == nil {
		t.Fatalf("item_count=1<<40 被静默转成 int32 回了响应（回绕后的数字会被当成真实规模缓存下去）")
	}
	if !strings.Contains(err.Error(), "item_count") {
		t.Fatalf("错误没点明是哪个字段溢出：%v", err)
	}
}

// Repository 未装配必须回 ErrSourceNotConfigured，不能让 gRPC 侧收到一次 panic。
func TestGetRecallConfigNilRepositoryIsAnErrorNotAPanic(t *testing.T) {
	l := NewGetRecallConfigLogic(context.Background(), &svc.ServiceContext{Config: config.Config{}})
	_, err := l.GetRecallConfig(&rpc.GetRecallConfigReq{Mid: 1})
	requireErrIs(t, err, repository.ErrSourceNotConfigured)
}
