package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestStatsRejectsGuardsBeforeTouchingDeps 三条守卫的次序：business → 空列表短路 → 超限。
// 空列表不是错误而是短路返回，所以单独列在成功列里；三者都必须**一次依赖都不碰**。
func TestStatsRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	over := make([]int64, 101)
	for i := range over {
		over[i] = int64(1000 + i)
	}
	cases := []struct {
		name    string
		in      *rpc.StatsReq
		wantErr error // 非 nil 表示必须报错；nil 表示短路成功
	}{
		{"business 空", &rpc.StatsReq{Business: "", OriginId: 1, MessageIds: []int64{101}, Mid: 7}, model.ErrInvalidBusiness},
		{"101 个 id 超限", &rpc.StatsReq{Business: likeBiz, OriginId: 1, MessageIds: over, Mid: 7}, model.ErrTooManyMessageIDs},
		{"business 优先于超限", &rpc.StatsReq{Business: "", OriginId: 1, MessageIds: over, Mid: 7}, model.ErrInvalidBusiness},
		{"id 列表为 nil 短路成功", &rpc.StatsReq{Business: likeBiz, OriginId: 1, MessageIds: nil, Mid: 7}, nil},
		{"id 列表为空切片短路成功", &rpc.StatsReq{Business: likeBiz, OriginId: 1, MessageIds: []int64{}, Mid: 7}, nil},
		{"空列表优先于超限判定", &rpc.StatsReq{Business: likeBiz, OriginId: 1, MessageIds: []int64{}, Mid: 0}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedStat(st, likeBiz, likeOrigin, likeMessage, 5, 2, 0, 0)
			seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
			before := st.log.snapshot()

			got, err := NewStatsLogic(context.Background(), newTestSvc(st)).Stats(tc.in)
			if tc.wantErr != nil {
				wantFail(t, tc.name, got, err, tc.wantErr)
			} else {
				wantNoErr(t, tc.name, err)
				if got == nil || got.Stats == nil {
					t.Fatalf("%s：短路也必须回非 nil 的 stats map，客户端会直接遍历", tc.name)
				}
				wantEQ(t, tc.name, "map 长度", len(got.Stats), 0)
			}
			wantNoCall(t, tc.name, st, before)
		})
	}

	t.Run("刚好 100 个不超限", func(t *testing.T) {
		st := newStore()
		ids := make([]int64, 100)
		for i := range ids {
			ids[i] = int64(1000 + i)
		}
		got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
			Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: ids, Mid: likeMid})
		wantNoErr(t, "100 个 id", err)
		wantEQ(t, "100 个 id", "无计数行 ⇒ 空 map", len(got.Stats), 0)
		wantEQ(t, "100 个 id", "批量只发一对查询（计数+状态）", len(st.log.opsFrom(0)), 2)
		wantCount(t, "100 个 id", st.log, "stat.FindMany:", 1)
	})
}

// TestStatsProjectsCountsAndUserState 逐字段投影：计数取 thumbup_stat，
// 用户态取 thumbup_like.state 再经 stateToRPC 映射，OriginId 是**请求回显**而不是行里的值。
func TestStatsProjectsCountsAndUserState(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
	seedStat(st, likeBiz, likeOrigin, 102, 17, 8, 0, 0)
	// 无计数行的对象（103）在下面的用例里单独处理。
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_500)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 102, model.LikeStateDislike, 1_700_000_600)
	// 干扰行：别人的点赞、别的 business 的计数。
	seedLike(st, likeBiz, 999, likeUpMid, likeOrigin, 101, model.LikeStateDislike, 1_700_000_700)
	seedStat(st, "dynamic", likeOrigin, 101, 77, 88, 0, 0)

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101, 102}, Mid: likeMid})
	wantNoErr(t, "Stats", err)
	wantOps(t, "Stats", st.log.opsFrom(0), []string{
		"stat.FindMany:archive:1:101|102",
		"like.FindStates:archive:7:101|102",
	})
	wantEQ(t, "Stats", "条数", len(got.Stats), 2)

	s101 := got.Stats[101]
	wantEQ(t, "Stats 101", "OriginId（回显请求值）", s101.OriginId, likeOrigin)
	wantEQ(t, "Stats 101", "MessageId（回显 key）", s101.MessageId, int64(101))
	wantEQ(t, "Stats 101", "LikeNumber", s101.LikeNumber, int64(5))
	wantEQ(t, "Stats 101", "DislikeNumber", s101.DislikeNumber, int64(2))
	wantEQ(t, "Stats 101", "LikeState", s101.LikeState, rpc.LikeState_STATE_LIKE)

	s102 := got.Stats[102]
	wantEQ(t, "Stats 102", "LikeNumber（不与 101 串）", s102.LikeNumber, int64(17))
	wantEQ(t, "Stats 102", "DislikeNumber", s102.DislikeNumber, int64(8))
	wantEQ(t, "Stats 102", "LikeState（点踩是独立态，不是取消）", s102.LikeState, rpc.LikeState_STATE_DISLIKE)
}

// TestStatsCancelledLikeIsUnspecified 取消过的历史行（state=0）报成 STATE_UNSPECIFIED，
// 计数照常返回——与 HasLike 同一口径，两处都得能区分「点踩」与「取消」。
func TestStatsCancelledLikeIsUnspecified(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 3, 1, 0, 0)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateCancel, 1_700_000_500)

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101}, Mid: likeMid})
	wantNoErr(t, "取消过的对象", err)
	wantEQ(t, "取消过的对象", "LikeState", got.Stats[101].LikeState, rpc.LikeState_STATE_UNSPECIFIED)
	wantEQ(t, "取消过的对象", "LikeNumber 不受影响", got.Stats[101].LikeNumber, int64(3))
}

