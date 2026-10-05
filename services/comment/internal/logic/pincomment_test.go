package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/comment/model"
	rpc "go-video/services/comment/rpc"
)

// pcSeedPair 布两条同目标的根评论：502 已经是置顶，501 待被置顶。
func pcSeedPair(t *testing.T, e *env) {
	t.Helper()
	a := distinctComment(501, 9001)
	a.Tp, a.State, a.Mid = 1, stateNormal, 7001
	seedComment(t, e.st, a)
	b := distinctComment(502, 9001)
	b.Tp, b.State, b.Mid = 1, statePinned, 7002
	seedComment(t, e.st, b)
}

// TestPinCommentGuards 参数守卫表：rpid / oid / admin_mid 非法必须在触库前拒绝。
// 三条都是内联 errors.New（model 包里没有对应哨兵，README 已知缺口 #13）。
func TestPinCommentGuards(t *testing.T) {
	cases := []struct {
		label   string
		mutate  func(*rpc.PinCommentReq)
		wantMsg string
	}{
		{"rpid 为 0", func(r *rpc.PinCommentReq) { r.Rpid = 0 }, "comment: invalid rpid"},
		{"rpid 为负", func(r *rpc.PinCommentReq) { r.Rpid = -2 }, "comment: invalid rpid"},
		{"oid 为 0", func(r *rpc.PinCommentReq) { r.Oid = 0 }, "comment: invalid oid"},
		{"oid 为负", func(r *rpc.PinCommentReq) { r.Oid = -8 }, "comment: invalid oid"},
		{"admin_mid 为 0", func(r *rpc.PinCommentReq) { r.AdminMid = 0 }, "comment: admin_mid required"},
		{"admin_mid 为负", func(r *rpc.PinCommentReq) { r.AdminMid = -1 }, "comment: admin_mid required"},
		{"三者都为 0 时按 rpid 报", func(r *rpc.PinCommentReq) { r.Rpid, r.Oid, r.AdminMid = 0, 0, 0 }, "comment: invalid rpid"},
		{"rpid 合法、oid 非法时按 oid 报", func(r *rpc.PinCommentReq) { r.Oid, r.AdminMid = 0, 0 }, "comment: invalid oid"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()
			req := &rpc.PinCommentReq{Rpid: 501, Oid: 9001, Pin: true, AdminMid: 9009}
			tc.mutate(req)

			reply, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(req)

			wantErrMessage(t, tc.label, err, tc.wantMsg)
			if reply != nil {
				t.Errorf("%s：拒绝后仍返回了 %+v", tc.label, reply)
			}
			wantNoCall(t, tc.label, e.st, before)
			wantEQ(t, tc.label, "库存行数", e.st.comments.countRows(), 0)
		})
	}
}

// TestPinCommentPromotesAndDemotesOldPin 置顶：目标行变 PINNED、同 oid 的旧置顶变 NORMAL，
// 并失效主体缓存 + 列表缓存。admin_mid 只是守卫，**从不进入数据层**（权限校验在 gateway）。
func TestPinCommentPromotesAndDemotesOldPin(t *testing.T) {
	e := newEnv(t)
	pcSeedPair(t, e)
	e.st.cache.warmStats(9001, 1, 7, 3) // 置顶不改计数，必须原样保留

	reply, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
		&rpc.PinCommentReq{Rpid: 501, Oid: 9001, Pin: true, AdminMid: 9009})
	wantNoErr(t, "置顶", err)
	if reply == nil {
		t.Fatalf("置顶成功却返回了 nil")
	}

	wantEQ(t, "置顶后", "501 state", e.st.comments.state(501), statePinned)
	wantEQ(t, "置顶后", "旧置顶 502 被降级", e.st.comments.state(502), stateNormal)
	wantEQ(t, "置顶后", "501 content 未被改写", e.st.comments.row(501).Content, "第 501 楼的内容")
	wantEQ(t, "置顶后", "501 oid 未被改写", e.st.comments.row(501).Oid, int64(9001))
	if _, ok := e.st.cache.statsOf(9001, 1); !ok {
		t.Errorf("置顶不该失效计数缓存（置顶不改计数）")
	}
	wantCount(t, "置顶", e.st.log, "comment.FindOne", 0) // 不校验归属/权限
	wantOps(t, "置顶链路", e.st.log.ops, []string{
		"comment.SetPinned:501/9001/true",
		"cache.DelOne:" + keyOne(501),
		"cache.Invalidate:" + invalidatePattern(9001, 0), // 注意 tp=0，见下一条用例
	})
}

