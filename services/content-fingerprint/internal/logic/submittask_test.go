package logic

// submittask_test.go 覆盖 SubmitTaskLogic.SubmitTask。
//
// 本方法要紧的三件事：
//  1. 守卫（asset_id、fp_type 枚举）必须发生在触库之前；
//  2. 任务落库是 PENDING + ctime/mtime 由 model 取 now，且 **task_id 必须回填成自增主键**
//     （Repository 用一条裸 UPDATE 完成，跨服务引用靠它，回填没发生就是脏数据）；
//  3. 同一 (asset_id, fp_type) 的重复提交由 uniq_asset_fptype 挡住并如实报错，
//     logic 不伪造「幂等成功」。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go-video/services/content-fingerprint/model"
	"go-video/services/content-fingerprint/rpc"
)

func TestSubmitTaskGuardsTouchNothing(t *testing.T) {
	cases := []struct {
		name  string
		in    *rpc.SubmitTaskReq
		wantE error
	}{
		{"asset_id=0", &rpc.SubmitTaskReq{AssetId: 0, FpType: rpc.FpType_FP_TYPE_VIDEO}, model.ErrInvalidAssetID},
		{"asset_id<0", &rpc.SubmitTaskReq{AssetId: -1, FpType: rpc.FpType_FP_TYPE_VIDEO}, model.ErrInvalidAssetID},
		{"fp_type UNSPECIFIED", &rpc.SubmitTaskReq{AssetId: 8801, FpType: rpc.FpType_FP_TYPE_UNSPECIFIED}, model.ErrInvalidFpType},
		{"fp_type 未知枚举 99", &rpc.SubmitTaskReq{AssetId: 8801, FpType: rpc.FpType(99)}, model.ErrInvalidFpType},
		{"两者都非法时先查 asset_id", &rpc.SubmitTaskReq{AssetId: 0, FpType: rpc.FpType_FP_TYPE_UNSPECIFIED}, model.ErrInvalidAssetID},
	}
	for _, tc := range cases {
		st := newStore()
		l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))
		reply, err := l.SubmitTask(tc.in)
		wantErrIs(t, tc.name, err, tc.wantE)
		wantEQ(t, tc.name, "拒绝时不返回半截 reply", reply == nil, true)
		wantNoCall(t, tc.name, st, 0)
	}
}

func TestSubmitTaskPersistsPendingTaskAndAlignsTaskID(t *testing.T) {
	st := newStore()
	l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()
	from := time.Now().Unix()

	reply, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8801, FpType: rpc.FpType_FP_TYPE_VIDEO})
	to := time.Now().Unix()
	wantNoErr(t, "提交视频指纹任务", err)
	wantEQ(t, "提交视频指纹任务", "task_id", reply.GetTaskId(), int64(1))
	wantEQ(t, "提交视频指纹任务", "asset_id", reply.GetAssetId(), int64(8801))
	wantEQ(t, "提交视频指纹任务", "fp_type", reply.GetFpType(), rpc.FpType_FP_TYPE_VIDEO)
	wantEQ(t, "提交视频指纹任务", "state 必须是 PENDING（本期不算指纹）", reply.GetState(), rpc.TaskState_TASK_STATE_PENDING)
	wantEQ(t, "提交视频指纹任务", "video_key 由 Worker 回写，建任务时为空", reply.GetVideoKey(), "")
	wantEQ(t, "提交视频指纹任务", "audio_key 由 Worker 回写，建任务时为空", reply.GetAudioKey(), "")
	wantUnixInRange(t, "提交视频指纹任务", "ctime", reply.GetCtime(), from, to)
	wantUnixInRange(t, "提交视频指纹任务", "mtime", reply.GetMtime(), from, to)

	// 顺序：先 INSERT 拿到自增主键，再执行 task_id 回填 SQL；反过来 task_id 就永远停在 0。
	wantOps(t, "提交视频指纹任务", st.log.opsFrom(before), []string{
		"task.Insert:8801/1",
		"conn.BackfillTaskID:1",
	})

	// 库存行才是事实：响应的 task_id 来自内存结构，只有库里那一行被回填了才算真的可引用。
	row := st.task.row(1)
	if row == nil {
		t.Fatalf("提交视频指纹任务：库里没有 id=1 的任务行")
	}
	wantEQ(t, "提交视频指纹任务", "库存 task_id 与主键对齐", row.TaskID, row.ID)
	wantEQ(t, "提交视频指纹任务", "库存 asset_id", row.AssetID, int64(8801))
	wantEQ(t, "提交视频指纹任务", "库存 fp_type", row.FpType, model.FpTypeVideo)
	wantEQ(t, "提交视频指纹任务", "库存 state", row.State, model.TaskStatePending)
	wantEQ(t, "提交视频指纹任务", "库存 video_key", row.VideoKey, "")
	wantEQ(t, "提交视频指纹任务", "库存指纹事实条数", st.rec.count(), 0)
	wantEQ(t, "提交视频指纹任务", "建任务不写任何缓存", st.log.countPrefix("cache."), 0)
	wantEQ(t, "提交视频指纹任务", "建任务不查指纹事实表", st.log.countPrefix("rec."), 0)
}

