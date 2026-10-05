package logic

// updatetaskresult_test.go 覆盖 UpdateTaskResultLogic.UpdateTaskResult（Worker 回调）。
//
// 依据 AGENTS.md §8，这条链路是「发布前置」的一部分：本方法要紧的四条规则
//  1. 目标状态只能是 SUCCEEDED / FAILED，PENDING 与 UNSPECIFIED 都不行（不能自我回退/悬空）；
//     且 task_id 守卫先于状态守卫；
//  2. 只有 SUCCEEDED 才写 fingerprint_record；FAILED 的半成品 key 只留在任务行，
//     绝不能进版权比对源；
//  3. 状态推进的次序是「先推进任务 → 再落指纹事实 → 最后失效任务缓存」，
//     任何一步失败都不许留下后续副作用（不半截）；
//  4. 同一 (asset_id, fp_type) 重复上报靠 fingerprint_record 的 uniq_asset_fptype 幂等：
//     只剩一行、主键不换、key/hash 被替换。
//
// 两处「吞掉下游错误」的现状由用例钉住并标为疑似缺陷（见 README 已知缺口），
// 修的时候这两条用例会红，连同注释一起改掉。

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

// pendingTask 布一行 PENDING 任务 + 一条对应的旧缓存值（判定「有没有失效缓存」的前提）。
func pendingTask(t *testing.T, st *store, taskID, assetID int64, fpType int32) *model.FingerprintTask {
	t.Helper()
	row := seedTask(t, st, &model.FingerprintTask{
		TaskID: taskID, AssetID: assetID, FpType: fpType,
		State: model.TaskStatePending, Ctime: 1_700_000_000, Mtime: 1_700_000_000,
	})
	st.cache.warmTask(row)
	return row
}

func TestUpdateTaskResultGuardsTouchNothing(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.UpdateResultReq
		wantE error
	}{
		{"task_id=0", &rpc.UpdateResultReq{TaskId: 0, State: rpc.TaskState_TASK_STATE_SUCCEEDED}, model.ErrInvalidTaskID},
		{"task_id<0", &rpc.UpdateResultReq{TaskId: -7, State: rpc.TaskState_TASK_STATE_FAILED}, model.ErrInvalidTaskID},
		{"目标状态 UNSPECIFIED", &rpc.UpdateResultReq{TaskId: 7001, State: rpc.TaskState_TASK_STATE_UNSPECIFIED}, model.ErrInvalidState},
		{"目标状态 PENDING（不许回退）", &rpc.UpdateResultReq{TaskId: 7001, State: rpc.TaskState_TASK_STATE_PENDING}, model.ErrInvalidState},
		{"目标状态未知枚举 99", &rpc.UpdateResultReq{TaskId: 7001, State: rpc.TaskState(99)}, model.ErrInvalidState},
		{"两者都非法时先查 task_id",
			&rpc.UpdateResultReq{TaskId: 0, State: rpc.TaskState_TASK_STATE_UNSPECIFIED}, model.ErrInvalidTaskID},
	}
	for _, tc := range cases {
		st := newStore()
		pendingTask(t, st, 7001, 500, model.FpTypeVideo)
		l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

		reply, err := l.UpdateTaskResult(tc.in)
		wantErrIs(t, tc.name, err, tc.wantE)
		wantEQ(t, tc.name, "不返回半截 reply", reply == nil, true)
		wantNoCall(t, tc.name, st, 0)
		// 守卫拒绝后：任务状态没动、缓存没失效、一条指纹事实都没写。
		wantEQ(t, tc.name, "库存状态仍是 PENDING", st.task.row(1).State, model.TaskStatePending)
		wantEQ(t, tc.name, "指纹事实条数", st.rec.count(), 0)
	}
}

