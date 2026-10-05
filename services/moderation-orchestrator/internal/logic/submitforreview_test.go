package logic

import (
	"errors"
	"testing"

	"go-video/services/moderation-orchestrator/model"
	"go-video/services/moderation-orchestrator/rpc"
)

// submitReq 是一个字段辨识度拉满的送审请求。
func submitReq(mut func(*rpc.SubmitReq)) *rpc.SubmitReq {
	in := &rpc.SubmitReq{
		SubmissionId: subAlice,
		ContentType:  rpc.ContentType_CONTENT_TYPE_VIDEO,
		Mid:          midAlice,
		UpMid:        upCarol,
		Business:     bizVideo,
		Reason:       "稿件首发",
		Ip:           "203.0.113.9",
	}
	if mut != nil {
		mut(in)
	}
	return in
}

// ①守卫表：四条参数校验都必须发生在打库之前，且一行数据都不落。
func TestSubmitForReviewGuardsRunBeforeAnyDependency(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.SubmitReq)
		want error
	}{
		{"submission_id 为 0", func(in *rpc.SubmitReq) { in.SubmissionId = 0 }, model.ErrInvalidSubmissionID},
		{"submission_id 负数", func(in *rpc.SubmitReq) { in.SubmissionId = -7 }, model.ErrInvalidSubmissionID},
		{"content_type 未指定", func(in *rpc.SubmitReq) { in.ContentType = rpc.ContentType_CONTENT_TYPE_UNSPECIFIED }, model.ErrInvalidContentType},
		{"mid 为 0", func(in *rpc.SubmitReq) { in.Mid = 0 }, model.ErrInvalidMid},
		{"mid 负数", func(in *rpc.SubmitReq) { in.Mid = -1 }, model.ErrInvalidMid},
		{"business 为空", func(in *rpc.SubmitReq) { in.Business = "" }, model.ErrInvalidBusiness},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			before := st.log.snapshot()
			reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(tc.mut))
			wantErrIs(t, "SubmitForReview / "+tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("SubmitForReview / %s：reply = %+v, want nil", tc.name, reply)
			}
			wantNoCall(t, "SubmitForReview / "+tc.name, st, before)
			wantEQ(t, "SubmitForReview / "+tc.name, "任务表行数", st.task.count(), 0)
		})
	}
}

// ②正常路径：查唯一键 → 插入 → 回填缓存，响应与库存逐字段一致，状态固定 PENDING。
func TestSubmitForReviewCreatesPendingTask(t *testing.T) {
	st := newStore()
	before := st.log.snapshot()

	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
	wantNoErr(t, "SubmitForReview", err)
	wantOps(t, "SubmitForReview", st.log.opsFrom(before), []string{
		"task.FindBySubmission:video.ugc/777001",
		"task.Insert:video.ugc/777001",
		"cache.SetTask:mod:task:501",
	})
	wantCtxCarried(t, "SubmitForReview", st.log)

	got := reply.GetTask()
	wantEQ(t, "SubmitForReview 投影", "task_id（自增主键回传）", got.TaskId, int64(501))
	wantEQ(t, "SubmitForReview 投影", "submission_id", got.SubmissionId, subAlice)
	wantEQ(t, "SubmitForReview 投影", "content_type", got.ContentType, rpc.ContentType_CONTENT_TYPE_VIDEO)
	wantEQ(t, "SubmitForReview 投影", "mid", got.Mid, midAlice)
	wantEQ(t, "SubmitForReview 投影", "up_mid", got.UpMid, upCarol)
	wantEQ(t, "SubmitForReview 投影", "business", got.Business, bizVideo)
	wantEQ(t, "SubmitForReview 投影", "reason", got.Reason, "稿件首发")
	wantEQ(t, "SubmitForReview 投影", "state", got.State, rpc.TaskState_TASK_STATE_PENDING)
	wantEQ(t, "SubmitForReview 投影", "operator", got.Operator, int64(0))

	row := st.task.rows[501]
	wantEQ(t, "SubmitForReview 库存", "state", row.State, stPending)
	wantEQ(t, "SubmitForReview 库存", "submission_id", row.SubmissionID, subAlice)
	wantEQ(t, "SubmitForReview 库存", "business", row.Business, bizVideo)
	assertAround(t, "SubmitForReview 库存", "ctime", row.Ctime, nowUnix(), 2)
	assertAround(t, "SubmitForReview 库存", "mtime", row.Mtime, nowUnix(), 2)
	wantEQ(t, "SubmitForReview 库存", "响应 ctime 与库存一致", got.Ctime, row.Ctime)

	cached, err := decodeTask(st.cache.raw(keyTask(501)))
	wantNoErr(t, "SubmitForReview 回填", err)
	wantEQ(t, "SubmitForReview 回填", "state", cached.State, stPending)
	wantEQ(t, "SubmitForReview 回填", "ID", cached.ID, int64(501))
}