// 提交完立刻能按返回的 task_id 读回同一行：uniq_task_id 回填链路的端到端不变量。
func TestSubmitTaskThenGetTaskReadsBackTheSameRow(t *testing.T) {
	st := newStore()
	submitted, err := NewSubmitTaskLogic(context.Background(), newTestSvc(st)).
		SubmitTask(&rpc.SubmitTaskReq{AssetId: 8802, FpType: rpc.FpType_FP_TYPE_AUDIO})
	wantNoErr(t, "提交音频指纹任务", err)

	before := st.log.snapshot()
	got, err := NewGetTaskLogic(context.Background(), newTestSvc(st)).GetTask(&rpc.TaskReq{TaskId: submitted.GetTaskId()})
	wantNoErr(t, "回读刚提交的任务", err)
	wantOps(t, "回读刚提交的任务", st.log.opsFrom(before), []string{
		"cache.Get:fp:task:1",
		"task.FindOne:1",
		"cache.Set:fp:task:1",
	})
	wantEQ(t, "回读刚提交的任务", "fp_type", got.GetFpType(), rpc.FpType_FP_TYPE_AUDIO)
	wantEQ(t, "回读刚提交的任务", "asset_id", got.GetAssetId(), submitted.GetAssetId())
	wantEQ(t, "回读刚提交的任务", "state", got.GetState(), rpc.TaskState_TASK_STATE_PENDING)
}

// 同一媒资的视频与音频是两条任务：uniq_asset_fptype 只按 (asset_id, fp_type) 去重。
func TestSubmitTaskVideoAndAudioAreSeparateTasks(t *testing.T) {
	st := newStore()
	l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))

	v, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8803, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "视频任务", err)
	a, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8803, FpType: rpc.FpType_FP_TYPE_AUDIO})
	wantNoErr(t, "音频任务", err)
	wantEQ(t, "两条任务的 task_id 必须不同", "task_id", v.GetTaskId() != a.GetTaskId(), true)
	wantStringsEQ(t, "同媒资双类型", "任务行", []string{
		taskLine(st.task.row(v.GetTaskId())), taskLine(st.task.row(a.GetTaskId()))}, []string{
		"task=1/asset=8803/type=1/vkey=/akey=/state=1",
		"task=2/asset=8803/type=2/vkey=/akey=/state=1"})
	wantOps(t, "同媒资双类型", st.log.ops, []string{
		"task.Insert:8803/1", "conn.BackfillTaskID:1",
		"task.Insert:8803/2", "conn.BackfillTaskID:2",
	})
}

