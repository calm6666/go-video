package logic

// gettaskresultlogic 的按方法用例。
//
// 本方法是 orchestrator 读机审结论的唯一入口，因此断言集中在两个方向：
//  1. 读侧只做**投影**：库存写着什么状态就回什么状态。PENDING/RUNNING 不能被洗成 SUCCEEDED，
//     FAILED/TIMEOUT 不能被洗成 SUCCEEDED（否则等于把「没跑完/跑挂了」当成「跑完且没问题」，
//     上游据此自动放行，违反 AGENTS.md §8）。
//  2. 一次都不写：本方法是只读投影，任何 Upsert / UpdateResult / 缓存失效调用都是越界。

import (
	"fmt"
	"testing"

	"go-video/services/moderation-worker/model"
	"go-video/services/moderation-worker/rpc"
)

const (
	// mwResultJSON 是两条命中片段的合法结果片段 JSON（第二段是全零值片段，用来验证逐字段投影）。
	mwResultJSON = `[{"start_ms":1200,"end_ms":1900,"text":"违禁词A","confidence":0.87,` +
		`"label":"sensitive_text","extra":"{\"bbox\":[1,2,3,4]}"},` +
		`{"start_ms":0,"end_ms":0,"text":"","confidence":0.5,"label":"","extra":""}]`
	// mwBrokenResultJSON 是**合法 JSON 但形状不对**（对象包了一层），
	// 复刻「别的版本写出来的结果」——GetTaskResult 现在会静默吞掉解析错误（缺陷 #7）。
	mwBrokenResultJSON = `{"segments":[]}`
)

// seedFinishedTask 静默布一条字段值彼此可辨的 worker_task，用于逐字段断言。
func seedFinishedTask(t *testing.T, st *store, workerTaskID, taskID string, state int32, resultJSON, errMsg string) *model.WorkerTask {
	t.Helper()
	return seedTask(t, st, &model.WorkerTask{
		WorkerTaskID:     workerTaskID,
		TaskID:           taskID,
		Capability:       int32(rpc.CapabilityType_CAPABILITY_OCR),
		MediaURI:         runMediaURI,
		DurationMs:       137500,
		ParamsJSON:       `{"language":"zh-CN"}`,
		TimeoutMs:        5000,
		TraceID:          "trace-mw-abc",
		State:            state,
		AlgorithmVersion: "ocr-v3.2.1",
		ElapsedMs:        842,
		ResultJSON:       resultJSON,
		ErrorMessage:     errMsg,
		Ctime:            1758600000,
		Mtime:            1758600123,
	})
}

// segmentLines 把一个片段列表摊成逐字段字符串（切片比较只能走 wantStringsEQ）。
func segmentLines(segs []*rpc.ResultSegment) []string {
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, fmt.Sprintf("%d|%d|%s|%v|%s|%s",
			s.GetStartMs(), s.GetEndMs(), s.GetText(), s.GetConfidence(), s.GetLabel(), s.GetExtra()))
	}
	return out
}

// getReply 调用并断言成功。
func getReply(t *testing.T, label string, st *store, in *rpc.TaskResultReq) *rpc.TaskResultReply {
	t.Helper()
	reply, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(in)
	wantNoErr(t, label, err)
	if reply == nil {
		t.Fatalf("%s：reply = nil, want 非空", label)
	}
	return reply
}

// === ① 守卫表 ===

func TestGetTaskResultGuardsRejectBeforeTouchingAnything(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.TaskResultReq
	}{
		{"两个 ID 都为空（既不知道该查哪条）", &rpc.TaskResultReq{}},
		{"worker_task_id 空且 task_id 空串", &rpc.TaskResultReq{WorkerTaskId: "", TaskId: ""}},
	}
	for _, c := range cases {
		st := newEnv()
		label := "GetTaskResult / " + c.name
		before := st.log.snapshot()

		reply, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(c.in)
		wantErrIs(t, label, err, model.ErrInvalidWorkerTaskID)
		wantNil(t, label, "reply", reply)
		wantNoCall(t, label, st, before)
	}
}

// === ② 正常路径：优先按 worker_task_id、逐字段投影 ===

