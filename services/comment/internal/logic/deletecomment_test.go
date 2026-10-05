package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// dcTarget 布一条「可被删」的评论：字段值互不相同，便于逐字段核对删除后的残留。
func dcTarget(t *testing.T, e *env) *model.Comment {
	t.Helper()
	c := distinctComment(501, 9001)
	c.Mid = 7007
	c.Content = "要被删掉的评论"
	c.State = stateNormal
	c.Ctime = 1_700_000_501
	c.Mtime = 1_700_000_599
	c.Root, c.Parent = 0, 0
	c.LikeCount, c.ReplyCount = 21, 4
	return seedComment(t, e.st, c)
}

// dcWarmCaches 预热该目标下的列表/计数缓存，以及一个**别的目标**的缓存
// （用来断言失效范围只覆盖本 oid+tp）。
func dcWarmCaches(e *env) {
	e.st.cache.warmList(9001, 1, "hot", 1, 20, "本目标的列表缓存")
	e.st.cache.warmList(9002, 1, "hot", 1, 20, "别的目标的列表缓存")
	e.st.cache.warmStats(9001, 1, 7, 3)
	e.st.cache.warmStats(9002, 1, 9, 9)
}

// TestDeleteCommentGuards 参数守卫表：rpid/mid 非法必须在触库前拒绝。
func TestDeleteCommentGuards(t *testing.T) {
	cases := []struct {
		label  string
		mutate func(*rpc.DeleteCommentReq)
	}{
		{"rpid 为 0", func(r *rpc.DeleteCommentReq) { r.Rpid = 0 }},
		{"rpid 为负", func(r *rpc.DeleteCommentReq) { r.Rpid = -7 }},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := &rpc.DeleteCommentReq{Rpid: 501, Mid: 7007}
			tc.mutate(req)

			reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(req)

			// 现状：参数非法也报「找不到或无权限」，客户端拿不到「你传错了」的区分
			// （README 已知缺口 #8）。
			wantErrIs(t, tc.label, err, model.ErrCommentNotFoundOrForbidden)
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
		})
	}

	for _, mid := range []int64{0, -3} {
		t.Run("mid 非法", func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
				&rpc.DeleteCommentReq{Rpid: 501, Mid: mid})

			wantErrIs(t, "mid 非法", err, model.ErrInvalidMid)
			if reply != nil {
				t.Errorf("mid 非法却返回了 %+v", reply)
			}
			wantNoCall(t, "mid 非法", e.st, before)
		})
	}

	t.Run("rpid 与 mid 同时非法时按 rpid 报", func(t *testing.T) {
		e := newEnv(t)
		before := e.st.log.snapshot()

		_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
			&rpc.DeleteCommentReq{Rpid: 0, Mid: 0})

		wantErrIs(t, "两者都非法", err, model.ErrCommentNotFoundOrForbidden)
		wantNoCall(t, "两者都非法", e.st, before)
	})
}

// TestDeleteCommentByOwnerSoftDeletesAndInvalidatesCaches 本人删除：
// 先查、再按 mid 门槛软删、最后失效主体缓存 + 本目标列表缓存 + 本目标计数缓存。
func TestDeleteCommentByOwnerSoftDeletesAndInvalidatesCaches(t *testing.T) {
	e := newEnv(t)
	before := dcTarget(t, e)
	dcWarmCaches(e)

	reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: 7007})
	wantNoErr(t, "本人删除", err)
	if reply == nil {
		t.Fatalf("删除成功却返回了 nil")
	}

	row := e.st.comments.row(501)
	wantEQ(t, "删除后", "state=DELETED", row.State, stateDeleted)
	// 软删不清文字：审核与申诉要留证据（AGENTS.md §8）。
	wantEQ(t, "删除后", "content 保留", row.Content, "要被删掉的评论")
	wantEQ(t, "删除后", "oid 保留", row.Oid, int64(9001))
	wantEQ(t, "删除后", "tp 保留", row.Tp, int32(1))
	wantEQ(t, "删除后", "mid 保留", row.Mid, int64(7007))
	wantEQ(t, "删除后", "ctime 保留", row.Ctime, int64(1_700_000_501))
	wantEQ(t, "删除后", "like_count 保留", row.LikeCount, int32(21))
	wantEQ(t, "删除后", "reply_count 保留", row.ReplyCount, int32(4))
	// 缺陷登记（README 已知缺口 #2）：model.SoftDelete 的两条 UPDATE 都把 mtime 写成**操作者 mid**，
	// 而不是时间戳，所以删除后的 mtime 落在 1970 年。这条断言是钉桩：修好后它必须改成 now±1。
	wantEQ(t, "删除后（缺陷）", "mtime 被写成 mid", row.Mtime, int64(7007))

	wantEQ(t, "失效范围", "本目标列表缓存已清", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "")
	wantEQ(t, "失效范围", "别的目标列表缓存不动", e.st.cache.listPayload(9002, 1, "hot", 1, 20), "别的目标的列表缓存")
	if _, ok := e.st.cache.statsOf(9001, 1); ok {
		t.Errorf("本目标计数缓存未失效，CommentStats 会继续返回删除前的快照")
	}
	if _, ok := e.st.cache.statsOf(9002, 1); !ok {
		t.Errorf("别的目标计数缓存被顺手删了")
	}
	if e.st.cache.oneOf(501) != nil {
		t.Errorf("主体缓存未失效：%+v", e.st.cache.oneOf(501))
	}
	wantEQ(t, "删除前", "布景 ctime 未被改写", before.Mtime, int64(1_700_000_599))
	wantOps(t, "本人删除链路", e.st.log.ops, []string{
		"comment.FindOne:501",
		"comment.SoftDelete:501/false",
		"cache.DelOne:" + keyOne(501),
		"cache.Invalidate:" + invalidatePattern(9001, 1),
		"cache.DelStats:" + keyStats(9001, 1),
	})
}

