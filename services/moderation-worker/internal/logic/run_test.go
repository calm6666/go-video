package logic

// run_test.go 覆盖 RunOCR / RunASR / RunImage / RunAudio 四个识别入口。
// 四者共用 prepareRun + finishRun，但**每个方法都单独跑完**守卫表、正常路径、
// 下游失败传播与不变量四组用例（能力值与错配方向各不相同，不是只跑一个代表方法）。
//
// 这里最要紧的三条方向（AGENTS.md §5/§8）：
//  1. 写库失败必须如实抛出，**绝不能回一个 TASK_STATE_SUCCEEDED**——orchestrator 会把这个
//     状态当成「机审执行完成」推进稿件；伪造成功等于自动放行。
//  2. worker 只写自己域的 worker_task，不写任何审核结论（Verdict）/稿件状态，
//     也不越界碰 orchestrator 的主数据。
//  3. 同一 worker_task_id 重投只能有一行，且终态必须落在合法 TaskState 枚举里。

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go-video/services/moderation-worker/internal/svc"
	"go-video/services/moderation-worker/model"
	"go-video/services/moderation-worker/rpc"
)

// runMethod 把一个 RPC 方法抽象成「名字 + 期望能力 + 调用闭包」。
type runMethod struct {
	name       string
	capability rpc.CapabilityType
	invoke     func(ctx context.Context, s *svc.ServiceContext, in *rpc.RunTaskReq) (*rpc.TaskResultReply, error)
}

var runMethods = []runMethod{
	{
		name: "RunOCR", capability: rpc.CapabilityType_CAPABILITY_OCR,
		invoke: func(ctx context.Context, s *svc.ServiceContext, in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
			return NewRunOCRLogic(ctx, s).RunOCR(in)
		},
	},
	{
		name: "RunASR", capability: rpc.CapabilityType_CAPABILITY_ASR,
		invoke: func(ctx context.Context, s *svc.ServiceContext, in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
			return NewRunASRLogic(ctx, s).RunASR(in)
		},
	},
	{
		name: "RunImage", capability: rpc.CapabilityType_CAPABILITY_IMAGE,
		invoke: func(ctx context.Context, s *svc.ServiceContext, in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
			return NewRunImageLogic(ctx, s).RunImage(in)
		},
	},
	{
		name: "RunAudio", capability: rpc.CapabilityType_CAPABILITY_AUDIO,
		invoke: func(ctx context.Context, s *svc.ServiceContext, in *rpc.RunTaskReq) (*rpc.TaskResultReply, error) {
			return NewRunAudioLogic(ctx, s).RunAudio(in)
		},
	},
}

// otherCapabilityOf 给出一个**不等于**本方法能力的合法能力：
// 用来验证「方法 ↔ 能力」绑定，防止 orchestrator 走错路由也能落库。
var otherCapabilityOf = map[rpc.CapabilityType]rpc.CapabilityType{
	rpc.CapabilityType_CAPABILITY_OCR:   rpc.CapabilityType_CAPABILITY_ASR,
	rpc.CapabilityType_CAPABILITY_ASR:   rpc.CapabilityType_CAPABILITY_IMAGE,
	rpc.CapabilityType_CAPABILITY_IMAGE: rpc.CapabilityType_CAPABILITY_AUDIO,
	rpc.CapabilityType_CAPABILITY_AUDIO: rpc.CapabilityType_CAPABILITY_OCR,
}

