package logic

// updateprogress_test.go 覆盖 UpdateProgress：三段入参守卫、状态机门槛（终态/脏数据）、
// 缓存与库两条校验读路径带来的并发窗口、errno/err_msg 的无条件覆盖，
// 以及「UPDATE 之后回读为 nil 却报成功」这条静默失败。
//
// 本服务的写路径只有单条 UPDATE、没有事务，所以失败类用例断言的是
// 「库里残留什么 + 语句顺序」，不涉及回滚（生产也没有可回滚的多语句）。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func progressCall(e *env, in *rpc.UpdateProgressReq) (*rpc.TaskReply, error) {
	return NewUpdateProgressLogic(context.Background(), e.svcCtx).UpdateProgress(in)
}

// seedProcessing101 布一条「转了一半」的任务：进度 50、带一条历史失败原因，
// 便于同时观察 progress / errno / err_msg 三个字段的覆盖行为。
func seedProcessing101(t *testing.T, st *store) *model.TranscodeTask {
	t.Helper()
	now := nowUnix()
	row := seedTask(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7,
		InputBucket: "in-bucket", InputKey: "raw/a.mp4",
		OutputBucket: "out-bucket", OutputKey: "hls/101/1080p.m3u8",
		State: model.TaskStateProcessing, Progress: 50,
		Errno: 7, ErrMsg: "ffmpeg killed by oom",
		Ctime: now - 300, Mtime: now - 60,
	})
	if row == nil {
		t.Fatal("布景失败")
	}
	return row
}

// guardPassReq 是通过三段守卫的最小请求（state=PROCESSING），用例只改被考察的字段。
func guardPassReq(taskID int64) *rpc.UpdateProgressReq {
	return &rpc.UpdateProgressReq{TaskId: taskID, Progress: 60, State: rpc.TaskState_TASK_STATE_PROCESSING}
}

// TestUpdateProgress从PENDING推到PROCESSING留下六步轨迹 钉住一次成功上报的完整依赖序：
// 守卫读（cache→库→回填）→ UPDATE → 删缓存 → 再读一次库。
// 结论有两条不好不显眼：① 守卫读**顺手回填**了缓存，随后又被自己删掉（第 3、5 步白做）；
// ② 最后那次回读**不再回填**（repository.go:197 走的是裸 FindOne），所以下一次 GetTask 还要回源。
func TestUpdateProgress从PENDING推到PROCESSING留下六步轨迹(t *testing.T) {
	e := newEnv(t)
	st := e.st
	now := nowUnix()
	seedTask(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7,
		InputBucket: "in-bucket", InputKey: "raw/a.mp4",
		OutputBucket: "out-bucket", OutputKey: "hls/101/1080p.m3u8",
		State: model.TaskStatePending, Ctime: now - 300, Mtime: now - 300,
	})

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 45, State: rpc.TaskState_TASK_STATE_PROCESSING,
	})
	wantNoErr(t, "PENDING→PROCESSING", err)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101",
		"transcode_task.FindOne:101",
		"cache.SetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/2/45",
		"cache.DelTask:tc:task:101",
		"transcode_task.FindOne:101",
	)

	row := st.task(101)
	wantTaskReplyMatchesRow(t, "应答 vs 库存", got, row)
	wantEq(t, "推进后", "state", int32(got.GetState()), model.TaskStateProcessing)
	wantEq(t, "推进后", "progress", got.GetProgress(), int32(45))
	// 不带的字段一个都不许被顺手改掉（UPDATE 只列了 5 个 SET 目标）。
	wantEq(t, "推进后", "asset_id 保持", row.AssetId, int64(5001))
	wantEq(t, "推进后", "template_id 保持", row.TemplateId, int64(7))
	wantEq(t, "推进后", "input_key 保持", row.InputKey, "raw/a.mp4")
	wantEq(t, "推进后", "output_key 保持", row.OutputKey, "hls/101/1080p.m3u8")
	wantEq(t, "推进后", "ctime 保持", row.Ctime, now-300)
	if row.Mtime <= row.Ctime {
		t.Errorf("mtime = %d, 应晚于 ctime = %d", row.Mtime, row.Ctime)
	}
	assertAround(t, "推进后", "mtime", row.Mtime, now, 2)
	// 结论 ①②：缓存被自己删干净，且回读不回填。
	if _, ok := st.cache.raw(keyTask(101)); ok {
		t.Error("上报完成后缓存里仍有任务快照，与生产语义不符")
	}
}

