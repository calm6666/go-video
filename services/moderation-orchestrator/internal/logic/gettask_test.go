package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// ①守卫：task_id 非法必须在打库/打缓存之前被拒（调用轨迹为空）。
func TestGetTaskGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		id   int64
	}{
		{"零", 0},
		{"负一", -1},
		{"负数", -999999},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()
			reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: tc.id})
			wantErrIs(t, "GetTask / task_id="+tc.name, err, model.ErrInvalidTaskID)
			if reply != nil {
				t.Errorf("GetTask / task_id=%d：reply = %+v, want nil（错误路径不得返回半成品）", tc.id, reply)
			}
			wantNoCall(t, "GetTask / task_id="+tc.name, st, before)
		})
	}
}

// ②正常路径：miss ⇒ 读穿 + 逐字段投影 + 回填缓存，调用顺序确定。
func TestGetTaskReadsThroughAndBackfillsCache(t *testing.T) {
	st := newStore()
	row := seedTask(st, 501, stDone, nil)
	before := st.log.snapshot()

	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
	wantNoErr(t, "GetTask", err)
	wantOps(t, "GetTask", st.log.opsFrom(before), []string{
		"cache.GetTask:mod:task:501",
		"task.FindOne:501",
		"cache.SetTask:mod:task:501",
	})
	wantCtxCarried(t, "GetTask", st.log)
	wantTaskFields(t, "GetTask 投影", reply.GetTask(), taskRPCOf(row))

	// 回填的必须是刚读出来的那一行（不是别的任务、也不是空壳）。
	got, err := decodeTask(st.cache.raw(keyTask(501)))
	wantNoErr(t, "GetTask 回填内容", err)
	wantEQ(t, "GetTask 回填内容", "ID", got.ID, int64(501))
	wantEQ(t, "GetTask 回填内容", "State", got.State, stDone)
	wantEQ(t, "GetTask 回填内容", "Business", got.Business, bizVideo)
	wantEQ(t, "GetTask 回填内容", "SubmissionID", got.SubmissionID, subAlice)
}

// ③缓存命中必须完全不查库；用「缓存与库分叉」证明读的是缓存。
// 这正是 SubmitForReview 重复送审污染缓存（缺口 #3）后客户端看到的状态。
func TestGetTaskCacheHitNeverTouchesDatabase(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil) // 库里还是 PENDING
	stale := *st.task.rows[501]
	stale.State = stDone
	stale.Mtime = 1_800_000_000
	st.cache.warmTask(&stale) // 缓存里已经是 DONE

	before := st.log.snapshot()
	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
	wantNoErr(t, "GetTask 命中", err)
	wantOps(t, "GetTask 命中", st.log.opsFrom(before), []string{"cache.GetTask:mod:task:501"})
	wantEQ(t, "GetTask 命中", "state", reply.GetTask().GetState(), rpc.TaskState_TASK_STATE_DONE)
	wantEQ(t, "GetTask 命中", "mtime", reply.GetTask().GetMtime(), int64(1_800_000_000))
	wantCount(t, "GetTask 命中", st.log, "task.FindOne", 0)
}

// ④缓存脏值（不可解析 / 没有主键）必须当 miss 回源，而不是把空对象回给调用方。
func TestGetTaskFallsBackToDatabaseOnUnusableCacheEntry(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"不是 JSON", []byte("not-json")},
		{"空对象", []byte(`{}`)},
		{"主键为 0", []byte(`{"ID":0,"State":3}`)},
		{"空字节", []byte("")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			row := seedTask(st, 501, stProcessing, nil)
			st.cache.warm(keyTask(501), tc.payload)

			before := st.log.snapshot()
			reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
			wantNoErr(t, "GetTask 脏缓存", err)
			wantOps(t, "GetTask 脏缓存", st.log.opsFrom(before), []string{
				"cache.GetTask:mod:task:501",
				"task.FindOne:501",
				"cache.SetTask:mod:task:501",
			})
			wantTaskFields(t, "GetTask 脏缓存", reply.GetTask(), taskRPCOf(row))
			// 脏值必须被库里的真值覆盖掉。
			got, err := decodeTask(st.cache.raw(keyTask(501)))
			wantNoErr(t, "GetTask 脏缓存重填", err)
			wantEQ(t, "GetTask 脏缓存重填", "State", got.State, stProcessing)
		})
	}
}

