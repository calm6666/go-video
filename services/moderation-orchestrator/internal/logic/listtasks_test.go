package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// seedThreeTasks 布三行辨识度不同的任务：mid / content_type / state 各不相同。
func seedThreeTasks(st *store) []*model.ModerationTask {
	a := seedTask(st, 501, stPending, func(t *model.ModerationTask) {
		t.Mid, t.Business, t.SubmissionID, t.ContentType = midAlice, bizVideo, 700001, ctVideo
		t.Reason = "稿件首发"
	})
	b := seedTask(st, 502, stDone, func(t *model.ModerationTask) {
		t.Mid, t.Business, t.SubmissionID, t.ContentType = midBob, bizComment, 700002, ctComment
		t.Reason = "评论举报"
		t.UpMid = 0
		t.Operator = opAdminDan
	})
	c := seedTask(st, 503, stAppealed, func(t *model.ModerationTask) {
		t.Mid, t.Business, t.SubmissionID, t.ContentType = midAlice, "danmaku.inline", 700003, ctDanmaku
		t.Reason = "弹幕复审"
	})
	return []*model.ModerationTask{a, b, c}
}

// ①守卫：ps 超限必须在打库之前被拒（本方法唯一的硬守卫）。
func TestListTasksPsGuardRunsBeforeQuery(t *testing.T) {
	for _, ps := range []int32{51, 100, 5000} {
		st := newStore()
		seedThreeTasks(st)
		before := st.log.snapshot()
		reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Ps: ps})
		wantErrIs(t, "ListTasks / ps="+itoa32(ps), err, model.ErrPsTooLarge)
		if reply != nil {
			t.Errorf("ListTasks / ps=%d：reply = %+v, want nil", ps, reply)
		}
		wantNoCall(t, "ListTasks / ps="+itoa32(ps), st, before)
	}
}

// ②入参归一：pn<=0→1、ps<=0→20，归一结果必须原样传给仓储（轨迹即断言对象）。
func TestListTasksNormalizesPagingBeforeQuery(t *testing.T) {
	cases := []struct {
		name           string
		pn, ps         int32
		wantPn, wantPs int32
		wantOps        []string
	}{
		{"默认零值", 0, 0, 1, 20, []string{"task.ListCount:0/0/0/1/20", "task.ListRows:0/0/0/1/20"}},
		{"pn 负数", -7, 0, 1, 20, []string{"task.ListCount:0/0/0/1/20", "task.ListRows:0/0/0/1/20"}},
		{"ps 负数", 1, -5, 1, 20, []string{"task.ListCount:0/0/0/1/20", "task.ListRows:0/0/0/1/20"}},
		{"ps 上界 50 合法", 2, 50, 2, 50, []string{"task.ListCount:0/0/0/2/50", "task.ListRows:0/0/0/2/50"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedThreeTasks(st)
			before := st.log.snapshot()
			req := &rpc.ListTasksReq{Pn: tc.pn, Ps: tc.ps}
			if _, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(req); err != nil {
				t.Fatalf("ListTasks：%v", err)
			}
			wantOps(t, "ListTasks 归一", st.log.opsFrom(before), tc.wantOps)
			// 归一是就地改写请求对象（gateway 若复用同一 req 会被污染），钉住这个行为。
			wantEQ(t, "ListTasks 归一", "req.pn（就地改写）", req.Pn, tc.wantPn)
			wantEQ(t, "ListTasks 归一", "req.ps（就地改写）", req.Ps, tc.wantPs)
		})
	}
}

// ②正常路径：无过滤 ⇒ 三行全回、按主键倒序、逐字段投影。
func TestListTasksProjectsEveryFieldInDescendingIDOrder(t *testing.T) {
	st := newStore()
	rows := seedThreeTasks(st)
	before := st.log.snapshot()

	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 10})
	wantNoErr(t, "ListTasks", err)
	wantCtxCarried(t, "ListTasks", st.log)
	wantOps(t, "ListTasks", st.log.opsFrom(before), []string{
		"task.ListCount:0/0/0/1/10",
		"task.ListRows:0/0/0/1/10",
	})
	wantEQ(t, "ListTasks", "total", reply.Total, int32(3))
	wantIntsEQ(t, "ListTasks", "顺序（id DESC）", taskIDs(reply.Tasks), []int64{503, 502, 501})
	for i, want := range []*model.ModerationTask{rows[2], rows[1], rows[0]} {
		wantTaskFields(t, "ListTasks 第 "+itoa(int64(i+1))+" 行", reply.Tasks[i], taskRPCOf(want))
	}
}