// TestStatsDropsObjectsWithoutStatRow 缺陷 #11 的现象固化：
// 响应 map 是按 thumbup_stat 的命中行构造的，所以**没有计数行的对象整条消失**，
// 即使用户已经给它点过赞（103），用户态也一起丢了。
// 客户端因此无法区分「还没有人点赞的新投稿」与「不存在的投稿」，
// 而且新投稿的点赞按钮状态会退化成未点赞。
func TestStatsDropsObjectsWithoutStatRow(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_500)
	// 103：有人点赞（含本人），但 thumbup_stat 里还没有行。
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 103, model.LikeStateLike, 1_700_000_555)

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101, 103, 404}, Mid: likeMid})
	wantNoErr(t, "无计数行", err)
	wantOps(t, "无计数行", st.log.opsFrom(0), []string{
		"stat.FindMany:archive:1:101|103|404",
		"like.FindStates:archive:7:101|103|404",
	})
	wantEQ(t, "缺陷 #11", "返回条数（只 1 条，不是 3 条）", len(got.Stats), 1)
	_, has103 := got.Stats[103]
	wantEQ(t, "缺陷 #11", "已点赞的 103 也消失", has103, false)
	_, has404 := got.Stats[404]
	wantEQ(t, "缺陷 #11", "不存在的 404 不出现", has404, false)
}

// TestStatsAnonymousSkipsStateQuery mid<=0 时不发第二条查询（省一次 IN 扫描），
// 而且必须**报成未指定**，不能因为 map 为空就误判成取消或已点赞。
func TestStatsAnonymousSkipsStateQuery(t *testing.T) {
	for _, mid := range []int64{0, -7} {
		st := newStore()
		seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
		// 库里确实有 7 号用户的点赞行，但匿名请求不得读到。
		seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_500)

		got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
			Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101}, Mid: mid})
		label := "mid=" + itoa(mid)
		wantNoErr(t, label, err)
		wantOps(t, label, st.log.opsFrom(0), []string{"stat.FindMany:archive:1:101"})
		wantCount(t, label, st.log, "like.FindStates:", 0)
		wantEQ(t, label, "LikeNumber 仍返回", got.Stats[101].LikeNumber, int64(5))
		wantEQ(t, label, "LikeState", got.Stats[101].LikeState, rpc.LikeState_STATE_UNSPECIFIED)
	}
}

// TestStatsOriginIdIsSingleValued origin_id 是整请求一个值，不能按对象区分。
// 传两个 origin 共用同一 message_id 时，只有请求的那个 origin 下的计数会被查出来，
// 但回显的 OriginId 恒等于请求值——跨 origin 的同一个 message_id 会串。
func TestStatsOriginIdIsSingleValued(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, 1, 101, 5, 2, 0, 0)
	seedStat(st, likeBiz, 2, 101, 77, 88, 0, 0) // 同 message_id、不同 origin

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: 2, MessageIds: []int64{101}})
	wantNoErr(t, "跨 origin", err)
	wantOps(t, "跨 origin", st.log.opsFrom(0), []string{"stat.FindMany:archive:2:101"})
	wantEQ(t, "跨 origin", "取的是 origin=2 的计数", got.Stats[101].LikeNumber, int64(77))
	wantEQ(t, "跨 origin", "OriginId 回显", got.Stats[101].OriginId, int64(2))
}

// TestStatsPropagatesCountQueryFailure 计数查询失败必须整体失败，
// 且不得继续发用户态查询（不能返回「计数全 0」的假成功，客户端会把点赞数清零）。
func TestStatsPropagatesCountQueryFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
	st.stat.failWith("FindMany", errBoom)

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101}, Mid: likeMid})
	wantFail(t, "FindMany 失败", got, err, errBoom)
	wantOps(t, "FindMany 失败后的调用", st.log.opsFrom(0), []string{"stat.FindMany:archive:1:101"})
	wantCount(t, "FindMany 失败", st.log, "like.FindStates:", 0)
}

// TestStatsPropagatesStateQueryFailure 计数已经查出来了，用户态查询失败仍要整体失败，
// 不能返回「计数对、状态全是未点赞」的半截结果。
func TestStatsPropagatesStateQueryFailure(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
	st.like.failWith("FindStates", errBoom)

	got, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101}, Mid: likeMid})
	wantFail(t, "FindStates 失败", got, err, errBoom)
	wantOps(t, "FindStates 失败后的调用", st.log.opsFrom(0), []string{
		"stat.FindMany:archive:1:101",
		"like.FindStates:archive:7:101",
	})
}

// TestStatsIsPureRead 读接口不得有任何副作用：不写关系行、不动计数、
// 不碰收藏/分享域，也不写缓存（Stats 没有缓存层，写了就是多余的一致性风险）。
// 这条同时是 AGENTS.md §5 的归属检查。
func TestStatsIsPureRead(t *testing.T) {
	st := newStore()
	seedStat(st, likeBiz, likeOrigin, 101, 5, 2, 0, 0)
	seedFavItem(st, likeMid, 101, 33, 2, 11, 0)
	before := st.log.snapshot()

	_, err := NewStatsLogic(context.Background(), newTestSvc(st)).
		Stats(&rpc.StatsReq{Business: likeBiz, OriginId: likeOrigin, MessageIds: []int64{101, 102}, Mid: likeMid})
	wantNoErr(t, "Stats", err)

	for _, prefix := range []string{"like.Upsert", "stat.Incr", "stat.UpdateChange", "favItem.", "folder.", "share.", "cache."} {
		wantCount(t, "只读检查", st.log, prefix, 0)
	}
	wantEQ(t, "只读检查", "调用只有两条 SELECT", st.log.snapshot()-before, 2)
}
