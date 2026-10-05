package logic

import (
	"context"
	"testing"

	"go-video/services/engagement/model"
	"go-video/services/engagement/rpc"
)

// TestUserLikesRejectsGuardsBeforeTouchingDeps business → mid → ps 三条守卫。
// pn<=0 不是错误而是被归一成 1，所以它单独出现在成功列里（表尾那条子用例）。
func TestUserLikesRejectsGuardsBeforeTouchingDeps(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UserLikesReq
		want error
	}{
		{"business 空", &rpc.UserLikesReq{Business: "", Mid: likeMid, Ps: 20}, model.ErrInvalidBusiness},
		{"mid 为 0", &rpc.UserLikesReq{Business: likeBiz, Mid: 0, Ps: 20}, model.ErrInvalidMid},
		{"mid 为负", &rpc.UserLikesReq{Business: likeBiz, Mid: -likeMid, Ps: 20}, model.ErrInvalidMid},
		{"ps 为 0", &rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 0}, model.ErrPsTooLarge},
		{"ps 为负", &rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: -1}, model.ErrPsTooLarge},
		{"ps 51 超限", &rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 51}, model.ErrPsTooLarge},
		{"business 优先于 mid", &rpc.UserLikesReq{Business: "", Mid: 0, Ps: 20}, model.ErrInvalidBusiness},
		{"mid 优先于 ps", &rpc.UserLikesReq{Business: likeBiz, Mid: 0, Ps: 999}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
			before := st.log.snapshot()
			got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).UserLikes(tc.in)
			wantFail(t, tc.name, got, err, tc.want)
			wantNoCall(t, tc.name, st, before)
		})
	}

	// pn 归一：-5 与 0 都按第 1 页处理（守卫发生在触库之前，所以库里的 key 一定是 1/ps）。
	for _, pn := range []int32{0, -5} {
		st := newStore()
		seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, likeMessage, model.LikeStateLike, 1_700_000_500)
		got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
			UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Pn: pn, Ps: 20})
		wantNoErr(t, "pn 归一", err)
		wantOps(t, "pn="+itoa(int64(pn)), st.log.opsFrom(0), []string{"like.ListByMid:archive:7:1/20"})
		wantEQ(t, "pn 归一", "总数", got.Total, int32(1))
	}
}

// TestUserLikesProjectsMessageIdAndCtime 逐字段投影：
// ItemRecord 只有 message_id 与 time 两位，time 取的是 **ctime**（首次点赞时间），
// 取消再点赞后被 Upsert 覆盖的 mtime 不会露出来。
// 同时锁定排序是 ctime DESC（与注释一致），且乱序布景必须被重排。
func TestUserLikesProjectsMessageIdAndCtime(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_101)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 102, model.LikeStateLike, 1_700_000_102)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 103, model.LikeStateLike, 1_700_000_103)
	// 同一行的 mtime 与 ctime 不同（取消后重新点赞的痕迹）：投影必须选 ctime。
	st.like.rows[likeKey{likeBiz, likeMid, 102}].Mtime = 1_700_999_999

	// 干扰行：别人的、别的 business、别的 origin。
	seedLike(st, likeBiz, 999, likeUpMid, likeOrigin, 201, model.LikeStateLike, 1_700_000_900)
	seedLike(st, "dynamic", likeMid, likeUpMid, likeOrigin, 202, model.LikeStateLike, 1_700_000_901)

	got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
		UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 50})
	wantNoErr(t, "UserLikes", err)
	wantOps(t, "UserLikes", st.log.opsFrom(0), []string{"like.ListByMid:archive:7:1/50"})
	wantEQ(t, "UserLikes", "Total", got.Total, int32(3))
	wantEQ(t, "UserLikes", "条数", len(got.Items), 3)

	wantStringsEQ(t, "UserLikes", "message_id 顺序（ctime 倒序）", []string{
		itoa(got.Items[0].MessageId), itoa(got.Items[1].MessageId), itoa(got.Items[2].MessageId),
	}, []string{"103", "102", "101"})
	wantEQ(t, "UserLikes", "102.Time 取 ctime 而非 mtime", got.Items[1].Time, int64(1_700_000_102))
	wantEQ(t, "UserLikes", "103.Time", got.Items[0].Time, int64(1_700_000_103))
	wantEQ(t, "UserLikes", "101.Time", got.Items[2].Time, int64(1_700_000_101))
}

// TestUserLikesCountsOnlyLikedState 域内不变量：列表与 total 都只算 state=1。
// 点踩（2）与取消（0）的历史行既不出现也不计数 —— 否则「我的点赞」里会混进点踩，
// 而 total 也会和分页结果不一致。
func TestUserLikesCountsOnlyLikedState(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_101)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 102, model.LikeStateDislike, 1_700_000_102)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 103, model.LikeStateCancel, 1_700_000_103)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 104, 7, 1_700_000_104) // 库里不该有的脏值

	got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
		UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 50})
	wantNoErr(t, "只算点赞态", err)
	wantEQ(t, "只算点赞态", "Total", got.Total, int32(1))
	wantEQ(t, "只算点赞态", "条数", len(got.Items), 1)
	wantEQ(t, "只算点赞态", "唯一一条", got.Items[0].MessageId, int64(101))
}

