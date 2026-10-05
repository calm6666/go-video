package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// processReq 是一次运营处理申诉的请求。
func processReq(appealID, handler int64, verdict rpc.Verdict, reason string) *rpc.ProcessAppealReq {
	return &rpc.ProcessAppealReq{
		AppealId: appealID, Handler: handler, FinalVerdict: verdict, FinalReason: reason, Ip: "203.0.113.12",
	}
}

// ①守卫表：appeal_id / handler / final_verdict / final_reason 四条都必须先于任何依赖调用被拒。
func TestProcessAppealGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		req  *rpc.ProcessAppealReq
		want error
	}{
		{"appeal_id 为 0", processReq(0, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"), model.ErrInvalidAppealID},
		{"appeal_id 负数", processReq(-601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"), model.ErrInvalidAppealID},
		{"handler 为 0", processReq(601, 0, rpc.Verdict_VERDICT_PASS, "复核成立"), model.ErrInvalidHandler},
		{"handler 负数", processReq(601, -9, rpc.Verdict_VERDICT_PASS, "复核成立"), model.ErrInvalidHandler},
		{"final_verdict 未指定", processReq(601, opAdminDan, rpc.Verdict_VERDICT_UNSPECIFIED, "复核成立"), model.ErrInvalidVerdict},
		{"final_reason 为空", processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, ""), model.ErrInvalidReason},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, stAppealed, nil)
			seedAppeal(st, 601, 501, nil)
			before := st.log.snapshot()

			reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).ProcessAppeal(tc.req)
			wantErrIs(t, "ProcessAppeal / "+tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("ProcessAppeal / %s：reply = %+v, want nil", tc.name, reply)
			}
			wantNoCall(t, "ProcessAppeal / "+tc.name, st, before)
			wantEQ(t, "ProcessAppeal / "+tc.name, "申诉仍未处理", st.appeal.rows[601].State, appealStatePending)
			wantEQ(t, "ProcessAppeal / "+tc.name, "任务仍在 APPEALED", st.task.state(501), stAppealed)
		})
	}
}

// ②正常路径（唯一的合法边 APPEALED → APPEAL_DONE）：8 步链路顺序 + 逐字段投影。
func TestProcessAppealFromAppealedClosesBothSides(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	seedResult(st, 501, vReject, nil)
	before := st.log.snapshot()

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立：原创证明充分"))
	wantNoErr(t, "ProcessAppeal", err)
	wantCtxCarried(t, "ProcessAppeal", st.log)
	wantOps(t, "ProcessAppeal", st.log.opsFrom(before), []string{
		"appeal.Update:601",
		"appeal.FindOne:601",
		"task.UpdateState:501->5",
		"cache.DelTask:mod:task:501",
		"cache.DelAppeal:mod:appeal:601",
		"cache.GetAppeal:mod:appeal:601",
		"appeal.FindOne:601",
		"cache.SetAppeal:mod:appeal:601",
	})
	wantStringsEQ(t, "ProcessAppeal", "状态迁移", st.task.advances, []string{"501:4->5"})
	wantEQ(t, "ProcessAppeal", "任务转 APPEAL_DONE", st.task.state(501), stAppealDone)

	got := reply.GetAppeal()
	wantEQ(t, "ProcessAppeal 投影", "appeal_id", got.AppealId, int64(601))
	wantEQ(t, "ProcessAppeal 投影", "task_id", got.TaskId, int64(501))
	wantEQ(t, "ProcessAppeal 投影", "mid（申诉人保留）", got.Mid, midAlice)
	wantEQ(t, "ProcessAppeal 投影", "content（申诉理由保留）", got.Content, "被误判为搬运，原创证明见附件")
	wantEQ(t, "ProcessAppeal 投影", "final_verdict", got.FinalVerdict, rpc.Verdict_VERDICT_PASS)
	wantEQ(t, "ProcessAppeal 投影", "final_reason", got.FinalReason, "复核成立：原创证明充分")
	wantEQ(t, "ProcessAppeal 投影", "handler", got.Handler, opAdminDan)
	wantEQ(t, "ProcessAppeal 投影", "ctime（受理时间不改）", got.Ctime, int64(1_700_000_200))
	assertAround(t, "ProcessAppeal 投影", "mtime（处理时间刷新）", got.Mtime, nowUnix(), 2)

	row := st.appeal.rows[601]
	wantEQ(t, "ProcessAppeal 库存", "state", row.State, appealStateHandled)
	wantEQ(t, "ProcessAppeal 库存", "final_verdict", row.FinalVerdict, vPass)
	wantEQ(t, "ProcessAppeal 库存", "handler", row.Handler, opAdminDan)
	// 回查后回填的缓存这次必须带主键（SubmitAppeal 写的那条是 ID=0 的废值）。
	cached, err := decodeAppeal(st.cache.raw(keyAppeal(601)))
	wantNoErr(t, "ProcessAppeal 回填", err)
	wantEQ(t, "ProcessAppeal 回填", "ID", cached.ID, int64(601))
	wantEQ(t, "ProcessAppeal 回填", "State", cached.State, appealStateHandled)
}