// ②过滤条件：mid / content_type / state 单独生效，且组合过滤是交集。
func TestListTasksFiltersAreAppliedPerColumn(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ListTasksReq
		args string
		want []int64
	}{
		{
			name: "按 mid（Alice 两行）",
			req:  &rpc.ListTasksReq{Mid: midAlice, Pn: 1, Ps: 10},
			args: "30001/0/0/1/10", want: []int64{503, 501},
		},
		{
			name: "按 mid（Bob 一行）",
			req:  &rpc.ListTasksReq{Mid: midBob, Pn: 1, Ps: 10},
			args: "30002/0/0/1/10", want: []int64{502},
		},
		{
			name: "按 content_type=评论",
			req:  &rpc.ListTasksReq{ContentType: rpc.ContentType_CONTENT_TYPE_COMMENT, Pn: 1, Ps: 10},
			args: "0/3/0/1/10", want: []int64{502},
		},
		{
			name: "按 state=已完成",
			req:  &rpc.ListTasksReq{State: rpc.TaskState_TASK_STATE_DONE, Pn: 1, Ps: 10},
			args: "0/0/3/1/10", want: []int64{502},
		},
		{
			name: "mid+state 交集",
			req:  &rpc.ListTasksReq{Mid: midAlice, State: rpc.TaskState_TASK_STATE_APPEALED, Pn: 1, Ps: 10},
			args: "30001/0/4/1/10", want: []int64{503},
		},
		{
			name: "mid+state 空交集",
			req:  &rpc.ListTasksReq{Mid: midBob, State: rpc.TaskState_TASK_STATE_PENDING, Pn: 1, Ps: 10},
			args: "30002/0/1/1/10", want: []int64{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedThreeTasks(st)
			before := st.log.snapshot()
			reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(tc.req)
			wantNoErr(t, "ListTasks 过滤", err)
			wantIntsEQ(t, "ListTasks 过滤", "命中集合", taskIDs(reply.Tasks), tc.want)
			wantEQ(t, "ListTasks 过滤", "total", reply.Total, int32(len(tc.want)))
			if len(tc.want) == 0 {
				// total==0 时 model 直接返回，不再发第二条 SELECT。
				wantOps(t, "ListTasks 过滤", st.log.opsFrom(before), []string{"task.ListCount:" + tc.args})
				return
			}
			wantOps(t, "ListTasks 过滤", st.log.opsFrom(before), []string{
				"task.ListCount:" + tc.args, "task.ListRows:" + tc.args,
			})
		})
	}
}

// ④本域不变量：过滤走的是 `> 0 才加条件`，所以「按 UNSPECIFIED 过滤」等于不过滤。
// TODO(缺陷)：登记 README 缺口 #9 —— state=0/content_type=0 无法作为查询条件，
// 运营按「未指定」筛脏数据时会拿到全表。
func TestListTasksCannotFilterByUnspecifiedEnums(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)
	ghost := seedTask(st, 504, stUnspecified, func(t *model.ModerationTask) {
		t.ContentType = ctUnspec
		t.Business, t.SubmissionID = "ghost", 700004
	})

	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{
		State:       rpc.TaskState_TASK_STATE_UNSPECIFIED,
		ContentType: rpc.ContentType_CONTENT_TYPE_UNSPECIFIED,
		Pn:          1, Ps: 10,
	})
	wantNoErr(t, "ListTasks 未指定过滤", err)
	wantIntsEQ(t, "ListTasks 未指定过滤", "实际返回全集", taskIDs(reply.Tasks), []int64{504, 503, 502, 501})
	wantEQ(t, "ListTasks 未指定过滤", "total", reply.Total, int32(4))
	// 脏状态/脏内容类型原样透传给运营后台（枚举外值）。
	wantEQ(t, "ListTasks 未指定过滤", "脏 state 透传", reply.Tasks[0].State, rpc.TaskState_TASK_STATE_UNSPECIFIED)
	wantEQ(t, "ListTasks 未指定过滤", "库存 state", ghost.State, stUnspecified)
}