// 重复上报同一 (asset_id, fp_type)：uniq_asset_fptype 挡住，第二次不得静默成功。
func TestSubmitTaskDuplicateAssetFpTypeIsRejectedByUniqueKey(t *testing.T) {
	st := newStore()
	l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))
	first, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8804, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "首次提交", err)

	before := st.log.snapshot()
	second, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8804, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantErrIs(t, "重复提交", err, errDuplicateKey)
	if !strings.Contains(err.Error(), "uniq_asset_fptype") {
		t.Errorf("重复提交：撞的不是 uniq_asset_fptype，而是 %v", err)
	}
	wantEQ(t, "重复提交", "拒绝时不返回半截 reply", second == nil, true)
	wantOps(t, "重复提交", st.log.opsFrom(before), []string{"task.Insert:8804/1"})
	wantEQ(t, "重复提交", "库里仍只有首次那一行", st.task.count(), 1)
	wantEQ(t, "重复提交", "首次任务的 task_id 未被改写", st.task.row(first.GetTaskId()).TaskID, int64(1))
}

// Insert 失败必须如实传出，且不留 task_id 回填、不写指纹事实、不碰缓存。
func TestSubmitTaskInsertFailurePropagates(t *testing.T) {
	st := newStore()
	dbDown := errors.New("fingerprint_task: connection is dead")
	st.task.failWith("Insert", dbDown)
	l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8805, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantErrIs(t, "建任务失败", err, dbDown)
	wantEQ(t, "建任务失败", "不返回伪成功", reply == nil, true)
	wantOps(t, "建任务失败", st.log.ops, []string{"task.Insert:8805/1"})
	wantEQ(t, "建任务失败", "一行都没落", st.task.count(), 0)
	wantEQ(t, "建任务失败", "不执行 task_id 回填", st.log.countPrefix("conn."), 0)
	wantEQ(t, "建任务失败", "不写指纹事实", st.log.countPrefix("rec."), 0)
	wantEQ(t, "建任务失败", "不写缓存", st.log.countPrefix("cache."), 0)
}

// 疑似缺陷（本用例只钉现状，不代表设计正确）：Repository 用 `_, _ =` 丢弃了
// task_id 回填 UPDATE 的错误，所以这条链路失败时 SubmitTask 仍然返回成功，
// 而库存行的 task_id 停在 0；紧接着任何一次提交都会撞 uniq_task_id（0 槽被占）。
// 若将来把该错误如实传出，本用例会红——那时连同本注释一起改掉。
func TestSubmitTaskIgnoresTaskIDBackfillFailure(t *testing.T) {
	st := newStore()
	dead := errors.New("executor: dead lock detected")
	st.conn.failWith("BackfillTaskID", dead)
	l := NewSubmitTaskLogic(context.Background(), newTestSvc(st))

	reply, err := l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8806, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantNoErr(t, "回填失败被吞掉（现状）", err)
	wantEQ(t, "回填失败被吞掉（现状）", "响应仍报 task_id=1", reply.GetTaskId(), int64(1))
	wantEQ(t, "回填失败被吞掉（现状）", "库存行 task_id 仍是 0", st.task.row(1).TaskID, int64(0))

	// 真实伤害：下一笔无关媒资的提交会撞 uniq_task_id，而不是撞自己的 asset 唯一键。
	_, err = l.SubmitTask(&rpc.SubmitTaskReq{AssetId: 8807, FpType: rpc.FpType_FP_TYPE_VIDEO})
	wantErrIs(t, "下一笔提交", err, errDuplicateKey)
	if !strings.Contains(err.Error(), "uniq_task_id") {
		t.Errorf("下一笔提交：撞的不是 uniq_task_id，而是 %v", err)
	}
	wantOps(t, "回填失败链路", st.log.ops, []string{
		"task.Insert:8806/1", "conn.BackfillTaskID:1",
		"task.Insert:8807/1",
	})
	wantEQ(t, "回填失败链路", "第二笔没落库", st.task.count(), 1)
}
