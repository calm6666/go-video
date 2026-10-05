package logic

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/internal/svc"
	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 回收四个方法：SubmitRetentionTask / ReportRetentionResult / ListRetentionTasks / GetRetentionTask。
//
// 回收是全服务唯一「授权删对象存储」的入口，所以用例主轴不是状态机好不好看，而是三条防线：
//
//	SubmitRetentionTask 一个对象都不删，它只登记「谁、为什么、可以删哪一批」（proto line 641-652）：
//	  审计字段（reason/operator/request_id）缺失或超列宽一律拒绝而不是截断保存 —— 截断后的归因是另一个人；
//	  定点回收（target_id>0）必须回读对象、校验房间归属、并确认当前状态允许回收，
//	  否则就是给 Worker 排一个空转任务，或更糟：排一个「删掉正在分发的流」的任务；
//	  批量任务（target_id=0 + expire_before>0）没有可回读的单一对象，准入由 Worker 逐行判定；
//	  三层幂等（request_id 预读 / 同对象未完成复用 / 撞唯一键回读）都必须「什么都不写」。
//	ReportRetentionResult 的计数是证据不是增量（覆盖写，见 RetentionPatch 注释）：
//	  三条自洽判据（非负且 deleted+skipped<=scanned、purge=false 时 deleted 必须为 0、
//	  从 RUNNING 进终态时计数不得回退）任一不满足就整条拒收，绝不替它改数；
//	  必须先认领（RUNNING）再报成功；终态只允许同值重放。
//	List/Get 只读：每个用例都以 wantNoWrites + wantEvents(nil) 收尾。
//
// 事件词表只有一条 livemedia.retention.finished：所以 Submit 侧每个用例都断言 wantEvents(nil)
// （不伪造「回收已登记」事件），而「业务写与 Outbox 同事务、失败一起回滚」只能落在上报侧。
//
// 本轮写用例时修掉两个缺陷（都在被测代码里留了注释）：
//   - 缺陷 #6：SubmitRetentionTask 对批量任务也调 locateTarget，FindOne(0) 必然查无此行，
//     于是「按 expire_before 超期清理」这条 proto 明写的用法永远登记不出来（只能定点回收）。
//     护栏是 TestSubmitRetentionTaskRegistersBatchSweepWithoutReadingObjects 里那句
//     「一条定位读都不许发生」。
//   - 缺陷 #7：ReportRetentionResult 在成功边把 patch.Errno/ErrMsg 置成 nil，
//     而 Patch 的 nil 语义是「本次不更新这一列」，结果上一次 RUNNING 的失败痕迹留在了
//     一条 SUCCEEDED 行上 —— 与同一处注释承诺的「成功边不保留失败痕迹」正好相反。
//     护栏是 TestReportRetentionResultClaimThenSucceededClearsTrace 后半段的三列清零。
//
// 已知缺口（未修，见 services/live-media/README §8）：operator 走 sanitizeOperator，
// 只做「非空 + 按列宽拒绝」，不脱敏 —— 调用方把签名地址写进 operator 会原样入库。
// 本文件的 reason 脱敏用例（TestSubmitRetentionTaskRedactsCredentialsInReason）只覆盖 reason 列。

// ---------------------------------------------------------------- 请求构造与小工具

const (
	retentionWindow   = 86400 * 30 // 「超期 30 天」的扫描面
	retentionText     = "超期清理"
	retentionOps      = "ops-retention"
	retentionWorker   = "worker-purge"
	retentionMissing  = int64(4242) // 一律用于「主键不存在」
	retentionOtherObj = int64(94001)
)

// submitRetentionReq 一份**批量**回收登记请求（target_id=0 + expire_before>0）。
func submitRetentionReq(kind rpc.RetentionTargetKind, requestID string) *rpc.SubmitRetentionTaskReq {
	return &rpc.SubmitRetentionTaskReq{
		TargetKind: kind, ExpireBefore: nowTS() - retentionWindow,
		Purge: true, BatchLimit: 0, Reason: retentionText, Operator: retentionOps,
		RequestId: requestID, TraceId: "trace-retention-submit",
	}
}

// pinnedRetentionReq 一份**定点**回收登记请求：房间声明成本房间，跨房间用例在此基础上改。
func pinnedRetentionReq(kind rpc.RetentionTargetKind, targetID int64, requestID string) *rpc.SubmitRetentionTaskReq {
	in := submitRetentionReq(kind, requestID)
	in.RoomId, in.TargetId, in.ExpireBefore = testRoomID, targetID, 0
	return in
}

func reportRetentionReq(retentionID, expectedVersion int64, state rpc.RetentionState) *rpc.ReportRetentionResultReq {
	return &rpc.ReportRetentionResultReq{
		RetentionId: retentionID, ExpectedVersion: expectedVersion, State: state,
		WorkerId: retentionWorker, TraceId: "trace-retention-report",
	}
}

func getRetentionReq(retentionID int64) *rpc.RetentionTaskReq {
	return &rpc.RetentionTaskReq{RetentionId: retentionID}
}

func listRetentionsReq(pn, ps int32) *rpc.ListRetentionTasksReq {
	return &rpc.ListRetentionTasksReq{Page: &rpc.PageParam{Pn: pn, Ps: ps}}
}

func submitRetention(ctx context.Context, svcCtx *svc.ServiceContext, in *rpc.SubmitRetentionTaskReq) (*rpc.LiveRetentionTaskInfo, error) {
	return NewSubmitRetentionTaskLogic(ctx, svcCtx).SubmitRetentionTask(in)
}

func reportRetention(ctx context.Context, svcCtx *svc.ServiceContext, in *rpc.ReportRetentionResultReq) (*rpc.LiveRetentionTaskInfo, error) {
	return NewReportRetentionResultLogic(ctx, svcCtx).ReportRetentionResult(in)
}

func mustRetention(t *testing.T, db *store, retentionID int64) *model.LiveRetentionTask {
	t.Helper()
	row, ok := db.retentions[retentionID]
	if !ok {
		t.Fatalf("回收任务 %d 不存在（库内 %d 行）", retentionID, len(db.retentions))
	}
	return row
}

// onlyRetention 库内恰好一行的回收任务：登记动作「不多不少一行」的判据。
func onlyRetention(t *testing.T, db *store, label string) *model.LiveRetentionTask {
	t.Helper()
	if len(db.retentions) != 1 {
		ids := make([]int64, 0, len(db.retentions))
		for id := range db.retentions {
			ids = append(ids, id)
		}
		t.Fatalf("%s：库内应恰好 1 行回收任务，实际 %d 行 %v", label, len(db.retentions), ids)
	}
	for _, row := range db.retentions {
		return row
	}
	return nil
}

func assertRetentionUnchanged(t *testing.T, db *store, retentionID int64, want model.LiveRetentionTask, label string) {
	t.Helper()
	got := mustRetention(t, db, retentionID)
	if *got != want {
		t.Fatalf("%s：非法路径改动了回收任务行：\n got=%+v\nwant=%+v", label, *got, want)
	}
}

// stripRetentionDrive 抹掉「状态推进本身要写的列」：state/version/mtime/trace_id。
func stripRetentionDrive(r model.LiveRetentionTask) model.LiveRetentionTask {
	r.State, r.Version, r.Mtime, r.TraceId = 0, 0, 0, ""
	return r
}

// stripRetentionEvidence 再抹掉「本次回执有权覆盖写的证据列」：三个计数 + 归因三列。
// 与 stripRetentionDrive 组合即「除状态机与回执证据外，一列都不许动」的判据。
func stripRetentionEvidence(r model.LiveRetentionTask) model.LiveRetentionTask {
	r.Scanned, r.Deleted, r.Skipped = 0, 0, 0
	r.FailReason, r.Errno, r.ErrMsg = 0, 0, ""
	return r
}

// targetLookups 定点回收的三类准入读 + 父回放任务读。
// 批量任务（target_id=0）必须一次都不发生 —— 这是缺陷 #6 的回归护栏。
var targetLookups = []string{
	"Segments.FindOne", "ReplayRefs.FindOne", "ReplayTasks.FindOne", "StreamOutputs.FindOne",
}

func wantNoTargetLookups(t *testing.T, db *store, label string) {
	t.Helper()
	for _, op := range targetLookups {
		wantCalls(t, db, op, 0, 0, label+"：不得回读单个对象")
	}
	wantCalls(t, db, "RetentionTasks.FindUnfinishedByTarget", 0, 0, label+"：批量任务不参与同对象去重")
}

// assertNoRetentionReads 钉「入参门禁发生在任何读之前」：连一次读都不许发生。
func assertNoRetentionReads(t *testing.T, db *store, label string) {
	t.Helper()
	wantNoTargetLookups(t, db, label)
	for _, op := range []string{"RetentionTasks.FindOne", "RetentionTasks.FindByRequestID",
		"RetentionTasks.List", "RetentionTasks.ListByState"} {
		wantCalls(t, db, op, 0, 0, label)
	}
}

func retentionOrder(rows []*rpc.LiveRetentionTaskInfo) string {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetRetentionId())
	}
	return joinInt64(ids)
}

func findRetentionRow(rows []*rpc.LiveRetentionTaskInfo, retentionID int64) (*rpc.LiveRetentionTaskInfo, bool) {
	for _, row := range rows {
		if row.GetRetentionId() == retentionID {
			return row, true
		}
	}
	return nil, false
}

func wantRetentionEchoesRow(t *testing.T, label string, info *rpc.LiveRetentionTaskInfo, row *model.LiveRetentionTask) {
	t.Helper()
	wantField(t, label, "retention_id", info.GetRetentionId(), row.RetentionId)
	wantField(t, label, "target_kind", int32(info.GetTargetKind()), row.TargetKind)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "target_id", info.GetTargetId(), row.TargetId)
	wantField(t, label, "expire_before", info.GetExpireBefore(), row.ExpireBefore)
	wantField(t, label, "purge", info.GetPurge(), row.Purge != 0)
	wantField(t, label, "batch_limit", info.GetBatchLimit(), row.BatchLimit)
	wantField(t, label, "state", int32(info.GetState()), row.State)
	wantField(t, label, "scanned", info.GetScanned(), row.Scanned)
	wantField(t, label, "deleted", info.GetDeleted(), row.Deleted)
	wantField(t, label, "skipped", info.GetSkipped(), row.Skipped)
	wantField(t, label, "reason", info.GetReason(), row.Reason)
	wantField(t, label, "operator", info.GetOperator(), row.Operator)
	wantField(t, label, "version", info.GetVersion(), row.Version)
	wantField(t, label, "fail_reason", int32(info.GetFailReason()), row.FailReason)
	wantField(t, label, "errno", info.GetErrno(), row.Errno)
	wantField(t, label, "err_msg", info.GetErrMsg(), row.ErrMsg)
	wantField(t, label, "request_id", info.GetRequestId(), row.RequestId)
	wantField(t, label, "trace_id", info.GetTraceId(), row.TraceId)
	wantField(t, label, "ctime", info.GetCtime(), row.Ctime)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
}

// verifiedSegment 一行可被定点回收的切片（录制任务只是它的归属外键，不参与判定）。
func verifiedSegment(db *store, mutate func(*model.LiveRecordSegment)) *model.LiveRecordSegment {
	rec := seedRecord(db, model.RecordStateStopped, nil)
	return seedSegment(db, rec.RecordId, 1, model.SegmentStateVerified, mutate)
}

// reclaimableReplayRef 一行「父任务已终态、生命周期正常」的回放引用：
// 定点回收回放产物的准入起点（SubmitRetentionTask 要么看到投影已标 Pending，
// 要么要求父任务已进终态，否则这份产物还在产出中，删它就是删半成品）。
func reclaimableReplayRef(db *store, mutate func(*model.LiveReplayAssetRef)) *model.LiveReplayAssetRef {
	task := seedReplay(db, model.ReplayStateCompleted, nil)
	return refForTask(db, task, mutate)
}

