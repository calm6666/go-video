package logic

// 未读计数修复入口（RecomputeUnread）：从 inbox_user_message 真值重算，
// 覆盖 inbox_unread_stat 并回填 Redis 加速副本。
//
// 要紧的结论：
//  1. 真值只有明细表一个来源：重算不得把被修复的两层（Redis 副本 / 快照表）当输入读，
//     否则「快照被误写」这类漂移永远修不好；
//  2. 快照必须是四个分类都存在的完整覆盖（GROUP BY 不出的分类记 0），
//     不能残留上一次的脏分类，也不能只写有未读的分类；
//  3. 幂等且可重复执行：连算两次结果一致，且不依赖事务（每条语句自动提交）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// driftedEnv 布 Alice 的 3 条未读（真值），然后把快照与 Redis 副本都写成明显错误的值：
// 快照每类 9、缓存每类 7。重算必须把两层都拉回 {系统2, 互动1, 内容0, 直播0}。
func driftedEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	seedUnreadFor(t, e.st, midAlice, []int32{
		model.CategorySystem, model.CategorySystem, model.CategoryEngagement,
	})
	seedStat(t, e.st, midAlice, map[int32]int64{
		model.CategorySystem: 9, model.CategoryEngagement: 9, model.CategoryContent: 9, model.CategoryLive: 9,
	}, 1700000500)
	e.st.cache.warm(midAlice, map[int32]int64{
		model.CategorySystem: 7, model.CategoryEngagement: 7, model.CategoryContent: 7, model.CategoryLive: 7,
	})
	return e
}

func TestRecomputeUnreadGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.RecomputeUnreadReq
	}{
		{"mid 为 0", &rpc.RecomputeUnreadReq{Mid: 0}},
		{"mid 为负", &rpc.RecomputeUnreadReq{Mid: -91001}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := driftedEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(tc.in)

			wantErrIs(t, tc.name, err, model.ErrInvalidMid)
			if reply != nil {
				t.Errorf("%s：守卫拒绝仍返回响应体 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			// 漂移的副本必须原样留着：守卫阶段连读都没读。
			wantEQ(t, tc.name, "缓存里的脏快照仍在", fmt.Sprint(e.st.cache.cached(midAlice)),
				fmt.Sprint(map[int32]int64{1: 7, 2: 7, 3: 7, 4: 7}))
		})
	}
}

// 正常路径：以明细真值覆盖两层派生数据，逐字段投影响应。
func TestRecomputeUnreadRepairsBothDerivedLayersFromTruth(t *testing.T) {
	e := driftedEnv(t)
	st := e.st
	before := st.log.snapshot()

	reply, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "重算未读", err)

	wantEQ(t, "重算响应", "Total", reply.Total, int64(3))
	wantMapEQ(t, "重算响应", "ByCategory", reply.ByCategory, map[int32]int64{
		model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	// 既不是缓存的 28，也不是快照的 36：说明真值取自明细表而不是两层派生数据。
	if reply.Total == 28 || reply.Total == 36 {
		t.Fatalf("重算把派生层当成了输入：Total=%d", reply.Total)
	}
	wantMapEQ(t, "被纠正的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantMapEQ(t, "被回填的缓存副本", "cache", st.cache.cached(midAlice), map[int32]int64{
		model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})

	wantOps(t, "重算的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantAbsent(t, "重算不得读 Redis 副本", st.log, "cache.Get")
	wantAbsent(t, "重算不得读快照表", st.log, "stat.ListByMid")
	wantAbsent(t, "重算不得开事务", st.log, "conn.TransactCtx")
}

// GROUP BY 不出的分类也要有行：快照是整体覆盖，不能保留上一轮的脏分类计数。
func TestRecomputeUnreadWritesAllFourCategoriesIncludingZeros(t *testing.T) {
	e := newEnv(t)
	st := e.st
	// 只布互动未读，但先把快照布成「内容/直播还有未读」的脏状态。
	seedUnreadFor(t, st, midAlice, []int32{model.CategoryEngagement})
	seedStat(t, st, midAlice, map[int32]int64{
		model.CategoryEngagement: 1, model.CategoryContent: 42, model.CategoryLive: 42,
	}, 1700000500)
	before := st.log.snapshot()

	reply, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "只布互动的重算", err)

	wantEQ(t, "只布互动的重算", "Total", reply.Total, int64(1))
	wantEQ(t, "响应里的分类键数", "len(ByCategory)", len(reply.ByCategory), len(model.AllCategories()))
	wantMapEQ(t, "响应", "ByCategory", reply.ByCategory, map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantMapEQ(t, "被覆盖的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantAbsent(t, "脏分类不得残留在快照", st.log, "stat.DeleteByMid")
	wantOps(t, "脏快照被整体覆盖的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/1", midAlice),
	})
}

