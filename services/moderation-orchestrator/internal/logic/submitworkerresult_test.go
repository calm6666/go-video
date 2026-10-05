package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// workerReq 是一次 worker 回写请求（辨识度值）。
func workerReq(taskID, workerID int64, verdict rpc.Verdict, reason string) *rpc.WorkerResultReq {
	return &rpc.WorkerResultReq{TaskId: taskID, WorkerId: workerID, Verdict: verdict, Reason: reason, Ip: "203.0.113.10"}
}

// ①守卫表：task_id / verdict 都必须先于任何依赖调用被拒。
func TestSubmitWorkerResultGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.WorkerResultReq
		want error
	}{
		{"task_id 为 0", workerReq(0, workerOne, rpc.Verdict_VERDICT_REJECT, "命中关键词"), model.ErrInvalidTaskID},
		{"task_id 负数", workerReq(-501, workerOne, rpc.Verdict_VERDICT_REJECT, "命中关键词"), model.ErrInvalidTaskID},
		{"verdict 未指定", workerReq(501, workerOne, rpc.Verdict_VERDICT_UNSPECIFIED, "命中关键词"), model.ErrInvalidVerdict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, stPending, nil) // 任务真实存在，也必须在打库前被拒
			before := st.log.snapshot()
			reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).SubmitWorkerResult(tc.req)
			wantErrIs(t, "SubmitWorkerResult / "+tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("SubmitWorkerResult / %s：reply = %+v, want nil", tc.name, reply)
			}
			wantNoCall(t, "SubmitWorkerResult / "+tc.name, st, before)
			wantEQ(t, "SubmitWorkerResult / "+tc.name, "任务状态没被动过", st.task.state(501), stPending)
			wantEQ(t, "SubmitWorkerResult / "+tc.name, "没写结论", st.result.count(501), 0)
		})
	}
}

// ②正常路径：PENDING → DONE，结论落库、两把缓存失效，顺序确定。
func TestSubmitWorkerResultFromPendingAdvancesToDone(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	before := st.log.snapshot()

	reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "机审命中搬运指纹 v3"))
	wantNoErr(t, "SubmitWorkerResult", err)
	if reply == nil {
		t.Fatalf("SubmitWorkerResult：reply = nil, want 非空 EmptyReply")
	}
	wantEQ(t, "SubmitWorkerResult", "EmptyReply 一个字段都没有", reply.ProtoReflect().Descriptor().Fields().Len(), 0)
	wantCtxCarried(t, "SubmitWorkerResult", st.log)
	wantOps(t, "SubmitWorkerResult", st.log.opsFrom(before), []string{
		"result.Upsert:501:3",
		"task.UpdateState:501->3",
		"cache.DelTask:mod:task:501",
		"cache.DelResult:mod:result:501",
	})
	wantStringsEQ(t, "SubmitWorkerResult", "成功的状态迁移", st.task.advances, []string{"501:1->3"})

	row := st.result.rows[501]
	wantEQ(t, "SubmitWorkerResult 结论", "task_id", row.TaskID, int64(501))
	wantEQ(t, "SubmitWorkerResult 结论", "verdict", row.Verdict, vReject)
	wantEQ(t, "SubmitWorkerResult 结论", "reason", row.Reason, "机审命中搬运指纹 v3")
	wantEQ(t, "SubmitWorkerResult 结论", "worker_id", row.WorkerID, workerOne)
	wantEQ(t, "SubmitWorkerResult 结论", "reviewer（机审留空）", row.Reviewer, int64(0))
	assertAround(t, "SubmitWorkerResult 结论", "ctime", row.Ctime, nowUnix(), 2)
	wantEQ(t, "SubmitWorkerResult 任务", "state", st.task.state(501), stDone)
}

// ④迁移表的两条入边都要能走通：PENDING→DONE 与 PROCESSING→DONE（漏一条边就是漏测）。
func TestSubmitWorkerResultAcceptsBothLegalSourceStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		from int32
		edge string
	}{
		{"PENDING", stPending, "501:1->3"},
		{"PROCESSING", stProcessing, "501:2->3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, tc.from, nil)
			if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
				SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_PASS, "机审通过")); err != nil {
				t.Fatalf("SubmitWorkerResult：%v", err)
			}
			wantStringsEQ(t, "SubmitWorkerResult / "+tc.name, "状态迁移", st.task.advances, []string{tc.edge})
			wantEQ(t, "SubmitWorkerResult / "+tc.name, "任务终态", st.task.state(501), stDone)
		})
	}
}