// stripRefRetention 在 stripRefProjection 之上再抹掉生命周期列：它是回收登记唯一有权推进的列，
// 两者组合即「除引用生命周期外，一列都不许动」的判据。
func stripRefRetention(r model.LiveReplayAssetRef) model.LiveReplayAssetRef {
	r.RetentionState = 0
	return r
}

// retentionForRef 一行指向该引用的回收任务（seedRetention 默认 purge=true、批量意图由 mutate 覆盖）。
func retentionForRef(db *store, state int32, ref *model.LiveReplayAssetRef,
	mutate func(*model.LiveRetentionTask)) *model.LiveRetentionTask {
	return seedRetention(db, state, func(r *model.LiveRetentionTask) {
		r.TargetKind, r.TargetId, r.RoomId, r.ExpireBefore = model.RetentionTargetReplay, ref.Id, ref.RoomId, 0
		if mutate != nil {
			mutate(r)
		}
	})
}

// ---------------------------------------------------------------- SubmitRetentionTask：入参门禁

// 未知 target_kind 必须在任何读之前拒绝：退化成「按 0 处理」等于把
// 「回收全世界」的意图交给一个不存在的对象类型去执行。
func TestSubmitRetentionTaskRejectsUnknownTargetKindBeforeAnyRead(t *testing.T) {
	kinds := []struct {
		name string
		kind rpc.RetentionTargetKind
	}{
		{"UNSPECIFIED", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_UNSPECIFIED},
		{"区间外 4", rpc.RetentionTargetKind(4)},
		{"负数", rpc.RetentionTargetKind(-1)},
	}
	for _, tc := range kinds {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			before := snapshotWrites(db)
			in := submitRetentionReq(tc.kind, "req-kind")
			in.TargetId = retentionMissing // 让「对象不存在」不能成为拒绝理由：只验枚举门禁

			info, err := submitRetention(context.Background(), svcCtx, in)
			wantFail(t, info, err, model.ErrInvalidTransition, "未知 target_kind")
			assertNoRetentionReads(t, db, "未知 target_kind")
			wantNoWrites(t, db, before, "未知 target_kind")
			wantEvents(t, db, nil, "未知 target_kind")
		})
	}
}

// target_id 与 expire_before 至少一个，且都不接受负数：
// 「-1 表示全部」这种隐式约定一旦被接受，就没有任何东西能挡住一次全库删除。
func TestSubmitRetentionTaskNeedsTargetIDOrExpireBefore(t *testing.T) {
	cases := []struct {
		name                           string
		targetID, expireBefore, roomID int64
	}{
		{"两者都是 0（回收全世界）", 0, 0, 0},
		{"两者都是 0 但声明了房间", 0, 0, testRoomID},
		{"target_id 为负", -1, nowTS(), 0},
		{"expire_before 为负", 0, -1, 0},
		{"room_id 为负（不接受 -1 表示全部）", 1, nowTS(), -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			before := snapshotWrites(db)
			in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, "req-scope")
			in.TargetId, in.ExpireBefore, in.RoomId = tc.targetID, tc.expireBefore, tc.roomID

			info, err := submitRetention(context.Background(), svcCtx, in)
			wantFail(t, info, err, model.ErrRetentionTargetRequired, tc.name)
			assertNoRetentionReads(t, db, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// 审计三件套（reason/operator/request_id）缺失或超列宽一律拒绝，且拒绝发生在读库之前：
// 超长不截断（截断后的归因文本可能属于另一次回收），空白等同于没填。
func TestSubmitRetentionTaskRequiresAuditAndIdempotencyFields(t *testing.T) {
	overlongReason := strings.Repeat("清", maxReasonRunes+1)
	overlongOperator := strings.Repeat("作", maxOperatorRunes+1)
	overlongRequest := strings.Repeat("r", maxRequestIDRunes+1)
	cases := []struct {
		name   string
		mutate func(*rpc.SubmitRetentionTaskReq)
		want   string
	}{
		{"reason 缺失", func(in *rpc.SubmitRetentionTaskReq) { in.Reason = "" }, model.ErrRetentionReasonRequired.Error()},
		{"reason 只有空白", func(in *rpc.SubmitRetentionTaskReq) { in.Reason = "   \t " }, model.ErrRetentionReasonRequired.Error()},
		{"reason 超长（拒绝而非截断）", func(in *rpc.SubmitRetentionTaskReq) { in.Reason = overlongReason }, "too long"},
		{"operator 缺失", func(in *rpc.SubmitRetentionTaskReq) { in.Operator = "" }, model.ErrOperatorRequired.Error()},
		{"operator 只有空白", func(in *rpc.SubmitRetentionTaskReq) { in.Operator = "  " }, model.ErrOperatorRequired.Error()},
		{"operator 超长（拒绝而非截断）", func(in *rpc.SubmitRetentionTaskReq) { in.Operator = overlongOperator }, "operator too long"},
		{"request_id 缺失", func(in *rpc.SubmitRetentionTaskReq) { in.RequestId = "" }, model.ErrEmptyRequestID.Error()},
		{"request_id 只有空白", func(in *rpc.SubmitRetentionTaskReq) { in.RequestId = " " }, model.ErrEmptyRequestID.Error()},
		{"request_id 超长", func(in *rpc.SubmitRetentionTaskReq) { in.RequestId = overlongRequest }, model.ErrEmptyRequestID.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			before := snapshotWrites(db)
			in := pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, retentionMissing, "req-audit")
			tc.mutate(in)

			info, err := submitRetention(context.Background(), svcCtx, in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s：应报 %q，实际 err=%v", tc.name, tc.want, err)
			}
			if !isNilPtr(info) {
				t.Fatalf("%s：失败路径不得带回响应体：%+v", tc.name, info)
			}
			assertNoRetentionReads(t, db, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// reason 是自由文本，必须先脱敏再落库；request_id 的归一值必须与落库值一致，
// 否则带空白的幂等键会「校验通过但回读不到」，重投就排成两个删除任务。
func TestSubmitRetentionTaskRedactsCredentialsInReason(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, "  req-reason-redact  ")
	in.Reason = "清理仍带签名的地址 " + leakSignedURL

	info, err := submitRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "reason 脱敏")

	row := onlyRetention(t, db, "reason 脱敏")
	wantRetentionEchoesRow(t, "reason 脱敏", info, row)
	wantField(t, "reason 脱敏", "request_id 归一后落库", row.RequestId, "req-reason-redact")
	if strings.Contains(row.Reason, leakMarker) {
		t.Fatalf("签名地址明文入库：%q", row.Reason)
	}
	if !strings.Contains(row.Reason, "清理仍带签名的地址") {
		t.Fatalf("脱敏不得把整段审计文本丢掉：%q", row.Reason)
	}
	wantEvents(t, db, nil, "reason 脱敏")
	wantNoLeak(t, db, "reason 脱敏")
}

// ---------------------------------------------------------------- SubmitRetentionTask：登记与批量扫描

// 批量扫描登记（缺陷 #6 的回归护栏）：三类对象都要能只凭 expire_before 排队，
// 且登记动作一条定位读都不发生 —— 否则超期清理（cron 唯一的用法）永远登记不出来。
func TestSubmitRetentionTaskRegistersBatchSweepWithoutReadingObjects(t *testing.T) {
	kinds := []struct {
		name string
		kind rpc.RetentionTargetKind
		want int32
	}{
		{"切片", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, model.RetentionTargetSegment},
		{"回放产物", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, model.RetentionTargetReplay},
		{"残留档位", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT, model.RetentionTargetStreamOutput},
	}
	for i, tc := range kinds {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			expire := nowTS() - retentionWindow
			before := snapshotWrites(db)

			in := submitRetentionReq(tc.kind, "req-batch-"+strconv.Itoa(i))
			in.ExpireBefore = expire
			info, err := submitRetention(context.Background(), svcCtx, in)
			info = wantOK(t, info, err, tc.name+"批量登记")

			row := onlyRetention(t, db, tc.name+"批量登记")
			wantRetentionEchoesRow(t, tc.name+"批量登记", info, row)
			wantNoTargetLookups(t, db, tc.name+"批量登记")
			wantField(t, tc.name+"批量登记", "target_kind", row.TargetKind, tc.want)
			wantField(t, tc.name+"批量登记", "target_id 保持 0（集合语义）", row.TargetId, int64(0))
			wantField(t, tc.name+"批量登记", "expire_before 原样落库", row.ExpireBefore, expire)
			wantField(t, tc.name+"批量登记", "room_id=0 即全局扫描面", row.RoomId, int64(0))
			wantField(t, tc.name+"批量登记", "登记态恒为 PENDING", row.State, model.RetentionStatePending)
			wantField(t, tc.name+"批量登记", "version 从 1 起", row.Version, int64(1))
			// 本方法一个对象都不删：计数恒为 0，真删由 Worker 回报（覆盖写）。
			wantField(t, tc.name+"批量登记", "scanned", row.Scanned, int32(0))
			wantField(t, tc.name+"批量登记", "deleted", row.Deleted, int32(0))
			wantField(t, tc.name+"批量登记", "skipped", row.Skipped, int32(0))
			// 主键必须来自 InsertTx 的返回值并回读成功（与缺陷 #2 同一口径）。
			wantField(t, tc.name+"批量登记", "回复带回了主键", info.GetRetentionId(), row.RetentionId)
			wantCalls(t, db, "DB.TransactCtx", 0, 1, tc.name+"批量登记")
			wantCalls(t, db, "RetentionTasks.InsertTx", 0, 1, tc.name+"批量登记")
			wantCalls(t, db, "RetentionTasks.Insert", 0, 0, tc.name+"批量登记：不得走事务外写入")
			wantField(t, tc.name+"批量登记", "副作用集合恰好是允许的那两个",
				firedWrites(db, before), "DB.TransactCtx,RetentionTasks.InsertTx")
			// 词表里没有「回收已登记」事件：不伪造 event_type，登记侧一条都不发。
			wantEvents(t, db, nil, tc.name+"批量登记")
			wantNoLeak(t, db, tc.name+"批量登记")
		})
	}
}

// 批量任务的 room_id 是调用方声明的扫描面，不会被任何对象归属改写：
// 0=全局，>0=只扫这个房间。
func TestSubmitRetentionTaskBatchRoomIsDeclaredScanScope(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	// 库里放一个别的房间的对象：批量登记的房间归属只能来自请求，不能来自它。
	seg := verifiedSegment(db, func(s *model.LiveRecordSegment) { s.RoomId = testRoomOther })

	in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, "req-batch-room")
	in.RoomId = testRoomID
	info, err := submitRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "批量登记声明房间")

	row := onlyRetention(t, db, "批量登记声明房间")
	wantRetentionEchoesRow(t, "批量登记声明房间", info, row)
	wantNoTargetLookups(t, db, "批量登记声明房间")
	wantField(t, "批量登记声明房间", "room_id 取请求声明值", row.RoomId, testRoomID)
	wantField(t, "批量登记声明房间", "没有被别的房间的对象改写", row.RoomId != seg.RoomId, true)
	wantEvents(t, db, nil, "批量登记声明房间")
}

