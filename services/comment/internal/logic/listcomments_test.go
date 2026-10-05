package logic

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// lcSeedThreeRoots 布三条互相可区分的根评论（rpid / 点赞 / 时间都不同）：
//
//	501 state=PINNED like=30 ctime=2000  ← 两种排序下都排第一（置顶优先）
//	502 state=NORMAL like= 5 ctime=3000  ← time 第二、hot 最后
//	503 state=NORMAL like=20 ctime= 500  ← hot 第二、time 最后
//
// 外加四条**不该出现在根评论列表里**的行：504 属于别的 oid、505 是 root=501 的回复、
// 506 已删除、507 审核驳回。
func lcSeedThreeRoots(t *testing.T, e *env) {
	t.Helper()
	mk := func(rpid int64, state int32, like int32, ctime int64) *model.Comment {
		c := distinctComment(rpid, 9001)
		c.Tp, c.Root, c.Parent = 1, 0, 0
		c.State, c.LikeCount, c.Ctime = state, like, ctime
		c.Mtime = ctime + 1
		return c
	}
	seedComment(t, e.st, mk(501, statePinned, 30, 2000))
	seedComment(t, e.st, mk(502, stateNormal, 5, 3000))
	seedComment(t, e.st, mk(503, stateNormal, 20, 500))
	seedComment(t, e.st, mk(504, stateNormal, 99, 9999)) // 换目标
	e.st.comments.rows[504].Oid = 9002
	r := mk(505, stateNormal, 88, 8888) // 是回复，不是根
	r.Root, r.Parent = 501, 501
	seedComment(t, e.st, r)
	seedComment(t, e.st, mk(506, stateDeleted, 77, 7777))
	seedComment(t, e.st, mk(507, stateRejected, 66, 6666))
}

func lcRpids(items []*rpc.CommentInfo) []string {
	var out []string
	for _, it := range items {
		out = append(out, itoa(it.GetRpid()))
	}
	return out
}

// lcHas 判断结果里是否混进了不该出现的 rpid。
func lcHas(rpids []string, want string) bool {
	return slices.Contains(rpids, want)
}

// TestListCommentsGuards 参数守卫表：目标与页大小非法必须在触缓存/触库前拒绝。
func TestListCommentsGuards(t *testing.T) {
	cases := []struct {
		label  string
		mutate func(*rpc.ListCommentsReq)
		want   error
	}{
		{"oid 为 0", func(r *rpc.ListCommentsReq) { r.Oid = 0 }, model.ErrInvalidTarget},
		{"oid 为负", func(r *rpc.ListCommentsReq) { r.Oid = -3 }, model.ErrInvalidTarget},
		{"ps 为 0", func(r *rpc.ListCommentsReq) { r.Ps = 0 }, model.ErrPsTooLarge},
		{"ps 为负", func(r *rpc.ListCommentsReq) { r.Ps = -1 }, model.ErrPsTooLarge},
		{"ps 超上限 49", func(r *rpc.ListCommentsReq) { r.Ps = 50 }, model.ErrPsTooLarge},
		{"ps 远超上限", func(r *rpc.ListCommentsReq) { r.Ps = 1000 }, model.ErrPsTooLarge},
		{"oid 与 ps 同时非法时按 oid 报", func(r *rpc.ListCommentsReq) { r.Oid, r.Ps = 0, 99 }, model.ErrInvalidTarget},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := &rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20}
			tc.mutate(req)

			reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(req)

			wantErrIs(t, tc.label, err, tc.want)
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
		})
	}
}

// TestListCommentsNormalizesPageNumberAndSort 页码归一与排序口径：
// pn<=0 折成 1（缓存 key 与 SQL 参数都必须是 1）；未知 sort 枚举静默按 hot（README 已知缺口 #9）。
func TestListCommentsNormalizesPageNumberAndSort(t *testing.T) {
	cases := []struct {
		label    string
		pn       int32
		sort     rpc.SortMode
		wantSort string
	}{
		{"pn=0 且未指定排序", 0, rpc.SortMode_SORT_UNSPECIFIED, "hot"},
		{"pn 为负且显式热度排序", -5, rpc.SortMode_SORT_HOT, "hot"},
		{"pn=1 且时间排序", 1, rpc.SortMode_SORT_TIME, "time"},
		{"pn=1 且未知枚举值", 1, rpc.SortMode(99), "hot"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			req := &rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: tc.pn, Ps: 20, Sort: tc.sort}

			_, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(req)
			wantNoErr(t, tc.label, err)

			key := keyList(9001, 1, tc.wantSort, 1, 20)
			wantOps(t, tc.label, e.st.log.ops, []string{
				"cache.GetList:" + key,
				"comment.ListRoots:9001/1/" + tc.wantSort + "/1/20",
				"cache.SetList:" + key,
			})
		})
	}
}

