package logic

// listtasks_test.go 覆盖 ListTasksLogic.ListTasks。
//
// 钉住三件事：
//  1. ps 上限守卫（>50 拒绝，恰好 50 放行）发生在触库之前；
//  2. 过滤条件走的是 **model 层编号**（rpc 枚举与 model 常量不同值时靠 taskStateToModel 映射），
//     且 pn/ps 原样透传——钳制在 model 里，logic 不做二次加工；
//  3. 列表顺序就是 model 的 ORDER BY ctime DESC，logic 不重排；下游失败必须整体报错，
//     不返回 total=0 的「看起来没有数据」伪成功。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

func TestListTasksGuardsTouchNothing(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 6100, AssetID: 700, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 100})
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	for _, ps := range []int32{51, 100, 1 << 20} {
		reply, err := l.ListTasks(&rpc.ListReq{Ps: ps})
		wantErrIs(t, "ps 超上限", err, model.ErrPsTooLarge)
		wantEQ(t, "ps 超上限", "不返回半截 reply", reply == nil, true)
	}
	wantNoCall(t, "ps>50 守卫", st, 0)
}

// 恰好等于阈值必须放行（守卫写成 >= 就会把这行打掉）。
func TestListTasksPsBoundaryIsExclusive(t *testing.T) {
	st := newStore()
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListTasks(&rpc.ListReq{AssetId: 700, Pn: 1, Ps: 50})
	wantNoErr(t, "ps=50 边界", err)
	wantEQ(t, "ps=50 边界", "放行条数", reply.GetTotal(), int32(0))

	before := st.log.snapshot()
	if _, err = l.ListTasks(&rpc.ListReq{Ps: 51}); err == nil {
		t.Fatalf("ps=51 应当被拒")
	}
	wantNoCall(t, "ps=51 守卫", st, before)
}

func TestListTasksPassesFiltersThroughToModel(t *testing.T) {
	cases := []struct {
		name      string
		state     rpc.TaskState
		wantState int32
	}{
		{"UNSPECIFIED 表示不过滤", rpc.TaskState_TASK_STATE_UNSPECIFIED, 0},
		{"PENDING", rpc.TaskState_TASK_STATE_PENDING, model.TaskStatePending},
		{"SUCCEEDED", rpc.TaskState_TASK_STATE_SUCCEEDED, model.TaskStateSucceeded},
		{"FAILED", rpc.TaskState_TASK_STATE_FAILED, model.TaskStateFailed},
		{"未知枚举回落 0（不过滤）", rpc.TaskState(99), 0},
	}
	for _, tc := range cases {
		st := newStore()
		seedTask(t, st, &model.FingerprintTask{TaskID: 6111, AssetID: 700, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 100})
		l := NewListTasksLogic(context.Background(), newTestSvc(st))

		_, err := l.ListTasks(&rpc.ListReq{AssetId: 700, State: tc.state, Pn: 3, Ps: 7})
		wantNoErr(t, tc.name, err)
		wantOps(t, tc.name, st.log.ops, []string{"task.List:700/" + itoa(int64(tc.wantState)) + "/3/7"})
		// 列表读侧不碰缓存，也不碰指纹事实表。
		wantEQ(t, tc.name, "无缓存调用", st.log.countPrefix("cache."), 0)
		wantEQ(t, tc.name, "无指纹事实调用", st.log.countPrefix("rec."), 0)
	}
}