// TestDeleteCommentByAdminBypassesOwnerCheck 管理员可删他人评论：门槛在 SQL 里被跳过，
// 但 mtime 仍然是操作者 mid（同缺陷 #2，这里换成管理员的 mid 以证明写进去的确实是操作者）。
func TestDeleteCommentByAdminBypassesOwnerCheck(t *testing.T) {
	e := newEnv(t)
	dcTarget(t, e)

	_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: 9009, Admin: true})
	wantNoErr(t, "管理员删除", err)

	row := e.st.comments.row(501)
	wantEQ(t, "管理员删除", "state", row.State, stateDeleted)
	wantEQ(t, "管理员删除", "mtime 是操作者 mid", row.Mtime, int64(9009))
	wantEQ(t, "管理员删除", "mid 仍是作者", row.Mid, int64(7007))
	wantOps(t, "管理员删除链路", e.st.log.ops, []string{
		"comment.FindOne:501",
		"comment.SoftDelete:501/true",
		"cache.DelOne:" + keyOne(501),
		"cache.Invalidate:" + invalidatePattern(9001, 1),
		"cache.DelStats:" + keyStats(9001, 1),
	})
}

// TestDeleteCommentByNonOwnerLeavesEverythingIntact 非本人且非管理员：SQL 门槛挡住，
// 状态不变、缓存一条都不失效（不能出现「拒绝但列表已经少了这条」的半截状态）。
func TestDeleteCommentByNonOwnerLeavesEverythingIntact(t *testing.T) {
	e := newEnv(t)
	dcTarget(t, e)
	dcWarmCaches(e)

	reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: 8888})

	wantErrIs(t, "非本人删除", err, model.ErrCommentNotFoundOrForbidden)
	if reply != nil {
		t.Errorf("非本人删除却返回了 %+v", reply)
	}
	wantEQ(t, "非本人删除", "状态未变", e.st.comments.state(501), stateNormal)
	wantEQ(t, "非本人删除", "mtime 未变", e.st.comments.row(501).Mtime, int64(1_700_000_599))
	wantEQ(t, "非本人删除", "列表缓存未失效", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "本目标的列表缓存")
	if _, ok := e.st.cache.statsOf(9001, 1); !ok {
		t.Errorf("非本人删除却失效了计数缓存")
	}
	wantOps(t, "非本人删除", e.st.log.ops, []string{
		"comment.FindOne:501",
		"comment.SoftDelete:501/false",
	})
}

// TestDeleteCommentMissingRowDoesNotTouchStore 评论不存在：FindOne 返回 nil 即拒绝，
// 不再发 UPDATE（model 的 admin 分支不检查受影响行数，这条防线只能靠 Repository 的先查一次）。
func TestDeleteCommentMissingRowDoesNotTouchStore(t *testing.T) {
	e := newEnv(t)
	dcWarmCaches(e)

	_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: 7007})

	wantErrIs(t, "评论不存在", err, model.ErrCommentNotFoundOrForbidden)
	wantOps(t, "评论不存在", e.st.log.ops, []string{"comment.FindOne:501"})
	wantCount(t, "评论不存在", e.st.log, "comment.SoftDelete", 0)
	if _, ok := e.st.cache.statsOf(9001, 1); !ok {
		t.Errorf("评论不存在却失效了计数缓存")
	}
}