// TestListCommentsProjectsEveryField 正常路径逐字段核对投影，并确认顺序按 hot 口径
// （置顶优先 → 点赞降序）、total 是全集条数而不是本页条数。
func TestListCommentsProjectsEveryField(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, ViewerMid: 4242, Sort: rpc.SortMode_SORT_HOT, Pn: 1, Ps: 20})
	wantNoErr(t, "ListComments", err)

	wantEQ(t, "列表", "total 为全集条数", reply.GetTotal(), int32(3))
	wantStringsEQ(t, "列表", "hot 顺序", lcRpids(reply.GetComments()), []string{"501", "503", "502"})

	for i, rpid := range []int64{501, 503, 502} {
		src := e.st.comments.row(rpid)
		if src == nil {
			t.Fatalf("布景缺失 %d", rpid)
		}
		assertCommentProjected(t, "第 "+itoa(int64(i+1))+" 条投影", reply.GetComments()[i], src)
	}
	// 明确不该出现的四条：别的目标 / 回复 / 已删除 / 审核驳回。
	rpids := lcRpids(reply.GetComments())
	for _, hidden := range []int64{504, 505, 506, 507} {
		if lcHas(rpids, itoa(hidden)) {
			t.Errorf("%d 不该出现在根评论列表里：%v", hidden, rpids)
		}
	}
	// 置顶那条的状态原样透出，不被折叠成 NORMAL。
	wantEQ(t, "列表", "置顶条 state", reply.GetComments()[0].GetState(), statePinned)
}

// TestListCommentsTimeSortChangesOrder 时间排序确实换了主键（而不是只换个缓存 key）。
func TestListCommentsTimeSortChangesOrder(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Sort: rpc.SortMode_SORT_TIME, Pn: 1, Ps: 20})
	wantNoErr(t, "时间排序", err)

	wantStringsEQ(t, "时间排序", "顺序（置顶仍优先）", lcRpids(reply.GetComments()), []string{"501", "502", "503"})
	wantCount(t, "时间排序", e.st.log, "comment.ListRoots:9001/1/time/", 1)
}

// TestListCommentsPagesWithoutOverlap 分页：第 1 页与第 2 页不重不漏，total 恒为全集。
func TestListCommentsPagesWithoutOverlap(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	lc := NewListCommentsLogic(context.Background(), e.svcCtx)

	p1, err := lc.ListComments(&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 2})
	wantNoErr(t, "第 1 页", err)
	wantStringsEQ(t, "第 1 页", "rpid", lcRpids(p1.GetComments()), []string{"501", "503"})
	wantEQ(t, "第 1 页", "total", p1.GetTotal(), int32(3))

	p2, err := lc.ListComments(&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 2, Ps: 2})
	wantNoErr(t, "第 2 页", err)
	wantStringsEQ(t, "第 2 页", "rpid", lcRpids(p2.GetComments()), []string{"502"})
	wantEQ(t, "第 2 页", "total", p2.GetTotal(), int32(3))

	p9, err := lc.ListComments(&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 9, Ps: 2})
	wantNoErr(t, "越界页", err)
	wantEQ(t, "越界页", "条数", len(p9.GetComments()), 0)
	wantEQ(t, "越界页", "total 仍是全集", p9.GetTotal(), int32(3))
}

// TestListCommentsServesSecondReadFromCache 读穿一次即回填：第二次读不得再打库，
// 并且返回的是**回填时那一版**数据（新插入的评论在 TTL 内看不到）。
func TestListCommentsServesSecondReadFromCache(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	lc := NewListCommentsLogic(context.Background(), e.svcCtx)
	req := &rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20}

	first, err := lc.ListComments(req)
	wantNoErr(t, "第一次读", err)
	wantEQ(t, "第一次读", "条数", len(first.GetComments()), 3)

	// 回填之后新来一条评论（不触发失效，模拟「另一个进程写的缓存还没过期」）。
	nine := distinctComment(509, 9001)
	nine.Tp = 1
	seedComment(t, e.st, nine)

	second, err := lc.ListComments(req)
	wantNoErr(t, "第二次读", err)
	wantEQ(t, "第二次读", "条数仍是缓存版", len(second.GetComments()), 3)
	wantEQ(t, "第二次读", "total 仍是缓存版", second.GetTotal(), int32(3))
	wantCount(t, "第二次读", e.st.log, "comment.ListRoots", 1)
	wantCount(t, "第二次读", e.st.log, "cache.GetList", 2)
	wantCount(t, "第二次读", e.st.log, "cache.SetList", 1)
}

// TestListCommentsCacheIsSharedAcrossViewers 列表缓存 key 不含 viewer_mid：
// 不同观看者共用同一份缓存。将来若按查看者做黑名单/屏蔽过滤，必须同时改 key
// （README 已知缺口 #10）。
func TestListCommentsCacheIsSharedAcrossViewers(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	lc := NewListCommentsLogic(context.Background(), e.svcCtx)

	_, err := lc.ListComments(&rpc.ListCommentsReq{Oid: 9001, Tp: 1, ViewerMid: 111, Pn: 1, Ps: 20})
	wantNoErr(t, "viewer 111", err)
	before := e.st.log.snapshot()

	other, err := lc.ListComments(&rpc.ListCommentsReq{Oid: 9001, Tp: 1, ViewerMid: 222, Pn: 1, Ps: 20})
	wantNoErr(t, "viewer 222", err)

	wantOps(t, "viewer 222 的读取", e.st.log.opsFrom(before), []string{
		"cache.GetList:" + keyList(9001, 1, "hot", 1, 20),
	})
	wantStringsEQ(t, "viewer 222 的读取", "结果与 viewer 111 相同", lcRpids(other.GetComments()), []string{"501", "503", "502"})
}