func TestGetTaskResultPrefersWorkerTaskIDOverTaskID(t *testing.T) {
	st := newEnv()
	// 布两行：worker_task_id 指向 a，task_id 指向 b。读到 a 才说明优先级正确；
	// 若有人把分支条件写反，就会读到 b 的 algorithm_version。
	seedFinishedTask(t, st, "wt-a", "tk-shared", model.TaskStateSucceeded, `[]`, "")
	seedFinishedTask(t, st, "wt-b", "tk-shared", model.TaskStateFailed, `[]`, "b 行才有的错误")
	before := st.log.snapshot()

	got := getReply(t, "优先级", st, &rpc.TaskResultReq{WorkerTaskId: "wt-a", TaskId: "tk-shared"})
	wantOps(t, "优先级", st.log.opsFrom(before), []string{"worker.FindOne:wt-a"})
	wantEQ(t, "优先级", "读到的是 wt-a", got.GetWorkerTaskId(), "wt-a")
	wantEQ(t, "优先级", "状态取自 wt-a（成功，不是 b 行的失败）", int32(got.GetState()), int32(model.TaskStateSucceeded))
	wantEQ(t, "优先级", "error_message 取自 wt-a", got.GetErrorMessage(), "")
}

func TestGetTaskResultProjectsEveryFieldByWorkerTaskID(t *testing.T) {
	st := newEnv()
	row := seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, mwResultJSON, "")
	before := st.log.snapshot()

	got := getReply(t, "按 worker_task_id 投影", st, &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
	wantOps(t, "按 worker_task_id 投影", st.log.opsFrom(before), []string{"worker.FindOne:" + runWorkerTaskID})

	wantEQ(t, "投影", "worker_task_id", got.GetWorkerTaskId(), row.WorkerTaskID)
	wantEQ(t, "投影", "task_id", got.GetTaskId(), row.TaskID)
	wantEQ(t, "投影", "capability", int32(got.GetCapability()), row.Capability)
	wantEQ(t, "投影", "state", int32(got.GetState()), row.State)
	wantEQ(t, "投影", "algorithm_version", got.GetAlgorithmVersion(), row.AlgorithmVersion)
	wantEQ(t, "投影", "elapsed_ms", got.GetElapsedMs(), row.ElapsedMs)
	wantEQ(t, "投影", "error_message", got.GetErrorMessage(), row.ErrorMessage)
	wantStringsEQ(t, "投影", "segments 逐字段", segmentLines(got.GetSegments()), []string{
		`1200|1900|违禁词A|0.87|sensitive_text|{"bbox":[1,2,3,4]}`,
		`0|0||0.5||`,
	})
	wantLegalTaskState(t, "投影", int32(got.GetState()))
}

func TestGetTaskResultByTaskIDPicksTheNewestRow(t *testing.T) {
	st := newEnv()
	// 同一 task_id 两行：mtime 不同（真实 SQL 是 ORDER BY mtime DESC LIMIT 1）。
	older := seedTask(t, st, &model.WorkerTask{
		WorkerTaskID: "wt-old", TaskID: runTaskID, Capability: int32(model.CapabilityOCR),
		State: model.TaskStateFailed, AlgorithmVersion: "ocr-v1", ErrorMessage: "老一版的失败",
		Ctime: 1758600000, Mtime: 1758600001,
	})
	newer := seedTask(t, st, &model.WorkerTask{
		WorkerTaskID: "wt-new", TaskID: runTaskID, Capability: int32(model.CapabilityImage),
		State: model.TaskStateSucceeded, AlgorithmVersion: "image-v9", ResultJSON: `[]`,
		Ctime: 1758600000, Mtime: 1758600999,
	})
	before := st.log.snapshot()

	got := getReply(t, "按 task_id 反查", st, &rpc.TaskResultReq{TaskId: runTaskID})
	wantOps(t, "按 task_id 反查", st.log.opsFrom(before), []string{"worker.FindByTaskID:" + runTaskID})
	wantEQ(t, "反查", "取最新一行的 worker_task_id", got.GetWorkerTaskId(), newer.WorkerTaskID)
	wantEQ(t, "反查", "取最新一行的 capability", int32(got.GetCapability()), newer.Capability)
	wantEQ(t, "反查", "取最新一行的算法版本", got.GetAlgorithmVersion(), newer.AlgorithmVersion)
	wantEQ(t, "反查", "老一版的失败原因不得串台", got.GetErrorMessage(), "")
	if got.GetWorkerTaskId() == older.WorkerTaskID {
		t.Errorf("反查：拿到了 mtime 更老的那一行，orchestrator 会按一次已废弃的失败结论推进")
	}
}

