// listrebuildtasks_test.go 覆盖 ListRebuildTasks：keyset 分页、页大小夹取、
// 状态过滤前置校验，以及「一页只读一次死信观测值」。
package logic

import (
	"context"
	"strconv"
	"testing"

	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

// seedTask 播种一行任务（静默，不写 callLog），返回内部自增 id。
func seedTask(s *testStore, taskID, state string) int64 {
	s.taskMd.seed(&model.SearchIndexTask{
		TaskID: taskID, Scope: model.ScopePartition, ScopeValue: "1000-1999", State: state,
		CursorValue: "100", Total: 10, Processed: 5, Failed: 1,
		TargetIndex: testPrefix + "_v1_900", Alias: testPrefix,
		Operator: "admin-1", RequestID: "req-" + taskID, LastError: "",
		Ctime: 100, Mtime: 200, StartedAt: 150,
	})
	return s.taskMd.rows[len(s.taskMd.rows)-1].ID
}

func newListStore(t *testing.T) *testStore {
	t.Helper()
	return newTestStore(t, defaultOptions())
}

func listLogic(s *testStore) *ListRebuildTasksLogic {
	return NewListRebuildTasksLogic(context.Background(), s.svcCtx)
}

// 分页必须走 keyset 游标：第 1 页返回最新两行并给出下一页游标，
// 第 2 页用该游标拿到剩下的行且不再给游标（末页判定靠「取满一页」）。
func TestListRebuildTasks_PagesByKeysetCursor(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStatePending)   // id 1
	seedTask(s, "sit_B", model.TaskStateRunning)   // id 2
	seedTask(s, "sit_C", model.TaskStateSucceeded) // id 3
	s.dlq.seedCount(model.DLQStateOpen, 7)

	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{Limit: 2})
	wantNoErr(t, err)
	wantEQ(t, len(reply.Tasks), 2, "第 1 页大小")
	// ORDER BY id DESC：新任务在前，运维列表不会先看到历史噪音。
	wantEQ(t, reply.Tasks[0].TaskId, "sit_C", "第 1 页第 1 行")
	wantEQ(t, reply.Tasks[1].TaskId, "sit_B", "第 1 页第 2 行")
	wantEQ(t, reply.NextCursor, "2", "游标是内部自增 id（不是 task_id），下一页据此续翻")
	// 死信数是「整页共享」的观测值：读一次，不随行数放大。
	wantEQ(t, reply.Tasks[0].DlqCount, int64(7), "DlqCount")
	wantEQ(t, reply.Tasks[1].DlqCount, int64(7), "DlqCount")
	wantOps(t, s.ops(), "task.List:||2", "dlq.Count:open")
	wantCount(t, s.ops(), "dlq.Count", 1)

	s.resetOps()
	page2, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{Limit: 2, Cursor: reply.NextCursor})
	wantNoErr(t, err)
	wantEQ(t, len(page2.Tasks), 1, "第 2 页只剩 1 行")
	wantEQ(t, page2.Tasks[0].TaskId, "sit_A", "第 2 页第 1 行")
	wantEQ(t, page2.NextCursor, "", "未取满一页就是末页，不能再给游标（否则前端死循环）")
	wantOps(t, s.ops(), "task.List:|2|2", "dlq.Count:open")
}

// 每行的字段映射走同一个 taskToRPC：断点游标与目标索引不能在这一层丢掉
// （丢了就无法判断重建停在哪个区间）。
func TestListRebuildTasks_MapsProgressFields(t *testing.T) {
	s := newListStore(t)
	seedRunningTask(s)
	row := s.taskMd.rows[0]

	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{Limit: 1})
	wantNoErr(t, err)
	wantEQ(t, len(reply.Tasks), 1, "行数")
	got := reply.Tasks[0]
	wantEQ(t, got.TaskId, row.TaskID, "TaskId")
	wantEQ(t, got.State, model.TaskStateRunning, "State")
	wantEQ(t, got.CursorValue, "100001", "CursorValue")
	wantEQ(t, got.TargetIndex, testPrefix+"_v1_555", "TargetIndex")
	wantEQ(t, got.RequestId, "req-1", "RequestId")
	wantEQ(t, got.StartedAt, int64(150), "StartedAt")
	wantEQ(t, got.FinishedAt, int64(0), "FinishedAt")
}

// 页大小夹取：0/负数/超过 100 都回落到默认 20，合法值原样下发。
func TestListRebuildTasks_ClampsLimit(t *testing.T) {
	for _, tc := range []struct {
		limit  int32
		wantKP string
	}{
		{0, "task.List:||20"},
		{-5, "task.List:||20"},
		{100, "task.List:||100"},
		{101, "task.List:||20"},
		{3, "task.List:||3"},
	} {
		t.Run(tc.wantKP, func(t *testing.T) {
			s := newListStore(t)
			_, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{Limit: tc.limit})
			wantNoErr(t, err)
			wantOps(t, s.ops(), tc.wantKP, "dlq.Count:open")
		})
	}
}