// ④幂等守卫：同一对象还在审核中（PENDING/PROCESSING）时重复送审必须被拒，且一行都不多写。
func TestSubmitForReviewRejectsDuplicateWhileUnderReview(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state int32
	}{
		{"PENDING", stPending},
		{"PROCESSING", stProcessing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			existing := seedTask(st, 501, tc.state, nil)
			before := st.log.snapshot()

			reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
			wantErrIs(t, "SubmitForReview 重复送审 / "+tc.name, err, model.ErrDuplicateTask)
			if reply != nil {
				t.Errorf("SubmitForReview 重复送审 / %s：reply = %+v, want nil", tc.name, reply)
			}
			wantOps(t, "SubmitForReview 重复送审 / "+tc.name, st.log.opsFrom(before),
				[]string{"task.FindBySubmission:video.ugc/777001"})
			wantCount(t, "SubmitForReview 重复送审 / "+tc.name, st.log, "task.Insert", 0)
			wantCount(t, "SubmitForReview 重复送审 / "+tc.name, st.log, "cache.", 0)
			wantEQ(t, "SubmitForReview 重复送审 / "+tc.name, "行数不变", st.task.count(), 1)
			wantEQ(t, "SubmitForReview 重复送审 / "+tc.name, "原状态不被改写", st.task.state(501), existing.State)
			// 幂等拒了之后，原任务的 ctime 也不许被「重投递」刷新（model 只改写内存形参）。
			wantEQ(t, "SubmitForReview 重复送审 / "+tc.name, "库存 ctime 保持", st.task.rows[501].Ctime, int64(1_700_000_000))
		})
	}
}

// ④本域不变量（AGENTS.md §8）：已终态任务**重新送审**目前会「答一个没发生的 PENDING」——
// 唯一键命中 ⇒ SQL 只回既有 id、一列都不改，但 repository 已经把内存对象的 state 改成 PENDING
// 并把它写进缓存；旧的审核结论仍挂在同一个 task_id 上。
// TODO(缺陷)：README 已知缺口 #3。修好后本用例应改为 wantErrIs(ErrDuplicateTask) 或 state 真的回到 PENDING。
func TestSubmitForReviewAfterTerminalStateLiesAboutState(t *testing.T) {
	for _, terminal := range []struct {
		name  string
		state int32
	}{
		{"DONE", stDone},
		{"APPEALED", stAppealed},
		{"APPEAL_DONE", stAppealDone},
		{"CANCELED", stCanceled},
	} {
		t.Run(terminal.name, func(t *testing.T) {
			st := newStore()
			seedTask(st, 501, terminal.state, nil)
			seedResult(st, 501, vReject, nil) // 上一轮结论

			before := st.log.snapshot()
			reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
			wantNoErr(t, "SubmitForReview 终态重送 / "+terminal.name, err)
			wantOps(t, "SubmitForReview 终态重送 / "+terminal.name, st.log.opsFrom(before), []string{
				"task.FindBySubmission:video.ugc/777001",
				"task.Insert:video.ugc/777001",
				"cache.SetTask:mod:task:501",
			})
			// 回包宣称新任务已入队……
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "响应 state",
				reply.GetTask().GetState(), rpc.TaskState_TASK_STATE_PENDING)
			// ……但库里还是上一轮的终态，且复用同一个 task_id。
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "库存 state", st.task.state(501), terminal.state)
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "复用同一主键", reply.GetTask().GetTaskId(), int64(501))
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "行数不增", st.task.count(), 1)
			// 缓存被写成 PENDING ⇒ 后续 GetTask 命中脏缓存，库里状态永远追不回来。
			cached, err := decodeTask(st.cache.raw(keyTask(501)))
			wantNoErr(t, "SubmitForReview 终态重送 / "+terminal.name, err)
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "缓存被写成 PENDING", cached.State, stPending)
			// 上一轮的 REJECT 结论仍挂在这个 task_id 上：新一轮审核还没跑，读侧已能拿到「旧结论」。
			wantEQ(t, "SubmitForReview 终态重送 / "+terminal.name, "旧结论仍在", st.result.rows[501].Verdict, vReject)
		})
	}
}

// ②reason 可以为空（举报复审等场景不填原因），business 才是要紧的命名空间。
func TestSubmitForReviewAcceptsEmptyReason(t *testing.T) {
	st := newStore()
	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(func(in *rpc.SubmitReq) {
		in.Reason = ""
	}))
	wantNoErr(t, "SubmitForReview 空 reason", err)
	wantEQ(t, "SubmitForReview 空 reason", "reason", reply.GetTask().GetReason(), "")
	wantEQ(t, "SubmitForReview 空 reason", "state", reply.GetTask().GetState(), rpc.TaskState_TASK_STATE_PENDING)
}