// ④审计证据：结案不得覆盖原始审核结论（moderation_result 一行不许动）。
func TestProcessAppealKeepsTheOriginalConclusion(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	before := *seedResult(st, 501, vReject, func(r *model.ModerationResult) {
		r.Reason = "机审命中搬运指纹"
		r.WorkerID = workerOne
		r.Ctime = 1_700_000_120
	})
	if _, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立")); err != nil {
		t.Fatalf("ProcessAppeal：%v", err)
	}
	after := *st.result.rows[501]
	wantEQ(t, "ProcessAppeal 保结论", "verdict 未被翻案覆盖", after.Verdict, before.Verdict)
	wantEQ(t, "ProcessAppeal 保结论", "reason 未被覆盖", after.Reason, before.Reason)
	wantEQ(t, "ProcessAppeal 保结论", "worker_id 未被覆盖", after.WorkerID, before.WorkerID)
	wantEQ(t, "ProcessAppeal 保结论", "ctime 未被覆盖", after.Ctime, before.Ctime)
	wantCount(t, "ProcessAppeal 保结论", st.log, "result.", 0)
	// 但翻案结论只落在申诉行上 ⇒ GetResult 读到的仍是 REJECT，内容所有者无从得知翻案。
	// TODO(缺陷)：README 已知缺口 #1、#13。
}

// ④重复处理必须被拒，且第一次的处理记录不许被覆盖。
func TestProcessAppealRejectsAlreadyHandledAppeal(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	logic := NewProcessAppealLogic(testCtx(), newTestSvc(st))

	if _, err := logic.ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "第一次：复核成立")); err != nil {
		t.Fatalf("ProcessAppeal 第一次：%v", err)
	}
	before := st.log.snapshot()
	reply, err := logic.ProcessAppeal(processReq(601, opAdminEve, rpc.Verdict_VERDICT_REJECT, "第二次：换个运营翻案"))
	wantErrIs(t, "ProcessAppeal 重复处理", err, model.ErrAppealAlreadyHandled)
	if reply != nil {
		t.Errorf("ProcessAppeal 重复处理：reply = %+v, want nil", reply)
	}
	wantOps(t, "ProcessAppeal 重复处理", st.log.opsFrom(before), []string{"appeal.Update:601"})
	wantEQ(t, "ProcessAppeal 重复处理", "第一次的处理人保留", st.appeal.rows[601].Handler, opAdminDan)
	wantEQ(t, "ProcessAppeal 重复处理", "第一次的最终结论保留", st.appeal.rows[601].FinalVerdict, vPass)
	wantEQ(t, "ProcessAppeal 重复处理", "第一次的说明保留", st.appeal.rows[601].FinalReason, "第一次：复核成立")
	wantEQ(t, "ProcessAppeal 重复处理", "申诉仍是已处理", st.appeal.rows[601].State, appealStateHandled)
	wantStringsEQ(t, "ProcessAppeal 重复处理", "状态只推进一次", st.task.advances, []string{"501:4->5"})
	wantCount(t, "ProcessAppeal 重复处理", &callLog{ops: st.log.opsFrom(before)}, "cache.", 0)
}

