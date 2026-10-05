package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// appealReq 是一次申诉请求（辨识度值）。
func appealReq(taskID, mid int64, content string) *rpc.AppealReq {
	return &rpc.AppealReq{TaskId: taskID, Mid: mid, Content: content, Ip: "203.0.113.11"}
}

// ①守卫表：task_id / mid / content 三条都必须先于任何依赖调用被拒。
func TestSubmitAppealGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.AppealReq
		want error
	}{
		{"task_id 为 0", appealReq(0, midAlice, "被误判"), model.ErrInvalidTaskID},
		{"task_id 负数", appealReq(-501, midAlice, "被误判"), model.ErrInvalidTaskID},
		{"mid 为 0", appealReq(501, 0, "被误判"), model.ErrInvalidMid},
		{"mid 负数", appealReq(501, -3, "被误判"), model.ErrInvalidMid},
		{"content 为空", appealReq(501, midAlice, ""), model.ErrInvalidContent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, stDone, nil)
			seedResult(st, 501, vReject, nil)
			before := st.log.snapshot()

			reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).SubmitAppeal(tc.req)
			wantErrIs(t, "SubmitAppeal / "+tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("SubmitAppeal / %s：reply = %+v, want nil", tc.name, reply)
			}
			wantNoCall(t, "SubmitAppeal / "+tc.name, st, before)
			wantEQ(t, "SubmitAppeal / "+tc.name, "申诉表没被写脏", st.appeal.countForTask(501), 0)
			wantEQ(t, "SubmitAppeal / "+tc.name, "任务仍在 DONE", st.task.state(501), stDone)
		})
	}
}

// ②正常路径（唯一的合法边 DONE → APPEALED）：逐字段投影 + 落库 + 顺序确定。
func TestSubmitAppealFromDoneCreatesPendingAppeal(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	seedResult(st, 501, vReject, nil)
	before := st.log.snapshot()

	reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midAlice, "原创证明在附件"))
	wantNoErr(t, "SubmitAppeal", err)
	wantCtxCarried(t, "SubmitAppeal", st.log)
	wantOps(t, "SubmitAppeal", st.log.opsFrom(before), []string{
		"appeal.Insert:501",
		"task.UpdateState:501->4",
		"cache.DelTask:mod:task:501",
		"cache.SetAppeal:mod:appeal:701",
	})
	wantStringsEQ(t, "SubmitAppeal", "状态迁移", st.task.advances, []string{"501:3->4"})
	wantEQ(t, "SubmitAppeal", "任务转 APPEALED", st.task.state(501), stAppealed)

	got := reply.GetAppeal()
	wantEQ(t, "SubmitAppeal 投影", "appeal_id（自增主键回传）", got.AppealId, int64(701))
	wantEQ(t, "SubmitAppeal 投影", "task_id", got.TaskId, int64(501))
	wantEQ(t, "SubmitAppeal 投影", "mid", got.Mid, midAlice)
	wantEQ(t, "SubmitAppeal 投影", "content", got.Content, "原创证明在附件")
	wantEQ(t, "SubmitAppeal 投影", "final_verdict（未处理留空）", got.FinalVerdict, rpc.Verdict_VERDICT_UNSPECIFIED)
	wantEQ(t, "SubmitAppeal 投影", "final_reason", got.FinalReason, "")
	wantEQ(t, "SubmitAppeal 投影", "handler", got.Handler, int64(0))
	assertAround(t, "SubmitAppeal 投影", "ctime", got.Ctime, nowUnix(), 2)
	assertAround(t, "SubmitAppeal 投影", "mtime", got.Mtime, nowUnix(), 2)

	row := st.appeal.rows[701]
	wantEQ(t, "SubmitAppeal 库存", "state（待处理）", row.State, appealStatePending)
	wantEQ(t, "SubmitAppeal 库存", "task_id", row.TaskID, int64(501))
	wantEQ(t, "SubmitAppeal 库存", "响应的 ctime 就是库存 ctime", got.Ctime, row.Ctime)

	cached, err := decodeAppeal(st.cache.raw(keyAppeal(701)))
	wantNoErr(t, "SubmitAppeal 回填", err)
	// TODO(缺陷)：回填的申诉对象主键是 0 ⇒ 这条缓存永远过不了 GetAppeal 的 `a.ID > 0` 判定，
	// 下一次读申诉必然回源（缓存写了等于没写）。见 README 已知缺口 #12。
	wantEQ(t, "SubmitAppeal 回填", "ID（repository 缓存早于 logic 回填主键）", cached.ID, int64(0))
	wantEQ(t, "SubmitAppeal 回填", "State", cached.State, appealStatePending)
	wantEQ(t, "SubmitAppeal 回填", "TaskID", cached.TaskID, int64(501))
	wantEQ(t, "SubmitAppeal 回填", "Content", cached.Content, "原创证明在附件")
}

