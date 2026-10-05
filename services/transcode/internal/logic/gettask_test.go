package logic

// gettask_test.go 覆盖 GetTask 的 cache-aside 读写序：命中不查库、miss 回源并回填、
// 回填吞错、脏缓存与缓存读失败一律不回源、查无此行不写负缓存、守卫先于触依赖。

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"go-video/services/transcode/model"
	"go-video/services/transcode/rpc"
)

func getTaskCall(e *env, taskID int64) (*rpc.TaskReply, error) {
	return NewGetTaskLogic(context.Background(), e.svcCtx).GetTask(&rpc.TaskReq{TaskId: taskID})
}

// seedRow101 布一条典型的 PROCESSING 任务（进度 55、带媒资四要素）。
func seedRow101(t *testing.T, st *store) *model.TranscodeTask {
	t.Helper()
	now := nowUnix()
	row := seedTask(t, st, &model.TranscodeTask{
		TaskId: 101, AssetId: 5001, TemplateId: 7,
		InputBucket: "in-bucket", InputKey: "raw/a.mp4",
		OutputBucket: "out-bucket", OutputKey: "hls/101/1080p.m3u8",
		State: model.TaskStateProcessing, Progress: 55,
		Ctime: now - 300, Mtime: now - 60,
	})
	if row == nil {
		t.Fatal("布景失败")
	}
	return row
}

// TestGetTask未命中时回源并回填整行 钉住读穿三步的**顺序**：先查缓存、再查库、最后回填；
// 并钉住回填的载荷就是库存那一行的 JSON（不是空对象、也不是应答的投影结果）。
func TestGetTask未命中时回源并回填整行(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedRow101(t, st)

	got, err := getTaskCall(e, 101)
	wantNoErr(t, "GetTask", err)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101",
		"transcode_task.FindOne:101",
		"cache.SetTask:tc:task:101",
	)
	wantTaskReplyMatchesRow(t, "应答 vs 库存", got, row)

	// 回填载荷逐字等于库存行的 JSON ⇒ 下一次读命中后拿到的必须是同一份数据。
	payload, ok := st.cache.raw(keyTask(101))
	if !ok {
		t.Fatal("回填没有写入 tc:task:101")
	}
	var decoded model.TranscodeTask
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("回填的不是合法 JSON：%v（载荷 %q）", err, payload)
	}
	wantTaskReplyMatchesRow(t, "回填载荷 vs 库存", toTaskReply(&decoded), row)
}

// TestGetTask命中缓存时不查库 与上一条构成对照：同一个 task_id 第二次读只剩一次缓存读。
func TestGetTask命中缓存时不查库(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedRow101(t, st)

	if _, err := getTaskCall(e, 101); err != nil { // 第一次读负责回填
		t.Fatalf("首次读失败：%v", err)
	}
	before := st.log.snapshot()
	got, err := getTaskCall(e, 101)
	wantNoErr(t, "第二次读", err)
	wantSeq(t, "第二次读的轨迹", st.log, before, "cache.GetTask:tc:task:101")
	wantEq(t, "第二次读", "progress 仍来自缓存", got.GetProgress(), int32(55))
}

// TestGetTask缓存里的旧进度胜出 钉住「60s 内读到过期进度」这一后果：库存已被 Worker 推到 90，
// 缓存里还是 55，应答就是 55，而且**一次库都不查**（所以 Worker 上报的进度对读侧最多晚 60s）。
func TestGetTask缓存里的旧进度胜出(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedRow101(t, st)
	warmTaskCache(t, st, row) // 缓存：progress=55

	st.tasks.poke(101, func(r *model.TranscodeTask) { r.Progress = 90 }) // 库存已前进

	got, err := getTaskCall(e, 101)
	wantNoErr(t, "脏缓存读", err)
	wantEq(t, "脏缓存读", "应答 progress（旧值）", got.GetProgress(), int32(55))
	wantEq(t, "脏缓存读", "库存 progress（新值）", st.task(101).Progress, int32(90))
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTask:tc:task:101")
}

