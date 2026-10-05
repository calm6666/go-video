package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestItemLikesRejectsGuardsBeforeTouchingDeps business → message_id → ps；
// pn<=0 不是错误而是被归一成 1（表里单独一条合法用例证明归一确实生效）。
func TestItemLikesRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ItemLikesReq
		want error
	}{
		{"business 空", &rpc.ItemLikesReq{Business: "", MessageId: 101, Ps: 20}, model.ErrInvalidBusiness},
		{"message_id 为 0", &rpc.ItemLikesReq{Business: "archive", MessageId: 0, Ps: 20}, model.ErrInvalidMessage},
		{"message_id 为负", &rpc.ItemLikesReq{Business: "archive", MessageId: -101, Ps: 20}, model.ErrInvalidMessage},
		{"ps 为 0", &rpc.ItemLikesReq{Business: "archive", MessageId: 101, Ps: 0}, model.ErrPsTooLarge},
		{"ps 为负", &rpc.ItemLikesReq{Business: "archive", MessageId: 101, Ps: -1}, model.ErrPsTooLarge},
		{"ps 51 超限", &rpc.ItemLikesReq{Business: "archive", MessageId: 101, Ps: 51}, model.ErrPsTooLarge},
		{"business 优先于 message_id", &rpc.ItemLikesReq{Business: "", MessageId: 0, Ps: 20}, model.ErrInvalidBusiness},
		{"message_id 优先于 ps", &rpc.ItemLikesReq{Business: "archive", MessageId: 0, Ps: 999}, model.ErrInvalidMessage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedLike(st, "archive", 10, 88, 1, 101, model.LikeStateLike, 1_700_000_500)
			before := st.log.snapshot()
			got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).ItemLikes(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}
}

// TestItemLikesProjectsMidAndCtime 逐字段投影 + 排序口径。
// 注意：logic 的注释写「按点赞时间倒序」，但 model 的 SQL 是 ORDER BY mid ASC，
// 本用例锁的是**实际**行为（mid 升序），注释与实现的偏差登记在 README。
func TestItemLikesProjectsMidAndCtime(t *testing.T) {
	st := newStore()
	// mid 乱序布景，ctime 也不同，确保排序只可能来自 mid ASC。
	seedLike(st, "archive", 30, 88, 1, 101, model.LikeStateLike, 1_700_000_300)
	seedLike(st, "archive", 10, 88, 1, 101, model.LikeStateLike, 1_700_000_100)
	seedLike(st, "archive", 20, 88, 1, 101, model.LikeStateLike, 1_700_000_200)
	// 干扰行：已取消、别的 message_id、别的 origin_id
	seedLike(st, "archive", 40, 88, 1, 101, model.LikeStateCancel, 1_700_000_400)
	seedLike(st, "archive", 50, 88, 1, 102, model.LikeStateLike, 1_700_000_500)
	seedLike(st, "archive", 60, 88, 2, 101, model.LikeStateLike, 1_700_000_600)

	got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Ps: 50})
	wantNoErr(t, "ItemLikes", err)
	wantOps(t, "ItemLikes", st.log.opsFrom(0), []string{"like.ListByItem:archive:1:101:0:1/50"})
	wantEQ(t, "ItemLikes", "点赞人数", len(got.Users), 3)

	wantStringsEQ(t, "ItemLikes", "mid 顺序", []string{
		itoa(got.Users[0].Mid), itoa(got.Users[1].Mid), itoa(got.Users[2].Mid),
	}, []string{"10", "20", "30"})
	for i, want := range []struct {
		mid  int64
		time int64
	}{{10, 1_700_000_100}, {20, 1_700_000_200}, {30, 1_700_000_300}} {
		wantEQ(t, "ItemLikes 第"+itoa(int64(i+1))+"人", "Mid", got.Users[i].Mid, want.mid)
		wantEQ(t, "ItemLikes 第"+itoa(int64(i+1))+"人", "Time（取 ctime）", got.Users[i].Time, want.time)
	}
	// 取消过的人不出现。
	for _, u := range got.Users {
		if u.Mid == 40 {
			t.Errorf("ItemLikes：已取消的 mid=40 仍出现在列表里")
		}
	}
}

