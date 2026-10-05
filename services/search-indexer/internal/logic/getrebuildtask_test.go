// getrebuildtask_test.go 覆盖 GetRebuildTask：任务本体查询与「观测值不得拖垮主查询」。
package logic

import (
	"context"
	"testing"

	"go-video/services/search-indexer/model"
	"go-video/services/search-indexer/rpc"
)

func seedRunningTask(s *testStore) *model.SearchIndexTask {
	task := &model.SearchIndexTask{
		TaskID: "sit_TASK00000000000000000", Scope: model.ScopePartition, ScopeValue: "1000-1999",
		State: model.TaskStateRunning, CursorValue: "100001", Total: 900, Processed: 500, Failed: 3,
		TargetIndex: testPrefix + "_v1_555", Alias: testPrefix, Operator: "admin-1",
		RequestID: "req-1", LastError: "esclient: HTTP 503", Ctime: 100, Mtime: 200,
		StartedAt: 150, FinishedAt: 0,
	}
	s.taskMd.seed(task)
	return task
}

func TestGetRebuildTask_HappyPath(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	want := seedRunningTask(s)
	s.dlq.seedCount(model.DLQStateOpen, 3)

	reply, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{TaskId: want.TaskID})
	wantNoErr(t, err)
	wantEQ(t, reply.TaskId, want.TaskID, "TaskId")
	wantEQ(t, reply.Scope, model.ScopePartition, "Scope")
	wantEQ(t, reply.ScopeValue, "1000-1999", "ScopeValue")
	wantEQ(t, reply.State, model.TaskStateRunning, "State")
	wantEQ(t, reply.CursorValue, "100001", "CursorValue（断点续跑游标必须如实上报）")
	wantEQ(t, reply.Total, int64(900), "Total")
	wantEQ(t, reply.Processed, int64(500), "Processed")
	wantEQ(t, reply.Failed, int64(3), "Failed")
	wantEQ(t, reply.TargetIndex, testPrefix+"_v1_555", "TargetIndex")
	wantEQ(t, reply.Alias, testPrefix, "Alias")
	wantEQ(t, reply.Operator, "admin-1", "Operator")
	wantEQ(t, reply.RequestId, "req-1", "RequestId")
	wantEQ(t, reply.Ctime, int64(100), "Ctime")
	wantEQ(t, reply.Mtime, int64(200), "Mtime")
	wantEQ(t, reply.StartedAt, int64(150), "StartedAt")
	wantEQ(t, reply.FinishedAt, int64(0), "未结束不得编造 FinishedAt")
	wantEQ(t, reply.LastError, "esclient: HTTP 503", "LastError（失败原因要能排障）")
	wantEQ(t, reply.DlqCount, int64(3), "DlqCount 是观测值，由调用方注入")
	wantOps(t, s.ops(),
		"task.FindOne:sit_TASK00000000000000000",
		"dlq.Count:open",
	)
}

// 失败终态必须原样可见（succeeded/failed/canceled 都是任务字段，不能美化，
// 也不能因为「已经结束了」就把断点游标抹掉）。
func TestGetRebuildTask_ReportsTerminalStates(t *testing.T) {
	for _, tc := range []struct{ state, cursor string }{
		{model.TaskStateSucceeded, "2000"},
		{model.TaskStateFailed, "1300"},
		{model.TaskStateCanceled, "80"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			s := newTestStore(t, defaultOptions())
			seedRunningTask(s)
			row := s.taskMd.rows[0]
			row.State = tc.state
			row.CursorValue = tc.cursor
			row.FinishedAt = 300
			row.LastError = "esclient: HTTP 503"

			reply, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{TaskId: row.TaskID})
			wantNoErr(t, err)
			wantEQ(t, reply.State, tc.state, "State 必须原样上报")
			wantEQ(t, reply.CursorValue, tc.cursor, "终态也要保留断点游标，否则无法人工续跑")
			wantEQ(t, reply.FinishedAt, int64(300), "FinishedAt")
			wantEQ(t, reply.LastError, "esclient: HTTP 503", "failed 的原因不能被抹平")
		})
	}
}

// 任务不存在：ErrTaskNotFound，并且在统计死信之前就返回。
func TestGetRebuildTask_NotFoundBeforeObservation(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.dlq.seedCount(model.DLQStateOpen, 9)
	reply, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{TaskId: "sit_MISSING"})
	wantErrIs(t, err, model.ErrTaskNotFound, "任务不存在")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantOps(t, s.ops(), "task.FindOne:sit_MISSING")
	wantNoCall(t, s.ops(), "dlq.")
}

// 空 task_id 不做「列表兜底」：走同一未找到分支。
func TestGetRebuildTask_EmptyTaskID(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	seedRunningTask(s)
	_, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{})
	wantErrIs(t, err, model.ErrTaskNotFound, "空 task_id")
	wantOps(t, s.ops(), "task.FindOne:")
}

// 观测值读失败：dlq_count 归零但主查询仍然成功（且留下日志）。
func TestGetRebuildTask_DeadLetterObservationFailureIsSwallowed(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	task := seedRunningTask(s)
	s.fail("dlq.Count", errOther)
	reply, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{TaskId: task.TaskID})
	wantNoErr(t, err, "死信统计失败不该让任务查询失败")
	wantEQ(t, reply.DlqCount, int64(0), "dlq_count 上报 0")
	wantEQ(t, reply.State, model.TaskStateRunning, "任务本体仍然如实返回")
	wantOps(t, s.ops(), "task.FindOne:"+task.TaskID, "dlq.Count:open")
}

// 任务表读失败必须传播。
func TestGetRebuildTask_FindFailurePropagates(t *testing.T) {
	s := newTestStore(t, defaultOptions())
	s.fail("task.FindOne", errOther)
	reply, err := NewGetRebuildTaskLogic(context.Background(), s.svcCtx).GetRebuildTask(&rpc.GetRebuildTaskReq{TaskId: "sit_X"})
	wantErrIs(t, err, errOther, "必须传播")
	if reply != nil {
		t.Fatalf("失败时响应必须为 nil, got %+v", reply)
	}
	wantNoCall(t, s.ops(), "dlq.")
}