// ②分页：两页不重叠且并集等于全集。
func TestListTasksPagesDoNotOverlapAndCoverEverything(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)

	p1, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 2})
	wantNoErr(t, "ListTasks 第 1 页", err)
	p2, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 2, Ps: 2})
	wantNoErr(t, "ListTasks 第 2 页", err)

	wantIntsEQ(t, "ListTasks 分页", "第 1 页", taskIDs(p1.Tasks), []int64{503, 502})
	wantIntsEQ(t, "ListTasks 分页", "第 2 页", taskIDs(p2.Tasks), []int64{501})
	wantEQ(t, "ListTasks 分页", "第 1 页 total", p1.Total, int32(3))
	wantEQ(t, "ListTasks 分页", "第 2 页 total", p2.Total, int32(3))

	if overlap := intersect(taskIDs(p1.Tasks), taskIDs(p2.Tasks)); len(overlap) != 0 {
		t.Errorf("ListTasks 分页：两页重叠 %v", overlap)
	}
}

func intersect(a, b []int64) []int64 {
	var out []int64
	for _, x := range a {
		for _, y := range b {
			if x == y {
				out = append(out, x)
			}
		}
	}
	return out
}

// 越界页：total 仍如实回，tasks 为空（不是报错）。
func TestListTasksBeyondLastPageReturnsEmptyButHonestTotal(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)
	before := st.log.snapshot()

	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 9, Ps: 2})
	wantNoErr(t, "ListTasks 越界页", err)
	wantEQ(t, "ListTasks 越界页", "total", reply.Total, int32(3))
	wantIntsEQ(t, "ListTasks 越界页", "tasks", taskIDs(reply.Tasks), []int64{})
	wantOps(t, "ListTasks 越界页", st.log.opsFrom(before), []string{
		"task.ListCount:0/0/0/9/2", "task.ListRows:0/0/0/9/2",
	})
}

// 空表：只发 COUNT，不发 SELECT。
func TestListTasksOnEmptyTableSkipsTheSecondQuery(t *testing.T) {
	st := newStore()
	before := st.log.snapshot()
	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 20})
	wantNoErr(t, "ListTasks 空表", err)
	wantOps(t, "ListTasks 空表", st.log.opsFrom(before), []string{"task.ListCount:0/0/0/1/20"})
	wantEQ(t, "ListTasks 空表", "total", reply.Total, int32(0))
	wantEQ(t, "ListTasks 空表", "tasks 非 nil", reply.Tasks == nil, false)
}

// ③下游失败传播：COUNT 失败 ⇒ 错误传出、不发 SELECT。
func TestListTasksPropagatesCountFailure(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)
	injected := errors.New("injected count timeout")
	st.task.failWith("ListCount", injected)

	before := st.log.snapshot()
	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 20})
	wantErrIs(t, "ListTasks COUNT 失败", err, injected)
	if reply != nil {
		t.Errorf("ListTasks COUNT 失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "ListTasks COUNT 失败", st.log.opsFrom(before), []string{"task.ListCount:0/0/0/1/20"})
}

// ③下游失败传播：SELECT 失败 ⇒ 错误传出（repository 把 total 丢成 0，所以只能靠 err 判定）。
func TestListTasksPropagatesRowsFailure(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)
	injected := errors.New("injected select timeout")
	st.task.failWith("ListRows", injected)

	reply, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 20})
	wantErrIs(t, "ListTasks SELECT 失败", err, injected)
	if reply != nil {
		t.Errorf("ListTasks SELECT 失败：reply = %+v, want nil", reply)
	}
}

// ListTasks 全程不碰 Redis（列表不缓存）也不碰其它表。
func TestListTasksIsCacheFreeAndWriteFree(t *testing.T) {
	st := newStore()
	seedThreeTasks(st)
	seedResult(st, 501, vPass, nil)
	seedAppeal(st, 601, 501, nil)

	if _, err := NewListTasksLogic(testCtx(), newTestSvc(st)).ListTasks(&rpc.ListTasksReq{Pn: 1, Ps: 20}); err != nil {
		t.Fatalf("ListTasks：%v", err)
	}
	wantCount(t, "ListTasks 不缓存", st.log, "cache.", 0)
	wantCount(t, "ListTasks 不缓存", st.log, "result.", 0)
	wantCount(t, "ListTasks 不缓存", st.log, "appeal.", 0)
	wantCount(t, "ListTasks 不缓存", st.log, "rule.", 0)
	wantCount(t, "ListTasks 不缓存", st.log, "task.UpdateState", 0)
	wantCount(t, "ListTasks 不缓存", st.log, "task.Insert", 0)
}
