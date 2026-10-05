package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// pcValidReq 是一条合法请求（root=0、parent=0 的直接评论）。
// tp=1 与迁移里「1 视频」一致；state=4 是契约推荐的「待审核」。
func pcValidReq() *rpc.PostCommentReq {
	return &rpc.PostCommentReq{
		Oid: 9001, Tp: 1, Root: 0, Parent: 0, Mid: 7007,
		Content: "这期剪辑比上一期好", State: statePending, TraceId: "trace-post-1",
	}
}

// TestPostCommentGuards 参数守卫表：全部必须在触库/触缓存之前拒绝。
// 顺序也是被测事实：oid → mid → content → root/parent 一致性。
func TestPostCommentGuards(t *testing.T) {
	cases := []struct {
		label   string
		mutate  func(*rpc.PostCommentReq)
		wantIs  error
		wantMsg string
	}{
		{"oid 为 0", func(r *rpc.PostCommentReq) { r.Oid = 0 }, model.ErrInvalidTarget, ""},
		{"oid 为负", func(r *rpc.PostCommentReq) { r.Oid = -5 }, model.ErrInvalidTarget, ""},
		{"mid 为 0", func(r *rpc.PostCommentReq) { r.Mid = 0 }, model.ErrInvalidMid, ""},
		{"mid 为负", func(r *rpc.PostCommentReq) { r.Mid = -3 }, model.ErrInvalidMid, ""},
		{"内容为空串", func(r *rpc.PostCommentReq) { r.Content = "" }, model.ErrContentEmpty, ""},
		{
			"只给 root 不给 parent",
			func(r *rpc.PostCommentReq) { r.Root, r.Parent = 500, 0 },
			nil, "comment: parent required when root specified",
		},
		{
			"oid 与 mid 与内容同时非法时按 oid 报",
			func(r *rpc.PostCommentReq) { r.Oid, r.Mid, r.Content = 0, 0, "" },
			model.ErrInvalidTarget, "",
		},
		{
			"mid 与内容同时非法时按 mid 报",
			func(r *rpc.PostCommentReq) { r.Mid, r.Content = 0, "" },
			model.ErrInvalidMid, "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := pcValidReq()
			tc.mutate(req)

			reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)

			if tc.wantIs != nil {
				wantErrIs(t, tc.label, err, tc.wantIs)
			} else {
				wantErrMessage(t, tc.label, err, tc.wantMsg)
			}
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
			wantEQ(t, tc.label, "库存行数", e.st.comments.countRows(), 0)
		})
	}
}

// TestPostCommentProjectsRowAndReply 正常路径逐字段核对：请求 → model.Comment → 库存行 → 回复体。
func TestPostCommentProjectsRowAndReply(t *testing.T) {
	e := newEnv(t)
	req := pcValidReq()
	req.Root, req.Parent = 500, 500 // 同时把盖楼字段带上

	reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)
	wantNoErr(t, "PostComment", err)

	row := e.st.comments.row(501) // 替身自增分配的第一条
	if row == nil {
		t.Fatalf("评论未落库，库存 rpid：%v", e.st.comments.rpidList())
	}
	wantEQ(t, "落库行", "rpid", row.Rpid, int64(501))
	wantEQ(t, "落库行", "oid", row.Oid, int64(9001))
	wantEQ(t, "落库行", "tp", row.Tp, int32(1))
	wantEQ(t, "落库行", "root", row.Root, int64(500))
	wantEQ(t, "落库行", "parent", row.Parent, int64(500))
	wantEQ(t, "落库行", "mid", row.Mid, int64(7007))
	wantEQ(t, "落库行", "content", row.Content, "这期剪辑比上一期好")
	wantEQ(t, "落库行", "state", row.State, statePending)
	// 真实 SQL 把两个计数写死为 0：新评论不可能带历史点赞/回复数。
	wantEQ(t, "落库行", "like_count", row.LikeCount, int32(0))
	wantEQ(t, "落库行", "reply_count", row.ReplyCount, int32(0))
	assertAround(t, "落库行", "ctime", row.Ctime, nowUnix(), 2)
	wantEQ(t, "落库行", "mtime 与 ctime 同刻", row.Mtime, row.Ctime)

	wantEQ(t, "回复体", "rpid", reply.GetRpid(), int64(501))
	wantEQ(t, "回复体", "ctime 取库存值", reply.GetCtime(), row.Ctime)

	// 主体缓存必须带主键（Repository 在 SetOne 前把 rpid 写回 c）：
	// 若漏掉写回，key 会变成 cmt:1:0，下面的 oneOf(501) 就是 nil。
	cached := e.st.cache.oneOf(501)
	if cached == nil {
		t.Fatalf("主体缓存未落 501：%+v", e.st.cache.ones)
	}
	wantEQ(t, "主体缓存", "content", cached.Content, "这期剪辑比上一期好")
	wantEQ(t, "主体缓存", "state", cached.State, statePending)
	if e.st.cache.oneOf(0) != nil {
		t.Errorf("主键为 0 的行被写进了缓存：%+v", e.st.cache.oneOf(0))
	}
	wantOps(t, "PostComment 写侧链路", e.st.log.ops, []string{
		"comment.Insert",
		"cache.SetOne:" + keyOne(501),
		"cache.Invalidate:" + invalidatePattern(9001, 1),
		"cache.DelStats:" + keyStats(9001, 1),
	})
}