// TestUpdateProgress三段守卫先于触库 钉住入参校验（task_id / progress / state）全部发生在
// 碰缓存和碰库之前，且库存与缓存分毫未动。state=PENDING 也在拒绝之列：Worker 不能把任务退回待处理。
func TestUpdateProgress三段守卫先于触库(t *testing.T) {
	cases := []struct {
		name   string
		req    *rpc.UpdateProgressReq
		want   error
		stored int32 // 期望库里 state 保持不变
	}{
		{"task_id 为 0", guardPassReq(0), model.ErrInvalidTaskID, model.TaskStateProcessing},
		{"task_id 为负", guardPassReq(-101), model.ErrInvalidTaskID, model.TaskStateProcessing},
		{"progress 为 -1", withProgress(guardPassReq(101), -1), model.ErrInvalidProgress, model.TaskStateProcessing},
		{"progress 为 101", withProgress(guardPassReq(101), 101), model.ErrInvalidProgress, model.TaskStateProcessing},
		{"progress 为 int32 上限", withProgress(guardPassReq(101), 1<<30), model.ErrInvalidProgress, model.TaskStateProcessing},
		{"state 未指定", withState(guardPassReq(101), rpc.TaskState_TASK_STATE_UNSPECIFIED), model.ErrInvalidState, model.TaskStateProcessing},
		{"state 退回 PENDING", withState(guardPassReq(101), rpc.TaskState_TASK_STATE_PENDING), model.ErrInvalidState, model.TaskStateProcessing},
		{"state 越界 99", withState(guardPassReq(101), rpc.TaskState(99)), model.ErrInvalidState, model.TaskStateProcessing},
		{"state 为负", withState(guardPassReq(101), rpc.TaskState(-2)), model.ErrInvalidState, model.TaskStateProcessing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			st := e.st
			seedProcessing101(t, st)
			before := st.task(101)

			got, err := progressCall(e, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if got != nil {
				t.Errorf("%s：拒绝时仍返回应答 %+v", tc.name, got)
			}
			wantNoCall(t, tc.name, st.log, 0)
			wantEq(t, tc.name, "库里 state 不变", st.task(101).State, before.State)
			wantEq(t, tc.name, "库里 progress 不变", st.task(101).Progress, before.Progress)
			wantEq(t, tc.name, "库里 errno 不变", st.task(101).Errno, before.Errno)
		})
	}
	// 判别性另一半：progress 的 0 与 100 两个端点、state 的 2/3/4 三个取值都合法。
	for _, p := range []int32{0, 100} {
		e := newEnv(t)
		seedProcessing101(t, e.st)
		_, err := progressCall(e, withProgress(guardPassReq(101), p))
		wantNoErr(t, "progress 端点合法", err)
		wantEq(t, "progress 端点合法", "落库值", e.st.task(101).Progress, p)
	}
	for _, s := range []rpc.TaskState{
		rpc.TaskState_TASK_STATE_PROCESSING, rpc.TaskState_TASK_STATE_SUCCEEDED, rpc.TaskState_TASK_STATE_FAILED,
	} {
		e := newEnv(t)
		seedProcessing101(t, e.st)
		_, err := progressCall(e, withState(guardPassReq(101), s))
		wantNoErr(t, "state 合法值放行", err)
		wantEq(t, "state 合法值放行", "落库值", e.st.task(101).State, int32(s))
	}
}

func withProgress(in *rpc.UpdateProgressReq, p int32) *rpc.UpdateProgressReq {
	in.Progress = p
	return in
}

func withState(in *rpc.UpdateProgressReq, s rpc.TaskState) *rpc.UpdateProgressReq {
	in.State = s
	return in
}