// 申诉不存在时的错误口径：model 的 `UPDATE ... WHERE id=? AND state=0` 影响 0 行，
// 与「已处理」共用同一个 ErrAppealAlreadyHandled，调用方分不清「投错了 ID」和「已被别人处理」。
// TODO(缺陷)：README 已知缺口 #14。
func TestProcessAppealOnMissingAppealIsReportedAsAlreadyHandled(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	before := st.log.snapshot()

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(4242, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantErrIs(t, "ProcessAppeal 申诉不存在", err, model.ErrAppealAlreadyHandled)
	if reply != nil {
		t.Errorf("ProcessAppeal 申诉不存在：reply = %+v, want nil", reply)
	}
	wantErrMessage(t, "ProcessAppeal 申诉不存在", err, "moderation: appeal already handled")
	wantOps(t, "ProcessAppeal 申诉不存在", st.log.opsFrom(before), []string{"appeal.Update:4242"})
	wantEQ(t, "ProcessAppeal 申诉不存在", "任务没被动过", st.task.state(501), stAppealed)
	wantCount(t, "ProcessAppeal 申诉不存在", st.log, "task.UpdateState", 0)
}

// ④本域不变量：只有 APPEALED 的任务该被推进 APPEAL_DONE。
// 现状是「任务不在 APPEALED ⇒ 申诉照样结案、任务原地不动、接口还回 200」，
// 因为 repository 把 ErrInvalidStateTransition 当成幂等信号咽掉了。
// TODO(缺陷)：README 已知缺口 #10。
func TestProcessAppealClosesAppealEvenWhenTaskStateIsIllegal(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		seed  bool
	}{
		{"任务不存在", -1, false},
		{"PENDING", stPending, true},
		{"DONE（还没人申诉过）", stDone, true},
		{"APPEAL_DONE（已结案）", stAppealDone, true},
		{"CANCELED", stCanceled, true},
		{"枚举外的脏值", stGhost, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			wantState := int32(-1)
			if tc.seed {
				wantState = tc.state
				seedTask(st, 501, tc.state, nil)
			}
			seedAppeal(st, 601, 501, nil)
			before := st.log.snapshot()

			reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
				ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
			wantNoErr(t, "ProcessAppeal 非法态 / "+tc.name, err)
			wantEQ(t, "ProcessAppeal 非法态 / "+tc.name, "申诉被标成已处理", st.appeal.rows[601].State, appealStateHandled)
			wantEQ(t, "ProcessAppeal 非法态 / "+tc.name, "最终结论已写入", st.appeal.rows[601].FinalVerdict, vPass)
			wantEQ(t, "ProcessAppeal 非法态 / "+tc.name, "任务状态原地不动", st.task.state(501), wantState)
			wantStringsEQ(t, "ProcessAppeal 非法态 / "+tc.name, "没有任何状态迁移", st.task.advances, nil)
			wantOps(t, "ProcessAppeal 非法态 / "+tc.name, st.log.opsFrom(before), []string{
				"appeal.Update:601",
				"appeal.FindOne:601",
				"task.UpdateState:501->5",
				"cache.DelTask:mod:task:501",
				"cache.DelAppeal:mod:appeal:601",
				"cache.GetAppeal:mod:appeal:601",
				"appeal.FindOne:601",
				"cache.SetAppeal:mod:appeal:601",
			})
			wantEQ(t, "ProcessAppeal 非法态 / "+tc.name, "回包给了已处理申诉", reply.GetAppeal().GetAppealId(), int64(601))
		})
	}
}

