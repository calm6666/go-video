package logic

// special_test.go 覆盖 UpSpecial（单 mid，缓存→DB→回填）与 UpsSpecial（批量，直打 DB）。
//
// 钉住的结论：
//   - UpSpecial 的回填只写「查到的那一组」，空表回填的是 `[]`（不是 cache.go 的 `{}` 空标记），
//     两者在读侧都算命中，所以「查无此人」同样被防击穿挡住，最长 1h；
//   - 缓存里的脏值 / 缓存故障都不会被伪装成「该 UP 没有特殊分组」：错误原样上抛、一次都不回库；
//   - 批量路径**完全不碰缓存**（repository.go:118-125 的策略），所以批量与单条之间不存在
//     「批量读到旧缓存」的问题，但批量也不会给单条路径预热；
//   - 批量对含非法元素的请求采取**逐条部分成功**：非法/不存在的 mid 从应答 map 里缺席，
//     而不是整批拒绝；上限 100 是整批拒绝（errTooManyMids），且拒绝发生在任何依赖调用之前；
//   - 重复提交（含重复 mid）幂等：IN 是集合语义，结果既不翻倍也不写库。
//
// 缺陷登记见 fakes_test.go 文件头（本文件复现 D3，并给出批量侧的对照面）。

import (
	"context"
	"fmt"
	"testing"

	"go-video/services/creator/rpc"
)

const (
	specMid = int64(11)
	specKey = "up:spec:11" // fakeKeySpecial(specMid)
)

// --- UpSpecial ---

func TestUpSpecialMissThenBackfillThenHit(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3, 5)

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "UpSpecial", err)
	wantInt64sEQ(t, "回源结果", "group_ids", got.GetUpSpecial().GetGroupIds(), []int64{3, 5})
	wantSeq(t, "miss→回源→回填", st.log, 0,
		"cache.GetSpecial:"+specKey,
		"up_special.FindOne:11",
		"cache.SetSpecial:"+specKey)
	// 回填的是 JSON 数组原样；model 无 ORDER BY，所以顺序＝行插入序（结论：顺序不保证稳定）。
	wantEQ(t, "回填的原始值", "raw", st.cacheRaw(t, specKey), "[3,5]")
	wantEQ(t, "回填 TTL", "ttl", st.cacheTTL(t, specKey), fakeTTLSpecial)

	before := st.log.snapshot()
	again, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "二读", err)
	wantInt64sEQ(t, "二读来自缓存", "group_ids", again.GetUpSpecial().GetGroupIds(), []int64{3, 5})
	wantSeq(t, "二读序列", st.log, before, "cache.GetSpecial:"+specKey)
}

// TestUpSpecialNoRowsCachesEmptyArray pin repository.go:111-114：
// 「无记录」先被换成 []int64{} 再回填，所以落的是真空数组 `[]`；
// cache.go:117 的 setEmpty 分支（`{}`）从本路径**写不出来**（只有 DelSpecial/别的入口才会），
// 但读侧两种形态都判为命中，防击穿效果一致。
func TestUpSpecialNoRowsCachesEmptyArray(t *testing.T) {
	st := newStore()

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "无记录的首读", err)
	wantEQ(t, "无记录应答为空列表", "group_ids 长度", len(got.GetUpSpecial().GetGroupIds()), 0)
	wantSeq(t, "无记录也回填", st.log, 0,
		"cache.GetSpecial:"+specKey,
		"up_special.FindOne:11",
		"cache.SetSpecial:"+specKey)
	wantEQ(t, "回填形态是空数组而非空标记", "raw", st.cacheRaw(t, specKey), "[]")

	before := st.log.snapshot()
	if _, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid}); err != nil {
		t.Fatalf("二读：%v", err)
	}
	wantSeq(t, "负缓存生效：二读不回库", st.log, before, "cache.GetSpecial:"+specKey)
}

// TestUpSpecialEmptyMarkCountsAsHit 覆盖另一种命中形态（`{}`）：算命中且不回库。
// 这一形态在真库里由别的入口写出来，读侧必须认账。
func TestUpSpecialEmptyMarkCountsAsHit(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3) // 库里明明有，但缓存说是空 → 缓存优先（一致性只靠失效，而本服务没有 up:spec 的失效入口）
	st.warmSpecial(specMid, fakeEmptyMark)

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "空标记命中", err)
	wantEQ(t, "应答长度", "group_ids 长度", len(got.GetUpSpecial().GetGroupIds()), 0)
	wantSeq(t, "命中不回库", st.log, 0, "cache.GetSpecial:"+specKey)
}