// TestPinCommentDoesNotInvalidateListCacheForItsTarget 缺陷钉桩（README 已知缺口 #14）：
// Repository.PinComment 用 tp=0 去失效列表缓存，而列表 key 是 cmt:list:<oid>:<tp>:...，
// 因此置顶**打不中任何真实 key**，被置顶的评论要等 TTL 到期才换到列表第一位；
// 而且列表里那条的 state 还是旧的 NORMAL（置顶标记丢失）。
func TestPinCommentDoesNotInvalidateListCacheForItsTarget(t *testing.T) {
	e := newEnv(t)
	pcSeedPair(t, e)
	e.st.cache.warmList(9001, 1, "hot", 1, 20, listCachePayload(t, []*model.Comment{e.st.comments.row(502)}, 1))

	_, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
		&rpc.PinCommentReq{Rpid: 501, Oid: 9001, Pin: true, AdminMid: 9009})
	wantNoErr(t, "置顶（缓存已预热）", err)

	stale := e.st.cache.listPayload(9001, 1, "hot", 1, 20)
	if stale == "" {
		t.Fatalf("置顶竟然失效了 tp=1 的列表缓存——缺陷已被修复，请同步删除本用例并更新 README")
	}

	before := e.st.log.snapshot()
	list, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 9001, Tp: 1, Pn: 1, Ps: 20})
	wantNoErr(t, "置顶后读列表", err)

	wantOps(t, "置顶后读列表", e.st.log.opsFrom(before), []string{
		"cache.GetList:" + keyList(9001, 1, "hot", 1, 20),
	})
	wantEQ(t, "置顶后读列表", "仍是缓存里那一版", len(list.GetComments()), 1)
	wantEQ(t, "置顶后读列表", "顺序仍是旧置顶", list.GetComments()[0].GetRpid(), int64(502))
	wantEQ(t, "置顶后读列表", "库存里 501 才是置顶", e.st.comments.state(501), statePinned)
}

// TestPinCommentUnpinOnlyAffectsPinnedRows 取消置顶带 state=3 门槛：
// 对非置顶行必须是空操作，不能把折叠/待审评论洗成 NORMAL（否则等于绕过审核改状态）。
func TestPinCommentUnpinOnlyAffectsPinnedRows(t *testing.T) {
	t.Run("取消置顶回落 NORMAL", func(t *testing.T) {
		e := newEnv(t)
		pcSeedPair(t, e)

		_, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
			&rpc.PinCommentReq{Rpid: 502, Oid: 9001, Pin: false, AdminMid: 9009})
		wantNoErr(t, "取消置顶", err)

		wantEQ(t, "取消置顶", "502 state", e.st.comments.state(502), stateNormal)
		wantEQ(t, "取消置顶", "501 不受影响", e.st.comments.state(501), stateNormal)
		wantOps(t, "取消置顶链路", e.st.log.ops, []string{
			"comment.SetPinned:502/9001/false",
			"cache.DelOne:" + keyOne(502),
			"cache.Invalidate:" + invalidatePattern(9001, 0),
		})
	})

	for _, st := range []int32{stateFolded, statePending, stateDeleted} {
		t.Run("非置顶行不被洗状态", func(t *testing.T) {
			e := newEnv(t)
			c := distinctComment(503, 9001)
			c.Tp, c.State = 1, st
			seedComment(t, e.st, c)

			_, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
				&rpc.PinCommentReq{Rpid: 503, Oid: 9001, Pin: false, AdminMid: 9009})
			wantNoErr(t, "取消不存在的置顶", err)

			wantEQ(t, "取消不存在的置顶", "state 原样保留", e.st.comments.state(503), st)
		})
	}
}

// TestPinCommentMissingTargetSilentlyDemotesOldPin 缺陷钉桩（README 已知缺口 #15）：
// 置顶一个不存在的 rpid 时，SQL 先无条件清掉本 oid 的旧置顶、再按 rpid 设新置顶，
// 第二条 UPDATE 匹配 0 行既不报错也不回滚 ⇒ 旧置顶被静默删除，且仍然返回成功。
func TestPinCommentMissingTargetSilentlyDemotesOldPin(t *testing.T) {
	e := newEnv(t)
	pcSeedPair(t, e)

	reply, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
		&rpc.PinCommentReq{Rpid: 404404, Oid: 9001, Pin: true, AdminMid: 9009})

	wantNoErr(t, "置顶不存在的评论", err)
	if reply == nil {
		t.Fatalf("置顶不存在的评论应返回成功（现状），否则本用例要改成断言错误")
	}
	wantEQ(t, "置顶不存在的评论", "旧置顶 502 被清掉", e.st.comments.state(502), stateNormal)
	wantEQ(t, "置顶不存在的评论", "库里没有新增行", e.st.comments.countRows(), 2)
	wantOps(t, "置顶不存在的评论", e.st.log.ops, []string{
		"comment.SetPinned:404404/9001/true",
		"cache.DelOne:" + keyOne(404404),
		"cache.Invalidate:" + invalidatePattern(9001, 0),
	})
}