// purge 与 batch_limit 的归一：批量上限是 Worker 每批删除的限流值，
// 越界夹取而不报错（拒绝只会让 cron 重投），但「未提供」必须落到配置默认而不是 0。
func TestSubmitRetentionTaskNormalizesPurgeAndClampsBatchLimit(t *testing.T) {
	cfg := testConf()
	cases := []struct {
		name      string
		requested int32
		want      int32
		purge     bool
	}{
		{"未提供 → 取默认", 0, cfg.DefaultRetentionBatchLimit, false},
		{"负数 → 取默认", -7, cfg.DefaultRetentionBatchLimit, true},
		{"1 → 原样", 1, 1, false},
		{"50 → 原样", 50, 50, true},
		{"恰好上限 → 原样", cfg.MaxRetentionBatchLimit, cfg.MaxRetentionBatchLimit, true},
		{"刚超上限 → 夹取", cfg.MaxRetentionBatchLimit + 1, cfg.MaxRetentionBatchLimit, true},
		{"远超上限 → 夹取", 99999, cfg.MaxRetentionBatchLimit, false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, "req-limit-"+strconv.Itoa(i))
			in.BatchLimit, in.Purge = tc.requested, tc.purge

			info, err := submitRetention(context.Background(), svcCtx, in)
			info = wantOK(t, info, err, tc.name)
			row := onlyRetention(t, db, tc.name)
			wantRetentionEchoesRow(t, tc.name, info, row)
			wantField(t, tc.name, "batch_limit", row.BatchLimit, tc.want)
			wantField(t, tc.name, "purge 折成 0/1", row.Purge, boolInt32(tc.purge))
			wantField(t, tc.name, "reason", row.Reason, retentionText)
			wantField(t, tc.name, "operator", row.Operator, retentionOps)
			wantField(t, tc.name, "trace_id", row.TraceId, "trace-retention-submit")
			wantEvents(t, db, nil, tc.name)
			wantNoLeak(t, db, tc.name)
		})
	}
	// 前置数据自校验：默认值必须严格小于上限，否则上面的夹取表测的就不是同一条判据。
	if cfg.DefaultRetentionBatchLimit <= 1 || cfg.DefaultRetentionBatchLimit >= cfg.MaxRetentionBatchLimit {
		t.Fatalf("testConf 与 etc/live-media.yaml 口径漂移：default=%d max=%d",
			cfg.DefaultRetentionBatchLimit, cfg.MaxRetentionBatchLimit)
	}
}

// ---------------------------------------------------------------- SubmitRetentionTask：幂等三层

// 第一层：request_id 命中既有任务 → 原样返回那一行。
// 这里故意把既有行造成「与本次请求完全不同的意图」（不同类型、不同计数、purge=0），
// 幂等回放的回复必须是既成事实那一行，而不是本次入参。
func TestSubmitRetentionTaskSameRequestIdReplaysExistingTask(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	existed := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.RequestId = "req-ret-idem"
		r.TargetKind = model.RetentionTargetSegment
		r.Scanned, r.Deleted, r.Skipped = 40, 12, 3
		r.Purge, r.BatchLimit = 0, 30
	})
	snapshot := *existed
	before := snapshotWrites(db)

	in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, " req-ret-idem ")
	in.TargetId = retentionMissing // 定点回收一个不存在的对象：幂等判定必须早于准入
	info, err := submitRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "同 request_id 重放")

	wantRetentionEchoesRow(t, "同 request_id 重放", info, mustRetention(t, db, existed.RetentionId))
	wantField(t, "同 request_id 重放", "返回既有 retention_id", info.GetRetentionId(), existed.RetentionId)
	wantField(t, "同 request_id 重放", "没有排队删除回放产物", int32(info.GetTargetKind()), model.RetentionTargetSegment)
	wantField(t, "同 request_id 重放", "deleted 仍是既有证据", info.GetDeleted(), int32(12))
	assertRetentionUnchanged(t, db, existed.RetentionId, snapshot, "同 request_id 重放")
	wantField(t, "同 request_id 重放", "没有第二行", len(db.retentions), 1)
	wantNoTargetLookups(t, db, "同 request_id 重放")
	wantNoWrites(t, db, before, "同 request_id 重放")
	wantEvents(t, db, nil, "同 request_id 重放")
}

// 第二层：同一对象已有未完成的回收任务 → 复用。
// 两个删除任务并存必然双删或误删（第二个删到的是同一批对象的残留行）。
func TestSubmitRetentionTaskReusesUnfinishedTaskOnSameTarget(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seg := verifiedSegment(db, nil)
	active := seedRetention(db, model.RetentionStatePending, func(r *model.LiveRetentionTask) {
		r.RequestId, r.TargetKind, r.TargetId, r.RoomId = "req-ret-active", model.RetentionTargetSegment, seg.Id, seg.RoomId
		r.Purge, r.Reason = 0, "上一轮的 dry_run"
	})
	snapshot := *active
	before := snapshotWrites(db)

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-ret-new-key"))
	info = wantOK(t, info, err, "同对象未完成复用")

	wantRetentionEchoesRow(t, "同对象未完成复用", info, mustRetention(t, db, active.RetentionId))
	wantField(t, "同对象未完成复用", "复用既有 retention_id", info.GetRetentionId(), active.RetentionId)
	wantField(t, "同对象未完成复用", "新 request_id 未落库（复用行保留原键）",
		mustRetention(t, db, active.RetentionId).RequestId, "req-ret-active")
	assertRetentionUnchanged(t, db, active.RetentionId, snapshot, "同对象未完成复用")
	wantField(t, "同对象未完成复用", "只有一行", len(db.retentions), 1)
	wantCalls(t, db, "RetentionTasks.FindUnfinishedByTarget", 0, 1, "同对象未完成复用")
	wantCalls(t, db, "RetentionTasks.InsertTx", 0, 0, "复用不得再 INSERT")
	wantNoWrites(t, db, before, "同对象未完成复用")
	wantEvents(t, db, nil, "同对象未完成复用")
}

// 已完成/已失败的对象允许再排一次（证据不回改，新行是新意图）；
// 但批量任务（target_id=0）是集合语义，可以重叠，不参与同对象去重。
func TestSubmitRetentionTaskTerminalTaskAndBatchTasksAreNotDeduped(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seg := verifiedSegment(db, nil)
	done := seedRetention(db, model.RetentionStateSucceeded, func(r *model.LiveRetentionTask) {
		r.RequestId, r.TargetKind, r.TargetId, r.RoomId = "req-ret-done", model.RetentionTargetSegment, seg.Id, seg.RoomId
		r.Scanned, r.Deleted = 1, 1
	})
	doneSnapshot := *done

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-ret-after-done"))
	info = wantOK(t, info, err, "终态之后重新排队")

	newRow := mustRetention(t, db, info.GetRetentionId())
	wantField(t, "终态之后重新排队", "是新行", newRow.RetentionId != done.RetentionId, true)
	wantField(t, "终态之后重新排队", "新行回到 PENDING", newRow.State, model.RetentionStatePending)
	wantField(t, "终态之后重新排队", "新行计数为 0", newRow.Deleted, int32(0))
	assertRetentionUnchanged(t, db, done.RetentionId, doneSnapshot, "终态之后重新排队")
	wantField(t, "终态之后重新排队", "两行并存", len(db.retentions), 2)

	// 批量任务：已有 PENDING 的批量扫描不挡第二次批量登记（集合可重叠，去重键只有 request_id）。
	in := submitRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, "req-ret-second-sweep")
	second, err := submitRetention(context.Background(), svcCtx, in)
	second = wantOK(t, second, err, "第二次批量扫描")
	wantField(t, "第二次批量扫描", "是新行", second.GetRetentionId() != newRow.RetentionId, true)
	wantField(t, "第二次批量扫描", "库里三行", len(db.retentions), 3)
	wantCalls(t, db, "RetentionTasks.FindUnfinishedByTarget", 0, 1, "批量任务不参与同对象去重")
	wantEvents(t, db, nil, "回收登记不发事件")
	wantNoLeak(t, db, "回收登记")
}

// 第三层：唯一键是幂等的最终防线。
// 时序：预读 request_id 时对手尚未提交（missOnce 复刻），进入事务前对手的行刚落库
// （挂在 FindUnfinishedByTarget 的一次性钩子上，且刻意指向另一个对象，让唯一键成为唯一命中点）
// → InsertTx 撞 uniq_request_id → 事务回滚 → 回读给出与预读同一结论。
func TestSubmitRetentionTaskConcurrentInsertLosesRequestIdRace(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seg := verifiedSegment(db, nil)
	db.missOnce("RetentionTasks.FindByRequestID")
	var competitor *model.LiveRetentionTask
	db.onHit("RetentionTasks.FindUnfinishedByTarget", func() {
		competitor = seedRetention(db, model.RetentionStatePending, func(r *model.LiveRetentionTask) {
			r.RequestId, r.TargetKind, r.TargetId = "req-ret-race", model.RetentionTargetReplay, retentionOtherObj
		})
	})

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-ret-race"))
	info = wantOK(t, info, err, "并发撞 request_id 必须回读既有行")

	wantField(t, "并发撞 request_id", "retention_id 是对手的", info.GetRetentionId(), competitor.RetentionId)
	wantField(t, "并发撞 request_id", "state 取库内真值", int32(info.GetState()), competitor.State)
	wantField(t, "并发撞 request_id", "库里只有一行", len(db.retentions), 1)
	wantCalls(t, db, "RetentionTasks.InsertTx", 0, 1, "只发过一次 INSERT")
	// 撞键发生在写入快照之前：本方法连「打算写」的那一行都不该留下。
	wantField(t, "并发撞 request_id", "没有留下写入快照", len(db.triedRetentions), 0)
	wantField(t, "并发撞 request_id", "幂等键读两次（预读 + 撞键回读）", db.count("RetentionTasks.FindByRequestID"), 2)
	wantEvents(t, db, nil, "并发撞 request_id")
	wantNoLeak(t, db, "并发撞 request_id")
}

// ---------------------------------------------------------------- SubmitRetentionTask：定点准入

// 定点回收的 room_id 一律取自对象自身归属（请求可以不声明），
// 而声明与归属不一致时必须拒绝 —— 这是跨房间回收的唯一拦截点。
func TestSubmitRetentionTaskPinnedTargetDecidesRoomAndBlocksCrossRoom(t *testing.T) {
	// 前置：对象属于「另一个房间」，因此 room_id=0 与 room_id=对象归属 都合法，声明别的房间必须被拒。
	for _, tc := range []struct {
		name    string
		claimed int64
		wantErr bool
	}{
		{"未声明房间 → 以对象归属为准", 0, false},
		{"声明与归属一致", testRoomOther, false},
		{"声明成别的房间（越权）", testRoomID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			seg := verifiedSegment(db, func(s *model.LiveRecordSegment) { s.RoomId = testRoomOther })
			before := snapshotWrites(db)

			in := pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-seg-room")
			in.RoomId = tc.claimed
			info, err := submitRetention(context.Background(), svcCtx, in)
			if tc.wantErr {
				wantFail(t, info, err, model.ErrInvalidRoomID, tc.name)
				wantField(t, tc.name, "未登记任务", len(db.retentions), 0)
				wantNoWrites(t, db, before, tc.name)
				wantEvents(t, db, nil, tc.name)
				return
			}
			info = wantOK(t, info, err, tc.name)
			row := onlyRetention(t, db, tc.name)
			wantRetentionEchoesRow(t, tc.name, info, row)
			wantField(t, tc.name, "room_id 取自切片行归属", row.RoomId, testRoomOther)
			wantField(t, tc.name, "target_id 是切片主键", row.TargetId, seg.Id)
			// 切片：本服务没有任何按单行的删除入口，登记只写任务行，不碰切片表。
			wantField(t, tc.name, "切片行仍在（登记不删对象）", countSegments(db, seg.RecordId), 1)
			wantCalls(t, db, "Segments.UpdateStateTx", 0, 0, "登记不得改写切片状态")
			wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, "非回放对象没有生命周期标记")
			wantField(t, tc.name, "副作用集合恰好是允许的那两个",
				firedWrites(db, before), "DB.TransactCtx,RetentionTasks.InsertTx")
			wantEvents(t, db, nil, tc.name)
			wantNoLeak(t, db, tc.name)
		})
	}
}