func TestUpdateTaskResultSucceededWritesBothFingerprints(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7001, 500, model.FpTypeVideo)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()
	from := time.Now().Unix()

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7001, State: rpc.TaskState_TASK_STATE_SUCCEEDED,
		VideoKey: "vk-500", VideoHash: "vh-500", AudioKey: "ak-500", AudioHash: "ah-500",
		Operator: "fingerprint-worker-01",
	})
	to := time.Now().Unix()
	wantNoErr(t, "回写成功", err)
	wantOps(t, "回写成功", st.log.opsFrom(before), []string{
		"task.FindOne:7001",  // 读旧行做状态机校验
		"task.Update:7001/2", // PENDING → SUCCEEDED
		"task.FindOne:7001",  // 回读最新值
		"rec.Upsert:500/1/vk-500",
		"rec.Upsert:500/2/ak-500",
		"cache.Del:fp:task:7001", // 最后失效任务缓存
	})
	wantUnixInRange(t, "回写成功", "mtime", reply.GetMtime(), from, to)
	wantEQ(t, "回写成功", "task_id", reply.GetTaskId(), int64(7001))
	wantEQ(t, "回写成功", "state", reply.GetState(), rpc.TaskState_TASK_STATE_SUCCEEDED)
	wantEQ(t, "回写成功", "video_key", reply.GetVideoKey(), "vk-500")
	wantEQ(t, "回写成功", "audio_key", reply.GetAudioKey(), "ak-500")
	wantEQ(t, "回写成功", "ctime 不被回写覆盖", reply.GetCtime(), int64(1_700_000_000))
	wantEQ(t, "回写成功", "asset_id 取自任务行", reply.GetAssetId(), int64(500))

	// 库存状态与两条指纹事实（asset_id 取自任务行，与请求里没有的 asset_id 无关）。
	wantEQ(t, "回写成功", "库存 state", st.task.row(1).State, model.TaskStateSucceeded)
	wantStringsEQ(t, "回写成功", "指纹事实", st.rec.keys(), []string{
		"id=1/asset=500/type=1/key=vk-500/hash=vh-500",
		"id=2/asset=500/type=2/key=ak-500/hash=ah-500",
	})
	if _, ok := st.cache.stored(7001); ok {
		t.Errorf("回写成功：任务缓存未失效，GetTask 会继续返回旧的 PENDING")
	}
	wantEQ(t, "回写成功", "缓存失效次数（只清 fp:task:* 这一个 key）", st.log.countPrefix("cache.Del"), 1)
}

// 只回写视频指纹时不许顺手写一条空的音频事实（record 表默认空串 key，
// 空指纹会被 Match 当成命中返回）。
func TestUpdateTaskResultSucceededWithOnlyVideoKeyWritesOneRecord(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7002, 501, model.FpTypeVideo)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7002, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk-501", VideoHash: "vh-501",
	})
	wantNoErr(t, "只回写视频指纹", err)
	wantOps(t, "只回写视频指纹", st.log.ops, []string{
		"task.FindOne:7002", "task.Update:7002/2", "task.FindOne:7002",
		"rec.Upsert:501/1/vk-501",
		"cache.Del:fp:task:7002",
	})
	wantEQ(t, "只回写视频指纹", "音频 key 为空串也如实回显", reply.GetAudioKey(), "")
	wantStringsEQ(t, "只回写视频指纹", "只有一行视频事实", st.rec.keys(),
		[]string{"id=1/asset=501/type=1/key=vk-501/hash=vh-501"})
}

// 两个 key 都为空的 SUCCEEDED：任务被判成功，但版权比对库里什么都没留下（现状钉住）。
func TestUpdateTaskResultSucceededWithNoKeyWritesNoFingerprint(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7003, 502, model.FpTypeVideo)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{TaskId: 7003, State: rpc.TaskState_TASK_STATE_SUCCEEDED})
	wantNoErr(t, "SUCCEEDED 但无指纹 key", err)
	wantOps(t, "SUCCEEDED 但无指纹 key", st.log.ops, []string{
		"task.FindOne:7003", "task.Update:7003/2", "task.FindOne:7003",
		"cache.Del:fp:task:7003",
	})
	wantEQ(t, "SUCCEEDED 但无指纹 key", "state", reply.GetState(), rpc.TaskState_TASK_STATE_SUCCEEDED)
	wantEQ(t, "SUCCEEDED 但无指纹 key", "指纹事实条数", st.rec.count(), 0)
}

// FAILED 只推进状态：半成品 key 留在任务行，绝不进 fingerprint_record。
func TestUpdateTaskResultFailedWritesNoFingerprint(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7004, 503, model.FpTypeAudio)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7004, State: rpc.TaskState_TASK_STATE_FAILED,
		VideoKey: "half-baked-vk", AudioKey: "half-baked-ak", AudioHash: "ah",
	})
	wantNoErr(t, "回写失败结论", err)
	wantOps(t, "回写失败结论", st.log.ops, []string{
		"task.FindOne:7004", "task.Update:7004/3", "task.FindOne:7004",
		"cache.Del:fp:task:7004",
	})
	wantEQ(t, "回写失败结论", "state", reply.GetState(), rpc.TaskState_TASK_STATE_FAILED)
	wantEQ(t, "回写失败结论", "库存 state", st.task.row(1).State, model.TaskStateFailed)
	wantEQ(t, "回写失败结论", "半成品 key 只留在任务行", st.task.row(1).VideoKey, "half-baked-vk")
	wantEQ(t, "回写失败结论", "指纹事实条数（失败不得污染版权比对源）", st.rec.count(), 0)
	wantCount(t, "回写失败结论", st.log, "rec.Upsert", 0)
}

