package logic

// 未读计数读接口（GetUnreadCount）：钉住「Redis 加速副本 -> 快照表 -> 明细重算」三层
// 回落顺序，以及每层的回写口径。
//
// 关键结论：缓存命中绝不能回源；缓存 miss 时读快照并回填；快照缺失时从明细重算，
// 并**同时**修快照表和 Redis；Redis 故障（表现为 miss）不得让读接口报错。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/inbox/model"
	"go-video/services/inbox/rpc"
)

// 与 seedThreeUnread 的布景一致：系统 2 条、互动 1 条。
var wantThreeUnread = map[int32]int64{
	model.CategorySystem: 2, model.CategoryEngagement: 1, model.CategoryContent: 0, model.CategoryLive: 0,
}

func TestGetUnreadCountGuards(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.GetUnreadCountReq
	}{
		{"mid 为 0", &rpc.GetUnreadCountReq{Mid: 0}},
		{"mid 为负", &rpc.GetUnreadCountReq{Mid: -42}},
		{"mid 非法且要求强制重算", &rpc.GetUnreadCountReq{Mid: 0, ForceRecompute: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedThreeUnread(t, e.st)
			before := e.st.log.snapshot()

			reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(tc.req)

			if reply != nil {
				t.Fatalf("%s：拒绝后仍返回响应体 %+v", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, model.ErrInvalidMid)
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// 缓存命中：只碰 Redis 一次，不回源快照表、不查明细。
func TestGetUnreadCountServesFromCacheWhenHot(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeUnread(t, st)
	st.cache.warm(midAlice, wantThreeUnread)
	before := st.log.snapshot()

	reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice})
	wantNoErr(t, "缓存命中读未读", err)

	wantEQ(t, "缓存命中", "Total", reply.Total, int64(3))
	wantMapEQ(t, "缓存命中", "ByCategory", reply.ByCategory, wantThreeUnread)
	// 缓存副本不带更新时间：这是当前契约的一部分（Mtime 只在回源时有值）。
	wantEQ(t, "缓存命中", "Mtime", reply.Mtime, int64(0))
	wantOps(t, "缓存命中的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
}

// 缓存 miss：读快照表并把结果回填 Redis，回填值必须与返回值一致。
func TestGetUnreadCountRefillsCacheFromStatSnapshot(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeUnread(t, st)
	before := st.log.snapshot()

	reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice})
	wantNoErr(t, "缓存 miss 读未读", err)

	wantEQ(t, "快照回源", "Total", reply.Total, int64(3))
	wantMapEQ(t, "快照回源", "ByCategory", reply.ByCategory, wantThreeUnread)
	// 快照来源的 Mtime 取快照行里最新的那个（布景给的是 1700000500）。
	wantEQ(t, "快照回源", "Mtime", reply.Mtime, int64(1700000500))
	wantOps(t, "快照回源的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantMapEQ(t, "回填写进 Redis 的副本", "cache", st.cache.cached(midAlice), wantThreeUnread)

	// 第二次读直接命中，不再碰快照表。
	second := st.log.snapshot()
	_, err = NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice})
	wantNoErr(t, "第二次读未读", err)
	wantOps(t, "第二次只读 Redis", st.log.opsFrom(second), []string{
		fmt.Sprintf("cache.Get:%d", midAlice),
	})
}

// 快照表也没有记录（新用户或快照被清）：从明细重算，同时修快照表和 Redis。
func TestGetUnreadCountRecomputesWhenStatMissing(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeUnread(t, st)
	delete(st.tb.stat, midAlice)
	before := st.log.snapshot()

	reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice})
	wantNoErr(t, "快照缺失读未读", err)

	wantEQ(t, "明细重算", "Total", reply.Total, int64(3))
	wantMapEQ(t, "明细重算", "ByCategory", reply.ByCategory, wantThreeUnread)
	assertAround(t, "明细重算", "Mtime", reply.Mtime, nowUnix(), 5)
	wantOps(t, "明细重算的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantMapEQ(t, "重算修好的快照表", "stat", st.statOf(midAlice), wantThreeUnread)
	wantMapEQ(t, "重算回填的 Redis", "cache", st.cache.cached(midAlice), wantThreeUnread)
}