// 回放产物：任务行与引用的 Normal→Pending 标记同事务提交（AGENTS.md §5），
// 不会出现「任务已登记但引用无标记」，也不会多出第三条写。
func TestSubmitRetentionTaskPinnedReplayMarksRefPendingInSameTransaction(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ref := reclaimableReplayRef(db, nil)
	refSnapshot := *ref
	before := snapshotWrites(db)

	in := pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, ref.Id, "req-replay-pin")
	in.RoomId = 0 // 房间由引用行决定，调用方不必重复声明
	info, err := submitRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "定点回收回放产物")

	row := onlyRetention(t, db, "定点回收回放产物")
	wantRetentionEchoesRow(t, "定点回收回放产物", info, row)
	wantField(t, "定点回收回放产物", "target_id 是引用行主键", row.TargetId, ref.Id)
	wantField(t, "定点回收回放产物", "room_id 取自引用行归属", row.RoomId, ref.RoomId)
	wantField(t, "定点回收回放产物", "expire_before 不参与定点回收", row.ExpireBefore, int64(0))
	cur := mustRef(t, db, ref.Id)
	wantField(t, "定点回收回放产物", "引用进入待回收", cur.RetentionState, model.RefRetentionStatePending)
	// 标记只推进生命周期一列：投影列、产物坐标、主键引用都不许被回收登记顺手改写。
	if stripRefRetention(stripRefProjection(*cur)) != stripRefProjection(refSnapshot) {
		t.Fatalf("回收登记改动了引用行的非生命周期列：\n got=%+v\nwant=%+v", *cur, refSnapshot)
	}
	wantCalls(t, db, "DB.TransactCtx", 0, 1, "定点回收回放产物")
	wantCalls(t, db, "RetentionTasks.InsertTx", 0, 1, "定点回收回放产物")
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 1, "定点回收回放产物")
	wantField(t, "定点回收回放产物", "副作用集合恰好是允许的那三个",
		firedWrites(db, before), "DB.TransactCtx,ReplayRefs.MarkRetentionStateTx,RetentionTasks.InsertTx")
	wantEvents(t, db, nil, "定点回收回放产物")
	wantNoLeak(t, db, "定点回收回放产物")
}

// 引用已被投影标成 Pending（生命周期由回收流程独占）时不再要求父任务终态，
// 且标记 UPDATE 必然 0 行 —— 按幂等处理而不是报错，也不重复排队。
func TestSubmitRetentionTaskAdmitsAlreadyPendingRefWithoutDoubleMarking(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	inFlight := seedReplay(db, model.ReplayStateUploading, nil) // 父任务在飞：只有已标记的引用才允许跳过终态检查
	ref := refForTask(db, inFlight, func(r *model.LiveReplayAssetRef) {
		r.RetentionState = model.RefRetentionStatePending
	})
	before := snapshotWrites(db)

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, ref.Id, "req-ref-already-pending"))
	info = wantOK(t, info, err, "引用已 Pending")

	row := onlyRetention(t, db, "引用已 Pending")
	wantRetentionEchoesRow(t, "引用已 Pending", info, row)
	wantField(t, "引用已 Pending", "状态推进只发生一次",
		mustRef(t, db, ref.Id).RetentionState, model.RefRetentionStatePending)
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "已标记的引用不必回读父任务")
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 1, "标记语句仍发一次（0 行=幂等）")
	wantField(t, "引用已 Pending", "副作用集合", firedWrites(db, before),
		"DB.TransactCtx,ReplayRefs.MarkRetentionStateTx,RetentionTasks.InsertTx")
	wantEvents(t, db, nil, "引用已 Pending")
}

// 准入矩阵：不存在的对象、已回收的引用、还在产出的产物、仍在线的档位一律失败关闭。
// 「登记一个空转任务」不是更宽松的错误，而是更糟的错误：Worker 会照着它去删。
func TestSubmitRetentionTaskRejectsIneligiblePinnedTargets(t *testing.T) {
	cases := []struct {
		name    string
		kind    rpc.RetentionTargetKind
		prepare func(db *store) int64
		want    error
	}{
		{"切片不存在", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT,
			func(db *store) int64 { return retentionMissing }, model.ErrSegmentNotFound},
		{"引用不存在", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY,
			func(db *store) int64 { return retentionMissing }, model.ErrReplayRefNotFound},
		{"引用已回收（不排第二次队）", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY,
			func(db *store) int64 {
				return reclaimableReplayRef(db, func(r *model.LiveReplayAssetRef) {
					r.RetentionState = model.RefRetentionStateReclaimed
				}).Id
			}, model.ErrInvalidTransition},
		{"父任务还在拼接中（产物仍在产出）", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY,
			func(db *store) int64 {
				return refForTask(db, seedReplay(db, model.ReplayStateUploading, nil), nil).Id
			}, model.ErrInvalidTransition},
		{"父任务行已不在（无法证明产物已停）", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY,
			func(db *store) int64 {
				return seedReplayRef(db, func(r *model.LiveReplayAssetRef) { r.ReplayId = retentionMissing }).Id
			}, model.ErrReplayTaskNotFound},
		{"档位不存在", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT,
			func(db *store) int64 { return retentionMissing }, model.ErrStreamOutputNotFound},
		{"档位仍在线（回收等于掐断正在分发的流）", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT,
			func(db *store) int64 { return seedOutput(db, model.StreamOutputStateOnline, nil).OutputId },
			model.ErrInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			targetID := tc.prepare(db)
			readsBefore := db.count("RetentionTasks.InsertTx")
			before := snapshotWrites(db)

			info, err := submitRetention(context.Background(), svcCtx,
				pinnedRetentionReq(tc.kind, targetID, "req-ineligible"))
			wantFail(t, info, err, tc.want, tc.name)
			// 失败关闭：既不登记任务，也不推进任何生命周期标记。
			wantField(t, tc.name, "未登记回收任务", len(db.retentions), 0)
			wantField(t, tc.name, "未产生写入尝试", len(db.triedRetentions), 0)
			wantNoWrites(t, db, before, tc.name)
			wantCalls(t, db, "RetentionTasks.InsertTx", readsBefore, 0, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// 已下线的档位是唯一可回收的分发残留；登记时没有任何标记联动（引用生命周期只属于回放产物）。
func TestSubmitRetentionTaskAdmitsOfflineStreamOutput(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	out := seedOutput(db, model.StreamOutputStateOffline, nil)
	before := snapshotWrites(db)

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT, out.OutputId, "req-output-offline"))
	info = wantOK(t, info, err, "定点回收已下线档位")

	row := onlyRetention(t, db, "定点回收已下线档位")
	wantRetentionEchoesRow(t, "定点回收已下线档位", info, row)
	wantField(t, "定点回收已下线档位", "room_id 取自档位行", row.RoomId, out.RoomId)
	wantField(t, "定点回收已下线档位", "档位行仍带着下线证据（登记不删对象）",
		mustOutput(t, db, out.OutputId).State, model.StreamOutputStateOffline)
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, "非回放对象没有生命周期标记")
	wantField(t, "定点回收已下线档位", "副作用集合", firedWrites(db, before),
		"DB.TransactCtx,RetentionTasks.InsertTx")
	wantEvents(t, db, nil, "定点回收已下线档位")
	wantNoLeak(t, db, "定点回收已下线档位")
}

// 标记与任务行同事务：标记写失败必须整笔回滚，不能留下「任务已登记但引用无标记」的半产品。
func TestSubmitRetentionTaskRollsBackTaskWhenMarkerWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ref := reclaimableReplayRef(db, nil)
	refSnapshot := *ref
	db.failOn("ReplayRefs.MarkRetentionStateTx", errModelDown)

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, ref.Id, "req-marker-down"))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("标记故障必须原样上抛：%v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("故障路径不得带回响应体：%+v", info)
	}
	wantField(t, "标记故障回滚", "没有留下任务行", len(db.retentions), 0)
	// 「发过一次 INSERT」是回滚可证的唯一途径：只看库内 0 行会把「没写」和「写了又回滚」读成同一件事。
	wantCalls(t, db, "RetentionTasks.InsertTx", 0, 1, "标记故障回滚：INSERT 真的发过")
	wantField(t, "标记故障回滚", "写入快照存在但库内无行", len(db.triedRetentions), 1)
	assertRefUnchanged(t, db, ref.Id, refSnapshot, "标记故障回滚")
	wantEvents(t, db, nil, "标记故障回滚")
	wantNoLeak(t, db, "标记故障回滚")
}

// 依赖故障一律 fail closed：读不到对象、读不到幂等键、写不进去，都不等于「没有回收任务」。
func TestSubmitRetentionTaskFailsClosedOnModelErrors(t *testing.T) {
	cases := []struct {
		name string
		kind rpc.RetentionTargetKind
		pin  bool
		ops  []string
	}{
		{"批量扫描", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, false,
			[]string{"RetentionTasks.FindByRequestID", "RetentionTasks.InsertTx", "DB.TransactCtx"}},
		{"定点切片", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, true,
			[]string{"RetentionTasks.FindUnfinishedByTarget", "Segments.FindOne"}},
		{"定点回放", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_REPLAY, true,
			[]string{"ReplayRefs.FindOne", "ReplayTasks.FindOne"}},
		{"定点档位", rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT, true,
			[]string{"StreamOutputs.FindOne"}},
	}
	for _, tc := range cases {
		for _, op := range tc.ops {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				db := newStore()
				svcCtx := newTestSvc(db)
				var targetID int64
				switch {
				case !tc.pin:
					targetID = 0
				case int32(tc.kind) == model.RetentionTargetSegment:
					targetID = verifiedSegment(db, nil).Id
				case int32(tc.kind) == model.RetentionTargetReplay:
					targetID = reclaimableReplayRef(db, nil).Id
				default:
					targetID = seedOutput(db, model.StreamOutputStateOffline, nil).OutputId
				}
				db.failOn(op, errModelDown)

				in := submitRetentionReq(tc.kind, "req-down-"+op)
				if tc.pin {
					in.RoomId, in.TargetId, in.ExpireBefore = testRoomID, targetID, 0
				}
				info, err := submitRetention(context.Background(), svcCtx, in)
				if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
					t.Fatalf("%s 故障必须原样上抛（不得吞错或当成功）：%v", op, err)
				}
				if !isNilPtr(info) {
					t.Fatalf("%s 故障时不得带回响应体：%+v", op, info)
				}
				wantField(t, op+" 故障", "未产出回收行", len(db.retentions), 0)
				wantEvents(t, db, nil, op+" 故障")
				// 整笔事务没提交成功时，引用不得留下任何生命周期推进（无引用时该判据自然成立）。
				if op == "DB.TransactCtx" || op == "RetentionTasks.InsertTx" {
					for _, ref := range db.replayRefs {
						wantField(t, op+" 故障", "引用生命周期未被推进",
							ref.RetentionState, model.RefRetentionStateNormal)
					}
				}
			})
		}
	}
}

// 提交已提交、回读失败：任务行必须留在库里（意图已经成立），错误原样上抛。
// 调用方用同一个 request_id 重投即可拿回结论 —— 这条恢复路径也在本用例里走通。
func TestSubmitRetentionTaskPostCommitRereadFailureKeepsCommittedTask(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seg := verifiedSegment(db, nil)
	db.failOn("RetentionTasks.FindOne", errModelDown)

	info, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-reread-error"))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("回读故障必须原样上抛：%v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("回读故障不得带回响应体：%+v", info)
	}
	row := onlyRetention(t, db, "回读故障")
	wantField(t, "回读故障", "任务已提交（PENDING）", row.State, model.RetentionStatePending)

	// 同一幂等键重投：预读命中既有行 → 不再排第二次删除意图，也不重复 INSERT。
	db.failOn("RetentionTasks.FindOne", nil)
	again, err := submitRetention(context.Background(), svcCtx,
		pinnedRetentionReq(rpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT, seg.Id, "req-reread-error"))
	again = wantOK(t, again, err, "重投回读")
	wantField(t, "重投回读", "拿回同一行", again.GetRetentionId(), row.RetentionId)
	wantField(t, "重投回读", "仍然只有一行", len(db.retentions), 1)
	wantCalls(t, db, "RetentionTasks.InsertTx", 1, 0, "重投不得再 INSERT")
	wantEvents(t, db, nil, "回读故障与重投")
}

// ---------------------------------------------------------------- ReportRetentionResult：门禁

