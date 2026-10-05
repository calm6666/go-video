package logic

// listtasks_test.go 覆盖 ListTasks：分页兜底与上限的实际口径、asset_id/state 两个过滤条件
// 「<=0 / =0 即不过滤」的真实后果、两条 SQL 的取舍（COUNT 为 0 不再发 SELECT）、
// 排序方向，以及全程不碰缓存。

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func listTasksCall(e *env, in *rpc.ListReq) (*rpc.TasksReply, error) {
	return NewListTasksLogic(context.Background(), e.svcCtx).ListTasks(in)
}

// seedThreeTasks 布 3 条任务：asset 11 的 101/102（PENDING/PROCESSING）+ asset 22 的 103（SUCCEEDED）。
func seedThreeTasks(t *testing.T, st *store) {
	t.Helper()
	now := nowUnix()
	seedTask(t, st, &model.TranscodeTask{TaskId: 101, AssetId: 11, TemplateId: 7, State: model.TaskStatePending, Progress: 0, Ctime: now - 300, Mtime: now - 300})
	seedTask(t, st, &model.TranscodeTask{TaskId: 102, AssetId: 11, TemplateId: 7, State: model.TaskStateProcessing, Progress: 40, Ctime: now - 200, Mtime: now - 200})
	seedTask(t, st, &model.TranscodeTask{TaskId: 103, AssetId: 22, TemplateId: 8, State: model.TaskStateSucceeded, Progress: 100, Ctime: now - 100, Mtime: now - 100})
}

func taskIDs(reply *rpc.TasksReply) []int64 {
	var ids []int64
	for _, task := range reply.GetTasks() {
		ids = append(ids, task.GetTaskId())
	}
	return ids
}

// TestListTasks按asset过滤且倒序返回 钉住 WHERE 生效 + `ORDER BY task_id DESC`
// （与模板列表的 ASC 方向相反，见 TestListTemplates按id正序返回）。
func TestListTasks按asset过滤且倒序返回(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)

	got, err := listTasksCall(e, &rpc.ListReq{AssetId: 11, Pn: 1, Ps: 10})
	wantNoErr(t, "ListTasks", err)
	wantSeq(t, "轨迹", st.log, 0,
		"transcode_task.Count:11/0", "transcode_task.Select:11/0/1/10/0")
	wantEq(t, "按 asset 过滤", "total", got.GetTotal(), int32(2))
	if !slices.Equal(taskIDs(got), []int64{102, 101}) {
		t.Errorf("按 asset 过滤的结果 = %v, want [102 101]（task_id 倒序，且不含 asset 22 的 103）", taskIDs(got))
	}
	wantCount(t, "列表无缓存", st.log, "cache.", 0)
}

// TestListTasks非正asset_id等于不过滤 钉住「asset_id<=0 不是查不到、而是查全表」：
// 0 与 -1 都返回别人的任务（total=3），所以调用方漏传 asset_id 时拿到的是跨媒资列表，
// 而不是空页（README 已知缺口 5）。判别性对照是上一条的 asset_id=11 只返回 2 行。
func TestListTasks非正asset_id等于不过滤(t *testing.T) {
	for _, assetID := range []int64{0, -1, -5001} {
		e := newEnv(t)
		st := e.st
		seedThreeTasks(t, st)

		got, err := listTasksCall(e, &rpc.ListReq{AssetId: assetID, Pn: 1, Ps: 10})
		wantNoErr(t, "非正 asset_id", err)
		wantSeq(t, "轨迹", st.log, 0,
			"transcode_task.Count:0/0", "transcode_task.Select:0/0/1/10/0")
		wantEq(t, "非正 asset_id", "total", got.GetTotal(), int32(3))
		if !slices.Equal(taskIDs(got), []int64{103, 102, 101}) {
			t.Errorf("asset_id=%d 的结果 = %v, want 全表 3 行倒序", assetID, taskIDs(got))
		}
	}
}

// TestListTasks按state过滤 钉住 4 个合法状态都能作为过滤条件命中，且过滤是**逐值**的而不是
// 「非 0 即全放」。这里用 state=SUCCEEDED 与 state=PENDING 各取到互不相交的一行。
func TestListTasks按state过滤(t *testing.T) {
	cases := []struct {
		state rpc.TaskState
		want  []int64
	}{
		{rpc.TaskState_TASK_STATE_PENDING, []int64{101}},
		{rpc.TaskState_TASK_STATE_PROCESSING, []int64{102}},
		{rpc.TaskState_TASK_STATE_SUCCEEDED, []int64{103}},
	}
	for _, tc := range cases {
		e := newEnv(t)
		st := e.st
		seedThreeTasks(t, st)

		got, err := listTasksCall(e, &rpc.ListReq{State: tc.state, Pn: 1, Ps: 10})
		wantNoErr(t, "state 过滤", err)
		wantSeq(t, "轨迹", st.log, 0,
			"transcode_task.Count:0/"+itoa(int32(tc.state)),
			"transcode_task.Select:0/"+itoa(int32(tc.state))+"/1/10/0")
		if !slices.Equal(taskIDs(got), tc.want) {
			t.Errorf("state=%v 的结果 = %v, want %v", tc.state, taskIDs(got), tc.want)
		}
	}
}