// TestUpdateProgress终态在守卫读里被检出则零写入 钉住状态机有效的一侧：
// 库存已是 SUCCEEDED ⇒ ErrTerminalState，UPDATE 一次都不发，失败原因也不会被写进去。
// 注意即使被拒，守卫读仍然回填了缓存（第 3 步），这是后面几条并发用例的根因。
func TestUpdateProgress终态在守卫读里被检出则零写入(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	st.tasks.poke(101, func(r *model.TranscodeTask) {
		r.State, r.Progress, r.Errno, r.ErrMsg = model.TaskStateSucceeded, 100, 0, ""
	})

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 100, State: rpc.TaskState_TASK_STATE_FAILED, Errno: 7, ErrMsg: "late failure",
	})
	wantErrIs(t, "终态拒绝", err, model.ErrTerminalState)
	if got != nil {
		t.Errorf("终态拒绝时仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101")
	wantCount(t, "终态拒绝", st.log, "transcode_task.UpdateProgress", 0)
	row := st.task(101)
	wantEq(t, "终态拒绝", "库里 state", row.State, model.TaskStateSucceeded)
	wantEq(t, "终态拒绝", "库里 progress", row.Progress, int32(100))
	wantEq(t, "终态拒绝", "库里 errno", row.Errno, int32(0))
	wantEq(t, "终态拒绝", "库里 err_msg", row.ErrMsg, "")
}

// TestUpdateProgress终态在守卫读之后被插队则丢失更新被判成功 是上一条的判别性另一半：
// 同一份终态数据，只要落在「守卫读之后、UPDATE 之前」这个窗口里就检不出来——
// Repository 的校验读不带 session（updateprogresslogic.go:44 走 GetTask）、
// UPDATE 也没有 `AND state = ?` 的 CAS 条件（transcodemodel.go:126），
// 于是 Worker B 刚落地的 FAILED + 失败原因被 Worker A 的 SUCCEEDED 无条件覆盖，整调用还报成功。
// 修法见 README 已知缺口 4。
func TestUpdateProgress终态在守卫读之后被插队则丢失更新被判成功(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTask(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7,
		InputBucket: "in", InputKey: "a", OutputBucket: "out", OutputKey: "b",
		State: model.TaskStatePending, Ctime: nowUnix() - 300, Mtime: nowUnix() - 300,
	})
	st.s.before("transcode_task.UpdateProgress", 1, func() {
		st.tasks.poke(101, func(r *model.TranscodeTask) {
			r.State, r.Progress, r.Errno, r.ErrMsg = model.TaskStateFailed, 88, 137, "ffmpeg segfault"
		})
	})

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 100, State: rpc.TaskState_TASK_STATE_SUCCEEDED,
	})
	wantNoErr(t, "丢失更新被判成功", err)
	row := st.task(101)
	wantEq(t, "丢失更新", "库里 state 覆盖掉了刚落的 FAILED", row.State, model.TaskStateSucceeded)
	wantEq(t, "丢失更新", "库里 progress", row.Progress, int32(100))
	wantEq(t, "丢失更新", "B 的 errno 被抹掉", row.Errno, int32(0))
	wantEq(t, "丢失更新", "B 的失败原因被抹掉", row.ErrMsg, "")
	wantEq(t, "丢失更新", "应答 state", int32(got.GetState()), model.TaskStateSucceeded)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/3/100", "cache.DelTask:tc:task:101", "transcode_task.FindOne:101")
	st.s.checkHooks(t)
}

// TestUpdateProgress的校验读走缓存所以终态可被回退 钉住 concurrency 的第二个入口：
// 守卫用的是 repository.GetTask（60s 缓存），缓存里那份过期快照写着 PENDING，
// 而库里其实早已 SUCCEEDED ⇒ 校验通过 ⇒ 无条件 UPDATE 把任务从终态**退回** PROCESSING。
// 与上一条的区别只在「旧状态从哪来」：这条来自缓存，那条来自语句间插队。
func TestUpdateProgress的校验读走缓存所以终态可被回退(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	st.tasks.poke(101, func(r *model.TranscodeTask) {
		r.State, r.Progress, r.Errno, r.ErrMsg = model.TaskStateSucceeded, 100, 0, ""
	})
	// 缓存里是一份早先的 PENDING 快照（真实场景：上一个 Worker 在推进前读过）。
	warmTaskCache(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7, State: model.TaskStatePending, Progress: 10,
	})

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 20, State: rpc.TaskState_TASK_STATE_PROCESSING,
	})
	wantNoErr(t, "过期快照放行", err)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/2/20",
		"cache.DelTask:tc:task:101",
		"transcode_task.FindOne:101",
	)
	wantCount(t, "守卫命中缓存", st.log, "transcode_task.FindOne:101", 1) // 只有回读那一次
	row := st.task(101)
	wantEq(t, "过期快照放行", "库里 state 从终态回退", row.State, model.TaskStateProcessing)
	wantEq(t, "过期快照放行", "库里 progress 回退", row.Progress, int32(20))
	wantEq(t, "过期快照放行", "应答 state", int32(got.GetState()), model.TaskStateProcessing)
}