// TestItemLikesLastMidSkipsPreviousPage last_mid 用于翻页去重（WHERE mid > last_mid）。
// 同时锁定 total 被 logic 丢弃：ItemLikesReply 里没有 total 字段，
// 所以 last_mid 过滤后客户端拿不到「还剩多少人」。
func TestItemLikesLastMidSkipsPreviousPage(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 10, 88, 1, 101, model.LikeStateLike, 1_700_000_100)
	seedLike(st, "archive", 20, 88, 1, 101, model.LikeStateLike, 1_700_000_200)
	seedLike(st, "archive", 30, 88, 1, 101, model.LikeStateLike, 1_700_000_300)

	got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, LastMid: 20, Ps: 50})
	wantNoErr(t, "last_mid 翻页", err)
	wantEQ(t, "last_mid 翻页", "剩余人数", len(got.Users), 1)
	wantEQ(t, "last_mid 翻页", "Mid", got.Users[0].Mid, int64(30))
	wantOps(t, "last_mid 翻页", st.log.opsFrom(0), []string{"like.ListByItem:archive:1:101:20:1/50"})
}

// TestItemLikesPaginatesAndClampsPn ps 合法、pn 越界与 pn<=0 的两种表现。
func TestItemLikesPaginatesAndClampsPn(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 10, 88, 1, 101, model.LikeStateLike, 1_700_000_100)
	seedLike(st, "archive", 20, 88, 1, 101, model.LikeStateLike, 1_700_000_200)
	seedLike(st, "archive", 30, 88, 1, 101, model.LikeStateLike, 1_700_000_300)

	// pn 缺省（0）被归一成 1：这条断言直接盯住归一逻辑，否则 LIMIT/OFFSET 会算成负数。
	got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Pn: 0, Ps: 2})
	wantNoErr(t, "pn 归一", err)
	wantEQ(t, "pn 归一", "首页人数", len(got.Users), 2)
	wantEQ(t, "pn 归一", "第一人", got.Users[0].Mid, int64(10))

	// 第二页只剩 1 人。
	page2, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Pn: 2, Ps: 2})
	wantNoErr(t, "第二页", err)
	wantEQ(t, "第二页", "人数", len(page2.Users), 1)
	wantEQ(t, "第二页", "Mid", page2.Users[0].Mid, int64(30))

	// 超出范围的页返回空列表（非 nil 响应、非错误）。
	empty, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "人数", len(empty.Users), 0)
}

// TestItemLikesEmptyObjectIssuesSingleCount 一个点赞人都没有时，model 的 COUNT 先返回 0
// 并**省略第二条 SELECT**；logic 返回空列表而不是错误。
func TestItemLikesEmptyObjectIssuesSingleCount(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 10, 88, 9, 999, model.LikeStateLike, 1_700_000_100)

	got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Ps: 20})
	wantNoErr(t, "无人点赞", err)
	if got == nil {
		t.Fatalf("ItemLikes() 响应 = nil")
	}
	wantEQ(t, "无人点赞", "人数", len(got.Users), 0)
	wantOps(t, "无人点赞", st.log.opsFrom(0), []string{"like.ListByItem:archive:1:101:0:1/20"})
}

// TestItemLikesPropagatesModelFailure 唯一依赖失败必须透出，不能返回空列表当「没人赞过」。
func TestItemLikesPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedLike(st, "archive", 10, 88, 1, 101, model.LikeStateLike, 1_700_000_100)
	st.like.failWith("ListByItem", errBoom)

	got, err := NewItemLikesLogic(context.Background(), newTestSvc(st)).
		ItemLikes(&rpc.ItemLikesReq{Business: "archive", OriginId: 1, MessageId: 101, Ps: 20})
	wantFail(t, "ListByItem 失败", got, err, errBoom)
	wantOps(t, "ListByItem 失败后的调用", st.log.opsFrom(0), []string{"like.ListByItem:archive:1:101:0:1/20"})
}