// === ③ 下游失败传播：两个读依赖各注入一次 ===

func TestGetTaskResultReadFailuresPropagateAndDoNotFallBack(t *testing.T) {
	cases := []struct {
		name        string
		in          *rpc.TaskResultReq
		failMethod  string
		wantOp      string
		forbiddenOp string
	}{
		{"按 worker_task_id 查、主键读失败", &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID},
			"FindOne", "worker.FindOne:" + runWorkerTaskID, "worker.FindByTaskID"},
		{"按 task_id 查、反查失败", &rpc.TaskResultReq{TaskId: runTaskID},
			"FindByTaskID", "worker.FindByTaskID:" + runTaskID, "worker.FindOne"},
	}
	for _, c := range cases {
		st := newEnv()
		seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, mwResultJSON, "")
		st.worker.failWith(c.failMethod, errEngineTimeout)
		label := "GetTaskResult / " + c.name
		before := st.log.snapshot()

		got, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(c.in)
		wantErrIs(t, label, err, errEngineTimeout)
		wantNil(t, label, "reply", got)
		// 关键：读失败就是失败，不能退化成 ErrTaskNotFound（那会让上游以为是「任务没跑过」），
		// 也不能改走另一条查询分支碰运气。
		wantOps(t, label, st.log.opsFrom(before), []string{c.wantOp})
		wantCount(t, label+" 不得改走另一条分支", st.log, c.forbiddenOp, 0)
	}
}

func TestGetTaskResultMissingRowIsNotFoundNotEmptyReply(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.TaskResultReq
		op   string
	}{
		{"worker_task_id 查无此行", &rpc.TaskResultReq{WorkerTaskId: "wt-ghost"}, "worker.FindOne:wt-ghost"},
		{"task_id 查无此行", &rpc.TaskResultReq{TaskId: "tk-ghost"}, "worker.FindByTaskID:tk-ghost"},
	}
	for _, c := range cases {
		st := newEnv()
		seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, `[]`, "")
		label := "GetTaskResult / " + c.name
		before := st.log.snapshot()

		got, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(c.in)
		wantErrIs(t, label, err, model.ErrTaskNotFound)
		wantNil(t, label, "reply", got)
		wantOps(t, label, st.log.opsFrom(before), []string{c.op})
	}
}

// === ④ 不变量 ===

