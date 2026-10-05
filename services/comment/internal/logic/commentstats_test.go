package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// csSeed 布一个目标下的完整楼群：3 条根评论 + 4 条回复（全部可见），
// 外加 1 条已删除、1 条审核驳回、1 条属于别的目标——三者都不该被计数。
// 期望：total=7、root_total=3（两个值刻意不同，写反了立刻红）。
func csSeed(t *testing.T, e *env) {
	t.Helper()
	mk := func(rpid, root int64, state int32) *model.Comment {
		c := distinctComment(rpid, 9001)
		c.Tp, c.Root, c.Parent, c.State = 1, root, root, state
		return c
	}
	seedComment(t, e.st, mk(501, 0, stateNormal))
	seedComment(t, e.st, mk(502, 0, stateFolded))
	seedComment(t, e.st, mk(503, 0, statePinned))
	seedComment(t, e.st, mk(601, 501, stateNormal))
	seedComment(t, e.st, mk(602, 501, stateNormal))
	seedComment(t, e.st, mk(603, 502, stateNormal))
	seedComment(t, e.st, mk(604, 503, stateNormal))
	seedComment(t, e.st, mk(605, 501, stateDeleted))
	seedComment(t, e.st, mk(606, 501, stateRejected))
	other := mk(701, 0, stateNormal) // 属于别的稿件
	other.Oid = 9002
	seedComment(t, e.st, other)
}

// TestCommentStatsGuards 目标非法必须在触缓存/触库前拒绝。
func TestCommentStatsGuards(t *testing.T) {
	for _, oid := range []int64{0, -1} {
		e := newEnv(t)
		before := e.st.log.snapshot()

		reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
			&rpc.CommentStatsReq{Oid: oid, Tp: 1})

		wantErrIs(t, "oid 非法", err, model.ErrInvalidTarget)
		if reply != nil {
			t.Errorf("oid=%d 却返回了 %+v", oid, reply)
		}
		wantNoCall(t, "oid 非法", e.st, before)
	}
}

// TestCommentStatsAcceptsAnyTargetType 现状：tp 不做取值校验（迁移里 tp 合法值是 1/4/9/10…），
// tp=0 会作为一个真实目标类型穿透到缓存 key 与 SQL。
func TestCommentStatsAcceptsAnyTargetType(t *testing.T) {
	e := newEnv(t)
	csSeed(t, e)

	reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 0})
	wantNoErr(t, "tp=0", err)
	wantEQ(t, "tp=0", "total", reply.GetTotal(), int64(0))
	wantEQ(t, "tp=0", "root_total", reply.GetRootTotal(), int64(0))
	wantOps(t, "tp=0", e.st.log.ops, []string{
		"cache.GetStats:" + keyStats(9001, 0),
		"comment.CountByTarget:9001/0",
		"cache.SetStats:" + keyStats(9001, 0),
	})
}

// TestCommentStatsCountsVisibleCommentsOnMiss 回源路径：total 含回复、root_total 只数根评论，
// 已删除/审核驳回/别的目标都不计入；查完回填 60 秒快照。
func TestCommentStatsCountsVisibleCommentsOnMiss(t *testing.T) {
	e := newEnv(t)
	csSeed(t, e)

	reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "CommentStats", err)

	wantEQ(t, "计数", "total 含回复", reply.GetTotal(), int64(7))
	wantEQ(t, "计数", "root_total 只数根", reply.GetRootTotal(), int64(3))
	wantOps(t, "计数链路", e.st.log.ops, []string{
		"cache.GetStats:" + keyStats(9001, 1),
		"comment.CountByTarget:9001/1",
		"cache.SetStats:" + keyStats(9001, 1),
	})
	got, ok := e.st.cache.statsOf(9001, 1)
	if !ok {
		t.Fatalf("计数快照未回填")
	}
	wantEQ(t, "回填快照", "total", got.total, int64(7))
	wantEQ(t, "回填快照", "root_total", got.rootTotal, int64(3))
}

// TestCommentStatsServesFromCacheWithoutCounting 命中缓存时不得再打库，
// 且返回的是缓存里那一版（计数最长滞后 cacheTTLStats=60 秒，README 已知缺口 #17）。
func TestCommentStatsServesFromCacheWithoutCounting(t *testing.T) {
	e := newEnv(t)
	csSeed(t, e)
	e.st.cache.warmStats(9001, 1, 99, 98) // 库里其实是 7/3

	reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "计数缓存命中", err)

	wantEQ(t, "计数缓存命中", "total", reply.GetTotal(), int64(99))
	wantEQ(t, "计数缓存命中", "root_total", reply.GetRootTotal(), int64(98))
	wantOps(t, "计数缓存命中", e.st.log.ops, []string{"cache.GetStats:" + keyStats(9001, 1)})
}