// 入参门禁全部发生在读库之前：状态/原因/版本/计数任一不合法时，
// 连「查一次当前行」都不允许发生（否则一个坏回执会被读成一次合法的乐观并发冲突）。
func TestReportRetentionResultValidatesArgumentsBeforeReading(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rpc.ReportRetentionResultReq)
		want   error
		msg    string
	}{
		{"retention_id=0", func(in *rpc.ReportRetentionResultReq) { in.RetentionId = 0 },
			model.ErrRetentionTaskNotFound, ""},
		{"retention_id 为负", func(in *rpc.ReportRetentionResultReq) { in.RetentionId = -1 },
			model.ErrRetentionTaskNotFound, ""},
		{"state UNSPECIFIED", func(in *rpc.ReportRetentionResultReq) { in.State = rpc.RetentionState_RETENTION_STATE_UNSPECIFIED },
			model.ErrInvalidTransition, ""},
		{"state PENDING 不是上报目标态", func(in *rpc.ReportRetentionResultReq) { in.State = rpc.RetentionState_RETENTION_STATE_PENDING },
			model.ErrInvalidTransition, ""},
		{"state 区间外", func(in *rpc.ReportRetentionResultReq) { in.State = rpc.RetentionState(6) },
			model.ErrInvalidTransition, ""},
		{"state 负数", func(in *rpc.ReportRetentionResultReq) { in.State = rpc.RetentionState(-1) },
			model.ErrInvalidTransition, ""},
		{"fail_reason 区间外", func(in *rpc.ReportRetentionResultReq) {
			in.State, in.FailReason = rpc.RetentionState_RETENTION_STATE_FAILED, rpc.FailureReason(9)
		}, model.ErrInvalidTransition, ""},
		{"fail_reason 负数", func(in *rpc.ReportRetentionResultReq) {
			in.State, in.FailReason = rpc.RetentionState_RETENTION_STATE_FAILED, rpc.FailureReason(-1)
		}, model.ErrInvalidTransition, ""},
		{"expected_version 为负", func(in *rpc.ReportRetentionResultReq) { in.ExpectedVersion = -1 },
			model.ErrVersionConflict, ""},
		{"scanned 为负", func(in *rpc.ReportRetentionResultReq) { in.Scanned = -1 }, model.ErrInvalidTransition, ""},
		{"deleted 为负", func(in *rpc.ReportRetentionResultReq) { in.Deleted = -1 }, model.ErrInvalidTransition, ""},
		{"skipped 为负", func(in *rpc.ReportRetentionResultReq) { in.Skipped = -1 }, model.ErrInvalidTransition, ""},
		{"deleted+skipped>scanned", func(in *rpc.ReportRetentionResultReq) {
			in.Scanned, in.Deleted, in.Skipped = 10, 8, 3
		}, model.ErrInvalidTransition, ""},
		{"FAILED 必须给具体归因", func(in *rpc.ReportRetentionResultReq) {
			in.State = rpc.RetentionState_RETENTION_STATE_FAILED
		}, model.ErrInvalidTransition, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRetention(db, model.RetentionStateRunning, nil)
			snapshot := *row
			before := snapshotWrites(db)

			in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
			tc.mutate(in)
			info, err := reportRetention(context.Background(), svcCtx, in)
			wantFail(t, info, err, tc.want, tc.name)
			assertNoRetentionReads(t, db, tc.name)
			assertRetentionUnchanged(t, db, row.RetentionId, snapshot, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}
}

// purge=false 的行只登记标记：上报 deleted>0 就是「没有删除授权却删了东西」，
// 必须拒收（并留 Errorf 告警），绝不能替它把数字改成 0 后收下。
func TestReportRetentionResultRejectsUnauthorisedDelete(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	dryRun := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.Purge = 0
	})
	snapshot := *dryRun
	before := snapshotWrites(db)

	in := reportRetentionReq(dryRun.RetentionId, dryRun.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted, in.Skipped = 10, 1, 0
	info, err := reportRetention(context.Background(), svcCtx, in)
	wantFail(t, info, err, model.ErrInvalidTransition, "dry_run 上报删除")
	assertRetentionUnchanged(t, db, dryRun.RetentionId, snapshot, "dry_run 上报删除")
	wantNoWrites(t, db, before, "dry_run 上报删除")
	wantEvents(t, db, nil, "dry_run 上报删除")

	// 同样的回执把 deleted 归零后就是合法的：判据是「越权」而不是「非零计数」。
	in.Deleted, in.Skipped = 0, 10
	okInfo, err := reportRetention(context.Background(), svcCtx, in)
	okInfo = wantOK(t, okInfo, err, "dry_run 全跳过")
	wantField(t, "dry_run 全跳过", "终态 SUCCEEDED", int32(okInfo.GetState()), model.RetentionStateSucceeded)
	wantField(t, "dry_run 全跳过", "deleted 恒为 0", okInfo.GetDeleted(), int32(0))
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "dry_run 全跳过")
}

// ---------------------------------------------------------------- ReportRetentionResult：状态机

// 认领 → 完成这条主路径，外加成功边把上一次 RUNNING 的失败痕迹擦干净（缺陷 #7 的回归护栏）。
func TestReportRetentionResultClaimThenSucceededClearsTrace(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRetention(db, model.RetentionStatePending, nil)
	was := *row

	// 第一段：PENDING→RUNNING 认领，带一次瞬时故障的归因（此时还没删成）。
	claim := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_RUNNING)
	claim.Scanned = 4
	claim.Errno, claim.ErrMsg = 503, "storage 暂不可用"
	claim.WorkerId = "  worker-purge  " // 首尾空白：入事件前必须归一
	claim.FailReason = rpc.FailureReason(model.ReasonStorage)
	claim.TraceId = "trace-retention-claim"
	info, err := reportRetention(context.Background(), svcCtx, claim)
	info = wantOK(t, info, err, "认领")

	// 必须取副本：mustRetention 返回的是库内活行指针，第二段上报会就地改写它，
	// 用着它做「第一段之后」的断言就会把新值当旧值比较。
	claimed := *mustRetention(t, db, row.RetentionId)
	wantRetentionEchoesRow(t, "认领", info, &claimed)
	wantField(t, "认领", "state", claimed.State, model.RetentionStateRunning)
	wantField(t, "认领", "version", claimed.Version, was.Version+1)
	wantField(t, "认领", "scanned 覆盖写", claimed.Scanned, int32(4))
	wantField(t, "认领", "errno 收下", claimed.Errno, int32(503))
	wantField(t, "认领", "fail_reason 收下", claimed.FailReason, model.ReasonStorage)
	wantField(t, "认领", "trace_id 换成本次回执", claimed.TraceId, "trace-retention-claim")
	// 认领不发事件：词表里没有「回收执行中」，也不伪造。
	wantEvents(t, db, nil, "认领")
	claimWrites := snapshotWrites(db)

	// 第二段：RUNNING→SUCCEEDED，回执不带归因 → 三列必须被擦干净。
	done := reportRetentionReq(row.RetentionId, claimed.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	done.Scanned, done.Deleted, done.Skipped = 10, 7, 3
	done.ExpectedVersion = claimed.Version
	info, err = reportRetention(context.Background(), svcCtx, done)
	info = wantOK(t, info, err, "完成")

	cur := mustRetention(t, db, row.RetentionId)
	wantRetentionEchoesRow(t, "完成", info, cur)
	wantField(t, "完成", "state", cur.State, model.RetentionStateSucceeded)
	wantField(t, "完成", "version", cur.Version, claimed.Version+1)
	wantField(t, "完成", "scanned 覆盖写（不是累加）", cur.Scanned, int32(10))
	wantField(t, "完成", "deleted", cur.Deleted, int32(7))
	wantField(t, "完成", "skipped", cur.Skipped, int32(3))
	wantField(t, "完成", "成功行不得留着 errno", cur.Errno, int32(0))
	wantField(t, "完成", "成功行不得留着 err_msg", cur.ErrMsg, "")
	wantField(t, "完成", "成功行不得留着 fail_reason", cur.FailReason, model.ReasonUnspecified)
	// 意图快照（回收哪一批、授权、限流、审计）在终态后不回改。
	if stripRetentionEvidence(stripRetentionDrive(*cur)) != stripRetentionEvidence(stripRetentionDrive(was)) {
		t.Fatalf("上报只该改状态机与证据列：\n got=%+v\nwant=%+v",
			stripRetentionEvidence(stripRetentionDrive(*cur)), stripRetentionEvidence(stripRetentionDrive(was)))
	}
	wantCalls(t, db, "RetentionTasks.UpdateState", 0, 0, "推进必须在事务内")
	wantField(t, "完成", "第二段的副作用集合", firedWrites(db, claimWrites),
		"DB.TransactCtx,RetentionTasks.UpdateStateTx,Outbox.Insert")
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "完成")
	wantNoLeak(t, db, "认领并完成")
}

// PENDING→SUCCEEDED 没有边：没认领就报成功，等于给一批从未扫过的对象补一份「已删除」证据。
// 而 PENDING→FAILED/CANCELLED 是合法边（什么都没删，不需要认领），两条判据必须分开钉住。
func TestReportRetentionResultRequiresClaimBeforeSuccessButNotFailure(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRetention(db, model.RetentionStatePending, nil)
	snapshot := *row
	before := snapshotWrites(db)

	skip := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	skip.Scanned, skip.Deleted = 5, 5
	info, err := reportRetention(context.Background(), svcCtx, skip)
	wantFail(t, info, err, model.ErrInvalidTransition, "未认领就报成功")
	assertRetentionUnchanged(t, db, row.RetentionId, snapshot, "未认领就报成功")
	wantNoWrites(t, db, before, "未认领就报成功")
	wantEvents(t, db, nil, "未认领就报成功")

	// PENDING→FAILED / PENDING→CANCELLED 都是合法边：什么都没删，不需要先认领。
	// 两条边各用一份新库——同一条行进第一个终态后，第二个上报撞的就是终态判定而非认领判定。
	for _, st := range []rpc.RetentionState{rpc.RetentionState_RETENTION_STATE_FAILED,
		rpc.RetentionState_RETENTION_STATE_CANCELLED} {
		label := "未认领直接进 " + strconv.Itoa(int(st))
		fresh := newStore()
		pending := seedRetention(fresh, model.RetentionStatePending, nil)
		in := reportRetentionReq(pending.RetentionId, pending.Version, st)
		in.FailReason = rpc.FailureReason(model.ReasonManual)
		okInfo, err := reportRetention(context.Background(), newTestSvc(fresh), in)
		okInfo = wantOK(t, okInfo, err, label)
		wantField(t, label, "state", int32(okInfo.GetState()), int32(st))
		wantField(t, label, "仍是同一行", len(fresh.retentions), 1)
		wantField(t, label, "意图快照不回改", mustRetention(t, fresh, pending.RetentionId).Reason,
			pending.Reason)
		wantCalls(t, fresh, "RetentionTasks.UpdateStateTx", 0, 1, label)
		wantEvents(t, fresh, []string{model.EventTypeRetentionFinished}, label+"：进终态发一条事件")
	}
}