// 状态机门槛（判定本身在 model 层，这里断言的是「失败后不留半截副作用」）。
func TestUpdateTaskResultRejectsNonPendingTaskAndStopsEarly(t *testing.T) {
	st := newStore()
	row := seedTask(t, st, &model.FingerprintTask{
		TaskID: 7005, AssetID: 504, FpType: model.FpTypeVideo,
		VideoKey: "already-written", State: model.TaskStateSucceeded, Ctime: 1, Mtime: 2,
	})
	st.cache.warmTask(row)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7005, State: rpc.TaskState_TASK_STATE_FAILED, VideoKey: "clobber-me",
	})
	wantErrIs(t, "重复回写已成功的任务", err, model.ErrIllegalState)
	wantEQ(t, "重复回写已成功的任务", "不返回伪成功", reply == nil, true)
	wantOps(t, "重复回写已成功的任务", st.log.opsFrom(before), []string{"task.FindOne:7005"})
	wantEQ(t, "重复回写已成功的任务", "库存状态没被改写", st.task.row(1).State, model.TaskStateSucceeded)
	wantEQ(t, "重复回写已成功的任务", "库存 video_key 没被覆盖", st.task.row(1).VideoKey, "already-written")
	wantEQ(t, "重复回写已成功的任务", "不写指纹事实", st.log.countPrefix("rec.Upsert"), 0)
	wantEQ(t, "重复回写已成功的任务", "不失效缓存（旧结论保留，等 TTL 或下次成功）", st.log.countPrefix("cache.Del"), 0)
}

func TestUpdateTaskResultUnknownTaskIsNotFound(t *testing.T) {
	st := newStore()
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{TaskId: 404, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk"})
	wantErrIs(t, "回写不存在的任务", err, model.ErrTaskNotFound)
	wantEQ(t, "回写不存在的任务", "不返回伪成功", reply == nil, true)
	wantOps(t, "回写不存在的任务", st.log.ops, []string{"task.FindOne:404"})
	wantEQ(t, "回写不存在的任务", "一行任务都没建", st.task.count(), 0)
	wantEQ(t, "回写不存在的任务", "不写指纹事实", st.rec.count(), 0)
	wantEQ(t, "回写不存在的任务", "不失效缓存", st.log.countPrefix("cache.Del"), 0)
}

func TestUpdateTaskResultUpdateFailureLeavesNoSideEffects(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7006, 505, model.FpTypeVideo)
	dbDown := errors.New("fingerprint_task UpdateResult: connection is dead")
	st.task.failWith("Update", dbDown)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7006, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk-505", AudioKey: "ak-505",
	})
	wantErrIs(t, "状态推进失败", err, dbDown)
	wantEQ(t, "状态推进失败", "不返回伪成功", reply == nil, true)
	wantOps(t, "状态推进失败", st.log.ops, []string{"task.FindOne:7006", "task.Update:7006/2"})
	wantEQ(t, "状态推进失败", "库存仍是 PENDING", st.task.row(1).State, model.TaskStatePending)
	wantEQ(t, "状态推进失败", "不写指纹事实", st.rec.count(), 0)
	wantEQ(t, "状态推进失败", "不失效缓存", st.log.countPrefix("cache.Del"), 0)
}

// 重复上报同一 (asset_id, fp_type) 的指纹：uniq_asset_fptype + ON DUPLICATE KEY UPDATE
// 保证只剩一行、主键不换、key/hash/ctime 被替换成最新值。
func TestUpdateTaskResultRepeatedReportIsIdempotentOnUniqueKey(t *testing.T) {
	st := newStore()
	old := seedRecord(t, st, &model.FingerprintRecord{AssetID: 506, FpType: model.FpTypeVideo, Key: "stale-vk", Hash: "stale-vh", Ctime: 123})
	pendingTask(t, st, 7007, 506, model.FpTypeVideo)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	_, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7007, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "fresh-vk", VideoHash: "fresh-vh",
	})
	wantNoErr(t, "重传指纹", err)
	wantStringsEQ(t, "重传指纹", "仍只有一行、主键不变、内容被替换", st.rec.keys(),
		[]string{"id=" + itoa(old.ID) + "/asset=506/type=1/key=fresh-vk/hash=fresh-vh"})
	got := st.rec.row(506, model.FpTypeVideo)
	wantEQ(t, "重传指纹", "hash 被替换", got.Hash, "fresh-vh")
	wantEQ(t, "重传指纹", "主键未换", got.ID, old.ID)
	if got.Ctime <= 123 {
		t.Errorf("重传指纹：ctime 未刷新，仍是 %d", got.Ctime)
	}
}