// TestGetTask查无此行返回哨兵且不写负缓存 钉住 model 的 (nil,nil) 在 repository 被翻译成
// ErrTaskNotFound；并且失败路径不留键（否则一条不存在的 task_id 会被缓存 60s）。
func TestGetTask查无此行返回哨兵且不写负缓存(t *testing.T) {
	e := newEnv(t)
	st := e.st

	got, err := getTaskCall(e, 404)
	wantErrIs(t, "不存在的任务", err, model.ErrTaskNotFound)
	if got != nil {
		t.Errorf("拒绝时仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:404", "transcode_task.FindOne:404")
	wantCount(t, "负缓存", st.log, "cache.SetTask", 0)
	if _, ok := st.cache.raw(keyTask(404)); ok {
		t.Error("查无此行却留下了缓存键")
	}
}

// TestGetTask脏缓存直接报错既不回源也不清键 钉住 repository.GetTask 的
// `json.Unmarshal 失败 ⇒ 返回错误`（repository.go:167-169）：读接口在 TTL 内被一条脏数据钉死，
// 而清键动作（DelTask）一次都没发生，所以只能等 60s 过期或有人恰好上报一次进度。
func TestGetTask脏缓存直接报错既不回源也不清键(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedRow101(t, st)
	st.cache.kv[keyTask(101)] = `{"TaskId":101,` // 半截 JSON

	got, err := getTaskCall(e, 101)
	wantErrContains(t, "脏缓存", err, "transcode/GetTask unmarshal cache")
	if got != nil {
		t.Errorf("脏缓存路径仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTask:tc:task:101")
	wantCount(t, "脏缓存不回源", st.log, "transcode_task.FindOne", 0)
	wantCount(t, "脏缓存不清键", st.log, "cache.DelTask", 0)
	if _, ok := st.cache.raw(keyTask(101)); !ok {
		t.Error("脏键被清掉了，与生产语义不符")
	}
}

// TestGetTask缓存读失败时不降级回源 钉住「Redis 抖动放大成任务读不可用」：
// 判别性对照是 TestGetTask未命中时回源并回填整行——miss（无错误）会走到 FindOne，
// 而带错误的 GetTask 直接返回，一次库都不查。
func TestGetTask缓存读失败时不降级回源(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedRow101(t, st)
	redisDown := errors.New("redis: connection refused")
	st.s.fail("cache.GetTask", redisDown)

	got, err := getTaskCall(e, 101)
	wantErrIs(t, "缓存读失败", err, redisDown)
	if got != nil {
		t.Errorf("缓存读失败仍返回应答 %+v", got)
	}
	wantSeq(t, "轨迹", st.log, 0, "cache.GetTask:tc:task:101")
	wantCount(t, "缓存读失败", st.log, "transcode_task.FindOne", 0)
}

// TestGetTask库读失败原样上抛且不回填 钉住错误包装（`transcode_task FindOne: %w`）与
// 「失败路径不留脏键」两件事。
func TestGetTask库读失败原样上抛且不回填(t *testing.T) {
	e := newEnv(t)
	st := e.st
	seedRow101(t, st)
	st.s.fail("transcode_task.FindOne", errors.New("error 1146: table doesn't exist"))

	_, err := getTaskCall(e, 101)
	wantErrContains(t, "库读失败", err, "transcode_task FindOne:")
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101")
	wantCount(t, "库读失败", st.log, "cache.SetTask", 0)
}

// TestGetTask回填失败被吞 钉住 repository.go:180 的 `_ = r.cache.SetTask(...)`：
// 缓存写挂了不影响读接口可用性（写侧宽容是对的），代价是下一次仍要回源。
func TestGetTask回填失败被吞(t *testing.T) {
	e := newEnv(t)
	st := e.st
	row := seedRow101(t, st)
	st.s.fail("cache.SetTask", errors.New("OOM command not allowed"))

	got, err := getTaskCall(e, 101)
	wantNoErr(t, "回填失败", err)
	wantTaskReplyMatchesRow(t, "回填失败仍返回数据", got, row)
	wantSeq(t, "轨迹", st.log, 0,
		"cache.GetTask:tc:task:101", "transcode_task.FindOne:101", "cache.SetTask:tc:task:101")
	if _, ok := st.cache.raw(keyTask(101)); ok {
		t.Error("SetTask 报错了但键仍然写进去了，替身与生产口径不符")
	}
}

// TestGetTask非正 task_id 先于任何依赖被拒 钉住守卫顺序（含 0 与负数两侧的边界）。
func TestGetTask非正task_id先于任何依赖被拒(t *testing.T) {
	for _, id := range []int64{0, -1, -101} {
		e := newEnv(t)
		seedRow101(t, e.st) // 库里有一行，用来证明「没去查」不是「查不到」
		got, err := getTaskCall(e, id)
		wantErrIs(t, "task_id 越界", err, model.ErrInvalidTaskID)
		if got != nil {
			t.Errorf("task_id=%d 拒绝时仍返回应答 %+v", id, got)
		}
		wantNoCall(t, "task_id 越界", e.st.log, 0)
	}
	// 判别性另一半：1 是合法最小值，会正常走到依赖调用。
	e := newEnv(t)
	seedRow101(t, e.st)
	_, err := getTaskCall(e, 1)
	wantErrIs(t, "task_id=1 走查库", err, model.ErrTaskNotFound)
	wantSeq(t, "轨迹", e.st.log, 0, "cache.GetTask:tc:task:1", "transcode_task.FindOne:1")
}