// 终态只允许同值重放；RUNNING 允许同值重放但不允许换数（表里没有自环）。
func TestReportRetentionResultReplayAndSelfTransitionRules(t *testing.T) {
	// 终态 + 完全同值：返回当前行，不 ++version、不再发事件。
	db := newStore()
	svcCtx := newTestSvc(db)
	done := seedRetention(db, model.RetentionStateSucceeded, func(r *model.LiveRetentionTask) {
		r.Scanned, r.Deleted, r.Skipped = 8, 6, 2
		r.State, r.Version = model.RetentionStateSucceeded, 3
	})
	doneSnapshot := *done
	before := snapshotWrites(db)

	replay := reportRetentionReq(done.RetentionId, 0, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	replay.Scanned, replay.Deleted, replay.Skipped = 8, 6, 2
	info, err := reportRetention(context.Background(), svcCtx, replay)
	info = wantOK(t, info, err, "终态同值重放")
	wantRetentionEchoesRow(t, "终态同值重放", info, &doneSnapshot)
	assertRetentionUnchanged(t, db, done.RetentionId, doneSnapshot, "终态同值重放")
	wantNoWrites(t, db, before, "终态同值重放")
	wantEvents(t, db, nil, "终态同值重放：重放不得补第二条事件")

	// 终态 + 不同计数 / 迟到想把行改回执行中：一律 ErrTerminalState。
	for _, tc := range []struct {
		name  string
		state rpc.RetentionState
		dl    int32
	}{
		{"终态但换了计数", rpc.RetentionState_RETENTION_STATE_SUCCEEDED, 5},
		{"迟到的 RUNNING 想把行拉回执行中", rpc.RetentionState_RETENTION_STATE_RUNNING, 6},
	} {
		in := reportRetentionReq(done.RetentionId, 0, tc.state)
		in.Scanned, in.Deleted, in.Skipped = 8, tc.dl, 2
		info, err := reportRetention(context.Background(), svcCtx, in)
		wantFail(t, info, err, model.ErrTerminalState, tc.name)
		assertRetentionUnchanged(t, db, done.RetentionId, doneSnapshot, tc.name)
	}
	wantEvents(t, db, nil, "终态拒绝")

	// RUNNING 自环：同值是重放，换数就是非法迁移（计数只在终态那一次被完整覆盖）。
	running := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.Scanned, r.Deleted, r.Skipped = 4, 1, 1
	})
	runningSnapshot := *running
	same := reportRetentionReq(running.RetentionId, 0, rpc.RetentionState_RETENTION_STATE_RUNNING)
	same.Scanned, same.Deleted, same.Skipped = 4, 1, 1
	info, err = reportRetention(context.Background(), svcCtx, same)
	info = wantOK(t, info, err, "RUNNING 同值重放")
	assertRetentionUnchanged(t, db, running.RetentionId, runningSnapshot, "RUNNING 同值重放")
	wantField(t, "RUNNING 同值重放", "version 未推进", info.GetVersion(), running.Version)

	ahead := reportRetentionReq(running.RetentionId, running.Version+5, rpc.RetentionState_RETENTION_STATE_RUNNING)
	ahead.Scanned, ahead.Deleted, ahead.Skipped = 4, 1, 1
	info, err = reportRetention(context.Background(), svcCtx, ahead)
	wantFail(t, info, err, model.ErrVersionConflict, "重放带了超前版本")

	changed := reportRetentionReq(running.RetentionId, running.Version, rpc.RetentionState_RETENTION_STATE_RUNNING)
	changed.Scanned, changed.Deleted, changed.Skipped = 6, 2, 1
	info, err = reportRetention(context.Background(), svcCtx, changed)
	wantFail(t, info, err, model.ErrInvalidTransition, "RUNNING 换数不是自环")
	assertRetentionUnchanged(t, db, running.RetentionId, runningSnapshot, "RUNNING 换数")
	wantEvents(t, db, nil, "RUNNING 侧全部不写事件")
}

// 已认领的行带着上一次上报的证据：任何一项回退都说明这回执不是同一批。
// 收下就等于抹掉已经发生的删除记录 —— 删除是不可逆动作，证据只能增不能减。
func TestReportRetentionResultCountsMayNotShrinkAfterClaim(t *testing.T) {
	cases := []struct {
		name                      string
		scanned, deleted, skipped int32
	}{
		{"scanned 回退", 5, 3, 0},
		{"deleted 回退", 9, 2, 0},
		{"skipped 回退", 9, 3, 0},
		{"全部归零", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
				r.Scanned, r.Deleted, r.Skipped = 9, 3, 1
			})
			snapshot := *row
			before := snapshotWrites(db)

			in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
			in.Scanned, in.Deleted, in.Skipped = tc.scanned, tc.deleted, tc.skipped
			info, err := reportRetention(context.Background(), svcCtx, in)
			wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
			assertRetentionUnchanged(t, db, row.RetentionId, snapshot, tc.name)
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
		})
	}

	// 反向对照：不减少（含持平）就合法，且是覆盖写而不是累加。
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.Scanned, r.Deleted, r.Skipped = 9, 3, 1
	})
	in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted, in.Skipped = 9, 3, 2
	info, err := reportRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "计数不回退")
	wantField(t, "计数不回退", "scanned 持平", info.GetScanned(), int32(9))
	wantField(t, "计数不回退", "skipped 增长", info.GetSkipped(), int32(2))
	wantField(t, "计数不回退", "deleted 不是累加成 6", info.GetDeleted(), int32(3))
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "计数不回退")
}

// ---------------------------------------------------------------- ReportRetentionResult：结论与现状对账

// 档位行仍在线却被上报「产物已删」：Worker 串了对象，必须拒收。
// 已下线（或批量任务根本不带单个 target_id）时才允许把 deleted 记上去。
func TestReportRetentionResultRejectsDeleteClaimForOnlineOutput(t *testing.T) {
	cases := []struct {
		name    string
		state   int32
		pinned  bool // false = 批量任务（target_id=0），不与单行档位对账
		deleted int32
		wantErr bool
	}{
		{"仍在线 + 声称删了产物", model.StreamOutputStateOnline, true, 2, true},
		{"已下线 + 声称删了产物", model.StreamOutputStateOffline, true, 2, false},
		{"仍在线但什么都没删", model.StreamOutputStateOnline, true, 0, false},
		{"批量任务（target_id=0）不针对单行对账", model.StreamOutputStateOnline, false, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			var target int64
			if tc.pinned {
				target = seedOutput(db, tc.state, nil).OutputId
			}
			row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
				r.TargetKind, r.TargetId, r.RoomId, r.ExpireBefore = model.RetentionTargetStreamOutput, target, testRoomID, 0
			})
			snapshot := *row
			before := snapshotWrites(db)

			in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
			in.Scanned, in.Deleted = 5, tc.deleted
			info, err := reportRetention(context.Background(), svcCtx, in)
			if tc.wantErr {
				wantFail(t, info, err, model.ErrInvalidTransition, tc.name)
				assertRetentionUnchanged(t, db, row.RetentionId, snapshot, tc.name)
				wantNoWrites(t, db, before, tc.name)
				wantEvents(t, db, nil, tc.name)
				return
			}
			info = wantOK(t, info, err, tc.name)
			wantField(t, tc.name, "state", int32(info.GetState()), model.RetentionStateSucceeded)
			wantEvents(t, db, []string{model.EventTypeRetentionFinished}, tc.name)
		})
	}

	// 对账要读档位行：读故障必须 fail closed，不能把「读不到」当成「已下线」。
	db := newStore()
	svcCtx := newTestSvc(db)
	target := seedOutput(db, model.StreamOutputStateOnline, nil).OutputId
	row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.TargetKind, r.TargetId, r.RoomId, r.ExpireBefore = model.RetentionTargetStreamOutput, target, testRoomID, 0
	})
	db.failOn("StreamOutputs.FindOne", errModelDown)
	before := snapshotWrites(db)
	in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted = 5, 2
	info, err := reportRetention(context.Background(), svcCtx, in)
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("档位读故障必须原样上抛：%v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("读故障不得带回响应体：%+v", info)
	}
	wantNoWrites(t, db, before, "档位读故障")
	wantEvents(t, db, nil, "档位读故障")
}

// ---------------------------------------------------------------- ReportRetentionResult：标记联动与证据

// 引用生命周期的三条结论：删成就 Reclaimed，什么都没删成回 Normal，
// purge=false 时即使 SUCCEEDED 也留在 Pending（对象存储根本没动，标成已回收就是假证据）。
func TestReportRetentionResultRefMarkerLinkage(t *testing.T) {
	cases := []struct {
		name          string
		purge         int32
		state         rpc.RetentionState
		failReason    rpc.FailureReason
		deleted       int32
		wantRefState  int32
		wantMarkCalls int
	}{
		{"成功且删成 → 已回收", 1, rpc.RetentionState_RETENTION_STATE_SUCCEEDED, 0, 3,
			model.RefRetentionStateReclaimed, 1},
		{"成功但没删（purge=false）→ 仍待回收", 0, rpc.RetentionState_RETENTION_STATE_SUCCEEDED, 0, 0,
			model.RefRetentionStatePending, 0},
		{"成功且删了 0 行（全被引用挡住）→ 仍待回收", 1, rpc.RetentionState_RETENTION_STATE_SUCCEEDED, 0, 0,
			model.RefRetentionStatePending, 0},
		{"失败且什么都没删 → 标记回退", 1, rpc.RetentionState_RETENTION_STATE_FAILED, rpc.FailureReason(model.ReasonStorage), 0,
			model.RefRetentionStateNormal, 1},
		{"取消且什么都没删 → 标记回退", 1, rpc.RetentionState_RETENTION_STATE_CANCELLED, 0, 0,
			model.RefRetentionStateNormal, 1},
		{"失败但删了一部分 → 保留待回收（残留对象仍需清理）", 1, rpc.RetentionState_RETENTION_STATE_FAILED,
			rpc.FailureReason(model.ReasonStorage), 2, model.RefRetentionStatePending, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			ref := reclaimableReplayRef(db, func(r *model.LiveReplayAssetRef) {
				r.RetentionState = model.RefRetentionStatePending
			})
			row := retentionForRef(db, model.RetentionStateRunning, ref, func(r *model.LiveRetentionTask) {
				r.Purge = tc.purge
				r.Scanned, r.Deleted, r.Skipped = 5, 0, 0
			})

			in := reportRetentionReq(row.RetentionId, row.Version, tc.state)
			in.Scanned, in.Deleted, in.Skipped = 5, tc.deleted, 5-tc.deleted
			in.FailReason = tc.failReason
			info, err := reportRetention(context.Background(), svcCtx, in)
			info = wantOK(t, info, err, tc.name)
			wantField(t, tc.name, "回收任务进终态", int32(info.GetState()), int32(tc.state))
			wantField(t, tc.name, "引用生命周期结论", mustRef(t, db, ref.Id).RetentionState, tc.wantRefState)
			wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, tc.wantMarkCalls, tc.name)
			wantEvents(t, db, []string{model.EventTypeRetentionFinished}, tc.name)
			wantNoLeak(t, db, tc.name)
		})
	}

	// 批量回收不给任何单行打标记：它删的是一批对象，逐行结论由 Worker 自己带回。
	db := newStore()
	svcCtx := newTestSvc(db)
	batch := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.Scanned, r.Deleted = 7, 7
	})
	in := reportRetentionReq(batch.RetentionId, batch.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted = 7, 7
	info, err := reportRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "批量任务进终态")
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, "批量任务不动单行标记")
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "批量任务进终态")
}

// SEGMENT 回收：本服务没有按单行切片的删除入口（model 只有按录制任务粒度的 Purge），
// 所以登记与上报都不碰切片表，只记告警并把结论留在任务行上。
func TestReportRetentionResultSegmentPurgeLeavesRowsAndStillRecordsEvidence(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seg := verifiedSegment(db, nil)
	row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.TargetKind, r.TargetId, r.RoomId, r.ExpireBefore = model.RetentionTargetSegment, seg.Id, seg.RoomId, 0
	})
	in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted = 1, 1

	info, err := reportRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "切片回收完成")
	wantField(t, "切片回收完成", "deleted 记上了", info.GetDeleted(), int32(1))
	wantField(t, "切片回收完成", "本地切片行仍在（不越权代删）", countSegments(db, seg.RecordId), 1)
	wantCalls(t, db, "Segments.UpdateStateTx", 0, 0, "切片状态不由回收推进")
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, "非回放对象没有生命周期标记")
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "切片回收完成")
}