// 疑似缺陷 #1（只钉现状）：指纹事实写失败被 `_ = err` 吞掉，任务照样返回 SUCCEEDED，
// 于是版权比对库里没有这一条、调用方读到的是「该媒资无指纹」。
// 修法是把 Upsert 与状态推进放进同一事务，或至少把错误如实传出。
func TestUpdateTaskResultSwallowsFingerprintWriteFailure(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7008, 507, model.FpTypeVideo)
	recDown := errors.New("fingerprint_record Upsert: dead lock, try restart")
	st.rec.failWith("Upsert", recDown)
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7008, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk-507", AudioKey: "ak-507",
	})
	wantNoErr(t, "指纹写入失败被吞（现状）", err)
	wantEQ(t, "指纹写入失败被吞（现状）", "任务仍报 SUCCEEDED", reply.GetState(), rpc.TaskState_TASK_STATE_SUCCEEDED)
	wantOps(t, "指纹写入失败被吞（现状）", st.log.ops, []string{
		"task.FindOne:7008", "task.Update:7008/2", "task.FindOne:7008",
		"rec.Upsert:507/1/vk-507",
		"rec.Upsert:507/2/ak-507",
		"cache.Del:fp:task:7008",
	})
	wantEQ(t, "指纹写入失败被吞（现状）", "版权比对库里一行都没有（半截数据）", st.rec.count(), 0)
}

// 疑似缺陷 #2（只钉现状）：缓存失效失败被忽略，GetTask 最长 5 分钟仍返回旧状态。
func TestUpdateTaskResultSwallowsCacheInvalidationFailure(t *testing.T) {
	st := newStore()
	pendingTask(t, st, 7009, 508, model.FpTypeVideo)
	st.cache.failWith("DelTask", errors.New("cache: del failed"))
	l := NewUpdateTaskResultLogic(context.Background(), newTestSvc(st))

	reply, err := l.UpdateTaskResult(&rpc.UpdateResultReq{
		TaskId: 7009, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk-509",
	})
	wantNoErr(t, "缓存失效失败被吞（现状）", err)
	wantEQ(t, "缓存失效失败被吞（现状）", "回写结果本身仍然成功", reply.GetState(), rpc.TaskState_TASK_STATE_SUCCEEDED)
	wantEQ(t, "缓存失效失败被吞（现状）", "库存状态已推进", st.task.row(1).State, model.TaskStateSucceeded)

	// 同一时刻读任务：命中的还是缓存里的 PENDING。
	got, err := NewGetTaskLogic(context.Background(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: 7009})
	wantNoErr(t, "失效失败后的读侧", err)
	wantEQ(t, "失效失败后的读侧", "读到的是旧缓存", got.GetState(), rpc.TaskState_TASK_STATE_PENDING)

	// 没有自愈路径：再回写同一任务会被状态机拒（只有 PENDING 允许），
	// 而被拒的分支连 DelTask 都不执行 ⇒ 这个 key 只能等 cacheTTLTask（300s）自然过期。
	before := st.log.snapshot()
	_, err = NewUpdateTaskResultLogic(context.Background(), newTestSvc(st)).
		UpdateTaskResult(&rpc.UpdateResultReq{TaskId: 7009, State: rpc.TaskState_TASK_STATE_SUCCEEDED, VideoKey: "vk-509"})
	wantErrIs(t, "自愈尝试", err, model.ErrIllegalState)
	wantOps(t, "自愈尝试", st.log.opsFrom(before), []string{"task.FindOne:7009"})
	wantEQ(t, "自愈尝试", "全程只有一次（失败的）缓存失效", st.log.countPrefix("cache.Del"), 1)
	if _, ok := st.cache.stored(7009); !ok {
		t.Errorf("自愈尝试：fp:task:7009 竟然被清掉了，与本用例要钉的现状不符")
	}
}