const (
	// runWorkerTaskID 固定成 26 位 ULID 形状，让调用序列能被精确断言（纪律 5）。
	runWorkerTaskID = "01HTMWW7R3N0CEQRSHVZBG6A9Y"
	// runWorkerTaskID2 与第一个只差最后一位（仍在 ULID 字母表内）。
	runWorkerTaskID2 = "01HTMWW7R3N0CEQRSHVZBG6A9Z"
	runTaskID        = "1789000001"
	runMediaURI      = "oss://go-video-moderation/mtv/2026-09-1789000001.m3u8"
	ulidAlphabet     = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

// newRunReq 构造一个能通过全部守卫的请求（每个用例各取一份，用例自行改字段）。
func newRunReq(m runMethod) *rpc.RunTaskReq {
	return &rpc.RunTaskReq{
		TaskId:       runTaskID,
		WorkerTaskId: runWorkerTaskID,
		Capability:   m.capability,
		MediaUri:     runMediaURI,
		DurationMs:   137500,
		Params:       map[string]string{"language": "zh-CN", "model_version": "v3"},
		TimeoutMs:    5000,
		TraceId:      "trace-mw-abc",
	}
}

// runOK 调用并断言成功。任何「本该失败却返回成功」的变异都会在这里当场红。
func runOK(t *testing.T, label string, m runMethod, st *store, in *rpc.RunTaskReq) *rpc.TaskResultReply {
	t.Helper()
	reply, err := m.invoke(newReqCtx(), st.svcCtx, in)
	wantNoErr(t, label, err)
	if reply == nil {
		t.Fatalf("%s：reply = nil, want 非空", label)
	}
	return reply
}

// opSuffix 拼 Upsert 的轨迹期望（与替身同源格式化，避免手写串对不上）。
func opUpsert(workerTaskID, taskID string) string {
	return fmt.Sprintf("worker.Upsert:%s/%s", workerTaskID, taskID)
}

func opFinish(workerTaskID string, state int32) string {
	return fmt.Sprintf("worker.UpdateResult:%s:%d", workerTaskID, state)
}

func opCacheDel(workerTaskID string) string {
	return fmt.Sprintf("cache.DelTaskResult:%s", keyTaskResult(workerTaskID))
}

// === ① 守卫表：三条守卫逐个拒绝，且必须在触库之前 ===

func TestRunGuardsRejectBeforeTouchingAnything(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.RunTaskReq, other rpc.CapabilityType)
		want   error
	}{
		{"task_id 为空", func(in *rpc.RunTaskReq, _ rpc.CapabilityType) { in.TaskId = "" },
			model.ErrInvalidTaskID},
		{"能力/URI 也同时不合法：只报第一个守卫（顺序即契约）",
			func(in *rpc.RunTaskReq, other rpc.CapabilityType) {
				in.TaskId = ""
				in.Capability = other
				in.MediaUri = ""
			}, model.ErrInvalidTaskID},
		{"capability=UNSPECIFIED（编排侧忘记填能力）",
			func(in *rpc.RunTaskReq, _ rpc.CapabilityType) {
				in.Capability = rpc.CapabilityType_CAPABILITY_UNSPECIFIED
			}, model.ErrCapabilityMismatch},
		{"capability 是别的能力（走错方法）",
			func(in *rpc.RunTaskReq, other rpc.CapabilityType) { in.Capability = other },
			model.ErrCapabilityMismatch},
		{"capability 越界（数字 9，proto 未定义）",
			func(in *rpc.RunTaskReq, _ rpc.CapabilityType) { in.Capability = rpc.CapabilityType(9) },
			model.ErrCapabilityMismatch},
		{"能力不匹配且 media_uri 也为空：先报能力",
			func(in *rpc.RunTaskReq, other rpc.CapabilityType) {
				in.Capability = other
				in.MediaUri = ""
			}, model.ErrCapabilityMismatch},
		{"media_uri 为空", func(in *rpc.RunTaskReq, _ rpc.CapabilityType) { in.MediaUri = "" },
			model.ErrInvalidMediaURI},
	}
	for _, m := range runMethods {
		for _, c := range cases {
			st := newEnv()
			in := newRunReq(m)
			c.mutate(in, otherCapabilityOf[m.capability])
			before := st.log.snapshot()

			reply, err := m.invoke(newReqCtx(), st.svcCtx, in)
			label := m.name + " / " + c.name
			wantErrIs(t, label, err, c.want)
			wantNil(t, label, "reply", reply)
			wantNoCall(t, label, st, before)
			wantEQ(t, label, "库里一行都没写", st.worker.count(), 0)
		}
	}
}

// === ② 正常路径：逐字段投影（库内行 + 回包） ===