// 终态事件的可追溯性：payload 只带主键、计数与状态编号，
// 计数取本次生效值（不是事务前的旧账），归因文本一律不入事件。
func TestReportRetentionResultFinishedEventPayload(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ref := reclaimableReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.RetentionState = model.RefRetentionStatePending
	})
	row := retentionForRef(db, model.RetentionStateRunning, ref, func(r *model.LiveRetentionTask) {
		r.ExpireBefore, r.BatchLimit = 0, 25
	})
	before := snapshotWrites(db)

	in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_FAILED)
	in.Scanned, in.Deleted, in.Skipped = 25, 0, 25
	in.FailReason = rpc.FailureReason(model.ReasonStorage)
	in.Errno = 12
	in.ErrMsg = "删除失败：" + leakSignedURL // 签名地址必须脱敏后才入库
	in.ExpectedVersion = row.Version
	info, err := reportRetention(context.Background(), svcCtx, in)
	info = wantOK(t, info, err, "回收失败上报")

	wantField(t, "回收失败上报", "err_msg 已脱敏", strings.Contains(mustRetention(t, db, row.RetentionId).ErrMsg, leakMarker), false)
	wantEvents(t, db, []string{model.EventTypeRetentionFinished}, "回收失败上报")
	ev := eventAt(t, db, 0)
	// event_type 写字面量而不是常量：常量被改名时 Topic 会跟着漂移，而下游订阅的是这个字符串。
	wantField(t, "回收结束事件", "event_type", ev.EventType, "livemedia.retention.finished")
	wantField(t, "回收结束事件", "event_type 与常量一致", ev.EventType, model.EventTypeRetentionFinished)
	wantField(t, "回收结束事件", "aggregate_type", ev.AggregateType, model.AggregateRetentionTask)
	wantField(t, "回收结束事件", "aggregate_id", ev.AggregateId, strconv.FormatInt(row.RetentionId, 10))
	wantField(t, "回收结束事件", "room_id 冗余列", ev.RoomId, ref.RoomId)
	wantField(t, "回收结束事件", "state 待投递", ev.State, model.OutboxStatePending)
	wantField(t, "回收结束事件", "schema_version", ev.SchemaVersion, model.EventSchemaVersion)
	if ev.EventId == "" {
		t.Fatal("回收结束事件没有 event_id，消费者无法去重")
	}

	p := eventPayload(t, ev)
	wantField(t, "回收结束事件 payload", "retention_id", toInt64(t, p, "retention_id"), row.RetentionId)
	wantField(t, "回收结束事件 payload", "target_kind", toInt64(t, p, "target_kind"), int64(model.RetentionTargetReplay))
	wantField(t, "回收结束事件 payload", "target_id", toInt64(t, p, "target_id"), ref.Id)
	wantField(t, "回收结束事件 payload", "room_id", toInt64(t, p, "room_id"), row.RoomId)
	wantField(t, "回收结束事件 payload", "expire_before 是登记时的意图快照", toInt64(t, p, "expire_before"), int64(0))
	wantField(t, "回收结束事件 payload", "purge", toInt64(t, p, "purge"), int64(1))
	wantField(t, "回收结束事件 payload", "prev_state", toInt64(t, p, "prev_state"), int64(model.RetentionStateRunning))
	wantField(t, "回收结束事件 payload", "state", toInt64(t, p, "state"), int64(model.RetentionStateFailed))
	wantField(t, "回收结束事件 payload", "scanned 取本次值", toInt64(t, p, "scanned"), int64(25))
	wantField(t, "回收结束事件 payload", "deleted 取本次值", toInt64(t, p, "deleted"), int64(0))
	wantField(t, "回收结束事件 payload", "skipped 取本次值", toInt64(t, p, "skipped"), int64(25))
	wantField(t, "回收结束事件 payload", "fail_reason", toInt64(t, p, "fail_reason"), int64(model.ReasonStorage))
	wantField(t, "回收结束事件 payload", "errno", toInt64(t, p, "errno"), int64(12))
	if got, _ := p["worker_id"].(string); got != retentionWorker {
		t.Fatalf("事件 worker_id 应为 %q，实际 %q", retentionWorker, got)
	}
	// 事件只带主键与计数：归因文本与对象坐标一律不进 payload（下游按主键回查本服务）。
	for _, key := range []string{"err_msg", "reason", "object_key", "bucket", "source_ref"} {
		if _, ok := p[key]; ok {
			t.Fatalf("payload 不应带 %q：%v", key, p)
		}
	}
	wantField(t, "回收失败上报", "副作用集合", firedWrites(db, before),
		"DB.TransactCtx,ReplayRefs.MarkRetentionStateTx,RetentionTasks.UpdateStateTx,Outbox.Insert")
	wantNoLeak(t, db, "回收结束事件")
}

// 业务写与 Outbox 同生共死：事件写失败时终态边必须一起回滚（引用标记也在同一事务里）。
func TestReportRetentionResultOutboxFailureRollsBackTerminalEdge(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ref := reclaimableReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.RetentionState = model.RefRetentionStatePending
	})
	row := retentionForRef(db, model.RetentionStateRunning, ref, nil)
	snapshot := *row
	refSnapshot := *mustRef(t, db, ref.Id)
	db.failOn("Outbox.Insert", errOutboxDown)

	in := reportRetentionReq(row.RetentionId, row.Version, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
	in.Scanned, in.Deleted = 5, 5
	info, err := reportRetention(context.Background(), svcCtx, in)
	if err == nil || !strings.Contains(err.Error(), errOutboxDown.Error()) {
		t.Fatalf("Outbox 故障必须原样上抛：%v", err)
	}
	if !isNilPtr(info) {
		t.Fatalf("Outbox 故障不得带回响应体：%+v", info)
	}
	assertRetentionUnchanged(t, db, row.RetentionId, snapshot, "Outbox 故障回滚")
	assertRefUnchanged(t, db, ref.Id, refSnapshot, "Outbox 故障回滚：引用不得先变成 Reclaimed")
	// 计数不参与回滚的是「发生过什么」：UPDATE 与标记都真发过，但库内一切回到事务前。
	wantCalls(t, db, "RetentionTasks.UpdateStateTx", 0, 1, "Outbox 故障回滚：UPDATE 发过")
	wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 1, "Outbox 故障回滚：标记发过")
	wantEvents(t, db, nil, "Outbox 故障回滚")
	if len(db.outbox) != 0 {
		t.Fatalf("Outbox 故障后不得留下事件行，实际 %d 条", len(db.outbox))
	}
	wantNoLeak(t, db, "Outbox 故障回滚")
}

// 条件 UPDATE 命中 0 行绝不能当成功：回读一次，区分「行不存在」「版本被抢先」「前置态不允许」。
// 交错用 db.onHit 在写入瞬间改库（回滚会撤销钩子造出来的态，见 fakes_test.go 头注释），
// 因此断言的是归因结论，而不是交错后的最终库态。
func TestReportRetentionResultClassifiesZeroRow(t *testing.T) {
	cases := []struct {
		name    string
		ev      int64
		hook    func(db *store, id int64)
		want    error
		wantMsg string
	}{
		{"版本被抢先 → 版本冲突", 1,
			func(db *store, id int64) { mustRetention(t, db, id).Version++ }, model.ErrVersionConflict, ""},
		{"已被并发推到终态且未声明版本 → 终态", 0,
			func(db *store, id int64) {
				r := mustRetention(t, db, id)
				r.State, r.Version = model.RetentionStateSucceeded, r.Version+1
			}, model.ErrTerminalState, ""},
		{"行消失 → 不存在", 0,
			func(db *store, id int64) { delete(db.retentions, id) }, model.ErrRetentionTaskNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			row := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
				r.Scanned, r.Deleted = 2, 1
			})
			db.onHit("RetentionTasks.UpdateStateTx", func() { tc.hook(db, row.RetentionId) })
			readsBefore := db.count("RetentionTasks.FindOne")

			in := reportRetentionReq(row.RetentionId, tc.ev, rpc.RetentionState_RETENTION_STATE_SUCCEEDED)
			in.Scanned, in.Deleted = 5, 3
			info, err := reportRetention(context.Background(), svcCtx, in)
			wantFail(t, info, err, tc.want, tc.name)
			wantCalls(t, db, "RetentionTasks.UpdateStateTx", 0, 1, tc.name+"：CAS 真的发过")
			wantCalls(t, db, "RetentionTasks.FindOne", readsBefore, 2, tc.name+"：预读 + 0 行归因各一次")
			// 0 行之后不得继续推进引用标记，也不得留下事件。
			wantCalls(t, db, "ReplayRefs.MarkRetentionStateTx", 0, 0, tc.name)
			wantEvents(t, db, nil, tc.name+"：整个事务回滚")
		})
	}
}

// 前置读失败与「查无此行」都是失败关闭：一个坏回执不能被读成「无事发生的成功」。
func TestReportRetentionResultFailsClosedOnMissingRowAndReadErrors(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedRetention(db, model.RetentionStatePending, nil)

	in := reportRetentionReq(retentionMissing, 0, rpc.RetentionState_RETENTION_STATE_RUNNING)
	info, err := reportRetention(context.Background(), svcCtx, in)
	wantFail(t, info, err, model.ErrRetentionTaskNotFound, "不存在的回收任务")
	wantCalls(t, db, "RetentionTasks.FindOne", 0, 1, "不存在的回收任务应真的查过一次")

	db.failOn("RetentionTasks.FindOne", errModelDown)
	in = reportRetentionReq(1, 0, rpc.RetentionState_RETENTION_STATE_RUNNING)
	before := snapshotWrites(db)
	info, err = reportRetention(context.Background(), svcCtx, in)
	wantModelDown(t, err)
	if !isNilPtr(info) {
		t.Fatalf("读故障不得带回响应体：%+v", info)
	}
	wantNoWrites(t, db, before, "读故障")
	wantEvents(t, db, nil, "读故障")
}

// ---------------------------------------------------------------- GetRetentionTask / ListRetentionTasks

// 逐列回显：scanned/deleted/skipped 是 Worker 覆盖写的结果证据，
// 与 failed 行的归因三列一起原样答出来（包括脏值），不做二次推断。
// 本方法刻意不走缓存：缓存会把「已登记未执行」和「已执行未清理」答成同一个值。
func TestGetRetentionTaskReturnsEveryCommittedColumnWithoutCache(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)
	row := seedRetention(db, model.RetentionStateFailed, func(r *model.LiveRetentionTask) {
		r.TargetKind, r.TargetId = model.RetentionTargetSegment, 3301
		r.RoomId, r.ExpireBefore, r.BatchLimit = testRoomOther, 1234567, 25
		r.Purge, r.Scanned, r.Deleted, r.Skipped = 1, 30, 0, 30
		r.Reason, r.Operator, r.Version = "房间注销后清理", "ops-bob", 9
		r.FailReason, r.Errno, r.ErrMsg = model.ReasonStorage, 7, "对象存储拒绝"
		r.RequestId, r.TraceId = "req-ret-get", "trace-ret-get"
	})
	// 故意让 deleted>0 与 state=FAILED 并存：这是 Worker 的部分失败证据，读侧不得替它美化。
	l := NewGetRetentionTaskLogic(context.Background(), svcCtx)
	for i := 0; i < 3; i++ {
		info, err := l.GetRetentionTask(getRetentionReq(row.RetentionId))
		info = wantOK(t, info, err, "查询回收任务")
		wantRetentionEchoesRow(t, "查询回收任务", info, row)
		wantField(t, "查询回收任务", "fail_reason 原样回显", int32(info.GetFailReason()), model.ReasonStorage)
		wantField(t, "查询回收任务", "skipped 原样回显", info.GetSkipped(), int32(30))
	}
	// 每次读都必须真的回源主表：无缓存时不允许「读到上一次副本」。
	wantCalls(t, db, "RetentionTasks.FindOne", 0, 3, "回收详情每次必回源")
	wantNoWrites(t, db, before, "查询回收任务")
	wantEvents(t, db, nil, "查询回收任务")
	wantNoLeak(t, db, "查询回收任务")
}

// 非正数主键直接判不存在且不查库；查无此行必须是错误而不是零值 Info
// （Worker 会把 version=0 当 expected_version，此后每次上报都永远撞版本冲突）。
func TestGetRetentionTaskRejectsBadIDAndMissingRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedRetention(db, model.RetentionStatePending, nil)
	before := snapshotWrites(db)

	for _, id := range []int64{0, -1} {
		info, err := NewGetRetentionTaskLogic(context.Background(), svcCtx).
			GetRetentionTask(getRetentionReq(id))
		wantFail(t, info, err, model.ErrRetentionTaskNotFound, "非正数 retention_id")
	}
	wantCalls(t, db, "RetentionTasks.FindOne", 0, 0, "非正数 retention_id 不得查库")

	info, err := NewGetRetentionTaskLogic(context.Background(), svcCtx).
		GetRetentionTask(getRetentionReq(retentionMissing))
	wantFail(t, info, err, model.ErrRetentionTaskNotFound, "不存在的回收任务")
	wantCalls(t, db, "RetentionTasks.FindOne", 0, 1, "不存在的回收任务应真的查过一次")
	wantNoWrites(t, db, before, "回收详情")
	wantEvents(t, db, nil, "回收详情")
}