// 全已读 / 空收件箱：重算得到的是零快照，而不是「没有结果」。
func TestRecomputeUnreadOfFullyReadInboxReturnsZeroSnapshot(t *testing.T) {
	e := driftedEnv(t)
	st := e.st
	// 静默把三条未读推进为已读（布景，不算被测调用）。
	for _, row := range st.tb.rows {
		if row.Mid == midAlice {
			row.ReadState = model.ReadStateRead
		}
	}
	before := st.log.snapshot()

	reply, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "全已读的重算", err)

	wantEQ(t, "全已读的重算", "Total", reply.Total, int64(0))
	wantMapEQ(t, "全已读的快照", "stat", st.statOf(midAlice), map[int32]int64{
		model.CategorySystem: 0, model.CategoryEngagement: 0, model.CategoryContent: 0, model.CategoryLive: 0,
	})
	wantOps(t, "全已读的重算调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/0", midAlice),
	})
	// 缓存写的是零快照而不是被删掉：下一次 GetUnreadCount 直接命中 0。
	wantEQ(t, "回填后缓存仍在", "命中", func() bool {
		_, ok := st.cache.Get(context.Background(), midAlice)
		return ok
	}(), true)
}

// 可重复执行：连算两次结果一致，第二次的输入已经是第一次的产物。
func TestRecomputeUnreadIsRepeatable(t *testing.T) {
	e := driftedEnv(t)
	st := e.st

	first, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "第一次重算", err)

	before := st.log.snapshot()
	second, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "第二次重算", err)

	wantEQ(t, "第二次重算", "Total 与首次一致", second.Total, first.Total)
	wantMapEQ(t, "第二次重算", "ByCategory", second.ByCategory, first.ByCategory)
	wantOps(t, "第二次重算的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantMapEQ(t, "重复重算后的快照", "stat", st.statOf(midAlice), first.ByCategory)
}

// 只重算请求里的那个 mid：别人的漂移不在本次修复范围内，不得被顺带改写。
func TestRecomputeUnreadIsScopedToRequestedMid(t *testing.T) {
	e := driftedEnv(t)
	st := e.st
	seedUnreadFor(t, st, midBob, []int32{model.CategoryLive, model.CategoryLive})
	bobStatBefore := fmt.Sprint(st.statOf(midBob))
	before := st.log.snapshot()

	_, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
		&rpc.RecomputeUnreadReq{Mid: midAlice})
	wantNoErr(t, "限定 mid 的重算", err)

	wantEQ(t, "Bob 的快照未被波及", "stat", fmt.Sprint(st.statOf(midBob)), bobStatBefore)
	wantOps(t, "限定 mid 的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
}

func TestRecomputeUnreadPropagatesDependencyErrors(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		fail    func(st *store, err error)
		wantOps []string
	}{
		{
			name:    "明细表重算失败",
			err:     errors.New("inbox-test-count-down"),
			fail:    func(st *store, err error) { st.users.failWith("CountUnreadByCategory", err) },
			wantOps: []string{"user.CountUnreadByCategory:"},
		},
		{
			name:    "快照覆盖失败",
			err:     errors.New("inbox-test-replace-down"),
			fail:    func(st *store, err error) { st.stats.failWith("ReplaceByMid", err) },
			wantOps: []string{"user.CountUnreadByCategory:", "stat.ReplaceByMid:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := driftedEnv(t)
			st := e.st
			tc.fail(st, tc.err)
			before := st.log.snapshot()

			reply, err := NewRecomputeUnreadLogic(context.Background(), e.svcCtx).RecomputeUnread(
				&rpc.RecomputeUnreadReq{Mid: midAlice})

			wantErrIs(t, tc.name, err, tc.err)
			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantOpsPrefix(t, tc.name+" 的调用链", st.log.opsFrom(before), tc.wantOps)
			wantAbsent(t, tc.name+"：不得回填缓存", st.log, "cache.Set")
			// 修复失败必须留痕可查：漂移的两层保持原样，而不是被写成半成品。
			wantEQ(t, tc.name+"：快照仍是漂移值", "stat", fmt.Sprint(st.statOf(midAlice)),
				fmt.Sprint(map[int32]int64{1: 9, 2: 9, 3: 9, 4: 9}))
			wantEQ(t, tc.name+"：缓存仍是漂移值", "cache", fmt.Sprint(st.cache.cached(midAlice)),
				fmt.Sprint(map[int32]int64{1: 7, 2: 7, 3: 7, 4: 7}))
		})
	}
}
