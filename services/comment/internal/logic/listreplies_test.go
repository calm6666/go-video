package logic

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// lrSeed 布一个楼：根评论 501 + 三条可见回复（ctime 各不相同，便于核对 ASC 顺序）
// + 两条不可见回复（已删除 / 审核驳回）+ 一条别的楼的回复。
//
//	601 ctime=1000 parent=501
//	603 ctime=2000 parent=501 state=FOLDED（折叠仍可见）
//	602 ctime=3000 parent=601 root=501（回复的回复仍然归到根评论这一楼）
//	604 state=DELETED、605 state=REJECTED、606 root=502
func lrSeed(t *testing.T, e *env) {
	t.Helper()
	mk := func(rpid, root, parent int64, state int32, ctime int64) *model.Comment {
		c := distinctComment(rpid, 9001)
		c.Tp, c.Root, c.Parent, c.State, c.Ctime = 1, root, parent, state, ctime
		c.Mtime = ctime + 7
		return c
	}
	seedComment(t, e.st, mk(501, 0, 0, stateNormal, 900))
	seedComment(t, e.st, mk(601, 501, 501, stateNormal, 1000))
	seedComment(t, e.st, mk(603, 501, 501, stateFolded, 2000))
	seedComment(t, e.st, mk(602, 501, 601, stateNormal, 3000))
	seedComment(t, e.st, mk(604, 501, 501, stateDeleted, 4000))
	seedComment(t, e.st, mk(605, 501, 501, stateRejected, 5000))
	seedComment(t, e.st, mk(606, 502, 502, stateNormal, 6000))
}

func lrRpids(items []*rpc.CommentInfo) []string {
	var out []string
	for _, it := range items {
		out = append(out, itoa(it.GetRpid()))
	}
	return out
}

// TestListRepliesGuards 参数守卫表：root 非法与页大小非法必须在触库前拒绝。
func TestListRepliesGuards(t *testing.T) {
	cases := []struct {
		label   string
		mutate  func(*rpc.ListRepliesReq)
		wantIs  error
		wantMsg string
	}{
		{"root 为 0", func(r *rpc.ListRepliesReq) { r.Root = 0 }, nil, "comment: invalid root"},
		{"root 为负", func(r *rpc.ListRepliesReq) { r.Root = -9 }, nil, "comment: invalid root"},
		{"ps 为 0", func(r *rpc.ListRepliesReq) { r.Ps = 0 }, model.ErrPsTooLarge, ""},
		{"ps 为负", func(r *rpc.ListRepliesReq) { r.Ps = -1 }, model.ErrPsTooLarge, ""},
		{"ps 超上限 49", func(r *rpc.ListRepliesReq) { r.Ps = 50 }, model.ErrPsTooLarge, ""},
		{"root 与 ps 同时非法时按 root 报", func(r *rpc.ListRepliesReq) { r.Root, r.Ps = 0, 99 }, nil, "comment: invalid root"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := &rpc.ListRepliesReq{Root: 501, Pn: 1, Ps: 20}
			tc.mutate(req)

			reply, err := NewListRepliesLogic(context.Background(), e.svcCtx).ListReplies(req)

			if tc.wantIs != nil {
				wantErrIs(t, tc.label, err, tc.wantIs)
			} else {
				wantErrMessage(t, tc.label, err, tc.wantMsg)
			}
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
		})
	}
}

// TestListRepliesProjectsAndOrdersByCtime 正常路径：按 ctime 升序盖楼、逐字段投影、
// total 是全集条数；并且**完全不经过缓存**（楼中楼每次都打库，README 已知缺口 #11）。
func TestListRepliesProjectsAndOrdersByCtime(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)

	reply, err := NewListRepliesLogic(context.Background(), e.svcCtx).ListReplies(
		&rpc.ListRepliesReq{Root: 501, ViewerMid: 4242, Pn: 1, Ps: 20})
	wantNoErr(t, "ListReplies", err)

	wantStringsEQ(t, "楼中楼", "ctime 升序", lrRpids(reply.GetReplies()), []string{"601", "603", "602"})
	wantEQ(t, "楼中楼", "total", reply.GetTotal(), int32(3))
	for i, rpid := range []int64{601, 603, 602} {
		assertCommentProjected(t, "第 "+itoa(int64(i+1))+" 条回复投影", reply.GetReplies()[i], e.st.comments.row(rpid))
	}
	// 折叠仍可见，已删除/审核驳回/别的楼不可见。
	got := lrRpids(reply.GetReplies())
	for _, hidden := range []string{"501", "604", "605", "606"} {
		if slices.Contains(got, hidden) {
			t.Errorf("%s 不该出现在 root=501 的回复里：%v", hidden, got)
		}
	}
	wantCount(t, "楼中楼", e.st.log, "cache.", 0)
	wantOps(t, "楼中楼", e.st.log.ops, []string{"comment.ListReplies:501/1/20"})
}