// TestUpdateProgress进度可以回退 钉住 UPDATE 不带 GREATEST/单调判定：
// Worker 重启后从 0 重报，会把已经跑到 90 的任务在**读侧**倒退回 10（对照：正常前进 95 也走同一条语句）。
// 判别性在于两条请求只有 progress 不同，所以红/绿差异只能来自「有没有单调保护」。
func TestUpdateProgress进度可以回退(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedTask(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7,
		InputBucket: "in", InputKey: "a", OutputBucket: "out", OutputKey: "b",
		State: model.TaskStateProcessing, Progress: 90, Ctime: nowUnix() - 300, Mtime: nowUnix() - 60,
	})

	_, err := progressCall(e, withProgress(guardPassReq(101), 10))
	wantNoErr(t, "进度回退", err)
	wantEq(t, "进度回退", "库里 progress", st.task(101).Progress, int32(10))

	_, err = progressCall(e, withProgress(guardPassReq(101), 95))
	wantNoErr(t, "进度前进（对照）", err)
	wantEq(t, "进度前进（对照）", "库里 progress", st.task(101).Progress, int32(95))
}

// TestUpdateProgress成功态可以与进度不一致 钉住 progress 与 state 之间没有交叉校验：
// 报 SUCCEEDED 时带 progress=3 也算成功落库，而 §8 里 SUCCEEDED 正是稿件推进
// READY_FOR_REVIEW 的唯一凭据（对照：报 PROCESSING 时带 3 同样合法，说明判定完全不看 progress）。
func TestUpdateProgress成功态可以与进度不一致(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 3, State: rpc.TaskState_TASK_STATE_SUCCEEDED,
	})
	wantNoErr(t, "SUCCEEDED 但进度 3", err)
	wantEq(t, "SUCCEEDED 但进度 3", "库里 state", st.task(101).State, model.TaskStateSucceeded)
	wantEq(t, "SUCCEEDED 但进度 3", "库里 progress", st.task(101).Progress, int32(3))
	wantEq(t, "SUCCEEDED 但进度 3", "应答 progress", got.GetProgress(), int32(3))
}

// TestUpdateProgress漏传errno会抹掉既有失败原因 钉住 `SET errno=?, err_msg=?` 的无条件覆盖：
// 判别性对照是同一条语句带上 errno/err_msg 时正常写入，所以差别只在「这次没带」。
// 后果：Worker 的中间态心跳会把上一次的失败原因清成 0/""，排障时任务看起来「一切正常」。
func TestUpdateProgress漏传errno会抹掉既有失败原因(t *testing.T) {
	t.Run("带上则写入", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedProcessing101(t, st)
		_, err := progressCall(e, &rpc.UpdateProgressReq{
			TaskId: 101, Progress: 55, State: rpc.TaskState_TASK_STATE_PROCESSING,
			Errno: 8, ErrMsg: "retry after network reset",
		})
		wantNoErr(t, "带上 errno", err)
		wantEq(t, "带上 errno", "库里 errno", st.task(101).Errno, int32(8))
		wantEq(t, "带上 errno", "库里 err_msg", st.task(101).ErrMsg, "retry after network reset")
	})

	t.Run("不带则清零", func(t *testing.T) {
		e := newEnv(t)
		st := e.st
		seedProcessing101(t, st) // 布景自带 errno=7 / err_msg="ffmpeg killed by oom"
		_, err := progressCall(e, guardPassReq(101))
		wantNoErr(t, "漏传 errno", err)
		wantEq(t, "漏传 errno", "库里 errno 被清零", st.task(101).Errno, int32(0))
		wantEq(t, "漏传 errno", "库里 err_msg 被清空", st.task(101).ErrMsg, "")
	})
}