// TestCommentStatsCountsPendingAsVisible 缺陷钉桩（README 已知缺口 #12）：
// 三处统计/列表 SQL 的可见口径都是 state NOT IN (2,5)，因此 state=4（PENDING 待审核）
// 的评论在审核结论回来之前就已经计入公开计数、并且会出现在公开列表里。
func TestCommentStatsCountsPendingAsVisible(t *testing.T) {
	e := newEnv(t)
	pending := distinctComment(501, 9001)
	pending.Tp, pending.State = 1, statePending
	seedComment(t, e.st, pending)
	folded := distinctComment(502, 9001)
	folded.Tp, folded.State = 1, stateFolded
	seedComment(t, e.st, folded)

	reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "待审计数", err)

	wantEQ(t, "待审计数", "total 已含未过审评论", reply.GetTotal(), int64(2))
	wantEQ(t, "待审计数", "root_total 已含未过审评论", reply.GetRootTotal(), int64(2))
	// 若把待审排除出可见口径，本用例应随之变红并同步更新 README。
}

// TestCommentStatsPropagatesFailures 两个下游各注入一次错误：错误传出且不留下脏快照。
func TestCommentStatsPropagatesFailures(t *testing.T) {
	boom := errors.New("comment: dependency is gone")

	t.Run("缓存读失败", func(t *testing.T) {
		e := newEnv(t)
		csSeed(t, e)
		e.st.cache.failWith("GetStats", boom)

		reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
			&rpc.CommentStatsReq{Oid: 9001, Tp: 1})

		wantErrIs(t, "缓存读失败", err, boom)
		if reply != nil {
			t.Errorf("缓存读失败却返回了 %+v", reply)
		}
		wantOps(t, "缓存读失败", e.st.log.ops, []string{"cache.GetStats:" + keyStats(9001, 1)})
	})

	// 缓存读失败时不降级回源：Redis 抖动直接反映为接口失败，而不是把每个请求都放去数全表。
	t.Run("计数查询失败", func(t *testing.T) {
		e := newEnv(t)
		csSeed(t, e)
		e.st.comments.failWith("CountByTarget", boom)

		reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
			&rpc.CommentStatsReq{Oid: 9001, Tp: 1})

		wantErrIs(t, "计数查询失败", err, boom)
		if reply != nil {
			t.Errorf("计数查询失败却返回了 %+v", reply)
		}
		wantOps(t, "计数查询失败", e.st.log.ops, []string{
			"cache.GetStats:" + keyStats(9001, 1),
			"comment.CountByTarget:9001/1",
		})
		if _, ok := e.st.cache.statsOf(9001, 1); ok {
			t.Errorf("计数查询失败却回填了快照")
		}
	})

	t.Run("IncrLikeCount 之外的写入不参与", func(t *testing.T) {
		// CommentStats 是纯读侧：不得触发任何计数写入。
		e := newEnv(t)
		csSeed(t, e)

		_, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
			&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
		wantNoErr(t, "纯读侧", err)

		wantCount(t, "纯读侧", e.st.log, "comment.IncrLike", 0)
		wantCount(t, "纯读侧", e.st.log, "comment.IncrReply", 0)
		wantCount(t, "纯读侧", e.st.log, "comment.SoftDelete", 0)
	})
}

// TestCommentStatsToleratesBackfillFailure 回填失败被吞：真值照样返回（README 已知缺口 #7）。
func TestCommentStatsToleratesBackfillFailure(t *testing.T) {
	e := newEnv(t)
	csSeed(t, e)
	e.st.cache.failWith("SetStats", errors.New("comment: redis is gone"))

	reply, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})

	wantNoErr(t, "回填失败", err)
	wantEQ(t, "回填失败", "total", reply.GetTotal(), int64(7))
	if _, ok := e.st.cache.statsOf(9001, 1); ok {
		t.Errorf("回填失败却写进了快照")
	}
}

// TestCommentStatsRefillsAfterDeleteInvalidation 删除会清掉快照，下一次统计必须重新数全表
// （而不是继续吃删除前的值）。这是 comment 域「计数与状态」最关键的一条联动。
func TestCommentStatsRefillsAfterDeleteInvalidation(t *testing.T) {
	e := newEnv(t)
	csSeed(t, e)

	first, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "首次统计", err)
	wantEQ(t, "首次统计", "total", first.GetTotal(), int64(7))

	_, err = NewDeleteCommentLogic(context.Background(), e.svcCtx).DeleteComment(
		&rpc.DeleteCommentReq{Rpid: 501, Mid: e.st.comments.row(501).Mid})
	wantNoErr(t, "删除根评论", err)

	second, err := NewCommentStatsLogic(context.Background(), e.svcCtx).CommentStats(
		&rpc.CommentStatsReq{Oid: 9001, Tp: 1})
	wantNoErr(t, "删除后统计", err)
	// 501 被删；它名下的 601/602 是回复（root=501），按可见口径仍然计入 total，但不算根。
	wantEQ(t, "删除后统计", "total", second.GetTotal(), int64(6))
	wantEQ(t, "删除后统计", "root_total", second.GetRootTotal(), int64(2))
	wantCount(t, "删除后统计", e.st.log, "comment.CountByTarget", 2)
}
