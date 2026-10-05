package logic

// cards3logic_test.go 覆盖 Cards3（logic/cards3logic.go:28-38 →
// repository.Cards internal/repository/account.go:90-123 → RawCards raw.go:105-145）。
//
// 被测判定链：按请求顺序逐个读 c3_<mid> → 命中的直接进结果 → 未命中的一批
// **只问一次** user-profile.Members → 合入结果 → 遍历 raw 回填 c3_<mid>。
//
// 钉住的事实：
//  1. mids 为 nil 或 []int64{} 时**零依赖调用**并回非 nil 空 map（account.go:91-93
//     的长度短路），因此 Cards3 的空入参不会打下游；
//  2. 本方法的 error 分支（cards3logic.go:30-33）不可达：RawCards 的四条 return
//     （nil client / Members 故障 / mid 查不到 / 正常）都是 (非 nil map, nil)，
//     所以「批量名片」在任何下游故障下都只会降级、不会失败；
//  3. 缓存读是**逐 mid** 的，且顺序与请求一致（顺序错了就是拿别人的名片应答）；
//     回填走 map 遍历，因此那几步只按集合比（wantOpsSet）；
//  4. 结果的**键**是请求的 mid，**载荷的 mid** 是下游回的 mid（raw.go:128-140），
//     两者可以不一致且无人校验（与 Card3 的缺口 20 同形）；
//  5. 下游 Members 故障时，回源结果退化成「只剩请求 mid 的名片」，
//     而且这份残值照样带 3600 秒 TTL 进缓存（缺口 17 的批量版）；
//  6. user-profile 未配置时（servicecontext.go:31 的条件分支）走 raw.go:108-113，
//     形状与第 5 条相同，但**一次下游都不调**。
//
// 覆盖不到的分支（如实声明）：
//   - cards3logic.go:34-36 的 `cards == nil` 兜底不可达（见第 2 条），
//     本文件用直调 Repository 的三个形状来归属「非 nil」保证。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/account/internal/repository"
	"go-video/services/account/model"
	"go-video/services/account/rpc"
)

func callCards3(t *testing.T, e *env, mids []int64) (*rpc.CardsReply, error) {
	t.Helper()
	return NewCards3Logic(context.Background(), e.svcCtx).Cards3(
		&rpc.MidsReq{Mids: mids, RealIp: "1.2.3.4"})
}

// TestCards3EmptyAndNilMidsTouchNothing mids 为 nil / 空切片：零调用 + 非 nil 空 map。
func TestCards3EmptyAndNilMidsTouchNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		mids []int64
	}{
		{"nil mids", nil},
		{"空切片 mids", []int64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, withDownstream(70001, Downstream{Member: fullMember(70001)}))
			e.st.log.reset()

			reply, err := callCards3(t, e, tc.mids)
			wantNoErr(t, "Cards3", err)
			if reply.GetCards() == nil {
				t.Fatalf("cards = nil, want 非 nil 空 map")
			}
			wantEQ(t, "空入参不得造出条目", "len", len(reply.GetCards()), 0)
			wantOps(t, "空入参不得触任何依赖", e.ops(0), nil)
		})
	}
}

// TestCards3AllCachedKeepsRequestOrderAndSkipsDownstream 全部命中：
// 逐 mid 读缓存、顺序即请求顺序、一次 Members 都不许多调。
func TestCards3AllCachedKeepsRequestOrderAndSkipsDownstream(t *testing.T) {
	const (
		a, b = int64(70001), int64(70002)
	)
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(cardKey(a), &rpc.Card{Mid: a, Name: "缓存里的 A"}, 11)
	st.cache.warmMsg(cardKey(b), &rpc.Card{Mid: b, Name: "缓存里的 B"}, 11)
	st.log.reset()

	// 故意按 b,a 的顺序请求：轨迹必须跟着变成 c3_b, c3_a。
	reply, err := callCards3(t, e, []int64{b, a})
	wantNoErr(t, "Cards3（全命中）", err)
	wantProto(t, "第一个位置该是 B 的名片", "card", reply.GetCards()[b], &rpc.Card{Mid: b, Name: "缓存里的 B"})
	wantProto(t, "第二个位置该是 A 的名片", "card", reply.GetCards()[a], &rpc.Card{Mid: a, Name: "缓存里的 A"})
	wantOps(t, "全命中只按序读两次缓存", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(b),
		"cache.CacheCard:" + cardKey(a),
	})
	wantNoOpsWith(t, "全命中路径", e.ops(0), "userProfile.")
	if ttl, ok := st.cache.ttlOf(cardKey(a)); !ok || ttl != 11 {
		t.Errorf("命中路径改了 TTL：%d(存在=%v), want 保持 11", ttl, ok)
	}
}