// 状态过滤：合法值直接进 SQL 条件；非法值在触库前就报错
// （否则一次拼错的状态会静默返回空列表，看起来像「没有任务」）。
func TestListRebuildTasks_StateFilterIsValidatedBeforeQuery(t *testing.T) {
	t.Run("合法过滤", func(t *testing.T) {
		s := newListStore(t)
		seedTask(s, "sit_A", model.TaskStatePending)
		seedTask(s, "sit_B", model.TaskStateRunning)
		seedTask(s, "sit_C", model.TaskStatePending)

		reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{State: model.TaskStateRunning})
		wantNoErr(t, err)
		wantEQ(t, len(reply.Tasks), 1, "running 过滤后只剩 1 行")
		wantEQ(t, reply.Tasks[0].TaskId, "sit_B", "保留的是匹配行")
		wantOps(t, s.ops(), "task.List:running||20", "dlq.Count:open")
	})

	for _, bad := range []string{"RUNNING", "succ", "  ", "unknown"} {
		t.Run("非法状态 "+strconv.Quote(bad), func(t *testing.T) {
			s := newListStore(t)
			seedTask(s, "sit_A", model.TaskStateRunning)
			reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{State: bad})
			wantErrIs(t, err, model.ErrInvalidScope, "未知状态必须报错")
			if reply != nil {
				t.Fatalf("失败时响应必须为 nil, got %+v", reply)
			}
			wantOps(t, s.ops()) // 连 task.List 都不该发出
		})
	}
}

// 空串状态 = 不过滤（与「非法状态」区分开，别把默认值当条件）。
func TestListRebuildTasks_EmptyStateMeansNoFilter(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStatePending)
	seedTask(s, "sit_B", model.TaskStateSucceeded)
	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{State: ""})
	wantNoErr(t, err)
	wantEQ(t, len(reply.Tasks), 2, "空状态不过滤")
	wantOps(t, s.ops(), "task.List:||20", "dlq.Count:open")
}

// 游标不是数字：SQL 层错误必须传播，并且不再浪费一次死信统计。
func TestListRebuildTasks_BadCursorPropagatesBeforeObservation(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStatePending)
	s.dlq.seedCount(model.DLQStateOpen, 4)

	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{Cursor: "abc"})
	wantErr(t, err, "非法游标")
	wantContains(t, err.Error(), "abc", "错误里要带上坏游标本身")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops(), "task.List:|abc|20")
	wantNoCall(t, s.ops(), "dlq.")
}

// 查任务失败必须传播（不是返回空列表）。
func TestListRebuildTasks_ListFailurePropagates(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStatePending)
	s.fail("task.List", errOther)
	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{})
	wantErrIs(t, err, errOther, "必须传播")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantNoCall(t, s.ops(), "dlq.")
}

// 死信统计读失败：整页 dlq_count 归零，但列表本身照常成功（观测值不该有否决权）。
func TestListRebuildTasks_ObservationFailureDegradesEveryRow(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStatePending)
	seedTask(s, "sit_B", model.TaskStateRunning)
	s.dlq.seedCount(model.DLQStateOpen, 9)
	s.fail("dlq.Count", errOther)

	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{})
	wantNoErr(t, err, "死信统计失败不该让列表失败")
	wantEQ(t, len(reply.Tasks), 2, "行数")
	wantEQ(t, reply.Tasks[0].DlqCount, int64(0), "第 1 行 dlq_count 上报 0")
	wantEQ(t, reply.Tasks[1].DlqCount, int64(0), "第 2 行 dlq_count 上报 0")
	wantEQ(t, reply.Tasks[0].TaskId, "sit_B", "顺序仍是 id 倒序，没有因为统计失败而丢行")
	wantEQ(t, reply.Tasks[0].State, model.TaskStateRunning, "任务本体字段不受影响")
	wantOps(t, s.ops(), "task.List:||20", "dlq.Count:open")
}

// 无任务：空列表而不是 nil（调用方要能区分「确实没有」与「没查」）。
func TestListRebuildTasks_EmptyResultIsNotNilSlice(t *testing.T) {
	s := newListStore(t)
	reply, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{})
	wantNoErr(t, err)
	if reply.Tasks == nil {
		t.Fatalf("空结果也应返回非 nil 切片")
	}
	wantEQ(t, len(reply.Tasks), 0, "行数")
	wantEQ(t, reply.NextCursor, "", "空页不给游标")
	wantOps(t, s.ops(), "task.List:||20", "dlq.Count:open")
}

// 列表只读自己的表，绝不碰 OpenSearch 与缓存。
func TestListRebuildTasks_TouchesNoIndexOrCache(t *testing.T) {
	s := newListStore(t)
	seedTask(s, "sit_A", model.TaskStateRunning)
	_, err := listLogic(s).ListRebuildTasks(&rpc.ListRebuildTasksReq{State: model.TaskStateRunning})
	wantNoErr(t, err)
	wantNoCall(t, s.ops(), "es.")
	wantNoCall(t, s.ops(), "cache.")
	wantNoCall(t, s.ops(), "version.")
}