func TestRunPersistsTaskAndReturnsPlaceholderResult(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		label := m.name + " 正常路径"
		// 布一条陈旧结果缓存：终态写入后必须被失效（见 FinishTask）。
		st.cache.warm(runWorkerTaskID, `[{"stale":true}]`)
		before := st.log.snapshot()

		reply := runOK(t, label, m, st, newRunReq(m))

		wantOps(t, label, st.log.opsFrom(before), []string{
			opUpsert(runWorkerTaskID, runTaskID),
			opFinish(runWorkerTaskID, model.TaskStateSucceeded),
			opCacheDel(runWorkerTaskID),
		})

		row := st.worker.snapshot(runWorkerTaskID)
		if row == nil {
			t.Fatalf("%s：worker_task 里没有这一行", label)
		}
		wantEQ(t, label, "worker_task_id", row.WorkerTaskID, runWorkerTaskID)
		wantEQ(t, label, "task_id", row.TaskID, runTaskID)
		wantEQ(t, label, "capability", row.Capability, int32(m.capability))
		wantEQ(t, label, "media_uri", row.MediaURI, runMediaURI)
		wantEQ(t, label, "duration_ms", row.DurationMs, int64(137500))
		wantEQ(t, label, "params_json", row.ParamsJSON, `{"language":"zh-CN","model_version":"v3"}`)
		wantEQ(t, label, "timeout_ms", row.TimeoutMs, int64(5000))
		wantEQ(t, label, "trace_id", row.TraceID, "trace-mw-abc")
		wantEQ(t, label, "终态", row.State, int32(model.TaskStateSucceeded))
		wantEQ(t, label, "algorithm_version", row.AlgorithmVersion, "")
		wantEQ(t, label, "elapsed_ms", row.ElapsedMs, int64(0))
		wantEQ(t, label, "result_json 是空数组而不是 null", row.ResultJSON, "[]")
		wantEQ(t, label, "error_message", row.ErrorMessage, "")
		assertAround(t, label, "ctime", row.Ctime, model.NowUnix(), 2)
		assertAround(t, label, "mtime", row.Mtime, model.NowUnix(), 2)

		// 回包逐字段，并与库存互相校验：答复说成功，库里必须真写着成功。
		wantEQ(t, label, "reply.worker_task_id", reply.GetWorkerTaskId(), row.WorkerTaskID)
		wantEQ(t, label, "reply.task_id", reply.GetTaskId(), row.TaskID)
		wantEQ(t, label, "reply.capability", reply.GetCapability(), m.capability)
		wantEQ(t, label, "reply.state 与库存一致", int32(reply.GetState()), row.State)
		wantEQ(t, label, "reply.algorithm_version", reply.GetAlgorithmVersion(), row.AlgorithmVersion)
		wantEQ(t, label, "reply.elapsed_ms", reply.GetElapsedMs(), row.ElapsedMs)
		wantEQ(t, label, "reply.error_message", reply.GetErrorMessage(), "")
		wantLegalTaskState(t, label+" 落库状态", row.State)

		_, hit := st.cache.cached(runWorkerTaskID)
		wantEQ(t, label, "结果缓存已失效", hit, false)
		wantEQ(t, label, "只写一行", st.worker.count(), 1)
	}
}

// 占位实现不得伪造命中证据：落库的 result_json 必须是空数组，回包 segments 必须为空。
// 若哪天有人为了让链路「看起来有结果」塞进一条 pass 片段，这条会红。
func TestRunDoesNotInventSegments(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		label := m.name + " 不伪造证据"
		reply := runOK(t, label, m, st, newRunReq(m))
		wantEQ(t, label, "segments 数量", len(reply.GetSegments()), 0)
		wantEQ(t, label, "库存 result_json", st.worker.snapshot(runWorkerTaskID).ResultJSON, "[]")
	}
}

func TestRunTimeoutMsFallsBackToConfigDefault(t *testing.T) {
	cases := []struct {
		name       string
		reqTimeout int64
		cfgDefault int64
		want       int64
	}{
		{"请求给 0 → 用配置默认", 0, 60000, 60000},
		{"请求给负数 → 用配置默认", -1, 45000, 45000},
		{"请求给正数 → 原样落库", 1500, 60000, 1500},
		{"配置也漏成 0 → 落 0（该任务实际没有超时上限）", 0, 0, 0},
	}
	for _, m := range runMethods {
		for _, c := range cases {
			st := newStore(c.cfgDefault)
			in := newRunReq(m)
			in.TimeoutMs = c.reqTimeout
			runOK(t, m.name+" / "+c.name, m, st, in)
			wantEQ(t, m.name+" / "+c.name, "timeout_ms 落库", st.worker.snapshot(runWorkerTaskID).TimeoutMs, c.want)
			wantEQ(t, m.name+" / "+c.name, "只写一行", st.worker.count(), 1)
		}
	}
}