// TestPostCommentReplyKeepsParentLinkAndDoesNotBumpCounters 盖楼关系的不变量：
// root/parent 原样入库（不继承父评论的 oid/tp，也不由 parent 反查 root），
// 但**回复数快照不会增长**——model.IncrReplyCount 在整个 comment 服务里没有调用方
// （README 已知缺口 #3），所以楼中楼计数永远是 0。
func TestPostCommentReplyKeepsParentLinkAndDoesNotBumpCounters(t *testing.T) {
	e := newEnv(t)
	seed := distinctComment(501, 9001)
	seed.ReplyCount = 3 // 历史快照
	seed.LikeCount = 11
	seedComment(t, e.st, seed)

	req := pcValidReq()
	req.Root, req.Parent = 501, 501
	req.Tp = 0 // 客户端回复时不传 tp：落库就是 0，不会继承根评论的 tp=1

	reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)
	wantNoErr(t, "发布回复", err)

	row := e.st.comments.row(reply.GetRpid())
	if row == nil {
		t.Fatalf("回复未落库，库存 rpid：%v", e.st.comments.rpidList())
	}
	wantEQ(t, "回复行", "root", row.Root, int64(501))
	wantEQ(t, "回复行", "parent", row.Parent, int64(501))
	wantEQ(t, "回复行", "oid 取请求值", row.Oid, int64(9001))
	wantEQ(t, "回复行", "tp 不继承父评论", row.Tp, int32(0))
	wantEQ(t, "回复行", "自身 reply_count", row.ReplyCount, int32(0))

	wantEQ(t, "根评论快照", "reply_count 未增长", e.st.comments.row(501).ReplyCount, int32(3))
	wantEQ(t, "根评论快照", "like_count 未受影响", e.st.comments.row(501).LikeCount, int32(11))
	wantCount(t, "回复发布", e.st.log, "comment.IncrReply", 0)
	wantCount(t, "回复发布", e.st.log, "comment.IncrLike", 0)
	// 盖楼不额外查父评论：root/parent 只是被照抄的字段，本服务不做存在性校验。
	wantCount(t, "回复发布", e.st.log, "comment.FindOne", 0)
}