// ④申诉不得覆盖原结论：机审/人审的 moderation_result 一行不许动（AGENTS.md §8 审计证据）。
func TestSubmitAppealNeverTouchesTheOriginalConclusion(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	before := *seedResult(st, 501, vReject, func(r *model.ModerationResult) {
		r.Reason = "第一轮结论：搬运指纹命中"
		r.WorkerID = workerOne
		r.Ctime = 1_700_000_120
	})
	if _, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midAlice, "原创证明在附件")); err != nil {
		t.Fatalf("SubmitAppeal：%v", err)
	}
	after := *st.result.rows[501]
	wantEQ(t, "SubmitAppeal 保结论", "结论 task_id", after.TaskID, before.TaskID)
	wantEQ(t, "SubmitAppeal 保结论", "结论 verdict", after.Verdict, before.Verdict)
	wantEQ(t, "SubmitAppeal 保结论", "结论 reason", after.Reason, before.Reason)
	wantEQ(t, "SubmitAppeal 保结论", "结论 worker_id", after.WorkerID, before.WorkerID)
	wantEQ(t, "SubmitAppeal 保结论", "结论 ctime", after.Ctime, before.Ctime)
	wantCount(t, "SubmitAppeal 保结论", st.log, "result.", 0)
}

// ④本域不变量（AGENTS.md §8 + 本服务注释「申诉只能在任务 DONE 后发起」）：
// 目前只有 DONE 会真的推进状态，但**其它任何状态（含任务不存在）都能申诉成功**。
// 申诉行照插、接口照回 200，只是任务状态原地不动 ⇒ 未出结论的内容可以被「申诉」污染，
// 运营工作台会收到一堆挂不上任务的申诉。
// TODO(缺陷)：README 已知缺口 #10。修好后本用例应改成 wantErrIs(ErrInvalidStateTransition)。
func TestSubmitAppealIsAcceptedInEveryIllegalTaskState(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		seed  bool
	}{
		{"任务不存在", -1, false},
		{"UNSPECIFIED(脏值)", stUnspecified, true},
		{"PENDING（还没机审）", stPending, true},
		{"PROCESSING（机审中）", stProcessing, true},
		{"APPEALED（已在申诉中）", stAppealed, true},
		{"APPEAL_DONE（申诉已结案）", stAppealDone, true},
		{"CANCELED（已撤销）", stCanceled, true},
		{"枚举外的脏值", stGhost, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			wantState := int32(-1) // -1 = 替身约定的「行不存在」
			if tc.seed {
				wantState = tc.state
				seedTask(st, 501, tc.state, nil)
			}
			before := st.log.snapshot()

			reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
				SubmitAppeal(appealReq(501, midAlice, "未出结论也要申诉"))
			wantNoErr(t, "SubmitAppeal 非法态 / "+tc.name, err)
			wantEQ(t, "SubmitAppeal 非法态 / "+tc.name, "申诉行仍然被写入", st.appeal.countForTask(501), 1)
			wantEQ(t, "SubmitAppeal 非法态 / "+tc.name, "任务状态原地不动", st.task.state(501), wantState)
			wantStringsEQ(t, "SubmitAppeal 非法态 / "+tc.name, "没有任何状态迁移", st.task.advances, nil)
			// 但迁移确实是失败了的：repository 把 ErrInvalidStateTransition 咽了。
			wantOps(t, "SubmitAppeal 非法态 / "+tc.name, st.log.opsFrom(before), []string{
				"appeal.Insert:501",
				"task.UpdateState:501->4",
				"cache.DelTask:mod:task:501",
				"cache.SetAppeal:mod:appeal:701",
			})
			wantEQ(t, "SubmitAppeal 非法态 / "+tc.name, "回包仍给了 appeal_id", reply.GetAppeal().GetAppealId(), int64(701))
		})
	}
}

// ④重复申诉不去重：同一用户对同一任务连投两次会拿到两条待处理申诉
// （迁移里 (task_id, mid) 无唯一约束，repository 只靠状态门槛去重，而门槛又被吞）。
// TODO(缺陷)：README 已知缺口 #11。
func TestSubmitAppealReplayCreatesSecondRow(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	logic := NewSubmitAppealLogic(testCtx(), newTestSvc(st))

	first, err := logic.SubmitAppeal(appealReq(501, midAlice, "第一次申诉"))
	wantNoErr(t, "SubmitAppeal 重放 1", err)
	second, err := logic.SubmitAppeal(appealReq(501, midAlice, "第一次申诉"))
	wantNoErr(t, "SubmitAppeal 重放 2", err)

	wantEQ(t, "SubmitAppeal 重放", "第一次拿到 701", first.GetAppeal().GetAppealId(), int64(701))
	wantEQ(t, "SubmitAppeal 重放", "第二次又拿到一个新主键", second.GetAppeal().GetAppealId(), int64(702))
	wantEQ(t, "SubmitAppeal 重放", "同一任务下的申诉行数", st.appeal.countForTask(501), 2)
	wantStringsEQ(t, "SubmitAppeal 重放", "状态只推进一次", st.task.advances, []string{"501:3->4"})
	wantEQ(t, "SubmitAppeal 重放", "任务停在 APPEALED", st.task.state(501), stAppealed)
}