// TestListCommentsServesWarmCacheWithoutTouchingDB 命中缓存时**不得**再查库，
// 且返回的就是缓存里那一版（用一条库里根本不存在的数据证明来源）。
func TestListCommentsServesWarmCacheWithoutTouchingDB(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	stale := distinctComment(601, 9001)
	stale.Tp, stale.Content, stale.LikeCount, stale.State = 1, "缓存里的旧文案", 77, stateFolded
	e.st.cache.warmList(9001, 1, "hot", 1, 20, listCachePayload(t, []*model.Comment{stale}, 42))

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})
	wantNoErr(t, "缓存命中", err)

	wantCount(t, "缓存命中", e.st.log, "comment.ListRoots", 0)
	wantEQ(t, "缓存命中", "total 取缓存值", reply.GetTotal(), int32(42))
	assertCommentProjected(t, "缓存命中投影", reply.GetComments()[0], stale)
	wantOps(t, "缓存命中", e.st.log.ops, []string{"cache.GetList:" + keyList(9001, 1, "hot", 1, 20)})
}

// TestListCommentsRejectsCorruptedCachePayload 缓存内容坏了必须报错，
// 不能静默回源掩盖问题，也不能返回伪造的空列表。
func TestListCommentsRejectsCorruptedCachePayload(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	e.st.cache.warmList(9001, 1, "hot", 1, 20, "这不是 JSON")

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})

	wantErrContains(t, "缓存被写坏", err, "unmarshal cache")
	if reply != nil {
		t.Errorf("缓存坏了却返回了 %+v", reply)
	}
	wantOps(t, "缓存被写坏", e.st.log.ops, []string{"cache.GetList:" + keyList(9001, 1, "hot", 1, 20)})
}

// TestListCommentsPropagatesCacheReadFailure 缓存读失败按错误传出，不降级成 miss 回源。
// 这是一条口径：Redis 抖动时列表接口直接失败，而不是每个请求都去打 MySQL。
func TestListCommentsPropagatesCacheReadFailure(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	boom := errors.New("comment: redis is gone")
	e.st.cache.failWith("GetList", boom)

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})

	wantErrIs(t, "缓存读失败", err, boom)
	if reply != nil {
		t.Errorf("缓存读失败却返回了 %+v", reply)
	}
	wantOps(t, "缓存读失败", e.st.log.ops, []string{"cache.GetList:" + keyList(9001, 1, "hot", 1, 20)})
}

// TestListCommentsPropagatesStoreFailure 回源失败原样传出，且不回填缓存。
func TestListCommentsPropagatesStoreFailure(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	boom := errors.New("comment: mysql is gone")
	e.st.comments.failWith("ListRoots", boom)

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})

	wantErrIs(t, "回源失败", err, boom)
	if reply != nil {
		t.Errorf("回源失败却返回了 %+v", reply)
	}
	wantOps(t, "回源失败", e.st.log.ops, []string{
		"cache.GetList:" + keyList(9001, 1, "hot", 1, 20),
		"comment.ListRoots:9001/1/hot/1/20",
	})
}

// TestListCommentsToleratesBackfillFailure 回填失败被吞：数据照样返回（README 已知缺口 #7）。
func TestListCommentsToleratesBackfillFailure(t *testing.T) {
	e := newEnv(t)
	lcSeedThreeRoots(t, e)
	e.st.cache.failWith("SetList", errors.New("comment: redis is gone"))

	reply, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})

	wantNoErr(t, "回填失败", err)
	wantEQ(t, "回填失败", "条数", len(reply.GetComments()), 3)
	wantEQ(t, "回填失败", "缓存里仍然是空的", e.st.cache.listPayload(9001, 1, "hot", 1, 20), "")
}

// TestListCommentsCachesEmptyPage 空结果也进缓存（30 秒）：没有评论的目标不会每次打库，
// 代价是新评论最长 30 秒后才可见——除非发布路径的失效打到本 key。
func TestListCommentsCachesEmptyPage(t *testing.T) {
	e := newEnv(t)
	lc := NewListCommentsLogic(context.Background(), e.svcCtx)
	req := &rpc.ListCommentsReq{Oid: 7777, Tp: 1, Pn: 1, Ps: 20}

	reply, err := lc.ListComments(req)
	wantNoErr(t, "空目标", err)
	wantEQ(t, "空目标", "条数", len(reply.GetComments()), 0)
	wantEQ(t, "空目标", "total", reply.GetTotal(), int32(0))
	if payload := e.st.cache.listPayload(7777, 1, "hot", 1, 20); payload == "" {
		t.Errorf("空结果没有被回填，后续每次读都会打库")
	}

	_, err = lc.ListComments(req)
	wantNoErr(t, "空目标第二次读", err)
	wantCount(t, "空目标第二次读", e.st.log, "comment.ListRoots", 1)
}