// TestUpdateProgress对脏state的行才报ErrInvalidTransition 钉住这条兜底的真实触发条件：
// IsTerminal 已经先拦掉 3/4，而 1/2 全部放行，所以只有 state 落在枚举之外（0 或越界值）时
// 才会走到 ErrInvalidTransition。库里那条 state=0 的行（迁移列默认值就是 0）永远推不动，
// 只能人工改库——README 已知缺口 7。判别性：同一请求打到 state=1 的行就成功。
func TestUpdateProgress对脏state的行才报ErrInvalidTransition(t *testing.T) {
	for _, dirty := range []int32{0, 5, 99} {
		e := newEnv(t)
		st := e.st
		seedTask(t, st, &model.TranscodeTask{
			TaskId: 101, AssetId: 5001, TemplateId: 7,
			InputBucket: "in", InputKey: "a", OutputBucket: "out", OutputKey: "b",
			State: dirty, Ctime: nowUnix() - 300, Mtime: nowUnix() - 300,
		})

		_, err := progressCall(e, guardPassReq(101))
		wantErrIs(t, "脏 state", err, model.ErrInvalidTransition)
		wantEq(t, "脏 state", "库里 state 不变", st.task(101).State, dirty)
		wantCount(t, "脏 state", st.log, "transcode_task.UpdateProgress", 0)
		wantSeq(t, "轨迹", st.log, 0,
			"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101")
	}
	e := newEnv(t)
	seedTask(t, e.st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7, State: model.TaskStatePending,
		InputBucket: "in", InputKey: "a", OutputBucket: "out", OutputKey: "b",
	})
	_, err := progressCall(e, guardPassReq(101))
	wantNoErr(t, "同样请求打到 PENDING 行则放行（对照）", err)
}

// TestUpdateProgress任务不存在时报ErrTaskNotFound且零写入 钉住守卫读的 not-found 短路。
func TestUpdateProgress任务不存在时报ErrTaskNotFound且零写入(t *testing.T) {
	e := newEnv(t)
	st := e.st

	got, err := progressCall(e, guardPassReq(404))
	wantErrIs(t, "不存在的任务", err, model.ErrTaskNotFound)
	if got != nil {
		t.Errorf("拒绝时仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTask:tc:task:404", "transcode_task.FindOne:404")
	wantCount(t, "不存在的任务", st.log, "transcode_task.UpdateProgress", 0)
	wantEq(t, "不存在的任务", "库里行数", st.taskRows(), 0)
}

// TestUpdateProgress的UPDATE失败时不删缓存 钉住 repository.go:193-197 的顺序：
// UPDATE 先失败就直接返回，缓存里**留着守卫读刚回填的旧快照**——于是 Worker 重试的每一次
// 都会从缓存读到同一个旧状态，60s 内既推不动也看不到真实进度。
func TestUpdateProgress的UPDATE失败时不删缓存(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	st.s.fail("transcode_task.UpdateProgress", errors.New("error 1213: deadlock found"))

	_, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 70, State: rpc.TaskState_TASK_STATE_FAILED, Errno: 1213, ErrMsg: "deadlock",
	})
	wantErrContains(t, "UPDATE 失败", err, "transcode_task UpdateProgress:")
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/4/70",
	)
	wantCount(t, "UPDATE 失败", st.log, "cache.DelTask", 0)
	wantCount(t, "UPDATE 失败", st.log, "transcode_task.FindOne", 1)
	row := st.task(101)
	wantEq(t, "UPDATE 失败", "库里 state 不变", row.State, model.TaskStateProcessing)
	wantEq(t, "UPDATE 失败", "库里 progress 不变", row.Progress, int32(50))
	if _, ok := st.cache.raw(keyTask(101)); !ok {
		t.Error("期望缓存里留着旧快照（repository 失败路径不清键）")
	}
}

// TestUpdateProgress的行在守卫读后消失时报ErrTaskNotFound 钉住 model 的 RowsAffected==0 口径：
// UPDATE 命中 0 行被翻译成 ErrTaskNotFound（不是「更新成功但没数据」）。
func TestUpdateProgress的行在守卫读后消失时报ErrTaskNotFound(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	st.s.before("transcode_task.UpdateProgress", 1, func() { st.tasks.remove(101) })

	_, err := progressCall(e, guardPassReq(101))
	wantErrIs(t, "行消失", err, model.ErrTaskNotFound)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/2/60",
	)
	wantCount(t, "行消失", st.log, "cache.DelTask", 0)
	wantEq(t, "行消失", "库里行数", st.taskRows(), 0)
	st.s.checkHooks(t)
}