// TestCards3PartialHitBatchesOnlyMisses 混合命中：只有 miss 的那批进 Members，
// 且顺序保持请求顺序；回填逐条落 c3_。
func TestCards3PartialHitBatchesOnlyMisses(t *testing.T) {
	const (
		a, b, c = int64(70001), int64(70002), int64(70003)
	)
	e := newEnv(t,
		withDownstream(b, Downstream{Member: &repository.UserProfileMember{
			UserProfileBase: repository.UserProfileBase{Mid: b, Name: "B 的真名"},
		}}),
		withDownstream(c, Downstream{Member: &repository.UserProfileMember{
			UserProfileBase: repository.UserProfileBase{Mid: c, Name: "C 的真名"},
		}}),
	)
	st := e.st
	st.cache.warmMsg(cardKey(a), &rpc.Card{Mid: a, Name: "缓存里的 A"}, 11)
	st.account.put(&model.Account{Mid: b, Status: 0})
	st.log.reset()

	reply, err := callCards3(t, e, []int64{a, b, c})
	wantNoErr(t, "Cards3", err)
	wantEQ(t, "三个请求 mid 都要有条目", "len", len(reply.GetCards()), 3)
	wantEQ(t, "命中项用缓存值", "A.name", reply.GetCards()[a].GetName(), "缓存里的 A")
	wantEQ(t, "未命中项回源值", "B.name", reply.GetCards()[b].GetName(), "B 的真名")
	wantEQ(t, "未命中项回源值", "C.name", reply.GetCards()[c].GetName(), "C 的真名")
	wantEQ(t, "B 的本地禁言不参与名片（与 Card3 的缺口 24 同形）",
		"silence", reply.GetCards()[b].GetSilence(), int32(0))

	ops := e.ops(0)
	wantOps(t, "缓存读按请求顺序 + 一次批量回源", ops[:4], []string{
		"cache.CacheCard:" + cardKey(a),
		"cache.CacheCard:" + cardKey(b),
		"cache.CacheCard:" + cardKey(c),
		"userProfile.Members:70002,70003", // 只带 miss，不含命中的 a
	})
	// 回填是 `for mid, card := range raw`（account.go:117-121），顺序不可预测。
	wantOpsSet(t, "两条 miss 都回填", ops[4:], []string{
		"cache.AddCacheCard:" + cardKey(b),
		"cache.AddCacheCard:" + cardKey(c),
	})
	wantCount(t, "命中的 a 不许被覆写", ops, "cache.AddCacheCard:"+cardKey(a), 0)
}

// TestCards3DuplicateMidsReadTwiceButDedup 请求里的重复 mid：缓存读两次（无短路），
// 结果 map 去重成一个条目，Members 也按重复列表问。
func TestCards3DuplicateMidsReadTwiceButDedup(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withDownstream(mid, Downstream{Member: fullMember(mid)}))
	e.st.log.reset()

	reply, err := callCards3(t, e, []int64{mid, mid})
	wantNoErr(t, "Cards3", err)
	wantEQ(t, "重复 mid 只留一个条目", "len", len(reply.GetCards()), 1)
	wantOps(t, "重复请求不做去重就直接读缓存/问下游", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(mid),
		"cache.CacheCard:" + cardKey(mid),
		"userProfile.Members:70001,70001",
		"cache.AddCacheCard:" + cardKey(mid),
	})
}

// TestCards3SingleMidCacheFaultFallsBackAlone 只有第二个 mid 的缓存读故障时，
// 只有它落到批量回源，第一个仍走命中（逐 mid 隔离，不整批降级）。
func TestCards3SingleMidCacheFaultFallsBackAlone(t *testing.T) {
	const (
		a, b = int64(70001), int64(70002)
	)
	boom := errors.New("account/test: redis 抖动")
	e := newEnv(t, withDownstream(b, Downstream{Member: &repository.UserProfileMember{
		UserProfileBase: repository.UserProfileBase{Mid: b, Name: "B 的真名"},
	}}))
	st := e.st
	st.cache.warmMsg(cardKey(a), &rpc.Card{Mid: a, Name: "缓存里的 A"}, 11)
	st.cache.warmMsg(cardKey(b), &rpc.Card{Mid: b, Name: "缓存里的 B"}, 11)
	st.cache.failOn("CacheCard", 2, boom) // 第二次读失败 → 吞成 miss（生产口径）
	st.log.reset()

	reply, err := callCards3(t, e, []int64{a, b})
	wantNoErr(t, "缓存故障不得外传", err)
	wantEQ(t, "首个仍取缓存值", "A.name", reply.GetCards()[a].GetName(), "缓存里的 A")
	wantEQ(t, "第二个回源取下游真值", "B.name", reply.GetCards()[b].GetName(), "B 的真名")
	wantOps(t, "只把失败的那个并入批量回源", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(a),
		"cache.CacheCard:" + cardKey(b),
		"userProfile.Members:" + itoa(b),
		"cache.AddCacheCard:" + cardKey(b),
	})
	if reply.GetCards()[b].GetName() == "缓存里的 B" {
		t.Errorf("缓存故障被当成了命中：拿回了布景值")
	}
}

