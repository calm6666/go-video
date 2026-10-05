package logic

// gettask_test.go 覆盖 GetTaskLogic.GetTask（判定链在 repository.Repository.GetTask：
// 缓存 → DB → 回填）。
//
// 这里钉住的方向：
//  1. 缓存命中必须以缓存为准（不得再查库），miss 才回源并回填**同一行**；
//  2. 「任务不存在」是 ErrTaskNotFound 这个错误，不是空 reply；且不写负缓存
//     （否则任务刚建好也会被 5 分钟里的空结果盖掉）；
//  3. 缓存读失败/解码失败必须是错误，不能被降级成「不存在」——上游 Worker 据此重跑或
//     判定任务丢失，把「Redis 挂了」读成「没有这个任务」是危险的降级方向；
//  4. 解码失败的脏缓存不会被自愈（本用例只钉现状，见 README 已知缺口）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

func TestGetTaskGuardsTouchNothing(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 1, AssetID: 900, FpType: model.FpTypeVideo, State: model.TaskStatePending, Ctime: 1_700_000_000, Mtime: 1_700_000_001})
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))

	for _, id := range []int64{0, -1, -900} {
		reply, err := l.GetTask(&rpc.TaskReq{TaskId: id})
		wantErrIs(t, "task_id 非正", err, model.ErrInvalidTaskID)
		wantEQ(t, "task_id 非正", "不返回半截 reply", reply == nil, true)
	}
	// 即使库里就有这一行，守卫也不许碰缓存与 DB。
	wantNoCall(t, "task_id<=0 守卫", st, 0)
}

func TestGetTaskCacheMissReadsThroughAndBackfills(t *testing.T) {
	st := newStore()
	row := seedTask(t, st, &model.FingerprintTask{
		TaskID: 4101, AssetID: 9101, FpType: model.FpTypeAudio,
		VideoKey: "v-key-4101", AudioKey: "a-key-4101",
		State: model.TaskStateSucceeded, Ctime: 1_700_000_000, Mtime: 1_700_000_500,
	})
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4101})
	wantNoErr(t, "读穿回源", err)
	wantOps(t, "读穿回源", st.log.opsFrom(before), []string{
		"cache.Get:fp:task:4101",
		"task.FindOne:4101",
		"cache.Set:fp:task:4101",
	})
	wantEQ(t, "读穿回源", "逐字段投影", replyLine(reply), taskLine(row))
	wantEQ(t, "读穿回源", "task_id", reply.GetTaskId(), int64(4101))
	wantEQ(t, "读穿回源", "asset_id", reply.GetAssetId(), int64(9101))
	wantEQ(t, "读穿回源", "fp_type", reply.GetFpType(), rpc.FpType_FP_TYPE_AUDIO)
	wantEQ(t, "读穿回源", "state", reply.GetState(), rpc.TaskState_TASK_STATE_SUCCEEDED)
	wantEQ(t, "读穿回源", "video_key", reply.GetVideoKey(), "v-key-4101")
	wantEQ(t, "读穿回源", "audio_key", reply.GetAudioKey(), "a-key-4101")
	wantEQ(t, "读穿回源", "ctime", reply.GetCtime(), row.Ctime)
	wantEQ(t, "读穿回源", "mtime", reply.GetMtime(), row.Mtime)

	// 回填的必须是刚读到的那一行本身（解码比对，不是「写了就算写了」）。
	back := st.cache.storedTask(4101)
	if back == nil {
		payload, _ := st.cache.stored(4101)
		t.Fatalf("读穿回源：缓存里没有可解码的 fp:task:4101（实际值 %q）", payload)
	}
	wantEQ(t, "读穿回源", "回填内容与库存行一致", taskLine(back), taskLine(row))
	wantEQ(t, "读穿回源", "回填 ctime", back.Ctime, row.Ctime)
	wantEQ(t, "读穿回源", "回填 mtime", back.Mtime, row.Mtime)

	// 整条读侧只允许出现这两次缓存调用（读穿 + 回填）：多一次就是有人偷偷加了别的 key
	// （例如尚未接线的 fp:match:*）。
	wantEQ(t, "读穿回源", "缓存调用次数", st.log.countPrefix("cache."), 2)
}