// 状态原样投影：非终态与失败终态都不得被洗成 SUCCEEDED。
func TestGetTaskResultNeverUpgradesNonSucceededStates(t *testing.T) {
	cases := []struct {
		state   int32
		errMsg  string
		caption string
	}{
		{model.TaskStatePending, "", "刚建好还没跑"},
		{model.TaskStateRunning, "", "执行中"},
		{model.TaskStateSucceeded, "", "成功"},
		{model.TaskStateFailed, "算法返回未知结论", "失败"},
		{model.TaskStateTimeout, "engine deadline exceeded", "超时"},
	}
	for _, c := range cases {
		st := newEnv()
		seedFinishedTask(t, st, runWorkerTaskID, runTaskID, c.state, `[]`, c.errMsg)
		label := fmt.Sprintf("GetTaskResult 状态 %d（%s）", c.state, c.caption)

		got := getReply(t, label, st, &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
		wantEQ(t, label, "state 原样返回", int32(got.GetState()), c.state)
		wantLegalTaskState(t, label, int32(got.GetState()))
		wantEQ(t, label, "error_message 原样返回", got.GetErrorMessage(), c.errMsg)
		wantEQ(t, label, "片段不凭空多出", len(got.GetSegments()), 0)
	}
}

// 只读投影：一次依赖调用都没有留下写入。
func TestGetTaskResultIsReadOnly(t *testing.T) {
	st := newEnv()
	seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, `[]`, "")
	before := st.log.snapshot()

	getReply(t, "只读", st, &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
	wantOps(t, "只读", st.log.opsFrom(before), []string{"worker.FindOne:" + runWorkerTaskID})
	wantCount(t, "只读", st.log, "worker.Upsert", 0)
	wantCount(t, "只读", st.log, "worker.UpdateResult", 0)
	wantCount(t, "只读", st.log, "cache.", 0)
	wantEQ(t, "只读", "库存状态没被改动", st.worker.snapshot(runWorkerTaskID).State, int32(model.TaskStateSucceeded))
}

// 缺陷 #7（登记，不顺手改）：gettaskresultlogic.go:56 用 `_ = json.Unmarshal` 吞掉解析错误。
// 于是「结果形状不对」与「识别成功且零命中」都回一个空列表 + 无错误，
// orchestrator 无从区分，坏数据会被当成「机审通过、没有命中」。
// 本用例钉住现状：坏 JSON 段仍然成功返回、segments 为 nil（与 `[]` 解析出的非空空列表可分辨）。
// 修好后（解析失败应报错）本用例会红，届时把 wantNoErr 换成 wantErrIs。
func TestGetTaskResultCorruptResultJSONIsSilentlyDropped(t *testing.T) {
	st := newEnv()
	row := seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, mwBrokenResultJSON, "")
	before := st.log.snapshot()

	got := getReply(t, "坏结果 JSON", st, &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
	wantOps(t, "坏结果 JSON", st.log.opsFrom(before), []string{"worker.FindOne:" + runWorkerTaskID})
	wantEQ(t, "坏结果 JSON", "库存里那段确实解析不了", st.worker.snapshot(runWorkerTaskID).ResultJSON, row.ResultJSON)
	if got.GetSegments() != nil {
		t.Errorf("坏结果 JSON：期望 segments 是 nil（解析失败被吞），实际 %v", segmentLines(got.GetSegments()))
	}
	wantEQ(t, "坏结果 JSON", "现状仍报 SUCCEEDED", int32(got.GetState()), int32(model.TaskStateSucceeded))
}

// 缺陷 #8（登记，不顺手改）：DB 的 state 是 TINYINT、没有 CHECK 约束，
// logic 直接 rpc.TaskState(task.State) 转换，越界值会被原样发给 orchestrator
// （proto3 未知枚举值在 wire 上按未知字段处理，消费端读到的是 UNSPECIFIED）。
// 本用例钉住「没有枚举校验」这一事实；加校验后应改成断言被拒绝。
func TestGetTaskResultPassesThroughUnknownState(t *testing.T) {
	st := newEnv()
	seedFinishedTask(t, st, runWorkerTaskID, runTaskID, 9, `[]`, "脏数据")
	before := st.log.snapshot()

	got := getReply(t, "越界状态", st, &rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
	wantOps(t, "越界状态", st.log.opsFrom(before), []string{"worker.FindOne:" + runWorkerTaskID})
	wantEQ(t, "越界状态", "现状：越界值被原样透传", int32(got.GetState()), int32(9))
	// 反面确认：这个值确实不在合法词表里（wantLegalTaskState 会红，所以只在断言里数一次）。
	for _, legal := range legalTaskStates() {
		if int32(got.GetState()) == legal {
			t.Errorf("越界状态：%d 竟然落在合法词表里，本用例的前提已经不成立", got.GetState())
		}
	}
}

// 请求 ctx 与取消：读侧至少把 l.ctx 透传到了 model（与 Run* 的缺陷 #3 相反，这里是正确的）。
func TestGetTaskResultUsesRequestContext(t *testing.T) {
	for _, in := range []*rpc.TaskResultReq{
		{WorkerTaskId: runWorkerTaskID},
		{TaskId: runTaskID},
	} {
		st := newEnv()
		seedFinishedTask(t, st, runWorkerTaskID, runTaskID, model.TaskStateSucceeded, `[]`, "")
		branch := "worker_task_id"
		method := "FindOne"
		if in.GetWorkerTaskId() == "" {
			branch = "task_id"
			method = "FindByTaskID"
		}
		_, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(in)
		wantNoErr(t, "GetTaskResult "+branch+" 的 ctx", err)
		wantEQ(t, "GetTaskResult "+branch, "读库拿到的是请求 ctx", st.probe.got(t, method, 0), true)
	}
}