func TestListTasksProjectsRowsInCtimeDescOrder(t *testing.T) {
	st := newStore()
	// uniq_asset_fptype 决定同媒资同类型只能有一行，所以布景用 (700,800)×(video,audio) 四个组合。
	// ctime 各不相同：真实 SQL 在 ctime 相等时顺序不确定，布景不能依赖平票。
	seedTask(t, st, &model.FingerprintTask{TaskID: 6101, AssetID: 700, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 100})
	seedTask(t, st, &model.FingerprintTask{TaskID: 6102, AssetID: 700, FpType: model.FpTypeAudio, State: model.TaskStateSucceeded, AudioKey: "a6102", Ctime: 300})
	seedTask(t, st, &model.FingerprintTask{TaskID: 6103, AssetID: 800, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 200})
	seedTask(t, st, &model.FingerprintTask{TaskID: 6104, AssetID: 800, FpType: model.FpTypeAudio, State: model.TaskStateFailed, Ctime: 400})
	l := NewListTasksLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.ListTasks(&rpc.ListReq{Pn: 1, Ps: 2})
	wantNoErr(t, "首页两条", err)
	wantOps(t, "首页两条", st.log.opsFrom(before), []string{"task.List:0/0/1/2"})
	wantEQ(t, "首页两条", "total 是过滤后的总数（不是本页条数）", reply.GetTotal(), int32(4))
	wantStringsEQ(t, "首页两条", "顺序=ctime DESC", replyLines(reply.GetTasks()), []string{
		"task=6104/asset=800/type=2/vkey=/akey=/state=3",
		"task=6102/asset=700/type=2/vkey=/akey=a6102/state=2",
	})
	wantEQ(t, "首页两条", "逐字段投影（audio 类型 + FAILED 状态）",
		replyLine(reply.GetTasks()[0]), "task=6104/asset=800/type=2/vkey=/akey=/state=3")
	wantEQ(t, "首页两条", "本页 ctime 也如实投影", reply.GetTasks()[0].GetCtime(), int64(400))
	wantEQ(t, "首页两条", "本页 mtime 缺省为 0 也如实投影", reply.GetTasks()[0].GetMtime(), int64(0))
}

func TestListTasksFiltersByAssetAndState(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 6101, AssetID: 700, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 100})
	seedTask(t, st, &model.FingerprintTask{TaskID: 6102, AssetID: 700, FpType: model.FpTypeAudio, State: model.TaskStateSucceeded, Ctime: 300})
	seedTask(t, st, &model.FingerprintTask{TaskID: 6103, AssetID: 800, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 200})
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListTasks(&rpc.ListReq{AssetId: 700, State: rpc.TaskState_TASK_STATE_PENDING, Pn: 1, Ps: 10})
	wantNoErr(t, "双条件过滤", err)
	wantEQ(t, "双条件过滤", "total", reply.GetTotal(), int32(1))
	wantStringsEQ(t, "双条件过滤", "只剩 asset=700 的 PENDING", replyLines(reply.GetTasks()),
		[]string{"task=6101/asset=700/type=1/vkey=/akey=/state=1"})
}

// 越页：total 仍如实报告，tasks 为空但不是 nil（客户端要能区分「没有下一页」与「字段缺失」）。
func TestListTasksBeyondLastPageReportsTotalWithoutRows(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 6104, AssetID: 800, FpType: model.FpTypeAudio, State: model.TaskStateFailed, Ctime: 400})
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListTasks(&rpc.ListReq{State: rpc.TaskState_TASK_STATE_FAILED, Pn: 2, Ps: 1})
	wantNoErr(t, "越页", err)
	wantEQ(t, "越页", "total", reply.GetTotal(), int32(1))
	wantEQ(t, "越页", "本页条数", len(reply.GetTasks()), 0)
}

func TestListTasksEmptyAnswersNonNilSlice(t *testing.T) {
	st := newStore()
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListTasks(&rpc.ListReq{AssetId: 999, Pn: 1, Ps: 20})
	wantNoErr(t, "空列表", err)
	wantEQ(t, "空列表", "total", reply.GetTotal(), int32(0))
	if reply.GetTasks() == nil {
		t.Errorf("空列表：tasks 是 nil，客户端按缺字段处理")
	}
	wantStringsEQ(t, "空列表", "tasks", replyLines(reply.GetTasks()), []string{})
}

func TestListTasksDownstreamFailureReturnsNoPartialReply(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 6101, AssetID: 700, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 100})
	dbDown := errors.New("fingerprint_task List count: connection is dead")
	st.task.failWith("List", dbDown)
	l := NewListTasksLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListTasks(&rpc.ListReq{Pn: 1, Ps: 20})
	wantErrIs(t, "列表查询失败", err, dbDown)
	// 关键：不能返回 (total=0, tasks=[], nil) 冒充「确实没有任务」。
	wantEQ(t, "列表查询失败", "不返回伪成功", reply == nil, true)
	wantOps(t, "列表查询失败", st.log.ops, []string{"task.List:0/0/1/20"})
}