// TestUpSpecialHasNoInvalidationPath 说明 up:spec 的滞后上限：本服务没有任何 RPC 写 up_special
// （分组归属由运营面直接改表），所以改完最长 1h 才可见；钩子保证「改表」真的发生在回填之前。
func TestUpSpecialStaleBackfillSurvivesUntilTTL(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.raceBefore("cache.SetSpecial", func() {
		st.seedSpecial(specMid, 5) // 就在要回填的这一瞬间，运营面把 mid 11 又加进分组 5
	})

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "被插队的读", err)
	wantInt64sEQ(t, "pin：本次读的是插队前的行集", "group_ids", got.GetUpSpecial().GetGroupIds(), []int64{3})

	// 交错后的事实：库里有两组，缓存却被钉成一组。
	wantEQ(t, "库侧组数", "up_special 行数", len(st.special.rows), 2)
	wantEQ(t, "pin 缺陷：回填把旧集合又写回缓存", "raw", st.cacheRaw(t, specKey), "[3]")

	again, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "二读", err)
	wantInt64sEQ(t, "pin 缺陷：读到旧集合", "group_ids", again.GetUpSpecial().GetGroupIds(), []int64{3})
	wantMethodCount(t, "二读不回库（脏缓存挡在中间）", st.log, "up_special.FindOne", 1)
	wantEQ(t, "脏值存活时长上限", "ttl", st.cacheTTL(t, specKey), fakeTTLSpecial)
	st.checkRaces(t)
}

// TestUpSpecialIgnoresIllegalMid 钉缺陷 D3：UpSpecial 没有 mid>0 守卫（同包其余读方法都有），
// mid=0/负数会照常触库并写进 `up:spec:0` 这样的键，应答看起来是「该用户无特殊分组」的成功结果。
// 对照面：UpAttr / UpSwitch / SetUpSwitch 都在 logic 层就回 errInvalidMid。
func TestUpSpecialIgnoresIllegalMid(t *testing.T) {
	for _, mid := range []int64{0, -7} {
		t.Run(fakeKeySpecial(mid), func(t *testing.T) {
			st := newStore()
			key := fakeKeySpecial(mid)

			got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
				UpSpecial(&rpc.UpSpecialReq{Mid: mid})
			wantNoErr(t, "pin D3：非法 mid 仍成功", err)
			wantEQ(t, "pin D3：伪装成「无分组」的成功应答", "group_ids 长度",
				len(got.GetUpSpecial().GetGroupIds()), 0)
			// 非法 mid 也走完「回源 + 回填」，说明守卫确实不存在。
			wantSeq(t, "pin D3 轨迹", st.log, 0,
				"cache.GetSpecial:"+key,
				"up_special.FindOne:"+fmt.Sprintf("%d", mid),
				"cache.SetSpecial:"+key)
			wantEQ(t, "非法 mid 的键也被写进缓存", "raw", st.cacheRaw(t, key), "[]")
		})
	}
}

func TestUpSpecialCorruptCacheFailsLoud(t *testing.T) {
	for _, raw := range []string{"not-json", `{"a":1}`, `"3"`} {
		t.Run(raw, func(t *testing.T) {
			st := newStore()
			st.seedSpecial(specMid, 3)
			st.warmSpecial(specMid, raw)

			got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
				UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
			if err == nil {
				t.Fatalf("脏缓存 %q 被当成正常应答：%#v", raw, got)
			}
			if got != nil {
				t.Errorf("出错时应答 = %#v, want nil", got)
			}
			// 不静默降级回库：否则「缓存里被人塞了脏值」这类故障会被永久掩盖。
			wantSeq(t, "轨迹止于缓存", st.log, 0, "cache.GetSpecial:"+specKey)
		})
	}
}

func TestUpSpecialCacheFailureIsNotDowngraded(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.cache.failWith("GetSpecial", errFakeRedis)

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantErrIs(t, "缓存故障原样透传", err, errFakeRedis)
	// repository.go:99-102 直接 return nil, err：不吞、不降级、不伪装成空列表。
	if got != nil {
		t.Errorf("应答 = %#v, want nil（不能降级成「无分组」）", got)
	}
	wantSeq(t, "缓存故障时不回库", st.log, 0, "cache.GetSpecial:"+specKey)
}

func TestUpSpecialDBFailureWrapped(t *testing.T) {
	st := newStore()
	st.special.failWith("FindOne", errFakeDB)

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	wantErrContains(t, "错误归属", err, "UpSpecial FindOne")
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	// 故障结果绝不能进缓存：否则一次抖动被钉成 1h 的「无分组」。
	wantSeq(t, "失败后不得回填", st.log, 0,
		"cache.GetSpecial:"+specKey,
		"up_special.FindOne:11")
	if st.cacheHas(specKey) {
		t.Errorf("DB 失败却写了缓存")
	}
}