// 任务不存在：GetTask 必须报 ErrTaskNotFound，并且不得把「不存在」缓存化。
func TestGetTaskUnknownTaskIsNotFound(t *testing.T) {
	st := newStore()
	before := st.log.snapshot()
	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 4242})
	wantErrIs(t, "GetTask 不存在", err, model.ErrTaskNotFound)
	if reply != nil {
		t.Errorf("GetTask 不存在：reply = %+v, want nil", reply)
	}
	wantOps(t, "GetTask 不存在", st.log.opsFrom(before), []string{
		"cache.GetTask:mod:task:4242",
		"task.FindOne:4242",
	})
	wantEQ(t, "GetTask 不存在", "缓存里有没有条目", st.cache.has(keyTask(4242)), false)
}

// ③下游失败传播（taskMd）：错误如实传出，且不落任何缓存。
func TestGetTaskPropagatesDatabaseFailure(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	injected := errors.New("injected mysql gone away")
	st.task.failWith("FindOne", injected)

	before := st.log.snapshot()
	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
	wantErrIs(t, "GetTask 读库失败", err, injected)
	if reply != nil {
		t.Errorf("GetTask 读库失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "GetTask 读库失败", st.log.opsFrom(before), []string{
		"cache.GetTask:mod:task:501",
		"task.FindOne:501",
	})
	wantCount(t, "GetTask 读库失败", st.log, "cache.SetTask", 0)
}

// 缓存读失败必须降级回源（本服务的 Cache 读错不冒泡，这点与 comment 相反）。
func TestGetTaskCacheReadFailureDegradesToDatabase(t *testing.T) {
	st := newStore()
	row := seedTask(st, 501, stAppealed, nil)
	injected := errors.New("injected redis dial timeout")
	st.cache.failWith("GetTask", injected)

	before := st.log.snapshot()
	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
	wantNoErr(t, "GetTask 缓存读失败", err)
	wantOps(t, "GetTask 缓存读失败", st.log.opsFrom(before), []string{
		"cache.GetTask:mod:task:501",
		"task.FindOne:501",
		"cache.SetTask:mod:task:501",
	})
	wantTaskFields(t, "GetTask 缓存读失败", reply.GetTask(), taskRPCOf(row))
}

// 缓存写失败被 repository 吞掉（`_ = r.cache.SetTask`）：读接口仍返回数据。
// TODO(缺陷)：登记 README 缺口 #7（写侧吞错，缓存与库的分叉无人知晓）。
func TestGetTaskCacheBackfillFailureIsSwallowed(t *testing.T) {
	st := newStore()
	row := seedTask(st, 501, stPending, nil)
	injected := errors.New("injected redis write timeout")
	st.cache.failWith("SetTask", injected)

	reply, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501})
	wantNoErr(t, "GetTask 回填失败", err)
	wantTaskFields(t, "GetTask 回填失败", reply.GetTask(), taskRPCOf(row))
	wantEQ(t, "GetTask 回填失败", "缓存没被写脏", st.cache.has(keyTask(501)), false)
	wantCount(t, "GetTask 回填失败", st.log, "cache.SetTask", 1)
}

// GetTask 不写任何业务表：读侧越权/误用也不会推进状态机。
func TestGetTaskHasNoWriteSideEffects(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	if _, err := NewGetTaskLogic(testCtx(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 501}); err != nil {
		t.Fatalf("GetTask：%v", err)
	}
	wantCount(t, "GetTask 只读", st.log, "task.UpdateState", 0)
	wantCount(t, "GetTask 只读", st.log, "task.Insert", 0)
	wantCount(t, "GetTask 只读", st.log, "result.Upsert", 0)
	wantCount(t, "GetTask 只读", st.log, "appeal.", 0)
	wantCount(t, "GetTask 只读", st.log, "rule.", 0)
	wantEQ(t, "GetTask 只读", "库存状态", st.task.state(501), stPending)
}