// TestListTasks的state等于0无法表达未指定态 钉住 model 的 `if state > 0` 把 UNSPECIFIED(0)
// 当成「不过滤」：库里那条 state=0 的脏数据（迁移列默认值就是 0，见
// 000001_create_transcode_tables.sql:16）在**任何**按状态过滤的查询里都取不到，
// 只会在不过滤的查询里混出来。判别性：state=1 取到 1 行，state=0 取到全部 2 行（含脏数据）。
func TestListTasks的state等于0无法表达未指定态(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := nowUnix()
	seedTask(t, st, &model.TranscodeTask{TaskId: 111, AssetId: 11, TemplateId: 7, State: 0, Ctime: now, Mtime: now})
	seedTask(t, st, &model.TranscodeTask{TaskId: 112, AssetId: 11, TemplateId: 7, State: model.TaskStatePending, Ctime: now, Mtime: now})

	all, err := listTasksCall(e, &rpc.ListReq{AssetId: 11, Pn: 1, Ps: 10}) // state 未指定
	wantNoErr(t, "不过滤状态", err)
	if !slices.Equal(taskIDs(all), []int64{112, 111}) {
		t.Errorf("不过滤状态时 = %v, want 含 state=0 的 111", taskIDs(all))
	}

	filtered, err := listTasksCall(e, &rpc.ListReq{AssetId: 11, State: rpc.TaskState_TASK_STATE_PENDING, Pn: 1, Ps: 10})
	wantNoErr(t, "过滤 PENDING", err)
	if !slices.Equal(taskIDs(filtered), []int64{112}) {
		t.Errorf("过滤 PENDING 时 = %v, want [112]", taskIDs(filtered))
	}
	wantSeq(t, "轨迹", st.log, 0,
		"transcode_task.Count:11/0", "transcode_task.Select:11/0/1/10/0",
		"transcode_task.Count:11/1", "transcode_task.Select:11/1/1/10/0")
}

// TestListTasks越界state在触库前被拒 钉住 logic 的枚举白名单：0 以外任何不在 1~4 的取值
// （含 5 这个刚好越界的值和负值）一律 ErrInvalidState，且一次依赖都不碰。
func TestListTasks越界state在触库前被拒(t *testing.T) {
	for _, state := range []rpc.TaskState{rpc.TaskState(5), rpc.TaskState(99), rpc.TaskState(-3)} {
		e := newEnv(t)
		seedThreeTasks(t, e.st)
		got, err := listTasksCall(e, &rpc.ListReq{State: state, Pn: 1, Ps: 10})
		wantErrIs(t, "越界 state", err, model.ErrInvalidState)
		if got != nil {
			t.Errorf("state=%d 拒绝时仍返回应答 %+v", state, got)
		}
		wantNoCall(t, "越界 state", e.st.log, 0)
	}
}

// TestListTasks的ps上限两侧 钉住 50/51 这条边界，以及 model 里 `ps > 50 → 20` 的兜底
// **在 logic 之后永远走不到**（README 已知缺口 6）：51 被 logic 拒掉，根本传不到 model。
// 负数也被同一条守卫拒掉，但错误文案只说「exceeds 50」。
func TestListTasks的ps上限两侧(t *testing.T) {
	t.Run("ps=50 放行", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedThreeTasks(t, st)
		got, err := listTasksCall(e, &rpc.ListReq{AssetId: 11, Pn: 1, Ps: 50})
		wantNoErr(t, "ps=50", err)
		wantSeq(t, "轨迹", st.log, 0, "transcode_task.Count:11/0", "transcode_task.Select:11/0/1/50/0")
		wantEq(t, "ps=50", "total", got.GetTotal(), int32(2))
	})
	for _, ps := range []int32{51, 1000, -1, -20} {
		t.Run("ps="+itoa(ps)+" 被拒", func(t *testing.T) {
			e := newEnv(t)
			seedThreeTasks(t, e.st)
			_, err := listTasksCall(e, &rpc.ListReq{AssetId: 11, Pn: 1, Ps: ps})
			wantErrIs(t, "ps 越界", err, model.ErrPsTooLarge)
			wantNoCall(t, "ps 越界", e.st.log, 0)
		})
	}
}