// ---------------------------------------------------------------- ListRetentionTasks

// 归一后的过滤值与分页参数只能靠 db.lastRetentionList 钉住：返回值里看不出 model 收到了什么。
// 特别地，room_id<=0 是「不过滤」而不是「只看全局任务」—— 0 同时是全局任务的合法列值，
// 本入口因此无法表达「只查全局任务」（契约缺口，见 README）。
func TestListRetentionTasksPassesNormalizedFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedRetention(db, model.RetentionStatePending, func(r *model.LiveRetentionTask) {
		r.TargetKind = model.RetentionTargetSegment
	})
	b := seedRetention(db, model.RetentionStateSucceeded, func(r *model.LiveRetentionTask) {
		r.TargetKind, r.RoomId = model.RetentionTargetStreamOutput, testRoomOther
	})
	c := seedRetention(db, model.RetentionStateRunning, func(r *model.LiveRetentionTask) {
		r.TargetKind = model.RetentionTargetReplay
		r.RoomId, r.TargetId, r.ExpireBefore = 0, 0, nowTS()-retentionWindow
	})
	before := snapshotWrites(db)
	svcL := NewListRetentionTasksLogic(context.Background(), svcCtx)

	in := listRetentionsReq(1, 10)
	in.RoomId = testRoomID
	reply, err := svcL.ListRetentionTasks(in)
	reply = wantOK(t, reply, err, "按房间过滤")
	// 只有 a 属于本房间：b 在别的房间，c 是 room_id=0 的全局扫描任务（它不是「本房间」的任务）。
	wantField(t, "按房间过滤", "排序为 retention_id DESC",
		retentionOrder(reply.GetTasks()), joinInt64([]int64{a.RetentionId}))
	wantField(t, "按房间过滤", "total 是过滤后的总数", reply.GetPage().GetTotal(), int32(1))
	ff := db.lastRetentionList
	if ff == nil {
		t.Fatal("model 未被调用，过滤条件无从校验")
	}
	wantField(t, "按房间过滤", "room_id", ff.RoomId, testRoomID)
	wantField(t, "按房间过滤", "未给的目标类型不过滤", ff.TargetKind, int32(0))
	wantField(t, "按房间过滤", "未给的状态不过滤", ff.State, int32(0))
	wantField(t, "按房间过滤", "pn", ff.Pn, int32(1))
	wantField(t, "按房间过滤", "ps", ff.Ps, int32(10))
	wantField(t, "按房间过滤", "max_ps 取配置", ff.MaxPageSize, testConf().MaxListPageSize)

	// 换一个房间必须换一批行：证明 room_id 真的进了条件而不是被忽略。
	inRoom := listRetentionsReq(1, 10)
	inRoom.RoomId = testRoomOther
	replyRoom, err := svcL.ListRetentionTasks(inRoom)
	replyRoom = wantOK(t, replyRoom, err, "换房间过滤")
	wantField(t, "换房间过滤", "命中行", retentionOrder(replyRoom.GetTasks()),
		joinInt64([]int64{b.RetentionId}))
	wantField(t, "换房间过滤", "room_id 传下去", db.lastRetentionList.RoomId, testRoomOther)

	in2 := listRetentionsReq(1, 10)
	in2.State = rpc.RetentionState_RETENTION_STATE_RUNNING
	reply2, err := svcL.ListRetentionTasks(in2)
	reply2 = wantOK(t, reply2, err, "按状态过滤")
	wantField(t, "按状态过滤", "行数", int32(len(reply2.GetTasks())), int32(1))
	wantField(t, "按状态过滤", "命中行", reply2.GetTasks()[0].GetRetentionId(), c.RetentionId)
	wantField(t, "按状态过滤", "传给 model 的 state", db.lastRetentionList.State, model.RetentionStateRunning)

	in3 := listRetentionsReq(1, 10)
	in3.TargetKind = rpc.RetentionTargetKind_RETENTION_TARGET_KIND_STREAM_OUTPUT
	reply3, err := svcL.ListRetentionTasks(in3)
	reply3 = wantOK(t, reply3, err, "按目标类型过滤")
	wantField(t, "按目标类型过滤", "命中行", reply3.GetTasks()[0].GetRetentionId(), b.RetentionId)
	wantField(t, "按目标类型过滤", "传给 model 的 target_kind",
		db.lastRetentionList.TargetKind, model.RetentionTargetStreamOutput)

	// 不带条件仍是合法的全表读（运营排障）：全局任务（room_id=0）必须出现在结果里，
	// 这同时钉住「room_id<=0 不过滤」这条口径 —— 它无法用来只看全局任务。
	reply4, err := svcL.ListRetentionTasks(listRetentionsReq(1, 10))
	reply4 = wantOK(t, reply4, err, "不带过滤条件")
	if _, ok := findRetentionRow(reply4.GetTasks(), c.RetentionId); !ok {
		t.Fatalf("全局扫描任务（room_id=0）必须出现： %+v", retentionOrder(reply4.GetTasks()))
	}
	wantField(t, "不带过滤条件", "total", reply4.GetPage().GetTotal(), int32(3))

	// PENDING 作为过滤条件必须可用：运营看不到「待执行」队列就等于没有队列。
	in5 := listRetentionsReq(1, 10)
	in5.State = rpc.RetentionState_RETENTION_STATE_PENDING
	if _, err := svcL.ListRetentionTasks(in5); err != nil {
		t.Fatalf("PENDING 是合法过滤值： %v", err)
	}
	wantField(t, "PENDING 过滤", "传给 model 的 state", db.lastRetentionList.State, model.RetentionStatePending)

	wantNoWrites(t, db, before, "回收列表只读")
	wantEvents(t, db, nil, "回收列表只读")
}

// 越界的枚举过滤值必须报错且不查库：退化成「恒空结果集」会把调用方的版本错误
// 伪装成「这个房间没有回收任务」。0（UNSPECIFIED）则必须是「不过滤」并原样传 0。
func TestListRetentionTasksRejectsOutOfRangeEnumFilters(t *testing.T) {
	cases := []struct {
		name  string
		kind  rpc.RetentionTargetKind
		state rpc.RetentionState
	}{
		{"target_kind=4", rpc.RetentionTargetKind(4), 0},
		{"target_kind 负数", rpc.RetentionTargetKind(-1), 0},
		{"state=6", 0, rpc.RetentionState(6)},
		{"state 负数", 0, rpc.RetentionState(-1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			seedRetention(db, model.RetentionStatePending, nil)
			before := snapshotWrites(db)

			in := listRetentionsReq(1, 20)
			in.TargetKind, in.State = tc.kind, tc.state
			reply, err := NewListRetentionTasksLogic(context.Background(), svcCtx).ListRetentionTasks(in)
			wantFail(t, reply, err, model.ErrInvalidTransition, tc.name)
			wantCalls(t, db, "RetentionTasks.List", 0, 0, "越界枚举不得查库")
			wantNoWrites(t, db, before, tc.name)
		})
	}

	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedRetention(db, model.RetentionStatePending, nil)
	in := listRetentionsReq(1, 20)
	in.TargetKind = rpc.RetentionTargetKind_RETENTION_TARGET_KIND_UNSPECIFIED
	in.State = rpc.RetentionState_RETENTION_STATE_UNSPECIFIED
	reply, err := NewListRetentionTasksLogic(context.Background(), svcCtx).ListRetentionTasks(in)
	reply = wantOK(t, reply, err, "UNSPECIFIED 表示不过滤")
	wantField(t, "UNSPECIFIED 表示不过滤", "行数", int32(len(reply.GetTasks())), int32(1))
	wantField(t, "UNSPECIFIED 表示不过滤", "命中行", reply.GetTasks()[0].GetRetentionId(), row.RetentionId)
	wantField(t, "UNSPECIFIED 表示不过滤", "传给 model 的 target_kind", db.lastRetentionList.TargetKind, int32(0))
	wantField(t, "UNSPECIFIED 表示不过滤", "传给 model 的 state", db.lastRetentionList.State, int32(0))
}

// 分页口径与其余列表方法一致：ps 越界夹取、pn 深翻页拒绝、pn<1 视为 1、空结果不是错误。
func TestListRetentionTasksPagingAndClamping(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	ids := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		ids = append(ids, seedRetention(db, model.RetentionStatePending, nil).RetentionId)
	}
	// 前置数据自校验：三行主键互不相同，否则分页与去重判定都会失真。
	if ids[0] == ids[1] || ids[1] == ids[2] || ids[0] == ids[2] {
		t.Fatalf("种子 retention_id 重复：%v", ids)
	}
	svcL := NewListRetentionTasksLogic(context.Background(), svcCtx)

	first, err := svcL.ListRetentionTasks(listRetentionsReq(1, 2))
	first = wantOK(t, first, err, "第一页")
	wantField(t, "第一页", "行数", int32(len(first.GetTasks())), int32(2))
	wantField(t, "第一页", "total 是全量数而非本页数", first.GetPage().GetTotal(), int32(3))
	wantField(t, "第一页", "pn 原样透传", db.lastRetentionList.Pn, int32(1))

	second, err := svcL.ListRetentionTasks(listRetentionsReq(2, 2))
	second = wantOK(t, second, err, "第二页")
	wantField(t, "第二页", "行数", int32(len(second.GetTasks())), int32(1))
	if _, ok := findRetentionRow(second.GetTasks(), first.GetTasks()[0].GetRetentionId()); ok {
		t.Fatal("第二页与第一页首行重叠：OFFSET 没生效")
	}

	big, err := svcL.ListRetentionTasks(listRetentionsReq(1, 999))
	big = wantOK(t, big, err, "ps 越界")
	// 越界不报错也不夹到上限：回落到默认页大小 20（越界值本身不可信，取默认比取上限更保守）。
	wantField(t, "ps 越界", "回落到默认 20", db.lastRetentionList.Ps, int32(20))
	zeroPn, err := svcL.ListRetentionTasks(listRetentionsReq(0, 0))
	zeroPn = wantOK(t, zeroPn, err, "pn/ps 非正数")
	wantField(t, "pn/ps 非正数", "pn 视为 1", db.lastRetentionList.Pn, int32(1))
	wantField(t, "pn/ps 非正数", "ps 取默认 20", db.lastRetentionList.Ps, int32(20))

	// 深翻页：OFFSET 的代价是线性扫描后丢弃，超过保护窗口直接拒绝（而不是慢到超时）。
	_, err = svcL.ListRetentionTasks(listRetentionsReq(202, 50))
	wantFail(t, (*rpc.ListRetentionTasksReply)(nil), err, model.ErrInvalidPage, "深翻页")
	lastCalls := db.count("RetentionTasks.List")
	_, err = svcL.ListRetentionTasks(listRetentionsReq(5000, 50))
	wantFail(t, (*rpc.ListRetentionTasksReply)(nil), err, model.ErrInvalidPage, "深翻页 2")
	wantCalls(t, db, "RetentionTasks.List", lastCalls, 0, "深翻页不得查库")

	// 空结果集是正常业务态，不是错误（过滤条件收窄到 0 行时）。
	in := listRetentionsReq(1, 20)
	in.RoomId = retentionMissing
	empty, err := svcL.ListRetentionTasks(in)
	empty = wantOK(t, empty, err, "空结果")
	wantField(t, "空结果", "行数", int32(len(empty.GetTasks())), int32(0))
	wantField(t, "空结果", "total", empty.GetPage().GetTotal(), int32(0))

	// 依赖故障 fail closed：读不动不等于「没有回收任务」。
	db.failOn("RetentionTasks.List", errModelDown)
	_, err = svcL.ListRetentionTasks(listRetentionsReq(1, 20))
	wantModelDown(t, err)
}