// ④越权申诉：mid 从不与 task.mid 比对，任何人都能替别人的稿件申诉。
// TODO(缺陷)：README 已知缺口 #10。
func TestSubmitAppealDoesNotCheckOwnership(t *testing.T) {
	st := newStore()
	task := seedTask(st, 501, stDone, nil)
	if _, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midBob, "我不是投稿人，但我替他申诉")); err != nil {
		t.Fatalf("SubmitAppeal 越权：%v", err)
	}
	wantEQ(t, "SubmitAppeal 越权", "任务属主没变", task.Mid, midAlice)
	wantEQ(t, "SubmitAppeal 越权", "申诉人却是别人", st.appeal.rows[701].Mid, midBob)
	wantEQ(t, "SubmitAppeal 越权", "任务仍被推进 APPEALED", st.task.state(501), stAppealed)
}

// ③下游失败传播（申诉表）：错误传出 ⇒ 既不推进任务状态，也不动缓存。
func TestSubmitAppealPropagatesInsertFailure(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	injected := errors.New("injected appeal insert timeout")
	st.appeal.failWith("Insert", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midAlice, "原创证明在附件"))
	wantErrIs(t, "SubmitAppeal 插入失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitAppeal 插入失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "SubmitAppeal 插入失败", st.log.opsFrom(before), []string{"appeal.Insert:501"})
	wantEQ(t, "SubmitAppeal 插入失败", "任务仍在 DONE", st.task.state(501), stDone)
	wantCount(t, "SubmitAppeal 插入失败", st.log, "task.UpdateState", 0)
	wantCount(t, "SubmitAppeal 插入失败", st.log, "cache.", 0)
}

// ③下游失败传播（任务状态 CAS）：错误传出，但**申诉行已经落库** ⇒ 两写无事务、半截数据。
// 且这次返回的是错误（与非法态被吞不同），调用方重试就会再多一行申诉。
// TODO(缺陷)：README 已知缺口 #2。
func TestSubmitAppealPropagatesStateFailureAndLeavesOrphanAppeal(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	injected := errors.New("injected update state timeout")
	st.task.failWith("UpdateState", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midAlice, "原创证明在附件"))
	wantErrIs(t, "SubmitAppeal 状态推进失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitAppeal 状态推进失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "SubmitAppeal 状态推进失败", st.log.opsFrom(before), []string{
		"appeal.Insert:501",
		"task.UpdateState:501->4",
	})
	wantEQ(t, "SubmitAppeal 状态推进失败", "申诉行残留", st.appeal.countForTask(501), 1)
	wantEQ(t, "SubmitAppeal 状态推进失败", "任务仍在 DONE（结论与申诉口径分叉）", st.task.state(501), stDone)
	wantCount(t, "SubmitAppeal 状态推进失败", st.log, "cache.", 0)
}

// 缓存写失败被吞：申诉与状态都已落库，接口仍成功，只是申诉详情缓存没回填。
// TODO(缺陷)：README 已知缺口 #7。
func TestSubmitAppealCacheFailureIsSwallowed(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stDone, nil)
	st.cache.failWith("DelTask", errors.New("injected redis timeout"))
	st.cache.failWith("SetAppeal", errors.New("injected redis timeout"))

	reply, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
		SubmitAppeal(appealReq(501, midAlice, "原创证明在附件"))
	wantNoErr(t, "SubmitAppeal 缓存失败", err)
	wantEQ(t, "SubmitAppeal 缓存失败", "申诉已落库", st.appeal.rows[701].Content, "原创证明在附件")
	wantEQ(t, "SubmitAppeal 缓存失败", "任务已 APPEALED", st.task.state(501), stAppealed)
	wantEQ(t, "SubmitAppeal 缓存失败", "但两个缓存都没动", st.cache.has(keyAppeal(701)), false)
	wantEQ(t, "SubmitAppeal 缓存失败", "任务缓存也没失效", st.cache.has(keyTask(501)), false)
	wantEQ(t, "SubmitAppeal 缓存失败", "task_id 照常回传", reply.GetAppeal().GetTaskId(), int64(501))
}

// content 无长度/空白校验：空格、超长（列宽 VARCHAR(1000））都照收。
// TODO(缺陷)：README 已知缺口 #8。
func TestSubmitAppealHasNoContentLengthOrBlankGuard(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"纯空白", "   "},
		{"换行", "\n"},
		{"超长 2000 字符", string(make([]byte, 2000))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, stDone, nil)
			if _, err := NewSubmitAppealLogic(testCtx(), newTestSvc(st)).
				SubmitAppeal(appealReq(501, midAlice, tc.content)); err != nil {
				t.Fatalf("SubmitAppeal / %s：%v", tc.name, err)
			}
			wantEQ(t, "SubmitAppeal / "+tc.name, "长度原样入库", len(st.appeal.rows[701].Content), len(tc.content))
			wantEQ(t, "SubmitAppeal / "+tc.name, "任务已推进", st.task.state(501), stAppealed)
		})
	}
}