// TestUpdateProgress回读为nil却返回空应答成功 钉住本轮最危险的一条现状：
// Repository.UpdateProgress 直接返回 taskMd.FindOne 的 (nil, nil)（repository.go:197），
// logic 又把它交给 toTaskReply(nil) ⇒ 应答是一个全零 TaskReply + err=nil。
// 调用方（Worker/上层状态机）看到「成功」，但 task_id 已经变 0、状态变 UNSPECIFIED，
// 于是 §8 的 READY_FOR_REVIEW 推进拿到的是空任务。判别性对照是同一条链路在行仍在时
// 返回的是完整应答（见本文件第一条用例）。修法见 README 已知缺口 8。
func TestUpdateProgress回读为nil却返回空应答成功(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	// 第 2 次 FindOne 就是 UPDATE 之后的回读。
	st.s.before("transcode_task.FindOne", 2, func() { st.tasks.remove(101) })

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 100, State: rpc.TaskState_TASK_STATE_SUCCEEDED,
	})
	wantNoErr(t, "回读为 nil", err)
	if got == nil {
		t.Fatal("生产返回了 nil 应答，与 toTaskReply(nil) 的语义不符")
	}
	wantEq(t, "空应答冒充成功", "task_id", got.GetTaskId(), int64(0))
	wantEq(t, "空应答冒充成功", "state", int32(got.GetState()), int32(rpc.TaskState_TASK_STATE_UNSPECIFIED))
	wantEq(t, "空应答冒充成功", "progress", got.GetProgress(), int32(0))
	wantEq(t, "空应答冒充成功", "asset_id", got.GetAssetId(), int64(0))
	wantEq(t, "库里行数", "残留", st.taskRows(), 0)
	wantCount(t, "UPDATE 确实发过", st.log, "transcode_task.UpdateProgress", 1)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101",
		"transcode_task.UpdateProgress:101/3/100", "cache.DelTask:tc:task:101", "transcode_task.FindOne:101")
	st.s.checkHooks(t)
}

// TestUpdateProgress删缓存失败被吞 钉住 repository.go:196 的 `_ = r.cache.DelTask(...)`：
// 库里已经写成新状态，缓存里却还是旧快照，而调用方拿到的应答是对的 ⇒
// 读侧与守卫侧接下来 60s 都在看旧数据（本文件「终态可被回退」那条用例的触发条件之一）。
func TestUpdateProgress删缓存失败被吞(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedProcessing101(t, st)
	warmTaskCache(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7, State: model.TaskStatePending, Progress: 10,
	})
	st.s.fail("cache.DelTask", errors.New("redis: read-only replica"))

	got, err := progressCall(e, &rpc.UpdateProgressReq{
		TaskId: 101, Progress: 80, State: rpc.TaskState_TASK_STATE_PROCESSING,
	})
	wantNoErr(t, "删缓存失败", err)
	wantEq(t, "删缓存失败", "应答 progress（来自库）", got.GetProgress(), int32(80))
	wantEq(t, "删缓存失败", "库里 progress", st.task(101).Progress, int32(80))
	payload, ok := st.cache.raw(keyTask(101))
	if !ok {
		t.Fatal("DelTask 报错却把键删掉了，替身与生产口径不符")
	}
	var stale model.TranscodeTask
	if err := json.Unmarshal([]byte(payload), &stale); err != nil {
		t.Fatalf("缓存载荷不是合法 JSON：%v", err)
	}
	wantEq(t, "删缓存失败", "缓存里仍是旧 progress", stale.Progress, int32(10))
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.UpdateProgress:101/2/80",
		"cache.DelTask:tc:task:101", "transcode_task.FindOne:101")
}

// TestUpdateProgress的缓存读失败挡住上报 与 TestGetTask缓存读失败时不降级回源 同源：
// 守卫读用的是 cache-aside 的 GetTask，所以 Redis 一抖，Worker 的进度上报整条链路直接失败，
// 一次 UPDATE 都不会发出（对照：miss 时正常回源并推进）。
func TestUpdateProgress的缓存读失败挡住上报(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedProcessing101(t, st)
	redisDown := errors.New("redis: connection refused")
	st.s.fail("cache.GetTask", redisDown)

	_, err := progressCall(e, guardPassReq(101))
	wantErrIs(t, "缓存读失败", err, redisDown)
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTask:tc:task:101")
	wantCount(t, "缓存读失败", st.log, "transcode_task.", 0)
	wantEq(t, "缓存读失败", "库里 progress 不变", st.task(101).Progress, row.Progress)
}