// force_recompute 是客户端发现计数异常时的自助修复入口：
// 必须跳过 Redis 与快照两层，直接以明细真值为准，并把两层都纠正过来。
func TestGetUnreadCountForceRecomputeRepairsBothLayers(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeUnread(t, st)
	drifted := map[int32]int64{
		model.CategorySystem: 9, model.CategoryEngagement: 9, model.CategoryContent: 9, model.CategoryLive: 9,
	}
	st.cache.warm(midAlice, drifted)
	st.tb.stat[midAlice] = map[int32]int64{1: 7, 2: 7, 3: 7, 4: 7}
	before := st.log.snapshot()

	reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice, ForceRecompute: true})
	wantNoErr(t, "强制重算", err)

	wantEQ(t, "强制重算", "Total 取明细真值", reply.Total, int64(3))
	wantMapEQ(t, "强制重算", "ByCategory", reply.ByCategory, wantThreeUnread)
	wantOps(t, "强制重算跳过前两层", st.log.opsFrom(before), []string{
		fmt.Sprintf("user.CountUnreadByCategory:%d", midAlice),
		fmt.Sprintf("stat.ReplaceByMid:%d/conn", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
	wantAbsent(t, "强制重算不得读 Redis 副本", st.log, "cache.Get")
	wantAbsent(t, "强制重算不得读快照表", st.log, "stat.ListByMid")
	wantMapEQ(t, "漂移的快照表被纠正", "stat", st.statOf(midAlice), wantThreeUnread)
	wantMapEQ(t, "漂移的 Redis 副本被纠正", "cache", st.cache.cached(midAlice), wantThreeUnread)
}

// Redis 不可用（UnreadCache.Get 表现为 miss）：读接口必须降级回源而不是报错。
func TestGetUnreadCountDegradesWhenCacheUnavailable(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeUnread(t, st)
	st.cache.unavailable = true
	before := st.log.snapshot()

	reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
		&rpc.GetUnreadCountReq{Mid: midAlice})
	wantNoErr(t, "缓存不可用时读未读", err)
	wantEQ(t, "缓存不可用", "Total", reply.Total, int64(3))
	wantMapEQ(t, "缓存不可用", "ByCategory", reply.ByCategory, wantThreeUnread)
	wantOps(t, "缓存不可用时的调用链", st.log.opsFrom(before), []string{
		fmt.Sprintf("cache.Get:%d", midAlice),
		fmt.Sprintf("stat.ListByMid:%d", midAlice),
		fmt.Sprintf("cache.Set:%d/3", midAlice),
	})
}

// 写接口返回的是权威计数，因此未读读失败必须报错，不能静默降级为 0。
func TestGetUnreadCountPropagatesDependencyErrors(t *testing.T) {
	errInjected := errors.New("inbox-test-unread-read-down")
	cases := []struct {
		name    string
		fail    func(st *store)
		wantOps []string
	}{
		{
			name:    "快照表读失败",
			fail:    func(st *store) { st.stats.failWith("ListByMid", errInjected) },
			wantOps: []string{"cache.Get:", "stat.ListByMid:"},
		},
		{
			name:    "明细重算读失败",
			fail:    func(st *store) { st.users.failWith("CountUnreadByCategory", errInjected) },
			wantOps: []string{"cache.Get:", "stat.ListByMid:", "user.CountUnreadByCategory:"},
		},
		{
			name:    "快照回写失败",
			fail:    func(st *store) { st.stats.failWith("ReplaceByMid", errInjected) },
			wantOps: []string{"cache.Get:", "stat.ListByMid:", "user.CountUnreadByCategory:", "stat.ReplaceByMid:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedThreeUnread(t, st)
			delete(st.tb.stat, midAlice) // 逼出「回源重算」这条最长链路
			tc.fail(st)
			before := st.log.snapshot()

			reply, err := NewGetUnreadCountLogic(context.Background(), e.svcCtx).GetUnreadCount(
				&rpc.GetUnreadCountReq{Mid: midAlice})

			if reply != nil {
				t.Fatalf("%s：下游失败仍返回响应体 %+v", tc.name, reply)
			}
			wantErrIs(t, tc.name, err, errInjected)
			got := st.log.opsFrom(before)
			if len(got) != len(tc.wantOps) {
				t.Fatalf("%s：调用序列 = [%s], want %d 项", tc.name, fmt.Sprint(got), len(tc.wantOps))
			}
			for i, prefix := range tc.wantOps {
				if len(got[i]) < len(prefix) || got[i][:len(prefix)] != prefix {
					t.Errorf("%s：第 %d 次调用 = %s, want 前缀 %s", tc.name, i+1, got[i], prefix)
				}
			}
			// 失败不得留下半截副本：Redis 里不能出现来源不明的计数。
			if st.cache.cached(midAlice) != nil {
				t.Errorf("%s：失败后 Redis 仍被写成 %v", tc.name, st.cache.cached(midAlice))
			}
		})
	}
}