func TestUpSpecialBackfillFailureStillAnswers(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3, 5)
	st.cache.failWith("SetSpecial", errFakeRedis)

	got, err := NewUpSpecialLogic(context.Background(), st.svcCtx()).
		UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "回填失败被忽略（repository.go:114 是 `_ =`）", err)
	wantInt64sEQ(t, "应答仍来自 DB", "group_ids", got.GetUpSpecial().GetGroupIds(), []int64{3, 5})
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetSpecial:"+specKey,
		"up_special.FindOne:11",
		"cache.SetSpecial:"+specKey)
	if st.cacheHas(specKey) {
		t.Errorf("SetSpecial 失败了却留下缓存值")
	}
}

func TestUpSpecialIsolatesPerMid(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.seedSpecial(12, 7)
	sc := st.svcCtx()

	first, err := NewUpSpecialLogic(context.Background(), sc).UpSpecial(&rpc.UpSpecialReq{Mid: specMid})
	wantNoErr(t, "读 11", err)
	wantInt64sEQ(t, "11 的分组", "group_ids", first.GetUpSpecial().GetGroupIds(), []int64{3})

	second, err := NewUpSpecialLogic(context.Background(), sc).UpSpecial(&rpc.UpSpecialReq{Mid: 12})
	wantNoErr(t, "读 12", err)
	wantInt64sEQ(t, "12 的分组（不得串号）", "group_ids", second.GetUpSpecial().GetGroupIds(), []int64{7})

	wantSeq(t, "两个 mid 各自一条 key", st.log, 0,
		"cache.GetSpecial:"+specKey,
		"up_special.FindOne:11",
		"cache.SetSpecial:"+specKey,
		"cache.GetSpecial:up:spec:12",
		"up_special.FindOne:12",
		"cache.SetSpecial:up:spec:12")
}

// --- UpsSpecial ---

func TestUpsSpecialEmptyBatchShortCircuits(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	before := st.log.snapshot()

	got, err := NewUpsSpecialLogic(context.Background(), st.svcCtx()).
		UpsSpecial(&rpc.UpsSpecialReq{})
	wantNoErr(t, "空批量", err)
	if got.GetUpSpecials() == nil || len(got.GetUpSpecials()) != 0 {
		t.Errorf("空批量应答 = %#v, want 空 map（不是 nil）", got.GetUpSpecials())
	}
	// logic 先短路，repository.go:121-123 的 len==0 兜底在 RPC 路径上不可达。
	wantNoCallAfter(t, "空批量", st.log, before)
	wantCacheUntouched(t, "空批量", st.log)
}

func TestUpsSpecialRejectsOverCapBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name    string
		n       int
		wantErr error
	}{
		{name: "恰好 100 放行", n: 100, wantErr: nil},
		{name: "101 拒绝", n: 101, wantErr: errTooManyMids},
		{name: "1000 拒绝", n: 1000, wantErr: errTooManyMids},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			mids := make([]int64, 0, tc.n)
			for i := 0; i < tc.n; i++ {
				mids = append(mids, int64(i+1)) // 全部合法且互不重复
			}
			before := st.log.snapshot()

			got, err := NewUpsSpecialLogic(context.Background(), st.svcCtx()).
				UpsSpecial(&rpc.UpsSpecialReq{Mids: mids})

			if tc.wantErr != nil {
				wantErrIs(t, t.Name(), err, tc.wantErr)
				if got != nil {
					t.Errorf("超限时应答 = %#v, want nil", got)
				}
				// 超限必须整批拒绝，且拒绝前零依赖调用（一条 SQL 都不该发出去）。
				wantNoCallAfter(t, t.Name(), st.log, before)
				return
			}
			wantNoErr(t, t.Name(), err)
			wantMethodCount(t, "100 个 mid 仍是单条 IN 查询", st.log, "up_special.FindMany", 1)
			wantEQ(t, "IN 列表长度", "mids", len(st.special.findManyCalls[0]), tc.n)
		})
	}
}