// TestPinCommentAcceptsForeignTargetAndRevivesDeletedRow 缺陷钉桩（README 已知缺口 #15）：
// 设新置顶那条 UPDATE 只按 rpid，不校验 rpid 是否属于传入的 oid，
// 于是别的稿件的**已删除**评论会被改成 PINNED(3)——按可见口径 state NOT IN (2,5) 它重新公开可见。
func TestPinCommentAcceptsForeignTargetAndRevivesDeletedRow(t *testing.T) {
	e := newEnv(t)
	foreign := distinctComment(701, 8888) // 属于另一稿件
	foreign.Tp, foreign.State = 1, stateDeleted
	seedComment(t, e.st, foreign)

	_, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
		&rpc.PinCommentReq{Rpid: 701, Oid: 9001, Pin: true, AdminMid: 9009})
	wantNoErr(t, "跨目标置顶", err)

	row := e.st.comments.row(701)
	wantEQ(t, "跨目标置顶", "state 被改成 PINNED", row.State, statePinned)
	wantEQ(t, "跨目标置顶", "oid 仍是原目标", row.Oid, int64(8888))
	wantCount(t, "跨目标置顶", e.st.log, "comment.FindOne", 0)

	before := e.st.log.snapshot()
	list, err := NewListCommentsLogic(context.Background(), e.svcCtx).ListComments(
		&rpc.ListCommentsReq{Oid: 8888, Tp: 1, Pn: 1, Ps: 20})
	wantNoErr(t, "原目标的列表", err)
	wantCount(t, "原目标的列表", e.st.log, "comment.ListRoots", 1)
	if len(list.GetComments()) != 1 || list.GetComments()[0].GetRpid() != 701 {
		t.Fatalf("已删除评论被跨目标置顶后应重新可见（现状钉桩），实际：%v", lrRpids(list.GetComments()))
	}
	wantEQ(t, "原目标的列表", "透出的 state", list.GetComments()[0].GetState(), statePinned)
	_ = before
}

// TestPinCommentPropagatesStoreFailure 置顶写失败：错误传出，且不得留下任何缓存失效
// （否则会出现「缓存已空但状态没改」的半截状态）。
func TestPinCommentPropagatesStoreFailure(t *testing.T) {
	e := newEnv(t)
	pcSeedPair(t, e)
	boom := errors.New("comment: mysql is gone")
	e.st.comments.failWith("SetPinned", boom)

	reply, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
		&rpc.PinCommentReq{Rpid: 501, Oid: 9001, Pin: true, AdminMid: 9009})

	wantErrIs(t, "置顶写失败", err, boom)
	if reply != nil {
		t.Errorf("置顶写失败却返回了 %+v", reply)
	}
	wantEQ(t, "置顶写失败", "501 状态未变", e.st.comments.state(501), stateNormal)
	wantEQ(t, "置顶写失败", "502 仍是置顶", e.st.comments.state(502), statePinned)
	wantOps(t, "置顶写失败", e.st.log.ops, []string{"comment.SetPinned:501/9001/true"})
}

// TestPinCommentToleratesCacheFailures 缓存失效失败被吞：状态已经改了但仍返回成功
// （README 已知缺口 #7、#14）。
func TestPinCommentToleratesCacheFailures(t *testing.T) {
	for _, tc := range []struct{ method, prefix string }{
		{"DelOne", "cache.DelOne"},
		{"InvalidateListByOid", "cache.Invalidate"},
	} {
		t.Run(tc.method+" 失败", func(t *testing.T) {
			e := newEnv(t)
			pcSeedPair(t, e)
			e.st.cache.failWith(tc.method, errors.New("comment: redis is gone"))

			_, err := NewPinCommentLogic(context.Background(), e.svcCtx).PinComment(
				&rpc.PinCommentReq{Rpid: 501, Oid: 9001, Pin: true, AdminMid: 9009})

			wantNoErr(t, tc.method+" 失败", err)
			wantEQ(t, tc.method+" 失败", "状态已改", e.st.comments.state(501), statePinned)
			wantCount(t, tc.method+" 失败", e.st.log, tc.prefix, 1)
		})
	}
}