// TestDeleteCommentAdminOnMissingRowAlsoRejected 管理员删不存在的行同样被 Repository 拦住
// （否则 model 的 admin 分支会静默返回 nil，logic 就报成功了）。
func TestDeleteCommentAdminOnMissingRowAlsoRejected(t *testing.T) {
	e := newEnv(t)

	_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 404, Mid: 9009, Admin: true})

	wantErrIs(t, "管理员删不存在的行", err, model.ErrCommentNotFoundOrForbidden)
	wantOps(t, "管理员删不存在的行", e.st.log.ops, []string{"comment.FindOne:404"})
}

// TestDeleteCommentPropagatesStoreFailures 两个下游各注入一次错误：
// 错误必须原样传出来，且不得留下半截数据或误清缓存。
func TestDeleteCommentPropagatesStoreFailures(t *testing.T) {
	boom := errors.New("comment: mysql is gone")

	t.Run("FindOne 失败", func(t *testing.T) {
		e := newEnv(t)
		dcTarget(t, e)
		dcWarmCaches(e)
		e.st.comments.failWith("FindOne", boom)

		reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
			&rpc.DeleteCommentReq{Rpid: 501, Mid: 7007})

		wantErrIs(t, "FindOne 失败", err, boom)
		if reply != nil {
			t.Errorf("FindOne 失败却返回了 %+v", reply)
		}
		wantOps(t, "FindOne 失败", e.st.log.ops, []string{"comment.FindOne:501"})
		wantEQ(t, "FindOne 失败", "状态未变", e.st.comments.state(501), stateNormal)
	})

	t.Run("SoftDelete 失败", func(t *testing.T) {
		e := newEnv(t)
		dcTarget(t, e)
		dcWarmCaches(e)
		e.st.comments.failWith("SoftDelete", boom)

		reply, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
			&rpc.DeleteCommentReq{Rpid: 501, Mid: 7007})

		wantErrIs(t, "SoftDelete 失败", err, boom)
		if reply != nil {
			t.Errorf("SoftDelete 失败却返回了 %+v", reply)
		}
		wantEQ(t, "SoftDelete 失败", "状态未变", e.st.comments.state(501), stateNormal)
		wantOps(t, "SoftDelete 失败", e.st.log.ops, []string{
			"comment.FindOne:501",
			"comment.SoftDelete:501/false",
		})
		wantEQ(t, "SoftDelete 失败", "列表缓存未失效", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "本目标的列表缓存")
	})

	// 缓存删除失败被吞：库已经改了，缓存失效却失败 ⇒ 列表最长脏 30 秒（README 已知缺口 #7）。
	// 这里断言仍然返回成功，是为了钉住「不回滚、不报错」这个既有取舍。
	for _, tc := range []struct{ method, prefix string }{
		{"DelOne", "cache.DelOne"},
		{"InvalidateListByOid", "cache.Invalidate"},
		{"DelStats", "cache.DelStats"},
	} {
		t.Run(tc.method+" 失败", func(t *testing.T) {
			e := newEnv(t)
			dcTarget(t, e)
			e.st.cache.failWith(tc.method, errors.New("comment: redis is gone"))

			_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
				&rpc.DeleteCommentReq{Rpid: 501, Mid: 7007})

			wantNoErr(t, tc.method+" 失败", err)
			wantEQ(t, tc.method+" 失败", "状态已改", e.st.comments.state(501), stateDeleted)
			wantCount(t, tc.method+" 失败", e.st.log, tc.prefix, 1)
		})
	}
}

// TestDeleteCommentKeepsCountsConsistent 删除后重新统计：总数与根数都要掉下来，
// 且这一步必须走回源（DelStats 已把快照清掉），而不是继续吃旧快照。
func TestDeleteCommentKeepsCountsConsistent(t *testing.T) {
	e := newEnv(t)
	root := distinctComment(501, 9001)
	root.Tp = 1
	seedComment(t, e.st, root)
	child := distinctComment(502, 9001)
	child.Tp, child.Root, child.Parent = 1, 501, 501
	seedComment(t, e.st, child)
	e.st.cache.warmStats(9001, 1, 99, 98) // 过期快照，必须被删除动作清掉

	_, err := NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: root.Mid})
	wantNoErr(t, "删除根评论", err)

	stats, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "删除后统计", err)
	// 根评论被删；它对自身 root 的那条回复按 SQL 口径（root=501 且 state 可见）仍在。
	wantEQ(t, "删除后统计", "total", stats.GetTotal(), int64(1))
	wantEQ(t, "删除后统计", "root_total", stats.GetRootTotal(), int64(0))
	wantCount(t, "删除后统计", e.st.log, "comment.CountByTarget", 1)

	list, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})
	wantNoErr(t, "删除后列表", err)
	wantEQ(t, "删除后列表", "条数", len(list.GetComments()), 0)
	wantEQ(t, "删除后列表", "总数", list.GetTotal(), int32(0))
}