// TestCards3DownstreamFaultDegradesToMidOnlyAndIsCached 批量回源故障：
// 每个请求 mid 都退化成「只剩 mid」的名片，不报错，而且残值带满 TTL 进缓存。
//
// TODO(缺陷)（README 缺口 17 的批量形态）：raw.go:121-128 吞掉 Members 的错误后
// 照样返回成功，account.go:117-121 于是无条件回填，一次下游抖动会把
// 「所有人都查不到名片」固化一小时（本批量接口的影响面比单查更大）。
func TestCards3DownstreamFaultDegradesToMidOnlyAndIsCached(t *testing.T) {
	const (
		a, b = int64(70001), int64(70002)
	)
	boom := errors.New("account/test: user-profile 宕机")
	e := newEnv(t,
		withDownstream(a, Downstream{Member: fullMember(a)}),
		withDownstream(b, Downstream{Member: fullMember(b)}),
	)
	st := e.st
	st.userProfile.failWith("Members", boom)
	st.log.reset()

	reply, err := callCards3(t, e, []int64{a, b})
	wantNoErr(t, "下游故障的 Cards3 必须降级不报错", err)
	wantProto(t, "只剩 mid 的降级名片 A", "card", reply.GetCards()[a], &rpc.Card{Mid: a})
	wantProto(t, "只剩 mid 的降级名片 B", "card", reply.GetCards()[b], &rpc.Card{Mid: b})
	ops := e.ops(0)
	wantOps(t, "读缓存 + 一次批量回源", ops[:3], []string{
		"cache.CacheCard:" + cardKey(a),
		"cache.CacheCard:" + cardKey(b),
		"userProfile.Members:70001,70002",
	})
	wantOpsSet(t, "降级残值照样回填两条", ops[3:], []string{
		"cache.AddCacheCard:" + cardKey(a),
		"cache.AddCacheCard:" + cardKey(b),
	})
	wantNotContains(t, "下游错误原文不得进报文", reply.String(), boom.Error())

	// 残值占住缓存：下游恢复后仍然读回「只剩 mid」。
	st.userProfile.clearFaults()
	st.log.reset()
	again, err := callCards3(t, e, []int64{a, b})
	wantNoErr(t, "恢复后的第二次 Cards3", err)
	wantProto(t, "残值被当真值命中", "card", again.GetCards()[a], &rpc.Card{Mid: a})
	wantNoOpsWith(t, "命中路径", e.ops(0), "userProfile.Members")
	for _, k := range []string{cardKey(a), cardKey(b)} {
		if ttl, ok := st.cache.ttlOf(k); !ok || ttl != 3600 {
			t.Errorf("%s 的降级值 TTL = %d(存在=%v), want 3600 —— 缺口 17 的影响窗口正是它", k, ttl, ok)
		}
	}
}

// TestCards3WithoutUserProfileShape 未配置 user-profile（servicecontext.go:31 条件不成立）：
// raw.go:108-113 直接回「只剩请求 mid」的名片，形状与故障降级相同，但零下游调用。
func TestCards3WithoutUserProfileShape(t *testing.T) {
	const mid = int64(70001)
	e := newEnv(t, withoutUserProfile(), withDownstream(mid, Downstream{Member: fullMember(mid)}))
	st := e.st
	st.log.reset()

	reply, err := callCards3(t, e, []int64{mid})
	wantNoErr(t, "未注入 user-profile 的 Cards3", err)
	wantProto(t, "只剩请求 mid 的名片", "card", reply.GetCards()[mid], &rpc.Card{Mid: mid})
	wantOps(t, "只有缓存读 + 回填，没有下游", e.ops(0), []string{
		"cache.CacheCard:" + cardKey(mid),
		"cache.AddCacheCard:" + cardKey(mid),
	})
	wantNoOpsWith(t, "未注入路径", e.ops(0), "userProfile.")
}

