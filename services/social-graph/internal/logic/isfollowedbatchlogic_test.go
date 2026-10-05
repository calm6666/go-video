package logic

// isfollowedbatchlogic_test.go 覆盖 IsFollowedBatch：守卫（含批量上限与空列表的先后次序）、
// 逐键投影、失败传播，以及「批量只走 DB、不碰缓存」的本域口径。

import (
	"context"
	"maps"
	"slices"
	"testing"

	"go-video/services/social-graph/model"
	"go-video/services/social-graph/rpc"
)

func TestIsFollowedBatchGuards(t *testing.T) {
	cases := []struct {
		name   string
		mid    int64
		owners []int64
		want   error
	}{
		{"mid 为 0", 0, []int64{3002}, model.ErrInvalidMid},
		{"mid 为负", -2, []int64{3002}, model.ErrInvalidMid},
		{"owners 101 个超限", 2001, ownersSized(101), model.ErrTooManyOwners},
		{"owners 200 个超限", 2001, ownersSized(200), model.ErrTooManyOwners},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)
			wantGuardRejected(t, e.st, c.name, c.want, func() error {
				_, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: c.mid, Owners: c.owners})
				return err
			})
		})
	}
}

// TestIsFollowedBatchEmptyOwnersShortCircuitsBeforeLimit 空列表在「超限」判定之前：
// 返回空 map 且不打库，不是错误。同时证明批量上限判定是 >100 而不是 >=100。
func TestIsFollowedBatchEmptyOwnersShortCircuitsBeforeLimit(t *testing.T) {
	cases := []struct {
		name   string
		owners []int64
	}{
		{"owners 为 nil", nil},
		{"owners 为空切片", []int64{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)

			got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: c.owners})
			wantNoErr(t, c.name, err)
			if got == nil {
				t.Fatalf("%s：响应 = nil, want 空 map 响应", c.name)
			}
			wantEQ(t, c.name, "map 长度", len(got.GetFollowing()), 0)
			wantNoCall(t, c.name, st, 0)
		})
	}

	t.Run("恰好 100 个放行", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)
		got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: ownersSized(100)})
		wantNoErr(t, "恰好 100 个", err)
		wantEQ(t, "恰好 100 个", "map 长度", len(got.GetFollowing()), 100)
		wantCount(t, "恰好 100 个", st.log, "follow.FindFollowings", 1)
	})
}

func TestIsFollowedBatchProjectsEachOwnerIndividually(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedFollow(st, &model.RelationFollow{Mid: 2001, FollowerMid: 3002, State: followNormal, Ctime: 1_650_000_100, Mtime: 1_650_000_101})
	seedFollow(st, &model.RelationFollow{Mid: 2001, FollowerMid: 3003, State: followGone, Ctime: 1_650_000_102, Mtime: 1_650_000_103})
	seedFollow(st, &model.RelationFollow{Mid: 2001, FollowerMid: 3004, State: followNormal, Ctime: 1_650_000_104, Mtime: 1_650_000_105})
	// 别人（3999）的关注关系不得串进来。
	seedFollow(st, &model.RelationFollow{Mid: 3999, FollowerMid: 3005, State: followNormal, Ctime: 1_650_000_106, Mtime: 1_650_000_107})
	l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: []int64{3002, 3003, 3004, 3005, 2001}})
	wantNoErr(t, "批量查询", err)
	out := got.GetFollowing()
	wantEQ(t, "批量查询", "键个数", len(out), 5)
	// 逐键断言（含自己、含未请求过的 3005）：只断言非空等于没断言。
	wantEQ(t, "批量查询", "3002 正常关注", out[3002], true)
	wantEQ(t, "批量查询", "3003 已取关", out[3003], false)
	wantEQ(t, "批量查询", "3004 正常关注", out[3004], true)
	wantEQ(t, "批量查询", "3005 无关系", out[3005], false)
	wantEQ(t, "批量查询", "2001 自查询", out[2001], false)
	wantInt64sEQ(t, "批量查询", "键集合", slices.Sorted(maps.Keys(out)), []int64{2001, 3002, 3003, 3004, 3005})

	// 一次 IN 查询搞定：既不查 N 次库，也不碰缓存（批量刻意绕开 Redis，见 Repository 注释）。
	wantOps(t, "批量查询链路", st.log.ops, []string{"follow.FindFollowings:2001/[3002 3003 3004 3005 2001]"})
	wantCount(t, "批量查询", st.log, "cache.", 0)
	wantCount(t, "批量查询", st.log, "stat.", 0)
}

// TestIsFollowedBatchDeduplicatesOwners 重复 owner 会被 map 合并：
// 请求 4 次同一目标只会得到 1 个键（proto 说「owner → 是否关注」，是映射不是数组）。
func TestIsFollowedBatchDeduplicatesOwners(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedFollow(st, &model.RelationFollow{Mid: 2001, FollowerMid: 3002, State: followNormal, Ctime: 1_650_000_110, Mtime: 1_650_000_111})
	l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: []int64{3002, 3002, 3002}})
	wantNoErr(t, "重复 owner", err)
	wantEQ(t, "重复 owner", "键个数", len(got.GetFollowing()), 1)
	wantEQ(t, "重复 owner", "3002", got.GetFollowing()[3002], true)
	wantOps(t, "重复 owner 链路", st.log.ops, []string{"follow.FindFollowings:2001/[3002 3002 3002]"})
}

// TestIsFollowedBatchDoesNotValidateOwnerValues owners 里没有 mid 一类的守卫：
// 0 与负数原样进 IN 列表，只会得到 false，不报错（钉住当前口径，防止「顺手加校验」无人察觉）。
func TestIsFollowedBatchDoesNotValidateOwnerValues(t *testing.T) {
	e := newEnv(t)
	st := e.st
	l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: []int64{0, -1}})
	wantNoErr(t, "非法 owner 值", err)
	wantEQ(t, "非法 owner 值", "0", got.GetFollowing()[0], false)
	wantEQ(t, "非法 owner 值", "-1", got.GetFollowing()[-1], false)
	wantCount(t, "非法 owner 值", st.log, "follow.FindFollowings:2001", 1)
}

func TestPropagatesIsFollowedBatchFailure(t *testing.T) {
	e := newEnv(t)
	st := e.st
	st.follows.failWith("FindFollowings", errStore)
	l := NewIsFollowedBatchLogic(context.Background(), e.svcCtx)

	got, err := l.IsFollowedBatch(&rpc.RelationsReq{Mid: 2001, Owners: []int64{3002, 3003}})
	wantErrIs(t, "批量查库失败", err, errStore)
	wantErrMessage(t, "批量查库失败", err, "relation_follow FindFollowings: social-graph-test: store unavailable")
	if got != nil {
		t.Fatalf("批量查库失败：响应 = %+v, want nil（不得回半截 map 伪装全 false）", got)
	}
	wantOps(t, "批量查库失败链路", st.log.ops, []string{"follow.FindFollowings:2001/[3002 3003]"})
}

// ownersSized 造 n 个互不相同的用户 ID（从 5000 起，避开用例布景用的 2001~3005）。
func ownersSized(n int) []int64 {
	out := make([]int64, 0, n)
	for i := range n {
		out = append(out, int64(5000+i))
	}
	return out
}