// ④本域不变量：verdict=REVIEW（转人审）也把任务推到 DONE —— 本服务没有人审队列，
// 「转人审」在状态机上与「机审通过」不可区分。
// TODO(缺陷)：README 已知缺口 #5。
func TestSubmitWorkerResultReviewVerdictStillClosesTask(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REVIEW, "疑似敏感，转人审")); err != nil {
		t.Fatalf("SubmitWorkerResult：%v", err)
	}
	wantEQ(t, "SubmitWorkerResult 转人审", "结论 verdict", st.result.rows[501].Verdict, vReview)
	wantEQ(t, "SubmitWorkerResult 转人审", "任务状态", st.task.state(501), stDone)
	wantStringsEQ(t, "SubmitWorkerResult 转人审", "状态迁移", st.task.advances, []string{"501:1->3"})
}

// ④回写只能到 DONE：状态机里不存在 APPROVED/PUBLISHED，worker 回调无法直接发布（AGENTS.md §8）。
func TestSubmitWorkerResultCanNeverPublish(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	for _, v := range []rpc.Verdict{rpc.Verdict_VERDICT_PASS, rpc.Verdict_VERDICT_REVIEW, rpc.Verdict_VERDICT_REJECT} {
		if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
			SubmitWorkerResult(workerReq(501, workerOne, v, "回写")); err != nil {
			t.Fatalf("SubmitWorkerResult / %v：%v", v, err)
		}
	}
	got := st.task.state(501)
	if got != stDone {
		t.Errorf("SubmitWorkerResult 终态：state = %d, want %d（DONE）", got, stDone)
	}
	for _, forbidden := range []int32{6, 7, 8} { // 稿件态 APPROVED/SCHEDULED/PUBLISHED 不许出现在任务表
		wantEQ(t, "SubmitWorkerResult 禁态", "state 不等于 "+itoa32(forbidden), got == forbidden, false)
	}
}

// ④worker 回写幂等：同一结论重复回写只推进一次（uniq_task ⇒ 永远只有一行结论）。
func TestSubmitWorkerResultReplayAdvancesOnlyOnce(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	logic := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st))

	req := workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "机审命中搬运指纹 v3")
	if _, err := logic.SubmitWorkerResult(req); err != nil {
		t.Fatalf("第一次回写：%v", err)
	}
	firstMtime := st.task.rows[501].Mtime
	firstID := st.result.rows[501].ID
	before := st.log.snapshot()
	if _, err := logic.SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "机审命中搬运指纹 v3")); err != nil {
		t.Fatalf("第二次回写：%v", err)
	}
	// 状态迁移只发生一次（CAS 第二次被拒后 repository 幂等返回）。
	wantStringsEQ(t, "SubmitWorkerResult 重放", "状态迁移次数", st.task.advances, []string{"501:1->3"})
	wantEQ(t, "SubmitWorkerResult 重放", "任务停在 DONE", st.task.state(501), stDone)
	wantEQ(t, "SubmitWorkerResult 重放", "结论仍只有一行（uniq_task）", st.result.count(501), 1)
	wantEQ(t, "SubmitWorkerResult 重放", "结论主键未变", st.result.rows[501].ID, firstID)
	if st.task.rows[501].Mtime != firstMtime {
		t.Errorf("SubmitWorkerResult 重放：任务 mtime 被第二次回写刷新（%d → %d）", firstMtime, st.task.rows[501].Mtime)
	}
	wantOps(t, "SubmitWorkerResult 重放", st.log.opsFrom(before), []string{
		"result.Upsert:501:3",
		"task.UpdateState:501->3",
	})
	wantCount(t, "SubmitWorkerResult 重放", &callLog{ops: st.log.opsFrom(before)}, "cache.", 0) // 迁移被拒 ⇒ 连缓存失效都不做
}