// ④结案没有权限模型：handler 只挡 <=0，申诉人自己也能批自己的申诉。
// TODO(缺陷)：README 已知缺口 #5（RPC 侧无鉴权；gateway/admin 才是以会话覆盖 handler 的地方）。
func TestProcessAppealDoesNotCheckHandlerIdentity(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, func(a *model.ModerationAppeal) { a.Mid = midAlice })

	if _, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, midAlice, rpc.Verdict_VERDICT_PASS, "我就是投稿人，我自己批")); err != nil {
		t.Fatalf("ProcessAppeal 自批：%v", err)
	}
	wantEQ(t, "ProcessAppeal 自批", "处理人 = 申诉人", st.appeal.rows[601].Handler, midAlice)
	wantEQ(t, "ProcessAppeal 自批", "任务已结案", st.task.state(501), stAppealDone)

	// 枚举外的 final_verdict 也照收。
	st2 := newStore()
	seedTask(st2, 502, stAppealed, nil)
	seedAppeal(st2, 602, 502, nil)
	if _, err := NewProcessAppealLogic(testCtx(), newTestSvc(st2)).
		ProcessAppeal(processReq(602, opAdminDan, rpc.Verdict(99), "未知结论")); err != nil {
		t.Fatalf("ProcessAppeal 未知结论：%v", err)
	}
	wantEQ(t, "ProcessAppeal 未知结论", "库存 verdict", st2.appeal.rows[602].FinalVerdict, vGhost)
}