// TestCards3AdoptsDownstreamMidUnderRequestedKey 键 = 请求 mid、载荷 mid = 下游回的 mid：
// 下游串号时 A 的名片挂在 B 的请求键上，响应里也没有任何不一致可被察觉。
//
// TODO(缺陷)（README 缺口 20 的批量形态）：raw.go:129 用 `result[mid] = card` 保留
// 请求键、card.Mid 却取 m.Mid（raw.go:130），两者无人比对。
func TestCards3AdoptsDownstreamMidUnderRequestedKey(t *testing.T) {
	const (
		requested = int64(70001)
		returned  = int64(999)
	)
	e := newEnv(t, withDownstream(requested, Downstream{Member: &repository.UserProfileMember{
		UserProfileBase: repository.UserProfileBase{Mid: returned, Name: "串号的名片"},
	}}))
	st := e.st
	st.log.reset()

	reply, err := callCards3(t, e, []int64{requested})
	wantNoErr(t, "Cards3", err)
	card, ok := reply.GetCards()[requested]
	if !ok {
		t.Fatalf("请求的 mid %d 在结果里没有条目：%v", requested, reply.GetCards())
	}
	wantEQ(t, "载荷 mid 跟着下游跑", "card.mid", card.GetMid(), returned)
	got, ok := msgAs[*rpc.Card](st.cache, cardKey(requested))
	if !ok {
		t.Fatalf("前提已变：%s 没被写（当前 key：%v）", cardKey(requested), st.cache.keys())
	}
	wantEQ(t, "请求键下挂着的 mid（键与载荷不一致）", "值", got.GetMid(), returned)
}

// TestCards3MidZeroGetsOwnKey mid=0 不校验，且 c3_0 与 c3_1 各自独立。
func TestCards3MidZeroGetsOwnKey(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.cache.warmMsg(cardKey(0), &rpc.Card{Mid: 0, Name: "零号名片"}, 11)
	st.log.reset()

	reply, err := callCards3(t, e, []int64{0, 1})
	wantNoErr(t, "Cards3", err)
	wantEQ(t, "c3_0 独立可读", "值", reply.GetCards()[0].GetName(), "零号名片")
	wantOps(t, "两个 mid 各读一次，1 才回源", e.ops(0)[:2], []string{
		"cache.CacheCard:c3_0", "cache.CacheCard:c3_1",
	})
	wantEQ(t, "只有未命中的 1 被回源", "条数",
		strings.Count(strings.Join(e.ops(0), "\n"), "userProfile.Members:1"), 1)
}

// TestCards3NonNilMapGuaranteeLivesInRepository 归属：非 nil map 与「永不报错」
// 都住在 account.go:91-123 + raw.go:105-145，不在 cards3logic.go:34-36。
func TestCards3NonNilMapGuaranteeLivesInRepository(t *testing.T) {
	const a, b = int64(70001), int64(70002)
	t.Run("空入参", func(t *testing.T) {
		e := newEnv(t, withoutUserProfile())
		cards, err := e.st.repo.Cards(context.Background(), nil)
		wantNoErr(t, "Repository.Cards", err)
		if cards == nil {
			t.Fatalf("Repository 回了 nil map")
		}
	})
	t.Run("全部命中", func(t *testing.T) {
		e := newEnv(t, withoutUserProfile())
		e.st.cache.warmMsg(cardKey(a), &rpc.Card{Mid: a}, 11)
		e.st.cache.warmMsg(cardKey(b), &rpc.Card{Mid: b}, 11)
		cards, err := e.st.repo.Cards(context.Background(), []int64{a, b})
		wantNoErr(t, "Repository.Cards", err)
		wantEQ(t, "两个都从缓存来", "len", len(cards), 2)
	})
	t.Run("批量读缓存全故障", func(t *testing.T) {
		e := newEnv(t, withoutUserProfile())
		e.st.cache.failWith("CacheCard", errors.New("account/test: redis 全挂"))
		cards, err := e.st.repo.Cards(context.Background(), []int64{a})
		wantNoErr(t, "Repository.Cards（缓存全故障）", err)
		if cards == nil {
			t.Fatalf("Repository 回了 nil map")
		}
		wantProto(t, "退化成只剩 mid", "card", cards[a], &rpc.Card{Mid: a})
	})
}