// ④已终态任务再回写：不报错、状态不动，但**旧结论被整行覆盖**（审计证据丢失）。
// TODO(缺陷)：README 已知缺口 #4。
func TestSubmitWorkerResultOnDoneTaskOverwritesAuditedConclusion(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	orig := seedResult(st, 501, vReject, func(r *model.ModerationResult) {
		r.Reason = "第一轮：命中搬运指纹"
		r.WorkerID = workerOne
		r.Ctime = 1_700_000_120
	})
	beforeOrig := *orig

	reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerTwo, rpc.Verdict_VERDICT_PASS, "第二轮另一个 worker"))
	wantNoErr(t, "SubmitWorkerResult 已终态回写", err)
	if reply == nil {
		t.Fatal("SubmitWorkerResult 已终态回写：reply = nil")
	}
	wantEQ(t, "SubmitWorkerResult 已终态回写", "任务仍是 DONE", st.task.state(501), stDone)
	wantStringsEQ(t, "SubmitWorkerResult 已终态回写", "没有任何状态迁移", st.task.advances, nil)
	// 结论行被换了内容：原 REJECT 证据没了，只剩第二个 worker 的 PASS。
	wantEQ(t, "SubmitWorkerResult 已终态回写", "结论被覆盖", st.result.rows[501].Verdict, vPass)
	wantEQ(t, "SubmitWorkerResult 已终态回写", "worker 被覆盖", st.result.rows[501].WorkerID, workerTwo)
	wantEQ(t, "SubmitWorkerResult 已终态回写", "原因被覆盖", st.result.rows[501].Reason, "第二轮另一个 worker")
	if st.result.rows[501].Ctime == beforeOrig.Ctime {
		t.Errorf("SubmitWorkerResult 已终态回写：结论 ctime 应被 Upsert 刷新，实际仍是 %d", beforeOrig.Ctime)
	}
	wantEQ(t, "SubmitWorkerResult 已终态回写", "结论仍只有一行", st.result.count(501), 1)
}

// ④申诉已结案（APPEAL_DONE）后迟到的 worker 回调：结论被洗，任务状态不动 ⇒ 「翻案」白做。
// TODO(缺陷)：README 已知缺口 #4。
func TestSubmitWorkerResultAfterAppealDoneSilentlyRewritesVerdict(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealDone, nil)
	seedAppeal(st, 601, 501, func(a *model.ModerationAppeal) {
		a.State = appealStateHandled
		a.FinalVerdict = vPass
		a.FinalReason = "申诉成立，原判撤回"
		a.Handler = opAdminDan
	})
	seedResult(st, 501, vReject, nil)

	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerTwo, rpc.Verdict_VERDICT_REJECT, "迟到回调")); err != nil {
		t.Fatalf("SubmitWorkerResult 结案后回写：%v", err)
	}
	wantEQ(t, "SubmitWorkerResult 结案后回写", "任务状态不动", st.task.state(501), stAppealDone)
	wantEQ(t, "SubmitWorkerResult 结案后回写", "结论被迟到回调改写", st.result.rows[501].Verdict, vReject)
	wantEQ(t, "SubmitWorkerResult 结案后回写", "读侧结论与申诉终论分叉", st.appeal.rows[601].FinalVerdict, vPass)
}

// ④未知 task_id 的回写：既不校验任务存在（无外键），又把结论写成了孤儿行，还回成功。
// TODO(缺陷)：README 已知缺口 #4。
func TestSubmitWorkerResultOnUnknownTaskWritesOrphanConclusion(t *testing.T) {
	st := newStore()
	before := st.log.snapshot()

	reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(4242, workerOne, rpc.Verdict_VERDICT_PASS, "谁的任务？"))
	wantNoErr(t, "SubmitWorkerResult 未知任务", err)
	if reply == nil {
		t.Fatal("SubmitWorkerResult 未知任务：reply = nil")
	}
	wantOps(t, "SubmitWorkerResult 未知任务", st.log.opsFrom(before), []string{
		"result.Upsert:4242:1",
		"task.UpdateState:4242->3",
	})
	wantEQ(t, "SubmitWorkerResult 未知任务", "任务表没有这一行", st.task.count(), 0)
	wantEQ(t, "SubmitWorkerResult 未知任务", "却写了孤儿结论", st.result.count(4242), 1)
}

// 越权/无身份：worker_id 不校验、不比对任务归属 ⇒ 任何进程都能替别人的任务定结论。
// TODO(缺陷)：README 已知缺口 #5（RPC 侧无鉴权、无 worker 身份与任务认领关系）。
func TestSubmitWorkerResultHasNoWorkerIdentityCheck(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stProcessing, nil)
	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, 0, rpc.Verdict_VERDICT_PASS, "worker_id=0 也照收")); err != nil {
		t.Fatalf("SubmitWorkerResult 无身份：%v", err)
	}
	wantEQ(t, "SubmitWorkerResult 无身份", "结论落了 worker_id=0", st.result.rows[501].WorkerID, int64(0))
	wantEQ(t, "SubmitWorkerResult 无身份", "任务被推进 DONE", st.task.state(501), stDone)

	// verdict 越界（枚举里没有的 99）也原样入库并推进状态。
	st2 := newStore()
	seedTask(st2, 502, stPending, nil)
	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st2)).
		SubmitWorkerResult(workerReq(502, workerOne, rpc.Verdict(99), "未知结论")); err != nil {
		t.Fatalf("SubmitWorkerResult 未知结论：%v", err)
	}
	wantEQ(t, "SubmitWorkerResult 未知结论", "库存 verdict", st2.result.rows[502].Verdict, vGhost)
	wantEQ(t, "SubmitWorkerResult 未知结论", "任务照样 DONE", st2.task.state(502), stDone)
}