func TestRunParamsJSONIsSerialisedVerbatim(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]string
		want   string
	}{
		{"无参数 → 空串（不是 \"null\"）", nil, ""},
		{"空 map → 空串", map[string]string{}, ""},
		{"多参数按 key 升序（json.Marshal 排序，可复现）",
			map[string]string{"model_version": "v3", "language": "zh-CN"},
			`{"language":"zh-CN","model_version":"v3"}`},
		{"含 HTML 敏感字符的值被转义（json.Marshal 默认口径）",
			map[string]string{"pattern": "<a>&"}, `{"pattern":"\u003ca\u003e\u0026"}`},
	}
	for _, c := range cases {
		st := newEnv()
		in := newRunReq(runMethods[0])
		in.Params = c.params
		runOK(t, "RunOCR / "+c.name, runMethods[0], st, in)
		row := st.worker.snapshot(runWorkerTaskID)
		wantEQ(t, "RunOCR / "+c.name, "params_json", row.ParamsJSON, c.want)
	}
}

func TestRunGeneratesWorkerTaskIDWhenAbsent(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range runMethods {
		st := newEnv()
		label := m.name + " 生成 worker_task_id"

		reply := runOK(t, label, m, st, reqWithWorkerID(m, ""))
		id := reply.GetWorkerTaskId()
		wantEQ(t, label, "ULID 长度", len(id), 26)
		for _, ch := range id {
			if !strings.ContainsRune(ulidAlphabet, ch) {
				t.Errorf("%s：worker_task_id %q 含非 ULID 字母表字符 %q", label, id, ch)
				break
			}
		}
		// 生成的 ID 不可预测，因此只数次数 + 回读库存（纪律 5）。
		wantCount(t, label, st.log, "worker.Upsert:", 1)
		wantCount(t, label, st.log, "worker.UpdateResult:", 1)
		wantCount(t, label, st.log, "cache.DelTaskResult:", 1)
		row := st.worker.snapshot(id)
		if row == nil {
			t.Fatalf("%s：回包给的 worker_task_id 没有落库，读侧永远查不到这次执行", label)
		}
		wantEQ(t, label, "落库 task_id", row.TaskID, runTaskID)
		wantEQ(t, label, "落库 capability", row.Capability, int32(m.capability))
		wantEQ(t, label, "只写一行", st.worker.count(), 1)
		if seen[id] {
			t.Errorf("%s：worker_task_id %s 与前面某个方法生成的重复", label, id)
		}
		seen[id] = true
	}
}

// === ③ 下游失败传播：每个下游各注入一次 ===

// 建任务失败：错误必须原样传出，且**一次终态写入都不许发生**。
// 这是本服务最要紧的一条：写不进库却回 SUCCEEDED，等于让稿件绕过机审被放行。
func TestRunCreateTaskFailurePropagatesAndWritesNothing(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		st.worker.failWith("Upsert", errEngineTimeout)
		label := m.name + " 建任务写库失败"
		before := st.log.snapshot()

		reply, err := m.invoke(newReqCtx(), st.svcCtx, newRunReq(m))
		wantErrIs(t, label, err, errEngineTimeout)
		wantNil(t, label, "reply", reply)
		if !strings.Contains(err.Error(), "create task") {
			t.Errorf("%s：错误没有标明失败发生在建任务阶段，运维会被误导：%v", label, err)
		}
		wantOps(t, label, st.log.opsFrom(before), []string{opUpsert(runWorkerTaskID, runTaskID)})
		wantEQ(t, label, "库里行数", st.worker.count(), 0)
		wantCount(t, label, st.log, "worker.UpdateResult", 0)
		wantCount(t, label, st.log, "cache.DelTaskResult", 0)
	}
}