func TestGetTaskCacheHitAnswersCacheNotDB(t *testing.T) {
	st := newStore()
	// 库里那一行和缓存里那一行**故意对不上**：若实现漏了缓存或反过来又查了一次库，下面就会露馅。
	seedTask(t, st, &model.FingerprintTask{
		TaskID: 4102, AssetID: 9102, FpType: model.FpTypeVideo,
		VideoKey: "db-only-key", State: model.TaskStatePending, Ctime: 1, Mtime: 2,
	})
	decoy := model.FingerprintTask{
		TaskID: 4102, AssetID: 9102, FpType: model.FpTypeAudio,
		VideoKey: "cache-only-key", AudioKey: "cache-only-audio",
		State: model.TaskStateFailed, Ctime: 111, Mtime: 222,
	}
	st.cache.warmTask(&decoy)
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4102})
	wantNoErr(t, "命中缓存", err)
	wantOps(t, "命中缓存", st.log.opsFrom(before), []string{"cache.Get:fp:task:4102"})
	wantEQ(t, "命中缓存", "fp_type 取自缓存", reply.GetFpType(), rpc.FpType_FP_TYPE_AUDIO)
	wantEQ(t, "命中缓存", "video_key 取自缓存", reply.GetVideoKey(), "cache-only-key")
	wantEQ(t, "命中缓存", "state 取自缓存", reply.GetState(), rpc.TaskState_TASK_STATE_FAILED)
	wantEQ(t, "命中缓存", "ctime 取自缓存", reply.GetCtime(), int64(111))
	wantEQ(t, "命中缓存", "不重复回填", st.log.countPrefix("cache.Set"), 0)
}

func TestGetTaskMissingIsNotFoundAndNotCached(t *testing.T) {
	st := newStore()
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 404})
	wantErrIs(t, "任务不存在", err, model.ErrTaskNotFound)
	wantEQ(t, "任务不存在", "不返回空 reply 冒充成功", reply == nil, true)
	wantOps(t, "任务不存在", st.log.opsFrom(before), []string{
		"cache.Get:fp:task:404",
		"task.FindOne:404",
	})
	// 负缓存会把「刚建好的任务」也盖掉 5 分钟，所以这里必须没有回填。
	if payload, ok := st.cache.stored(404); ok {
		t.Errorf("任务不存在：仍写了负缓存 %q", payload)
	}
}

func TestGetTaskCacheReadFailureFailsLoudWithoutFallback(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 4103, AssetID: 9103, FpType: model.FpTypeVideo, State: model.TaskStateSucceeded, Ctime: 1, Mtime: 2})
	rdsDown := errors.New("content-fingerprint/cache: redis is unreachable")
	st.cache.failWith("GetTask", rdsDown)
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4103})
	wantErrIs(t, "缓存读失败", err, rdsDown)
	wantEQ(t, "缓存读失败", "不返回伪成功", reply == nil, true)
	wantOps(t, "缓存读失败", st.log.ops, []string{"cache.Get:fp:task:4103"})
	// 现状：读缓存失败直接返回错误，不回源 DB（既不谎报不存在，也不静默降级）。
	wantEQ(t, "缓存读失败", "没有偷偷回源", st.log.countPrefix("task.FindOne"), 0)
}

func TestGetTaskDBFailurePropagatesWithoutBackfill(t *testing.T) {
	st := newStore()
	dbDown := errors.New("fingerprint_task FindOne: connection is dead")
	st.task.failWith("FindOne", dbDown)
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4104})
	wantErrIs(t, "回源失败", err, dbDown)
	wantEQ(t, "回源失败", "不返回伪成功", reply == nil, true)
	wantOps(t, "回源失败", st.log.ops, []string{
		"cache.Get:fp:task:4104",
		"task.FindOne:4104",
	})
	wantEQ(t, "回源失败", "不把失败写进缓存", st.log.countPrefix("cache.Set"), 0)
}