// ③下游失败传播（结论表）：错误如实传出，任务**绝不能**被推进 DONE。
func TestSubmitWorkerResultPropagatesUpsertFailureWithoutAdvancing(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	injected := errors.New("injected result insert timeout")
	st.result.failWith("Upsert", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_PASS, "机审通过"))
	wantErrIs(t, "SubmitWorkerResult 结论写失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitWorkerResult 结论写失败：reply = %+v, want nil（失败不得伪装成已受理）", reply)
	}
	wantOps(t, "SubmitWorkerResult 结论写失败", st.log.opsFrom(before), []string{"result.Upsert:501:1"})
	wantEQ(t, "SubmitWorkerResult 结论写失败", "任务仍待审", st.task.state(501), stPending)
	wantEQ(t, "SubmitWorkerResult 结论写失败", "无状态迁移", len(st.task.advances), 0)
	wantCount(t, "SubmitWorkerResult 结论写失败", st.log, "cache.", 0)
}

// ③下游失败传播（任务表 CAS）：错误传出，但**结论已经落库** ⇒ 两写无事务，状态与结论分叉。
// 且缓存没失效，GetTask 仍回旧的 PENDING、GetResult 已能读到 PASS。
// TODO(缺陷)：README 已知缺口 #2（无事务/无 Outbox）。
func TestSubmitWorkerResultPropagatesStateFailureAndLeavesHalfWritten(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	injected := errors.New("injected update state timeout")
	st.task.failWith("UpdateState", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_PASS, "机审通过"))
	wantErrIs(t, "SubmitWorkerResult 状态推进失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitWorkerResult 状态推进失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "SubmitWorkerResult 状态推进失败", st.log.opsFrom(before), []string{
		"result.Upsert:501:1",
		"task.UpdateState:501->3",
	})
	wantEQ(t, "SubmitWorkerResult 状态推进失败", "半截结论已落库", st.result.rows[501].Verdict, vPass)
	wantEQ(t, "SubmitWorkerResult 状态推进失败", "任务仍 PENDING", st.task.state(501), stPending)
	wantCount(t, "SubmitWorkerResult 状态推进失败", st.log, "cache.", 0)
}

// 缓存失效失败被吞（`_ = r.cache.DelTask/DelResult`）：状态已改但脏缓存再活 60s。
// TODO(缺陷)：README 已知缺口 #7。
func TestSubmitWorkerResultCacheInvalidationFailureIsSwallowed(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	st.cache.warmTask(st.task.rows[501]) // 脏缓存：PENDING
	st.cache.failWith("DelTask", errors.New("injected redis del timeout"))
	st.cache.failWith("DelResult", errors.New("injected redis del timeout"))

	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_REJECT, "机审拒绝")); err != nil {
		t.Fatalf("SubmitWorkerResult 缓存失效失败：%v", err)
	}
	wantEQ(t, "SubmitWorkerResult 缓存失效失败", "库里已 DONE", st.task.state(501), stDone)
	cached, err := decodeTask(st.cache.raw(keyTask(501)))
	wantNoErr(t, "SubmitWorkerResult 缓存失效失败", err)
	wantEQ(t, "SubmitWorkerResult 缓存失效失败", "缓存仍是 PENDING（下一次 GetTask 会读到旧态）", cached.State, stPending)
	wantOps(t, "SubmitWorkerResult 缓存失效失败", st.log.ops, []string{
		"result.Upsert:501:3",
		"task.UpdateState:501->3",
		"cache.DelTask:mod:task:501",
		"cache.DelResult:mod:result:501",
	})
}

// 回写不读规则表、不建申诉：本域只推进任务态 + 落结论。
func TestSubmitWorkerResultTouchesNothingElse(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stPending, nil)
	seedAppeal(st, 601, 501, nil)
	if _, err := NewSubmitWorkerResultLogic(testCtx(), newTestSvc(st)).
		SubmitWorkerResult(workerReq(501, workerOne, rpc.Verdict_VERDICT_PASS, "机审通过")); err != nil {
		t.Fatalf("SubmitWorkerResult：%v", err)
	}
	wantCount(t, "SubmitWorkerResult 边界", st.log, "rule.", 0)
	wantCount(t, "SubmitWorkerResult 边界", st.log, "appeal.", 0)
	wantCount(t, "SubmitWorkerResult 边界", st.log, "task.FindOne", 0)
	wantEQ(t, "SubmitWorkerResult 边界", "申诉行未被动", st.appeal.rows[601].State, appealStatePending)
}