// 写终态失败：同样必须报错，并且**留下的是 PENDING**（缺陷 #1：两步写、无事务、无补偿）。
// 读侧必须能把这个 PENDING 如实报出去，不能被洗成 SUCCEEDED。
func TestRunFinishFailureIsNotReportedAsSuccess(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		st.worker.failWith("UpdateResult", errEngineTimeout)
		label := m.name + " 写终态失败"
		before := st.log.snapshot()

		reply, err := m.invoke(newReqCtx(), st.svcCtx, newRunReq(m))
		wantErrIs(t, label, err, errEngineTimeout)
		wantNil(t, label, "reply", reply)
		if !strings.Contains(err.Error(), "finish task") {
			t.Errorf("%s：错误没有标明失败发生在写终态阶段：%v", label, err)
		}
		wantOps(t, label, st.log.opsFrom(before), []string{
			opUpsert(runWorkerTaskID, runTaskID),
			opFinish(runWorkerTaskID, model.TaskStateSucceeded),
		})
		row := st.worker.snapshot(runWorkerTaskID)
		if row == nil {
			t.Fatalf("%s：建任务已经落库，这里应该还能读到那一行", label)
		}
		// 缺陷 #1（登记，不顺手改）：这一行停在 PENDING，没有事务回滚也没有重试协程推它走。
		wantEQ(t, label, "半截数据：行停在 PENDING", row.State, int32(model.TaskStatePending))
		wantCount(t, label+" 终态没写成就不能碰缓存", st.log, "cache.DelTaskResult", 0)

		got, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(
			&rpc.TaskResultReq{WorkerTaskId: runWorkerTaskID})
		wantNoErr(t, label+" 读侧", err)
		wantEQ(t, label, "读侧不得把失败洗成通过", int32(got.GetState()), int32(model.TaskStatePending))
	}
}

// 结果缓存失效失败：现状是被 Repository.FinishTask 用 `_ =` 吞掉（缺陷 #2）。
// 本用例钉住现象：DB 写着 SUCCEEDED、Redis 里那条陈旧结果还在。
// 修好后（错误该传出）本用例会红，届时请把断言改成 wantErrIs + reply==nil。
func TestRunResultCacheInvalidationFailureIsSwallowed(t *testing.T) {
	st := newEnv()
	st.cache.failWith("DelTaskResult", errCacheUnavailable)
	st.cache.warm(runWorkerTaskID, `[{"stale":true}]`)
	label := "RunOCR 缓存失效失败"

	reply := runOK(t, label, runMethods[0], st, newRunReq(runMethods[0]))
	wantEQ(t, label, "现状：仍回 SUCCEEDED", int32(reply.GetState()), int32(model.TaskStateSucceeded))
	wantEQ(t, label, "现状：库内仍是 SUCCEEDED", st.worker.snapshot(runWorkerTaskID).State, int32(model.TaskStateSucceeded))
	wantOps(t, label, st.log.opsFrom(0), []string{
		opUpsert(runWorkerTaskID, runTaskID),
		opFinish(runWorkerTaskID, model.TaskStateSucceeded),
		opCacheDel(runWorkerTaskID),
	})
	v, hit := st.cache.cached(runWorkerTaskID)
	wantEQ(t, label, "缺陷：陈旧缓存仍留在 Redis", hit, true)
	wantEQ(t, label, "缺陷：陈旧缓存内容", v, `[{"stale":true}]`)
}

// === ④ 不变量 ===

// ctx 传播：请求 ctx 只被终态写入用到，建任务走的是 context.Background()（缺陷 #3）。
// 探针直接读替身记录的 ctx，不靠日志。
func TestRunCreateTaskIgnoresRequestContext(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		label := m.name + " ctx 传播"
		runOK(t, label, m, st, newRunReq(m))
		wantEQ(t, label, "缺陷：CreateTask 没吃请求 ctx", st.probe.got(t, "Upsert", 0), false)
		wantEQ(t, label, "FinishTask 吃的是请求 ctx", st.probe.got(t, "UpdateResult", 0), true)
	}
}

// 缺陷 #3 的真实后果：客户端/编排侧取消后，建任务照旧发生（用 Background 写库），
// 终态写入才被取消 ⇒ 留下一条永远停在 PENDING 的行，而调用方已经拿到 Canceled。
func TestRunCancelledRequestLeaksPendingRow(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		ctx, cancel := context.WithCancel(newReqCtx())
		cancel()
		label := m.name + " 请求已取消"
		before := st.log.snapshot()

		reply, err := m.invoke(ctx, st.svcCtx, newRunReq(m))
		wantErrIs(t, label, err, context.Canceled)
		wantNil(t, label, "reply", reply)
		wantOps(t, label, st.log.opsFrom(before), []string{
			opUpsert(runWorkerTaskID, runTaskID),
			opFinish(runWorkerTaskID, model.TaskStateSucceeded),
		})
		wantEQ(t, label, "泄漏的半截行", st.worker.snapshot(runWorkerTaskID).State, int32(model.TaskStatePending))
		wantCount(t, label, st.log, "cache.DelTaskResult", 0)
	}
}