// ③下游失败传播（申诉 Update）：错误传出 ⇒ 任务状态、申诉行、缓存全都不动。
func TestProcessAppealPropagatesUpdateFailure(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	injected := errors.New("injected appeal update timeout")
	st.appeal.failWith("Update", injected)

	before := st.log.snapshot()
	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantErrIs(t, "ProcessAppeal Update 失败", err, injected)
	if reply != nil {
		t.Errorf("ProcessAppeal Update 失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "ProcessAppeal Update 失败", st.log.opsFrom(before), []string{"appeal.Update:601"})
	wantEQ(t, "ProcessAppeal Update 失败", "申诉仍未处理", st.appeal.rows[601].State, appealStatePending)
	wantEQ(t, "ProcessAppeal Update 失败", "原结论字段没被写脏", st.appeal.rows[601].FinalReason, "")
	wantEQ(t, "ProcessAppeal Update 失败", "任务仍在 APPEALED", st.task.state(501), stAppealed)
	wantCount(t, "ProcessAppeal Update 失败", st.log, "cache.", 0)
}

// ③下游失败传播（回查申诉）：Update 之后那一次 FindOne 失败 ⇒ 接口报错，
// 但申诉已经结案、任务状态还没推进 ⇒ 半截状态 + 重试会拿到 ErrAppealAlreadyHandled（看起来像「已处理成功」）。
// TODO(缺陷)：README 已知缺口 #2。
func TestProcessAppealPropagatesReadBackFailureAndLeavesHalfAdvanced(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	injected := errors.New("injected appeal select timeout")
	st.appeal.failWith("FindOne", injected)

	before := st.log.snapshot()
	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantErrIs(t, "ProcessAppeal 回查失败", err, injected)
	if reply != nil {
		t.Errorf("ProcessAppeal 回查失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "ProcessAppeal 回查失败", st.log.opsFrom(before), []string{
		"appeal.Update:601",
		"appeal.FindOne:601",
	})
	wantEQ(t, "ProcessAppeal 回查失败", "申诉已被标记结案（残留）", st.appeal.rows[601].State, appealStateHandled)
	wantEQ(t, "ProcessAppeal 回查失败", "任务却仍在 APPEALED", st.task.state(501), stAppealed)
	wantCount(t, "ProcessAppeal 回查失败", st.log, "cache.", 0)

	// 运营重试 ⇒ 拿到的是「已处理」，永远看不到「任务没推进」这件事。
	_, err = NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantErrIs(t, "ProcessAppeal 回查失败后重试", err, model.ErrAppealAlreadyHandled)
}

// ③下游失败传播（任务 CAS 非迁移错误）：错误传出，但申诉已结案 ⇒ 状态与申诉分叉。
func TestProcessAppealPropagatesStateFailure(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	injected := errors.New("injected task update timeout")
	st.task.failWith("UpdateState", injected)

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantErrIs(t, "ProcessAppeal 状态推进失败", err, injected)
	if reply != nil {
		t.Errorf("ProcessAppeal 状态推进失败：reply = %+v, want nil", reply)
	}
	wantEQ(t, "ProcessAppeal 状态推进失败", "申诉已结案（残留）", st.appeal.rows[601].State, appealStateHandled)
	wantEQ(t, "ProcessAppeal 状态推进失败", "任务仍在 APPEALED", st.task.state(501), stAppealed)
	wantCount(t, "ProcessAppeal 状态推进失败", st.log, "cache.", 0)
}

// ④回查读的是缓存：ProcessAppeal 自己刚 DelAppeal，所以正常路径必然回源；
// 若 DelAppeal 失败（被吞），回查就会读到 SubmitAppeal 留下的旧值 ⇒ 接口回的是处理前的申诉。
// TODO(缺陷)：README 已知缺口 #7。
func TestProcessAppealReadBackHitsStaleCacheWhenInvalidationFails(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	st.cache.warmAppeal(st.appeal.rows[601]) // SubmitAppeal 之后缓存里是「未处理」那一版（这里给它带上主键，绕开 ID>0 判定）
	st.cache.failWith("DelAppeal", errors.New("injected redis del timeout"))

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantNoErr(t, "ProcessAppeal 脏缓存回查", err)
	wantEQ(t, "ProcessAppeal 脏缓存回查", "库存确实结案了", st.appeal.rows[601].State, appealStateHandled)
	wantEQ(t, "ProcessAppeal 脏缓存回查", "库存 final_verdict", st.appeal.rows[601].FinalVerdict, vPass)
	// 但回包是缓存里的旧值：没有最终结论、没有处理人。
	wantEQ(t, "ProcessAppeal 脏缓存回查", "回包 final_verdict（陈旧）", reply.GetAppeal().GetFinalVerdict(), rpc.Verdict_VERDICT_UNSPECIFIED)
	wantEQ(t, "ProcessAppeal 脏缓存回查", "回包 handler（陈旧）", reply.GetAppeal().GetHandler(), int64(0))
	wantEQ(t, "ProcessAppeal 脏缓存回查", "回包 state 看不见但主键在", reply.GetAppeal().GetAppealId(), int64(601))
	wantCount(t, "ProcessAppeal 脏缓存回查", st.log, "appeal.FindOne", 1)
}

// 缓存失效失败被吞：任务状态、申诉状态都已落库，接口仍成功。
func TestProcessAppealCacheFailuresAreSwallowed(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	st.cache.failWith("DelTask", errors.New("injected redis del timeout"))
	st.cache.failWith("DelAppeal", errors.New("injected redis del timeout"))
	st.cache.failWith("GetAppeal", errors.New("injected redis get timeout"))
	st.cache.failWith("SetAppeal", errors.New("injected redis set timeout"))

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_PASS, "复核成立"))
	wantNoErr(t, "ProcessAppeal 缓存全挂", err)
	wantEQ(t, "ProcessAppeal 缓存全挂", "任务已结案", st.task.state(501), stAppealDone)
	wantEQ(t, "ProcessAppeal 缓存全挂", "回查仍拿到真值（降级回源）", reply.GetAppeal().GetFinalReason(), "复核成立")
	wantCount(t, "ProcessAppeal 缓存全挂", st.log, "cache.", 4)
}

// final_reason 无长度校验（列宽 VARCHAR(500））。
// TODO(缺陷)：README 已知缺口 #8。
func TestProcessAppealHasNoFinalReasonLengthGuard(t *testing.T) {
	st := newStore()
	seedTask(st, 501, stAppealed, nil)
	seedAppeal(st, 601, 501, nil)
	long := string(make([]byte, 1200))

	reply, err := NewProcessAppealLogic(testCtx(), newTestSvc(st)).
		ProcessAppeal(processReq(601, opAdminDan, rpc.Verdict_VERDICT_REJECT, long))
	wantNoErr(t, "ProcessAppeal 超长说明", err)
	wantEQ(t, "ProcessAppeal 超长说明", "长度原样入库", len(st.appeal.rows[601].FinalReason), 1200)
	wantEQ(t, "ProcessAppeal 超长说明", "回包长度一致", len(reply.GetAppeal().GetFinalReason()), 1200)
}
