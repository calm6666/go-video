package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestHasLikeRejectsGuardsBeforeTouchingDeps 四档入参检查的顺序：business → mid → 空列表 → 超限。
// 「空列表」不是错误而是直接返回空 map，但必须**不触库**（否则客户端传空数组就能打 DB）。
func TestHasLikeRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(1000 + i)
	}
	cases := []struct {
		name string
		in   *rpc.HasLikeReq
		want error
	}{
		{name: "business 空", in: &rpc.HasLikeReq{Business: "", Mid: 7, MessageIds: []int64{101}}, want: model.ErrInvalidBusiness},
		{name: "mid 为 0", in: &rpc.HasLikeReq{Business: "archive", Mid: 0, MessageIds: []int64{101}}, want: model.ErrInvalidMid},
		{name: "mid 为负", in: &rpc.HasLikeReq{Business: "archive", Mid: -7, MessageIds: []int64{101}}, want: model.ErrInvalidMid},
		{name: "business 优先于 mid", in: &rpc.HasLikeReq{Business: "", Mid: 0}, want: model.ErrInvalidBusiness},
		{name: "101 个 id 超限", in: &rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: ids}, want: model.ErrTooManyMessageIDs},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedLike(st, "archive", 7, 88, 1, 101, model.LikeStateLike, 1_700_000_500)
			before := st.log.snapshot()
			got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).HasLike(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}

	t.Run("空列表短路返回空 map", func(t *testing.T) {
		st := newStore()
		before := st.log.snapshot()
		got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).
			HasLike(&rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: nil})
		wantNoErr(t, "空列表", err)
		if got == nil {
			t.Fatalf("空列表应返回空 map 而不是 nil")
		}
		wantEQ(t, "空列表", "states 长度", len(got.States), 0)
		wantNoCall(t, "空列表", st, before)
	})

	t.Run("刚好 100 个不超限", func(t *testing.T) {
		st := newStore()
		ids := make([]int64, 100)
		for i := range ids {
			ids[i] = int64(1000 + i)
		}
		got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).
			HasLike(&rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: ids})
		wantNoErr(t, "100 个 id", err)
		wantEQ(t, "100 个 id", "states 长度", len(got.States), 0)
		wantCount(t, "100 个 id", st.log, "like.FindStates:", 1)
	})
}

// TestHasLikeProjectsStatesFieldByField 逐字段投影，并锁定三件事：
//  1. 没有记录的对象**不出现在 map 里**（不是返回一个 state=0 的假条目）；
//  2. 已取消（state=0）的历史行照样返回，报成 STATE_UNSPECIFIED + 原 ctime；
//  3. Time 取的是 ctime（首次点赞时间），不是 mtime。
func TestHasLikeProjectsStatesFieldByField(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 7, 88, 1, 101, model.LikeStateLike, 1_700_000_500)
	seedLike(st, "archive", 7, 88, 1, 102, model.LikeStateDislike, 1_700_000_600)
	seedLike(st, "archive", 7, 88, 1, 103, model.LikeStateCancel, 1_700_000_700)
	// 别人的记录、别的 business 都不该被读到
	seedLike(st, "archive", 99, 88, 1, 101, model.LikeStateLike, 1_700_000_800)
	seedLike(st, "dynamic", 7, 88, 1, 101, model.LikeStateLike, 1_700_000_900)

	got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).
		HasLike(&rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: []int64{101, 102, 103, 404}})
	wantNoErr(t, "HasLike", err)
	wantOps(t, "HasLike", st.log.opsFrom(0), []string{"like.FindStates:archive:7:101|102|103|404"})
	wantEQ(t, "HasLike", "状态条数", len(got.States), 3)

	_, absent := got.States[404]
	wantEQ(t, "HasLike", "无记录对象不出现在 map 里", absent, false)

	liked := got.States[101]
	wantEQ(t, "HasLike", "101.Mid", liked.Mid, int64(7))
	wantEQ(t, "HasLike", "101.Time（取 ctime）", liked.Time, int64(1_700_000_500))
	wantEQ(t, "HasLike", "101.State", liked.State, rpc.LikeState_STATE_LIKE)

	disliked := got.States[102]
	wantEQ(t, "HasLike", "102.Mid", disliked.Mid, int64(7))
	wantEQ(t, "HasLike", "102.Time", disliked.Time, int64(1_700_000_600))
	wantEQ(t, "HasLike", "102.State", disliked.State, rpc.LikeState_STATE_DISLIKE)

	cancelled := got.States[103]
	wantEQ(t, "HasLike", "103.Mid", cancelled.Mid, int64(7))
	wantEQ(t, "HasLike", "103.Time（取消后 ctime 仍是首次时间）", cancelled.Time, int64(1_700_000_700))
	wantEQ(t, "HasLike", "103.State", cancelled.State, rpc.LikeState_STATE_UNSPECIFIED)
}

// TestHasLikeDeduplicatesMessageIDs IN (?) 传重复 id 时结果 map 只有一条，
// 且只发一次查询（不是按 id 逐个查）。
func TestHasLikeDeduplicatesMessageIDs(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 7, 88, 1, 101, model.LikeStateLike, 1_700_000_500)

	got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).
		HasLike(&rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: []int64{101, 101, 101}})
	wantNoErr(t, "重复 id", err)
	wantEQ(t, "重复 id", "map 条数", len(got.States), 1)
	wantOps(t, "重复 id", st.log.opsFrom(0), []string{"like.FindStates:archive:7:101|101|101"})
}

// TestHasLikePropagatesModelFailure 唯一依赖失败必须透出，且不能返回「全 false」的
// 假成功——客户端会据此把已点赞的按钮点亮成未点赞。
func TestHasLikePropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 7, 88, 1, 101, model.LikeStateLike, 1_700_000_500)
	st.like.failWith("FindStates", errBoom)

	got, err := NewHasLikeLogic(context.Background(), newTestSvc(st)).
		HasLike(&rpc.HasLikeReq{Business: "archive", Mid: 7, MessageIds: []int64{101}})
	wantFail(t, "FindStates 失败", got, err, errBoom)
	wantOps(t, "FindStates 失败后的调用", st.log.opsFrom(0), []string{"like.FindStates:archive:7:101"})
}

// TestStateToRPCCoversUnknownRawValue stateToRPC 的 default 分支：库里若有脏值
// （TINYINT 允许 3、-1）必须落到 STATE_UNSPECIFIED，而不是 panic 或误报已点赞。
func TestStateToRPCCoversUnknownRawValue(t *testing.T) {
	cases := []struct {
		raw  int32
		want rpc.LikeState
	}{
		{0, rpc.LikeState_STATE_UNSPECIFIED},
		{1, rpc.LikeState_STATE_LIKE},
		{2, rpc.LikeState_STATE_DISLIKE},
		{3, rpc.LikeState_STATE_UNSPECIFIED},
		{-1, rpc.LikeState_STATE_UNSPECIFIED},
		{127, rpc.LikeState_STATE_UNSPECIFIED},
	}
	for _, tc := range cases {
		wantEQ(t, "stateToRPC("+itoa(int64(tc.raw))+")", "映射结果", stateToRPC(tc.raw), tc.want)
	}
}