// TestPostCommentReplayIsNotIdempotent 现状钉桩：PostComment 没有幂等键
// （PostCommentReq.trace_id 在 comment 表里没有落点，也没有 request_id 唯一约束），
// 因此同一请求重放两次会落两行、加两次失效。AGENTS.md §5 要求写接口具备幂等键——
// 本用例把「目前不具备」钉成红灯，实现幂等时必须一并改它（README 已知缺口 #4）。
func TestPostCommentReplayIsNotIdempotent(t *testing.T) {
	e := newEnv(t)
	req := pcValidReq()

	first, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)
	wantNoErr(t, "第一次发布", err)
	second, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)
	wantNoErr(t, "重放发布", err)

	if first.GetRpid() == second.GetRpid() {
		t.Fatalf("重放拿到了同一个 rpid %d，与「无幂等键」的现状不符", first.GetRpid())
	}
	wantEQ(t, "重放", "库存行数", e.st.comments.countRows(), 2)
	wantStringsEQ(t, "重放", "库存 rpid", e.st.comments.rpidList(), []string{"501", "502"})
	wantCount(t, "重放", e.st.log, "comment.Insert", 2)
	wantCount(t, "重放", e.st.log, "cache.DelStats", 2)
}

// TestPostCommentHasNoContentLengthOrBlankGuard 缺守卫的现状：只有「空串」被拒，
// 纯空白与超过 comment.content VARBINARY(9000) 的长度都直接进 SQL（README 已知缺口 #6）。
// 这里断言的是「确实触库了」，与守卫类断言相反，因此同样可失败。
func TestPostCommentHasNoContentLengthOrBlankGuard(t *testing.T) {
	cases := []struct {
		label   string
		content string
	}{
		{"纯空白内容", "   \n\t "},
		{"超过 VARBINARY(9000)", strings.Repeat("a", 9001)},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			req := pcValidReq()
			req.Content = tc.content

			reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(req)
			wantNoErr(t, tc.label, err)

			row := e.st.comments.row(reply.GetRpid())
			if row == nil {
				t.Fatalf("%s：请求未被拒绝，但也没有落库", tc.label)
			}
			wantEQ(t, tc.label, "落库长度", len(row.Content), len(tc.content))
			wantCount(t, tc.label, e.st.log, "comment.Insert", 1)
		})
	}
}

// TestPostCommentPropagatesInsertFailure 落库失败必须报错且不留下任何缓存动作：
// 不能出现「列表缓存已失效但评论根本没写进去」的半截状态。
func TestPostCommentPropagatesInsertFailure(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("comment: mysql is gone")
	e.st.comments.failWith("Insert", boom)
	e.st.cache.warmStats(9001, 1, 7, 3)
	e.st.cache.warmList(9001, 1, "hot", 1, 20, "旧列表")

	reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(pcValidReq())

	wantErrIs(t, "落库失败", err, boom)
	if reply != nil {
		t.Errorf("落库失败却返回了 %+v", reply)
	}
	wantEQ(t, "落库失败", "库存行数", e.st.comments.countRows(), 0)
	wantOps(t, "落库失败", e.st.log.ops, []string{"comment.Insert"})
	wantEQ(t, "落库失败", "列表缓存未被清掉", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "旧列表")
	if _, ok := e.st.cache.statsOf(9001, 1); !ok {
		t.Errorf("落库失败却失效了计数缓存")
	}
}

// TestPostCommentToleratesCacheFailures 缓存三处写失败都被 repository 吞掉（`_ =`）：
// 发布本身已成功，所以仍然返回成功。代价是列表最长脏 30 秒、计数最长脏 60 秒（README 已知缺口 #7）。
func TestPostCommentToleratesCacheFailures(t *testing.T) {
	cases := []struct {
		method string
		prefix string // 轨迹里对应的缓存调用前缀
	}{
		{"SetOne", "cache.SetOne"},
		{"InvalidateListByOid", "cache.Invalidate"},
		{"DelStats", "cache.DelStats"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" 失败", func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.failWith(tc.method, errors.New("comment: redis is gone"))

			reply, err := NewPostCommentLogic(context.Background(), e.svcCtx).PostComment(pcValidReq())

			wantNoErr(t, tc.method+" 失败", err)
			wantEQ(t, tc.method+" 失败", "rpid", reply.GetRpid(), int64(501))
			wantEQ(t, tc.method+" 失败", "库存已落", e.st.comments.countRows(), 1)
			wantCount(t, tc.method+" 失败", e.st.log, tc.prefix, 1)
		})
	}
}