// TestListTasks的pn与ps兜底发生在model 钉住 0 是「合法值」并被兜底成第 1 页 / 每页 20 条：
// pn=0、pn=-5、ps=0 都落到 `Select:…/1/20/0`，也就是漏传分页参数会拿到**更大**的默认页
// 而不是空页或报错（README 已知缺口 6 的另一半）。
// 注意 ps 的负数走不到这里——logic 先一步拒掉了（见 TestListTasks的ps上限两侧）。
func TestListTasks的pn与ps兜底发生在model(t *testing.T) {
	cases := []struct{ pn, ps int32 }{{0, 0}, {-5, 0}, {1, 0}}
	for _, tc := range cases {
		e := newEnv(t)
		st := e.st
		seedThreeTasks(t, st)
		got, err := listTasksCall(e, &rpc.ListReq{Pn: tc.pn, Ps: tc.ps})
		wantNoErr(t, "pn/ps 兜底", err)
		wantSeq(t, "轨迹", st.log, 0,
			"transcode_task.Count:0/0", "transcode_task.Select:0/0/1/20/0")
		wantEq(t, "pn/ps 兜底", "total", got.GetTotal(), int32(3))
		wantEq(t, "pn/ps 兜底", "返回行数", int32(len(got.GetTasks())), int32(3))
	}
}

// TestListTasks第二页取到的是倒序后的后半段 钉住 pn/ps 真的参与 offset（判别性：
// ps=2/pn=1 得 [103,102]，pn=2 得 [101]，两页并集等于全集且不相交）。
func TestListTasks第二页取到的是倒序后的后半段(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)

	first, err := listTasksCall(e, &rpc.ListReq{Pn: 1, Ps: 2})
	wantNoErr(t, "第一页", err)
	second, err := listTasksCall(e, &rpc.ListReq{Pn: 2, Ps: 2})
	wantNoErr(t, "第二页", err)

	wantSeq(t, "轨迹", st.log, 0,
		"transcode_task.Count:0/0", "transcode_task.Select:0/0/1/2/0",
		"transcode_task.Count:0/0", "transcode_task.Select:0/0/2/2/2")
	if !slices.Equal(taskIDs(first), []int64{103, 102}) {
		t.Errorf("第一页 = %v, want [103 102]", taskIDs(first))
	}
	if !slices.Equal(taskIDs(second), []int64{101}) {
		t.Errorf("第二页 = %v, want [101]", taskIDs(second))
	}
	wantEq(t, "翻页", "第二页的 total 仍是全量", second.GetTotal(), int32(3))
}

// TestListTasks越界页返回空列表但total照报 钉住「total>0 而 tasks 为空」这一姿态：
// 客户端只看 total 会以为还有数据（offset 已翻过结尾），本服务不给任何提示。
func TestListTasks越界页返回空列表但total照报(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)

	got, err := listTasksCall(e, &rpc.ListReq{Pn: 9, Ps: 10})
	wantNoErr(t, "越界页", err)
	wantSeq(t, "轨迹", st.log, 0,
		"transcode_task.Count:0/0", "transcode_task.Select:0/0/9/10/80")
	wantEq(t, "越界页", "total", got.GetTotal(), int32(3))
	wantEq(t, "越界页", "返回行数", int32(len(got.GetTasks())), int32(0))
}

// TestListTasks总数为0时不发第二条SQL 钉住 model 的 `total == 0 → return`短路（
// transcodemodel.go:107-109）：空结果只花一次 COUNT，也不会把 nil 行集冒充成 total>0。
func TestListTasks总数为0时不发第二条SQL(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)

	got, err := listTasksCall(e, &rpc.ListReq{AssetId: 99, Pn: 1, Ps: 10})
	wantNoErr(t, "空结果", err)
	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Count:99/0")
	wantEq(t, "空结果", "total", got.GetTotal(), int32(0))
	wantEq(t, "空结果", "返回行数", int32(len(got.GetTasks())), int32(0))
}

// TestListTasks的COUNT失败上抛 钉住错误包装与「COUNT 失败时绝不发 SELECT」的顺序结论。
func TestListTasks的COUNT失败上抛(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)
	st.s.fail("transcode_task.Count", errors.New("error 1053: server shutdown"))

	_, err := listTasksCall(e, &rpc.ListReq{Pn: 1, Ps: 10})
	wantErrContains(t, "COUNT 失败", err, "transcode_task List count:")
	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Count:0/0")
	wantCount(t, "COUNT 失败", st.log, "transcode_task.Select", 0)
}

// TestListTasks的SELECT失败时total被丢弃 钉住 model 的 `return nil, 0, err`：
// 已经拿到的 total 不会随错误返回，所以调用方只能按失败处理（不能拿到半截结果）。
func TestListTasks的SELECT失败时total被丢弃(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedThreeTasks(t, st)
	st.s.fail("transcode_task.Select", errors.New("error 1146: table doesn't exist"))

	got, err := listTasksCall(e, &rpc.ListReq{Pn: 1, Ps: 10})
	wantErrContains(t, "SELECT 失败", err, "transcode_task List:")
	if got != nil {
		t.Errorf("SELECT 失败仍返回应答 total=%d", got.GetTotal())
	}
	wantSeq(t, "轨迹", st.log, 0, "transcode_task.Count:0/0", "transcode_task.Select:0/0/1/10/0")
}

// itoa 让轨迹期望与子用例名能带上数值键（避免手写字符串拼接错位）。
func itoa(v int32) string { return strconv.Itoa(int(v)) }