// 终态词表：本期占位实现只会要求写 SUCCEEDED，整个服务不存在写 FAILED/TIMEOUT 的代码路径，
// 因此「引擎超时/返回未知结论 ⇒ 降级人工」在 worker 侧目前无处表达（README 已知缺口 #6）。
// 任何越出 TaskState 枚举的状态值都会让本用例红。
func TestRunOnlyEverAsksForALegalTerminalState(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		label := m.name + " 状态词表"
		reply := runOK(t, label, m, st, newRunReq(m))
		if len(st.worker.writtenStates) != 1 {
			t.Fatalf("%s：期望只要求写一次终态，实际 %v", label, st.worker.writtenStates)
		}
		wantLegalTaskState(t, label, st.worker.writtenStates[0])
		wantEQ(t, label, "占位实现的唯一终态", st.worker.writtenStates[0], int32(model.TaskStateSucceeded))
		wantLegalTaskState(t, label+" 回包状态", int32(reply.GetState()))

		// 越界依赖检查：整条链路只能碰 worker_task 与本服务自己的 key 前缀（AGENTS.md §5）。
		for _, op := range st.log.ops {
			if !strings.HasPrefix(op, "worker.") && !strings.HasPrefix(op, "cache.") {
				t.Errorf("%s：出现了不属于本服务数据面的调用 %q", label, op)
			}
			if strings.HasPrefix(op, "cache.AcquireTaskLock") || strings.HasPrefix(op, "cache.ReleaseTaskLock") {
				t.Errorf("%s：不该在这一版本里出现运行锁调用 %q", label, op)
			}
		}
	}
}

// 幂等：同一 worker_task_id 重投只能有一行，且 ON DUPLICATE 只改 mtime，
// 第一次请求的媒体信息不会被第二次悄悄覆盖。
func TestRunReplayWithSameWorkerTaskIDKeepsSingleRow(t *testing.T) {
	st := newEnv()
	first := newRunReq(runMethods[0])
	first.MediaUri = "oss://bucket/first.m3u8"
	first.DurationMs = 1000
	first.TraceId = "trace-1"
	r1 := runOK(t, "重放第一次", runMethods[0], st, first)

	before := st.log.snapshot()
	second := newRunReq(runMethods[0])
	second.MediaUri = "oss://bucket/second.m3u8"
	second.DurationMs = 2000
	second.TraceId = "trace-2"
	r2 := runOK(t, "重放第二次", runMethods[0], st, second)

	wantOps(t, "重放第二次", st.log.opsFrom(before), []string{
		opUpsert(runWorkerTaskID, runTaskID),
		opFinish(runWorkerTaskID, model.TaskStateSucceeded),
		opCacheDel(runWorkerTaskID),
	})
	wantEQ(t, "重放", "仍然只有一行", st.worker.count(), 1)
	row := st.worker.snapshot(runWorkerTaskID)
	wantEQ(t, "重放", "media_uri 保持第一次（ON DUPLICATE 只改 mtime）", row.MediaURI, first.MediaUri)
	wantEQ(t, "重放", "duration_ms 保持第一次", row.DurationMs, first.DurationMs)
	wantEQ(t, "重放", "trace_id 保持第一次", row.TraceID, first.TraceId)
	wantEQ(t, "重放", "capability 保持第一次", row.Capability, int32(rpc.CapabilityType_CAPABILITY_OCR))
	wantEQ(t, "重放", "两次回包同一 worker_task_id", r2.GetWorkerTaskId(), r1.GetWorkerTaskId())
	wantEQ(t, "重放", "重放不会多写一行状态", len(st.worker.writtenStates), 2)
}

// 幂等的反面（缺陷 #6）：worker_task_id 相同但 task_id 换了，库里 task_id 保持第一次，
// 回包却按第二次请求回答，orchestrator 拿第二次 task_id 反查必然 NotFound。
func TestRunReplayUnderDifferentTaskIDAnswersSomethingItNeverStored(t *testing.T) {
	st := newEnv()
	first := newRunReq(runMethods[0])
	runOK(t, "第一次下发", runMethods[0], st, first)

	second := newRunReq(runMethods[0])
	second.TaskId = "1789000099"
	r2 := runOK(t, "换 task_id 重投", runMethods[0], st, second)

	row := st.worker.snapshot(runWorkerTaskID)
	wantEQ(t, "重投", "只有一行", st.worker.count(), 1)
	wantEQ(t, "重投", "库存 task_id 仍是第一次", row.TaskID, runTaskID)
	wantEQ(t, "重投", "缺陷：回包报的是第二次入参的 task_id", r2.GetTaskId(), "1789000099")

	_, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(
		&rpc.TaskResultReq{TaskId: "1789000099"})
	wantErrIs(t, "重投后按回包给的 task_id 反查", err, model.ErrTaskNotFound)
}