// TestListRepliesNormalizesPageNumber pn<=0 折成 1，且归一发生在触库之前。
func TestListRepliesNormalizesPageNumber(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)

	_, err := NewListRepliesLogic(context.Background(), e.svcCtx).ListReplies(
		&rpc.ListRepliesReq{Root: 501, Pn: 0, Ps: 20})
	wantNoErr(t, "pn=0", err)

	wantOps(t, "pn=0", e.st.log.ops, []string{"comment.ListReplies:501/1/20"})
}

// TestListRepliesPagesWithoutOverlap 分页不重不漏，total 恒为全集。
func TestListRepliesPagesWithoutOverlap(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)
	lr := NewListRepliesLogic(context.Background(), e.svcCtx)

	p1, err := lr.ListReplies(&rpc.ListRepliesReq{Root: 501, Pn: 1, Ps: 2})
	wantNoErr(t, "第 1 页", err)
	wantStringsEQ(t, "第 1 页", "rpid", lrRpids(p1.GetReplies()), []string{"601", "603"})
	wantEQ(t, "第 1 页", "total", p1.GetTotal(), int32(3))

	p2, err := lr.ListReplies(&rpc.ListRepliesReq{Root: 501, Pn: 2, Ps: 2})
	wantNoErr(t, "第 2 页", err)
	wantStringsEQ(t, "第 2 页", "rpid", lrRpids(p2.GetReplies()), []string{"602"})
	wantEQ(t, "第 2 页", "total", p2.GetTotal(), int32(3))
}

// TestListRepliesReplyOfReplyStaysInSameFloor 盖楼口径：回复的回复（parent=601、root=501）
// 仍然只在 root=501 这一楼里出现，601 下面不会长出子楼。
func TestListRepliesReplyOfReplyStaysInSameFloor(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)
	lr := NewListRepliesLogic(context.Background(), e.svcCtx)

	sub, err := lr.ListReplies(&rpc.ListRepliesReq{Root: 601, Pn: 1, Ps: 20})
	wantNoErr(t, "查 601 的子楼", err)
	wantEQ(t, "601 的子楼", "条数", len(sub.GetReplies()), 0)
	wantEQ(t, "601 的子楼", "total", sub.GetTotal(), int32(0))

	// 602 的 parent 字段仍然透出，客户端才能渲染「回复了谁」。
	main, err := lr.ListReplies(&rpc.ListRepliesReq{Root: 501, Pn: 1, Ps: 20})
	wantNoErr(t, "查 501 的楼", err)
	last := main.GetReplies()[len(main.GetReplies())-1]
	wantEQ(t, "楼内最后一条", "rpid", last.GetRpid(), int64(602))
	wantEQ(t, "楼内最后一条", "root", last.GetRoot(), int64(501))
	wantEQ(t, "楼内最后一条", "parent", last.GetParent(), int64(601))
}

// TestListRepliesDoesNotValidateRootExistence 现状：本服务不校验 root 是否存在、是否已被删除，
// 因此对一个不存在的楼也返回成功空列表，并且一次 FindOne 都不发。
func TestListRepliesDoesNotValidateRootExistence(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)

	reply, err := NewListRepliesLogic(context.Background(), e.svcCtx).ListReplies(
		&rpc.ListRepliesReq{Root: 424242, Pn: 1, Ps: 20})
	wantNoErr(t, "不存在的楼", err)
	wantEQ(t, "不存在的楼", "条数", len(reply.GetReplies()), 0)
	wantEQ(t, "不存在的楼", "total", reply.GetTotal(), int32(0))
	wantOps(t, "不存在的楼", e.st.log.ops, []string{"comment.ListReplies:424242/1/20"})
	wantCount(t, "不存在的楼", e.st.log, "comment.FindOne", 0)
}

// TestListRepliesPropagatesStoreFailure 回源失败原样传出，不返回伪造的空列表。
func TestListRepliesPropagatesStoreFailure(t *testing.T) {
	e := newEnv(t)
	lrSeed(t, e)
	boom := errors.New("comment: mysql is gone")
	e.st.comments.failWith("ListReplies", boom)

	reply, err := NewListRepliesLogic(context.Background(), e.svcCtx).ListReplies(
		&rpc.ListRepliesReq{Root: 501, Pn: 1, Ps: 20})

	wantErrIs(t, "回源失败", err, boom)
	if reply != nil {
		t.Errorf("回源失败却返回了 %+v", reply)
	}
	wantOps(t, "回源失败", e.st.log.ops, []string{"comment.ListReplies:501/1/20"})
}