// TestUserLikesTotalIsFullCountAcrossPages total 是**全量** COUNT，不随页码变化：
// 客户端据此算总页数，若实现把 total 改成「本页条数」分页就会死循环。
// 越界的页返回空列表但仍带回全量 total（不是错误）。
func TestUserLikesTotalIsFullCountAcrossPages(t *testing.T) {
	st := newStore()
	for i := range 5 {
		msg := int64(101 + i)
		seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, msg, model.LikeStateLike, int64(1_700_000_100+i*10))
	}
	l := NewUserLikesLogic(context.Background(), newTestSvc(st))

	page1, err := l.UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Pn: 1, Ps: 2})
	wantNoErr(t, "第一页", err)
	wantEQ(t, "第一页", "Total", page1.Total, int32(5))
	wantEQ(t, "第一页", "条数", len(page1.Items), 2)
	wantEQ(t, "第一页", "ctime 最新的一条排第一", page1.Items[0].MessageId, int64(105))

	page3, err := l.UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Pn: 3, Ps: 2})
	wantNoErr(t, "第三页", err)
	wantEQ(t, "第三页", "Total 不变", page3.Total, int32(5))
	wantEQ(t, "第三页", "只剩一条", len(page3.Items), 1)
	wantEQ(t, "第三页", "最后一条", page3.Items[0].MessageId, int64(101))

	// 越界页：空列表 + 全量 total，且响应体非 nil。
	over, err := l.UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	if over == nil {
		t.Fatalf("越界页响应 = nil")
	}
	wantEQ(t, "越界页", "Total 仍是全量", over.Total, int32(5))
	wantEQ(t, "越界页", "条数", len(over.Items), 0)

	wantOps(t, "三页的查询序列", st.log.opsFrom(0), []string{
		"like.ListByMid:archive:7:1/2",
		"like.ListByMid:archive:7:3/2",
		"like.ListByMid:archive:7:9/2",
	})
}

// TestUserLikesUserWithoutLikesReturnsZeroTotal 没有任何点赞时 total=0、items 为空切片，
// 而不是错误——model 的 COUNT 先返回 0 并**省略第二条 SELECT**，
// 所以这里必须只有 1 次调用（替身把两条 SELECT 合成一个接口方法，见 README 覆盖边界）。
func TestUserLikesUserWithoutLikesReturnsZeroTotal(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, 999, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_101)
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 102, model.LikeStateCancel, 1_700_000_102)

	got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
		UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 20})
	wantNoErr(t, "无点赞", err)
	wantEQ(t, "无点赞", "响应非 nil", got != nil, true)
	wantOps(t, "无点赞", st.log.opsFrom(0), []string{"like.ListByMid:archive:7:1/20"})
	wantEQ(t, "无点赞", "Total", got.Total, int32(0))
	wantEQ(t, "无点赞", "条数", len(got.Items), 0)
}

// TestUserLikesPropagatesModelFailure 唯一依赖失败必须透出，
// 不能返回「total=0 的空列表」——那会被客户端渲染成「你还没点赞过任何内容」。
func TestUserLikesPropagatesModelFailure(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_101)
	st.like.failWith("ListByMid", errBoom)

	got, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
		UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 20})
	wantFail(t, "ListByMid 失败", got, err, errBoom)
	wantOps(t, "ListByMid 失败后的调用", st.log.opsFrom(0), []string{"like.ListByMid:archive:7:1/20"})
}

// TestUserLikesIsPureRead 列表接口不得有副作用：不写关系行、不动计数、
// 不碰收藏/分享，也不写任何缓存（UserLikes 没有缓存层，加了就是多一处不一致）。
func TestUserLikesIsPureRead(t *testing.T) {
	st := newStore()
	seedLike(st, likeBiz, likeMid, likeUpMid, likeOrigin, 101, model.LikeStateLike, 1_700_000_101)
	seedStat(st, likeBiz, likeOrigin, 101, 9, 1, 0, 0)
	seedFavItem(st, likeMid, 101, 33, 2, 11, 0)
	before := st.log.snapshot()

	_, err := NewUserLikesLogic(context.Background(), newTestSvc(st)).
		UserLikes(&rpc.UserLikesReq{Business: likeBiz, Mid: likeMid, Ps: 20})
	wantNoErr(t, "UserLikes", err)
	for _, prefix := range []string{"like.Upsert", "stat.", "favItem.", "folder.", "share.", "cache."} {
		wantCount(t, "只读检查", st.log, prefix, 0)
	}
	wantEQ(t, "只读检查", "只有一次查询", st.log.snapshot()-before, 1)
	wantEQ(t, "只读检查", "计数行未被改动", st.stat.get(likeBiz, likeOrigin, 101).LikeNumber, int64(9))
}
