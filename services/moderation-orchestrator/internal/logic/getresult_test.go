package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// ①守卫：task_id 非法必须在打库/打缓存之前被拒。
func TestGetResultGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		id   int64
	}{
		{"零", 0},
		{"负一", -1},
		{"负数", -4242},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedResult(st, 501, vReject, nil) // 库里确实有结论，也不能被读
			before := st.log.snapshot()
			reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: tc.id})
			wantErrIs(t, "GetResult / task_id="+tc.name, err, model.ErrInvalidTaskID)
			if reply != nil {
				t.Errorf("GetResult / task_id=%d：reply = %+v, want nil", tc.id, reply)
			}
			wantNoCall(t, "GetResult / task_id="+tc.name, st, before)
		})
	}
}

// ②正常路径：逐字段投影 + 回填，顺序确定。
func TestGetResultReadsThroughAndBackfillsCache(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	row := seedResult(st, 501, vReject, nil)
	before := st.log.snapshot()

	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantNoErr(t, "GetResult", err)
	wantOps(t, "GetResult", st.log.opsFrom(before), []string{
		"cache.GetResult:mod:result:501",
		"result.FindOne:501",
		"cache.SetResult:mod:result:501",
	})
	wantCtxCarried(t, "GetResult", st.log)
	wantResultFields(t, "GetResult 投影", reply.GetResult(), resultRPCOf(row))

	got, err := decodeResult(st.cache.raw(keyResult(501)))
	wantNoErr(t, "GetResult 回填内容", err)
	wantEQ(t, "GetResult 回填内容", "TaskID", got.TaskID, int64(501))
	wantEQ(t, "GetResult 回填内容", "Verdict", got.Verdict, vReject)
	wantEQ(t, "GetResult 回填内容", "WorkerID", got.WorkerID, workerOne)
}

// ③缓存命中不查库；结论与库分叉时客户端拿到的是缓存里那份。
func TestGetResultCacheHitNeverTouchesDatabase(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	inDB := seedResult(st, 501, vReject, nil)
	stale := *inDB
	stale.Verdict = vPass
	stale.Reason = "缓存里的旧结论"
	stale.Ctime = 1_800_000_999
	st.cache.warmResult(&stale)

	before := st.log.snapshot()
	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantNoErr(t, "GetResult 命中", err)
	wantOps(t, "GetResult 命中", st.log.opsFrom(before), []string{"cache.GetResult:mod:result:501"})
	wantEQ(t, "GetResult 命中", "verdict", reply.GetResult().GetVerdict(), rpc.Verdict_VERDICT_PASS)
	wantEQ(t, "GetResult 命中", "reason", reply.GetResult().GetReason(), "缓存里的旧结论")
	wantCount(t, "GetResult 命中", st.log, "result.FindOne", 0)
}

// 脏缓存必须回源，不能回一个 verdict=0 的「未指定结论」。
func TestGetResultFallsBackToDatabaseOnUnusableCacheEntry(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"不是 JSON", []byte("{")},
		{"空对象", []byte(`{}`)},
		{"task_id 为 0", []byte(`{"TaskID":0,"Verdict":1}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, stDone, nil)
			row := seedResult(st, 501, vReview, nil)
			st.cache.warm(keyResult(501), tc.payload)

			before := st.log.snapshot()
			reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
			wantNoErr(t, "GetResult 脏缓存", err)
			wantOps(t, "GetResult 脏缓存", st.log.opsFrom(before), []string{
				"cache.GetResult:mod:result:501",
				"result.FindOne:501",
				"cache.SetResult:mod:result:501",
			})
			wantResultFields(t, "GetResult 脏缓存", reply.GetResult(), resultRPCOf(row))
		})
	}
}

// 任务存在但还没出结论：必须是 ErrResultNotFound，而不是 verdict=UNSPECIFIED 的假结论。
// （AGENTS.md §8「审核不可用时新内容不得默认放行」——回空结论会被客户端当成 PASS 用。）
func TestGetResultWithoutConclusionIsNotFound(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	before := st.log.snapshot()

	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantErrIs(t, "GetResult 无结论", err, model.ErrResultNotFound)
	if reply != nil {
		t.Errorf("GetResult 无结论：reply = %+v, want nil", reply)
	}
	wantOps(t, "GetResult 无结论", st.log.opsFrom(before), []string{
		"cache.GetResult:mod:result:501",
		"result.FindOne:501",
	})
	wantEQ(t, "GetResult 无结论", "不落脏缓存", st.cache.has(keyResult(501)), false)
}

// ③下游失败传播（resultMd）：错误如实传出。
func TestGetResultPropagatesDatabaseFailure(t *testing.T) {
	st := newStore()
	seedResult(st, 501, vPass, nil)
	injected := errors.New("injected mysql gone away")
	st.result.failWith("FindOne", injected)

	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantErrIs(t, "GetResult 读库失败", err, injected)
	if reply != nil {
		t.Errorf("GetResult 读库失败：reply = %+v, want nil", reply)
	}
	wantCount(t, "GetResult 读库失败", st.log, "cache.SetResult", 0)
}

// 缓存读失败降级回源。
func TestGetResultCacheReadFailureDegradesToDatabase(t *testing.T) {
	st := newStore()
	row := seedResult(st, 501, vReject, nil)
	st.cache.failWith("GetResult", errors.New("injected redis dial timeout"))

	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantNoErr(t, "GetResult 缓存读失败", err)
	wantResultFields(t, "GetResult 缓存读失败", reply.GetResult(), resultRPCOf(row))
}

// 缓存写失败被吞（`_ = r.cache.SetResult`）。TODO(缺陷)：README 缺口 #13。
func TestGetResultCacheBackfillFailureIsSwallowed(t *testing.T) {
	st := newStore()
	row := seedResult(st, 501, vReject, nil)
	st.cache.failWith("SetResult", errors.New("injected redis write timeout"))

	reply, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501})
	wantNoErr(t, "GetResult 回填失败", err)
	wantResultFields(t, "GetResult 回填失败", reply.GetResult(), resultRPCOf(row))
	wantEQ(t, "GetResult 回填失败", "缓存仍为空", st.cache.has(keyResult(501)), false)
}

// GetResult 完全不碰任务表：它无法判断结论所属任务是否已终态（缺口 #4 的读侧表现）。
func TestGetResultNeverReadsTaskTable(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	seedResult(st, 501, vPass, nil)

	if _, err := NewGetResultLogic(testCtx(), newTestSvc(st)).GetResult(&rpc.ResultReq{TaskId: 501}); err != nil {
		t.Fatalf("GetResult：%v", err)
	}
	wantCount(t, "GetResult 只读结论", st.log, "task.", 0)
	wantCount(t, "GetResult 只读结论", st.log, "appeal.", 0)
	wantCount(t, "GetResult 只读结论", st.log, "result.Upsert", 0)
}