func TestGetTaskBackfillFailureStillAnswersTheTask(t *testing.T) {
	st := newStore()
	row := seedTask(t, st, &model.FingerprintTask{TaskID: 4105, AssetID: 9105, FpType: model.FpTypeAudio, State: model.TaskStateFailed, Ctime: 7, Mtime: 9})
	st.cache.failWith("SetTask", errors.New("cache: setex failed"))
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4105})
	wantNoErr(t, "回填失败只是少一次缓存", err)
	wantOps(t, "回填失败只是少一次缓存", st.log.ops, []string{
		"cache.Get:fp:task:4105",
		"task.FindOne:4105",
		"cache.Set:fp:task:4105",
	})
	wantEQ(t, "回填失败只是少一次缓存", "reply 与库存行一致", replyLine(reply), taskLine(row))
	if payload, ok := st.cache.stored(4105); ok {
		t.Errorf("回填失败只是少一次缓存：SetTask 已注入失败，缓存里不该有值，实际 %q", payload)
	}
}

// 脏缓存（JSON 不可解码）必须是错误，并且**不会被自愈**：本用例钉住现状，
// 修的时候应改成「解码失败即删除该 key 并回源」。
func TestGetTaskCorruptCacheIsLoudAndSticky(t *testing.T) {
	st := newStore()
	seedTask(t, st, &model.FingerprintTask{TaskID: 4106, AssetID: 9106, FpType: model.FpTypeVideo, State: model.TaskStateSucceeded, Ctime: 1, Mtime: 2})
	st.cache.warm(4106, `{"TaskID":4106,"State":`)
	l := NewGetTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.GetTask(&rpc.TaskReq{TaskId: 4106})
	wantErrContains(t, "脏缓存", err, "unmarshal cache")
	if errors.Is(err, model.ErrTaskNotFound) {
		t.Errorf("脏缓存：被降级成了 ErrTaskNotFound（%v）", err)
	}
	wantEQ(t, "脏缓存", "不返回伪成功", reply == nil, true)
	wantOps(t, "脏缓存", st.log.ops, []string{
		"cache.Get:fp:task:4106",
	})
	payload, ok := st.cache.stored(4106)
	wantEQ(t, "脏缓存", "脏值未被清除（5 分钟内每次读都失败）", ok, true)
	wantEQ(t, "脏缓存", "脏值内容", payload, `{"TaskID":4106,"State":`)
}

// 未知/越界的状态与类型码必须落到 UNSPECIFIED，而不是被 int32 直转成看不存在的枚举值。
func TestGetTaskProjectsEnumsWithFallbackForUnknownCodes(t *testing.T) {
	cases := []struct {
		name     string
		fpType   int32
		state    int32
		wantFP   rpc.FpType
		wantStat rpc.TaskState
	}{
		{"video+succeeded", model.FpTypeVideo, model.TaskStateSucceeded, rpc.FpType_FP_TYPE_VIDEO, rpc.TaskState_TASK_STATE_SUCCEEDED},
		{"audio+failed", model.FpTypeAudio, model.TaskStateFailed, rpc.FpType_FP_TYPE_AUDIO, rpc.TaskState_TASK_STATE_FAILED},
		{"pending", 0, model.TaskStatePending, rpc.FpType_FP_TYPE_UNSPECIFIED, rpc.TaskState_TASK_STATE_PENDING},
		{"越界码回落 UNSPECIFIED", 7, 9, rpc.FpType_FP_TYPE_UNSPECIFIED, rpc.TaskState_TASK_STATE_UNSPECIFIED},
	}
	for _, tc := range cases {
		st := newStore()
		seedTask(t, st, &model.FingerprintTask{
			TaskID: 5000, AssetID: 9200, FpType: tc.fpType, State: tc.state, Ctime: 3, Mtime: 4,
		})
		reply, err := NewGetTaskLogic(context.Background(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 5000})
		wantNoErr(t, tc.name, err)
		wantEQ(t, tc.name, "fp_type", reply.GetFpType(), tc.wantFP)
		wantEQ(t, tc.name, "state", reply.GetState(), tc.wantStat)
	}
}