// TestUpsSpecialNeverTouchesCache 钉住批量策略（repository.go:118-125）：
// 两条重复请求都直打 DB，一次缓存都不碰——既不会被脏缓存挡住，也不给单条路径预热。
func TestUpsSpecialNeverTouchesCache(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3, 5)
	in := &rpc.UpsSpecialReq{Mids: []int64{specMid}}
	l := NewUpsSpecialLogic(context.Background(), st.svcCtx())

	first, err := l.UpsSpecial(in)
	wantNoErr(t, "首次", err)
	wantInt64sEQ(t, "批量结果", "group_ids", first.GetUpSpecials()[specMid].GetGroupIds(), []int64{3, 5})
	before := st.log.snapshot()
	second, err := l.UpsSpecial(in)
	wantNoErr(t, "重复提交", err)
	wantSeq(t, "重复提交的轨迹与首次一致（无 no-op 分支）", st.log, before, "up_special.FindMany:n=1")

	// 幂等看结果：两次应答完全相同，库里一行没多。
	if a, b := first.String(), second.String(); a != b {
		t.Errorf("重复提交应答不一致：首次 %s, 重复 %s", a, b)
	}
	wantEQ(t, "行数增量", "up_special 行数", len(st.special.rows), 2)
	wantCacheUntouched(t, "批量路径", st.log)
	if st.cacheHas(specKey) {
		t.Errorf("批量把值写进了单条缓存键，会让两条路径的口径分叉")
	}
}

// TestUpsSpecialPartialSuccessForBadElements 钉批量对非法元素的策略：**逐条部分成功**。
// 不存在的 mid、非法 mid（0/负数）都从应答 map 里缺席；整批不因它们失败，
// 且入参列表原样进 SQL（不去重、不排序、不过滤）——所以调用方必须按 map 里有没有键来判断，
// 不能按下标取（缺陷 D3 的对照面：单条路径不校验 mid，批量路径也不校验，只是结果口径不同）。
func TestUpsSpecialPartialSuccessForBadElements(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.seedSpecial(12, 7)

	in := &rpc.UpsSpecialReq{Mids: []int64{12, specMid, 999, 0, -5, 12}}
	got, err := NewUpsSpecialLogic(context.Background(), st.svcCtx()).UpsSpecial(in)
	wantNoErr(t, "含非法元素的批量", err)

	wantInt64sEQ(t, "IN 列表原样透传（重复的 12 没被去掉）", "mids",
		st.special.findManyCalls[0], []int64{12, 11, 999, 0, -5, 12})

	lists := got.GetUpSpecials()
	wantEQ(t, "应答只含真实存在的 mid", "map 大小", len(lists), 2)
	wantInt64sEQ(t, "11 的分组", "group_ids", lists[specMid].GetGroupIds(), []int64{3})
	wantInt64sEQ(t, "12 的分组", "group_ids", lists[12].GetGroupIds(), []int64{7})
	for _, absent := range []int64{999, 0, -5} {
		if _, ok := lists[absent]; ok {
			t.Errorf("查不到的 mid=%d 却出现在应答里：%#v", absent, lists[absent])
		}
	}
	// 重复 mid 不会让结果翻倍（IN 是集合语义），也不会让它出现两次。
	wantEQ(t, "12 的分组数", "group_ids 长度", len(lists[12].GetGroupIds()), 1)
}

func TestUpsSpecialDBFailurePropagates(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.special.failWith("FindMany", errFakeDB)

	got, err := NewUpsSpecialLogic(context.Background(), st.svcCtx()).
		UpsSpecial(&rpc.UpsSpecialReq{Mids: []int64{specMid, 12}})
	wantErrIs(t, "DB 失败透传", err, errFakeDB)
	if got != nil {
		t.Errorf("应答 = %#v, want nil", got)
	}
	// 注意：批量这条 repository 路径**不包错误归属**（与 UpSpecial/UpsSpecial 之外的方法一致地裸返），
	// 见 repository.go:124；所以这里只断言 errors.Is，不断言前缀。
	wantSeq(t, "轨迹", st.log, 0, "up_special.FindMany:n=2")
	wantCacheUntouched(t, "失败路径", st.log)
}

// TestUpsSpecialProjectsEveryPresentMid 锁投影本身：map 的键就是 mid，值只带 group_ids，
// 且值永不为 nil（调用方 range 后可直接取字段）。
func TestUpsSpecialProjectsEveryPresentMid(t *testing.T) {
	st := newStore()
	st.seedSpecial(specMid, 3)
	st.seedSpecial(12, 7, 8)

	got, err := NewUpsSpecialLogic(context.Background(), st.svcCtx()).
		UpsSpecial(&rpc.UpsSpecialReq{Mids: []int64{specMid, 12}})
	wantNoErr(t, "批量", err)
	lists := got.GetUpSpecials()
	for mid, want := range map[int64][]int64{specMid: {3}, 12: {7, 8}} {
		row, ok := lists[mid]
		if !ok {
			t.Fatalf("mid=%d 缺席", mid)
		}
		if row == nil {
			t.Fatalf("mid=%d 的应答值是 nil，调用方 range 后取字段会 panic", mid)
		}
		wantInt64sEQ(t, "投影值", "group_ids", row.GetGroupIds(), want)
	}
	wantEQ(t, "键集合恰好等于「被查且存在」的 mid", "map 大小", len(lists), 2)
}