// 缺陷 #4：运行锁与 ErrTaskAlreadyRunning 都是死代码，并发重入无人拦。
// 这里先把锁抢走（真实实现下 AcquireRunLock 会返回 false），再下发同一任务：
// 现状是它照样完整跑完并把状态改回 SUCCEEDED。
func TestRunNeverAcquiresTheReentryLock(t *testing.T) {
	for _, m := range runMethods {
		st := newEnv()
		st.cache.locks[keyTaskLock(runWorkerTaskID)] = 1
		label := m.name + " 并发重入"

		runOK(t, label, m, st, newRunReq(m))
		wantCount(t, label+" 缺陷：从未尝试抢锁", st.log, "cache.AcquireTaskLock", 0)
		wantCount(t, label+" 缺陷：从未释放锁", st.log, "cache.ReleaseTaskLock", 0)
		wantEQ(t, label, "锁仍被占着（没人释放，也没人查）", st.cache.locked(runWorkerTaskID), true)
		wantInt32sEQ(t, label, "重复任务仍被完整执行一遍", st.worker.writtenStates, []int32{model.TaskStateSucceeded})
	}
}

// 缺陷 #5：internal/repository 的包注释声称「(task_id, capability) 联合唯一约束防重入」，
// 但 deploy/migrations/moderation-worker/000001_create_worker_task.sql 里只有
// 非唯一的 idx_task_id_mtime。于是同一 task_id + 能力换个工作任务号就能写进第二行，
// 而按 task_id 反查时读到哪一行是不确定的（ORDER BY mtime DESC，同秒并列由 MySQL 决定）。
func TestSameTaskIDAndCapabilityCreatesSecondRow(t *testing.T) {
	st := newEnv()
	r1 := runOK(t, "同 task_id 第一次", runMethods[0], st, reqWithWorkerID(runMethods[0], runWorkerTaskID))
	r2 := runOK(t, "同 task_id 第二次", runMethods[0], st, reqWithWorkerID(runMethods[0], runWorkerTaskID2))

	wantEQ(t, "同 task_id 重入", "两行（防重入约束并不存在）", st.worker.count(), 2)
	wantEQ(t, "同 task_id 重入", "第一次回包 id", r1.GetWorkerTaskId(), runWorkerTaskID)
	wantEQ(t, "同 task_id 重入", "第二次回包 id", r2.GetWorkerTaskId(), runWorkerTaskID2)
	a := st.worker.snapshot(runWorkerTaskID)
	b := st.worker.snapshot(runWorkerTaskID2)
	if a == nil || b == nil {
		t.Fatalf("两行都应该在库里：%v / %v", a, b)
	}
	wantEQ(t, "同 task_id 重入", "两行 task_id 相同", a.TaskID, b.TaskID)
	wantEQ(t, "同 task_id 重入", "两行 capability 相同", a.Capability, b.Capability)

	// 按 task_id 反查只能给出其中一行：具体哪一行不由代码决定，因此只断言「不报错且命中本 task_id」。
	got, err := NewGetTaskResultLogic(newReqCtx(), st.svcCtx).GetTaskResult(&rpc.TaskResultReq{TaskId: runTaskID})
	wantNoErr(t, "同 task_id 重入 反查", err)
	wantEQ(t, "同 task_id 重入 反查", "task_id", got.GetTaskId(), runTaskID)
	if got.GetWorkerTaskId() != runWorkerTaskID && got.GetWorkerTaskId() != runWorkerTaskID2 {
		t.Errorf("同 task_id 重入 反查：返回了不存在的 worker_task_id %q", got.GetWorkerTaskId())
	}
}

// reqWithWorkerID 生成一个指定 worker_task_id 的合法请求。
func reqWithWorkerID(m runMethod, workerTaskID string) *rpc.RunTaskReq {
	in := newRunReq(m)
	in.WorkerTaskId = workerTaskID
	return in
}