// ③无枚举/长度校验：content_type 越界、超长 business/reason 都能入库。
// TODO(缺陷)：README 已知缺口 #8（reason/business 无长度校验、content_type 不校验枚举）。
func TestSubmitForReviewHasNoEnumOrLengthValidation(t *testing.T) {
	st := newStore()
	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(func(in *rpc.SubmitReq) {
		in.ContentType = rpc.ContentType(42)
		in.Business = string(long) // 列宽 VARCHAR(64)
		in.Reason = "  "
	}))
	wantNoErr(t, "SubmitForReview 脏枚举", err)
	wantEQ(t, "SubmitForReview 脏枚举", "content_type 原样回传", reply.GetTask().GetContentType(), rpc.ContentType(42))
	row := st.task.rows[reply.GetTask().GetTaskId()]
	wantEQ(t, "SubmitForReview 脏枚举", "库存 content_type", row.ContentType, ctGhost)
	wantEQ(t, "SubmitForReview 脏枚举", "库存 business 长度（超列宽，MySQL 会 1406）", len(row.Business), 400)
	wantEQ(t, "SubmitForReview 脏枚举", "空白 reason 照收", row.Reason, "  ")
}

// ③下游失败传播：唯一键预查失败 ⇒ 错误传出、一行都不写。
func TestSubmitForReviewPropagatesLookupFailure(t *testing.T) {
	st := newStore()
	injected := errors.New("injected mysql gone away")
	st.task.failWith("FindBySubmission", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
	wantErrIs(t, "SubmitForReview 预查失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitForReview 预查失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "SubmitForReview 预查失败", st.log.opsFrom(before), []string{"task.FindBySubmission:video.ugc/777001"})
	wantEQ(t, "SubmitForReview 预查失败", "不落半截任务", st.task.count(), 0)
	wantCount(t, "SubmitForReview 预查失败", st.log, "cache.", 0)
}

// ③下游失败传播：Insert 失败 ⇒ 错误传出、不回填缓存、task_id 不回传。
func TestSubmitForReviewPropagatesInsertFailure(t *testing.T) {
	st := newStore()
	injected := errors.New("injected insert duplicate key")
	st.task.failWith("Insert", injected)

	before := st.log.snapshot()
	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
	wantErrIs(t, "SubmitForReview 插入失败", err, injected)
	if reply != nil {
		t.Errorf("SubmitForReview 插入失败：reply = %+v, want nil", reply)
	}
	wantOps(t, "SubmitForReview 插入失败", st.log.opsFrom(before), []string{
		"task.FindBySubmission:video.ugc/777001",
		"task.Insert:video.ugc/777001",
	})
	wantCount(t, "SubmitForReview 插入失败", st.log, "cache.", 0)
	wantEQ(t, "SubmitForReview 插入失败", "无脏缓存", st.cache.has(keyTask(501)), false)
}

// 缓存写失败被吞（`_ = r.cache.SetTask`）：任务已落库，送审仍然成功。
// TODO(缺陷)：README 已知缺口 #7。
func TestSubmitForReviewCacheFailureIsSwallowed(t *testing.T) {
	st := newStore()
	st.cache.failWith("SetTask", errors.New("injected redis write timeout"))

	reply, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil))
	wantNoErr(t, "SubmitForReview 缓存失败", err)
	wantEQ(t, "SubmitForReview 缓存失败", "任务已落库", st.task.state(501), stPending)
	wantEQ(t, "SubmitForReview 缓存失败", "但缓存没写上", st.cache.has(keyTask(501)), false)
	wantEQ(t, "SubmitForReview 缓存失败", "task_id 照常回传", reply.GetTask().GetTaskId(), int64(501))
}

// ④送审不派发 worker、不推 PROCESSING、不写结论：任务只能停在 PENDING。
// （AGENTS.md §8：送审 ≠ 通过；本服务也没有 moderation-worker client 与 MQ 生产者。）
func TestSubmitForReviewNeverAdvancesBeyondPending(t *testing.T) {
	st := newStore()
	if _, err := NewSubmitForReviewLogic(testCtx(), newTestSvc(st)).SubmitForReview(submitReq(nil)); err != nil {
		t.Fatalf("SubmitForReview：%v", err)
	}
	wantEQ(t, "SubmitForReview 只入队", "库存 state", st.task.state(501), stPending)
	wantCount(t, "SubmitForReview 只入队", st.log, "task.UpdateState", 0)
	wantCount(t, "SubmitForReview 只入队", st.log, "result.", 0)
	wantCount(t, "SubmitForReview 只入队", st.log, "appeal.", 0)
	wantCount(t, "SubmitForReview 只入队", st.log, "rule.", 0) // 规则版本/操作人必填：无人读规则表
	wantStringsEQ(t, "SubmitForReview 只入队", "成功的状态迁移", st.task.advances, nil)
}
