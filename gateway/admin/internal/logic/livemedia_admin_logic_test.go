package logic

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"google.golang.org/grpc"
)

// 本文件只验证网关侧面向 live-media 运营面的口径，与 recommend / cron / live 三服务的测试同一套边界：
//
//  1. rpc→types **逐字段不丢**：version/heartbeat_at/timeout_at/attempt/max_attempts 是「能不能停、
//     还能不能重试」的依据，last_seq/gap_count/segment_count/recorded_duration_ms 是「回放有没有洞」
//     的证据，scanned/deleted/skipped 是「有没有误删」的凭证，review_state/published_at 是 video 侧
//     投影 —— 裁掉任意一条都等于让后台靠猜；
//  2. 入参**原样交给下游**：幂等键、对象存储引用、档位参数快照、时间/序号区间都不改写、不补默认值、
//     不去重（0 在本域普遍是合法哨兵）；
//  3. 写入口门槛：会话身份（缺失即 fail-closed）、operator 只能由会话渲染成 admin:<admin_id>、
//     request_id 非空（ApplyReplayContentState 无该位，见 admin.api 契约缺口）、
//     枚举目标位不得是 UNSPECIFIED、引用必须成对、区间不得倒置、寻址主体必须齐；
//  4. 未配置下游（LiveMediaRPC 为空 → 客户端 nil）、请求体缺失、入参形态非法时**不得**产生下游调用，
//     也不得折叠成空结果冒充成功；
//  5. 空列表投影成 []（非 nil），与「下游没接」严格区分。
//
// 刻意不断言下游业务结论（AGENTS.md §5/§8）：任务当前状态能不能停/重试/取消、attempt 是否用尽、
// 切片区间是否完整、档位是否仍被引用、回收对象能否删、稿件是否可发布——全部由 services/live-media
// 与 video/asset 判定。该服务的 logic 由另一个 agent 并行落地，这里一律用假客户端返回预置回包，
// 只固定「网关不复算、原样转发、原样回传」。
//
// 打桩方式与 recommend/cron 一致：内嵌生成的 client 接口 + 只覆盖本轮用到的 23 个方法。
// 五个 Report* 刻意不实现：一旦被网关调用会命中 nil 接口而 panic，测试直接炸——
// 这就是「Worker 回报口不得从后台开出」这条边界在测试里的兜底闸。

var errLiveMediaFakeDownstream = errors.New("live-media downstream unavailable")

// --- 假客户端 ---

type liveMediaAdminFake struct {
	livemediarpc.LiveMediaClient

	err   error
	calls int

	// 预置回包（同一类型在读写口之间共用，便于逐字段投影断言只写一份）。
	transcodeInfo *livemediarpc.LiveTranscodeTaskInfo
	outputInfo    *livemediarpc.StreamOutputInfo
	recordInfo    *livemediarpc.LiveRecordTaskInfo
	segmentInfo   *livemediarpc.RecordSegmentInfo
	replayInfo    *livemediarpc.LiveReplayTaskInfo
	refInfo       *livemediarpc.ReplayAssetRefInfo
	retentionInfo *livemediarpc.LiveRetentionTaskInfo

	listTranscodeReply *livemediarpc.ListLiveTranscodeTasksReply
	listOutputReply    *livemediarpc.ListStreamOutputsReply
	listRecordReply    *livemediarpc.ListLiveRecordTasksReply
	listSegmentReply   *livemediarpc.ListRecordSegmentsReply
	listReplayReply    *livemediarpc.ListReplayTasksReply
	listRefReply       *livemediarpc.ListReplayAssetRefsReply
	listRetentionReply *livemediarpc.ListRetentionTasksReply

	// 入参捕获。
	startTranscodeReq  *livemediarpc.StartLiveTranscodeReq
	stopTranscodeReq   *livemediarpc.StopLiveTranscodeReq
	retryTranscodeReq  *livemediarpc.RetryLiveTranscodeReq
	cancelTranscodeReq *livemediarpc.CancelLiveTranscodeReq
	getTranscodeReq    *livemediarpc.LiveTranscodeTaskReq
	listTranscodeReq   *livemediarpc.ListLiveTranscodeTasksReq

	upsertOutputReq *livemediarpc.UpsertStreamOutputReq
	offlineOutpReq  *livemediarpc.OfflineStreamOutputReq
	listOutputReq   *livemediarpc.ListStreamOutputsReq

	startRecordReq  *livemediarpc.StartLiveRecordReq
	stopRecordReq   *livemediarpc.StopLiveRecordReq
	getRecordReq    *livemediarpc.LiveRecordTaskReq
	listRecordReq   *livemediarpc.ListLiveRecordTasksReq
	listSegmentReq  *livemediarpc.ListRecordSegmentsReq
	submitReplayReq *livemediarpc.SubmitReplayTaskReq
	bindAssetReq    *livemediarpc.BindReplayAssetReq
	applyStateReq   *livemediarpc.ApplyReplayContentStateReq
	getReplayReq    *livemediarpc.ReplayTaskReq
	listReplayReq   *livemediarpc.ListReplayTasksReq
	listRefReq      *livemediarpc.ListReplayAssetRefsReq

	submitRetentionReq *livemediarpc.SubmitRetentionTaskReq
	getRetentionReq    *livemediarpc.RetentionTaskReq
	listRetentionReq   *livemediarpc.ListRetentionTasksReq
}

func (f *liveMediaAdminFake) StartLiveTranscode(_ context.Context, in *livemediarpc.StartLiveTranscodeReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveTranscodeTaskInfo, error) {
	f.calls++
	f.startTranscodeReq = in
	return f.transcodeInfo, f.err
}

func (f *liveMediaAdminFake) StopLiveTranscode(_ context.Context, in *livemediarpc.StopLiveTranscodeReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveTranscodeTaskInfo, error) {
	f.calls++
	f.stopTranscodeReq = in
	return f.transcodeInfo, f.err
}

func (f *liveMediaAdminFake) RetryLiveTranscode(_ context.Context, in *livemediarpc.RetryLiveTranscodeReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveTranscodeTaskInfo, error) {
	f.calls++
	f.retryTranscodeReq = in
	return f.transcodeInfo, f.err
}

func (f *liveMediaAdminFake) CancelLiveTranscode(_ context.Context, in *livemediarpc.CancelLiveTranscodeReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveTranscodeTaskInfo, error) {
	f.calls++
	f.cancelTranscodeReq = in
	return f.transcodeInfo, f.err
}

func (f *liveMediaAdminFake) GetLiveTranscodeTask(_ context.Context, in *livemediarpc.LiveTranscodeTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveTranscodeTaskInfo, error) {
	f.calls++
	f.getTranscodeReq = in
	return f.transcodeInfo, f.err
}

func (f *liveMediaAdminFake) ListLiveTranscodeTasks(_ context.Context, in *livemediarpc.ListLiveTranscodeTasksReq,
	_ ...grpc.CallOption) (*livemediarpc.ListLiveTranscodeTasksReply, error) {
	f.calls++
	f.listTranscodeReq = in
	return f.listTranscodeReply, f.err
}

func (f *liveMediaAdminFake) UpsertStreamOutput(_ context.Context, in *livemediarpc.UpsertStreamOutputReq,
	_ ...grpc.CallOption) (*livemediarpc.StreamOutputInfo, error) {
	f.calls++
	f.upsertOutputReq = in
	return f.outputInfo, f.err
}

func (f *liveMediaAdminFake) OfflineStreamOutput(_ context.Context, in *livemediarpc.OfflineStreamOutputReq,
	_ ...grpc.CallOption) (*livemediarpc.StreamOutputInfo, error) {
	f.calls++
	f.offlineOutpReq = in
	return f.outputInfo, f.err
}

func (f *liveMediaAdminFake) ListStreamOutputs(_ context.Context, in *livemediarpc.ListStreamOutputsReq,
	_ ...grpc.CallOption) (*livemediarpc.ListStreamOutputsReply, error) {
	f.calls++
	f.listOutputReq = in
	return f.listOutputReply, f.err
}

func (f *liveMediaAdminFake) StartLiveRecord(_ context.Context, in *livemediarpc.StartLiveRecordReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveRecordTaskInfo, error) {
	f.calls++
	f.startRecordReq = in
	return f.recordInfo, f.err
}

func (f *liveMediaAdminFake) StopLiveRecord(_ context.Context, in *livemediarpc.StopLiveRecordReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveRecordTaskInfo, error) {
	f.calls++
	f.stopRecordReq = in
	return f.recordInfo, f.err
}

func (f *liveMediaAdminFake) GetLiveRecordTask(_ context.Context, in *livemediarpc.LiveRecordTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveRecordTaskInfo, error) {
	f.calls++
	f.getRecordReq = in
	return f.recordInfo, f.err
}

func (f *liveMediaAdminFake) ListLiveRecordTasks(_ context.Context, in *livemediarpc.ListLiveRecordTasksReq,
	_ ...grpc.CallOption) (*livemediarpc.ListLiveRecordTasksReply, error) {
	f.calls++
	f.listRecordReq = in
	return f.listRecordReply, f.err
}

func (f *liveMediaAdminFake) ListRecordSegments(_ context.Context, in *livemediarpc.ListRecordSegmentsReq,
	_ ...grpc.CallOption) (*livemediarpc.ListRecordSegmentsReply, error) {
	f.calls++
	f.listSegmentReq = in
	return f.listSegmentReply, f.err
}

func (f *liveMediaAdminFake) SubmitReplayTask(_ context.Context, in *livemediarpc.SubmitReplayTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveReplayTaskInfo, error) {
	f.calls++
	f.submitReplayReq = in
	return f.replayInfo, f.err
}

func (f *liveMediaAdminFake) GetReplayTask(_ context.Context, in *livemediarpc.ReplayTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveReplayTaskInfo, error) {
	f.calls++
	f.getReplayReq = in
	return f.replayInfo, f.err
}

func (f *liveMediaAdminFake) ListReplayTasks(_ context.Context, in *livemediarpc.ListReplayTasksReq,
	_ ...grpc.CallOption) (*livemediarpc.ListReplayTasksReply, error) {
	f.calls++
	f.listReplayReq = in
	return f.listReplayReply, f.err
}

func (f *liveMediaAdminFake) BindReplayAsset(_ context.Context, in *livemediarpc.BindReplayAssetReq,
	_ ...grpc.CallOption) (*livemediarpc.ReplayAssetRefInfo, error) {
	f.calls++
	f.bindAssetReq = in
	return f.refInfo, f.err
}

func (f *liveMediaAdminFake) ApplyReplayContentState(_ context.Context, in *livemediarpc.ApplyReplayContentStateReq,
	_ ...grpc.CallOption) (*livemediarpc.ReplayAssetRefInfo, error) {
	f.calls++
	f.applyStateReq = in
	return f.refInfo, f.err
}

func (f *liveMediaAdminFake) ListReplayAssetRefs(_ context.Context, in *livemediarpc.ListReplayAssetRefsReq,
	_ ...grpc.CallOption) (*livemediarpc.ListReplayAssetRefsReply, error) {
	f.calls++
	f.listRefReq = in
	return f.listRefReply, f.err
}

func (f *liveMediaAdminFake) SubmitRetentionTask(_ context.Context, in *livemediarpc.SubmitRetentionTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveRetentionTaskInfo, error) {
	f.calls++
	f.submitRetentionReq = in
	return f.retentionInfo, f.err
}

func (f *liveMediaAdminFake) GetRetentionTask(_ context.Context, in *livemediarpc.RetentionTaskReq,
	_ ...grpc.CallOption) (*livemediarpc.LiveRetentionTaskInfo, error) {
	f.calls++
	f.getRetentionReq = in
	return f.retentionInfo, f.err
}

func (f *liveMediaAdminFake) ListRetentionTasks(_ context.Context, in *livemediarpc.ListRetentionTasksReq,
	_ ...grpc.CallOption) (*livemediarpc.ListRetentionTasksReply, error) {
	f.calls++
	f.listRetentionReq = in
	return f.listRetentionReply, f.err
}

// --- 测试脚手架 ---

// liveMediaSvc 只填本轮用到的客户端，其余字段留零值：logic 不该依赖别的下游。
func liveMediaSvc(c livemediarpc.LiveMediaClient) *svc.ServiceContext {
	return &svc.ServiceContext{LiveMedia: c}
}

// liveMediaSessionCtx 模拟 AdminPermission 中间件已解析出会话身份的请求上下文。
func liveMediaSessionCtx() context.Context {
	return middleware.WithAdmin(context.Background(), middleware.AdminIdentity{
		AdminID: 77,
		Roles:   []string{"live_media_ops"},
	})
}

// liveMediaOperatorFixture 是会话身份渲染出的 operator：77 取自上面的会话，表单无法声明。
const liveMediaOperatorFixture = "admin:77"

// liveMediaAssertEnvelope 固定后台面四字段信封：成功恒 code=0/message=ok，
// 且排障读数一律 ttl=0（客户端不该缓存「此刻的下游事实」）。
func liveMediaAssertEnvelope(t *testing.T, code int, message string, ttl int64) {
	t.Helper()
	if code != 0 || message != "ok" || ttl != 0 {
		t.Fatalf("响应信封不符合后台口径：code=%d message=%q ttl=%d", code, message, ttl)
	}
}

// liveMediaMust 把路由表里的 any 入参还原成指针类型；传 nil 表示「请求体缺失」。
func liveMediaMust[T any](t *testing.T, body any) *T {
	t.Helper()
	p, ok := body.(*T)
	if !ok {
		t.Fatalf("入参类型 = %T, want *%T", body, *new(T))
	}
	return p
}

// liveMediaErrString 断言错误信息里含某个片段（片段取门槛消息里的字段名，便于定位表单）。
func liveMediaErrString(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误含 %q，实际为 nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("错误 = %q, want contains %q", err.Error(), want)
	}
}

// --- 合法入参 fixture（每条路由一份） ---

func liveMediaTranscodeGetBody() *types.ParamLiveMediaTranscodeGet {
	return &types.ParamLiveMediaTranscodeGet{TaskId: 501}
}

func liveMediaTranscodeListBody() *types.ParamLiveMediaTranscodeList {
	return &types.ParamLiveMediaTranscodeList{RoomId: 1201, SessionId: 33, State: 2, TemplateId: 7, Pn: 2, Ps: 20}
}

func liveMediaOutputListBody() *types.ParamLiveMediaOutputList {
	return &types.ParamLiveMediaOutputList{RoomId: 1201, SessionId: 33, IncludeOffline: true, Pn: 1, Ps: 20}
}

func liveMediaRecordGetBody() *types.ParamLiveMediaRecordGet {
	return &types.ParamLiveMediaRecordGet{RecordId: 601}
}

func liveMediaRecordListBody() *types.ParamLiveMediaRecordList {
	return &types.ParamLiveMediaRecordList{RoomId: 1201, SessionId: 33, State: 2, Pn: 1, Ps: 20}
}

func liveMediaSegmentListBody() *types.ParamLiveMediaRecordSegmentList {
	return &types.ParamLiveMediaRecordSegmentList{RecordId: 601, State: 4, AfterSeq: 10, Limit: 200}
}

func liveMediaReplayGetBody() *types.ParamLiveMediaReplayGet {
	return &types.ParamLiveMediaReplayGet{ReplayId: 701}
}

func liveMediaReplayListBody() *types.ParamLiveMediaReplayList {
	return &types.ParamLiveMediaReplayList{RoomId: 1201, SessionId: 33, State: 6, Pn: 1, Ps: 20}
}

func liveMediaReplayAssetListBody() *types.ParamLiveMediaReplayAssetList {
	return &types.ParamLiveMediaReplayAssetList{RoomId: 1201, SessionId: 33, ReviewState: 3, AnchorMid: 99, Pn: 1, Ps: 20}
}

func liveMediaRetentionGetBody() *types.ParamLiveMediaRetentionGet {
	return &types.ParamLiveMediaRetentionGet{RetentionId: 801}
}

func liveMediaRetentionListBody() *types.ParamLiveMediaRetentionList {
	return &types.ParamLiveMediaRetentionList{TargetKind: 1, State: 3, RoomId: 1201, Pn: 1, Ps: 20}
}

func liveMediaTranscodeStartBody() *types.ParamLiveMediaTranscodeStart {
	return &types.ParamLiveMediaTranscodeStart{
		RoomId: 1201, SessionId: 33, TemplateId: 7, BitrateLevel: 3, Protocol: 1,
		SourceRef: "rtmp://ingest.example.com/live/1201", AnchorMid: 99,
		MaxAttempts: 3, TimeoutSeconds: 120, RequestId: "req-start-1", TraceId: "trace-start",
	}
}

func liveMediaTranscodeStopBody() *types.ParamLiveMediaTranscodeStop {
	return &types.ParamLiveMediaTranscodeStop{
		TaskId: 501, ExpectedVersion: 9, Reason: 7, RequestId: "req-stop-1", TraceId: "trace-stop",
	}
}

func liveMediaTranscodeRetryBody() *types.ParamLiveMediaTranscodeRetry {
	return &types.ParamLiveMediaTranscodeRetry{
		TaskId: 501, ExpectedVersion: 0, Reason: "上游断流已修复，重开一路 720p",
		RequestId: "req-retry-1", TraceId: "trace-retry",
	}
}

func liveMediaTranscodeCancelBody() *types.ParamLiveMediaTranscodeCancel {
	return &types.ParamLiveMediaTranscodeCancel{
		TaskId: 501, ExpectedVersion: 2, Reason: 7, RequestId: "req-cancel-1", TraceId: "trace-cancel",
	}
}

func liveMediaOutputUpsertBody() *types.ParamLiveMediaOutputUpsert {
	return &types.ParamLiveMediaOutputUpsert{
		RoomId: 1201, SessionId: 33, TaskId: 501, BitrateLevel: 3, Protocol: 1,
		Bucket: "live-hls", ObjectKey: "1201/33/hd.m3u8", CdnDomain: "cdn.example.com",
		Width: 1280, Height: 720, BitrateKbps: 2000, Fps: 30,
		OnlineExpireAt: 1893456000, RequestId: "req-upsert-1", TraceId: "trace-upsert",
	}
}

func liveMediaOutputOfflineBody() *types.ParamLiveMediaOutputOffline {
	return &types.ParamLiveMediaOutputOffline{
		OutputId: 900, Reason: 2, RequestId: "req-offline-1", TraceId: "trace-offline",
	}
}

func liveMediaRecordStartBody() *types.ParamLiveMediaRecordStart {
	return &types.ParamLiveMediaRecordStart{
		RoomId: 1201, SessionId: 33, SourceTaskId: 0, StartAt: 100, EndAt: 200,
		SegmentSeconds: 6, TimeoutSeconds: 90,
		OutputBucket: "live-record", OutputPrefix: "1201/33/", RequestId: "req-recstart-1",
		TraceId: "trace-recstart",
	}
}

func liveMediaRecordStopBody() *types.ParamLiveMediaRecordStop {
	return &types.ParamLiveMediaRecordStop{
		RecordId: 601, ExpectedVersion: 4, EndAt: 0, Reason: 7,
		RequestId: "req-recstop-1", TraceId: "trace-recstop",
	}
}

func liveMediaReplaySubmitBody() *types.ParamLiveMediaReplaySubmit {
	return &types.ParamLiveMediaReplaySubmit{
		RoomId: 1201, SessionId: 33, RecordId: 601, FromSeq: 2, ToSeq: 88,
		StartAt: 100, EndAt: 200, AllowGaps: true, AnchorMid: 99,
		Title: "回放：某场直播全场", Description: "由 3 号录制任务的切片拼接",
		RequestId: "req-replaysubmit-1", TraceId: "trace-replaysubmit",
	}
}

func liveMediaReplayBindBody() *types.ParamLiveMediaReplayAssetBind {
	return &types.ParamLiveMediaReplayAssetBind{
		ReplayId: 701, AssetId: 4001, Aid: 0, Bvid: "",
		Bucket: "live-replay", ObjectKey: "replay/701.mp4", DurationMs: 600000,
		RequestId: "req-bind-1", TraceId: "trace-bind",
	}
}

func liveMediaReplayContentStateBody() *types.ParamLiveMediaReplayContentState {
	return &types.ParamLiveMediaReplayContentState{
		ReplayId: 701, AssetId: 0, ReviewState: 3, PublishedAt: 1893456000,
		EventId: "evt-content-published-1", TraceId: "trace-state",
	}
}

func liveMediaRetentionSubmitBody() *types.ParamLiveMediaRetentionSubmit {
	return &types.ParamLiveMediaRetentionSubmit{
		TargetKind: 1, RoomId: 1201, TargetId: 0, ExpireBefore: 1893456000,
		Purge: false, BatchLimit: 100, Reason: "切片已被回放合并吸收，按保留期回收",
		RequestId: "req-retention-1", TraceId: "trace-retention",
	}
}

// --- 下游回包 fixture 与对应的后台投影（逐字段成对，缺一列就 DeepEqual 不等） ---

func liveMediaTranscodeInfoFixture() *livemediarpc.LiveTranscodeTaskInfo {
	return &livemediarpc.LiveTranscodeTaskInfo{
		TaskId: 501, RoomId: 1201, LiveSessionId: 33, TemplateId: 7,
		BitrateLevel: livemediarpc.BitrateLevel_BITRATE_LEVEL_HD,
		Protocol:     livemediarpc.StreamProtocol_STREAM_PROTOCOL_HLS,
		SourceRef:    "rtmp://ingest.example.com/live/1201", AnchorMid: 99,
		State:    livemediarpc.LiveTranscodeState_LIVE_TRANSCODE_STATE_RUNNING,
		Progress: 55, Attempt: 1, MaxAttempts: 3,
		StartedAt: 1700000100, StoppedAt: 0, HeartbeatAt: 1700000200, TimeoutAt: 1700000300,
		Version: 9, Reason: livemediarpc.FailureReason_FAILURE_REASON_MANUAL,
		Errno: 42, ErrMsg: "上游断流，等待 Worker 收尾",
		RequestId: "req-start-1", TraceId: "trace-start", Ctime: 1700000000, Mtime: 1700000250,
	}
}

func liveMediaTranscodeAPIFixture() types.LiveMediaTranscodeTaskInfo {
	return types.LiveMediaTranscodeTaskInfo{
		TaskId: 501, RoomId: 1201, LiveSessionId: 33, TemplateId: 7,
		BitrateLevel: 3, Protocol: 1,
		SourceRef: "rtmp://ingest.example.com/live/1201", AnchorMid: 99,
		State: 2, Progress: 55, Attempt: 1, MaxAttempts: 3,
		StartedAt: 1700000100, StoppedAt: 0, HeartbeatAt: 1700000200, TimeoutAt: 1700000300,
		Version: 9, Reason: 7, Errno: 42, ErrMsg: "上游断流，等待 Worker 收尾",
		RequestId: "req-start-1", TraceId: "trace-start", Ctime: 1700000000, Mtime: 1700000250,
	}
}

func liveMediaOutputInfoFixture() *livemediarpc.StreamOutputInfo {
	return &livemediarpc.StreamOutputInfo{
		OutputId: 900, RoomId: 1201, LiveSessionId: 33, TaskId: 501,
		BitrateLevel: livemediarpc.BitrateLevel_BITRATE_LEVEL_HD,
		Protocol:     livemediarpc.StreamProtocol_STREAM_PROTOCOL_HLS,
		Bucket:       "live-hls", ObjectKey: "1201/33/hd.m3u8", CdnDomain: "cdn.example.com",
		Width: 1280, Height: 720, BitrateKbps: 2000, Fps: 30,
		State: 2, OnlineAt: 1700000100, OfflineAt: 1700000400, OnlineExpireAt: 1893456000,
		RequestId: "req-upsert-1", Ctime: 1700000000, Mtime: 1700000400,
		Reason: livemediarpc.FailureReason_FAILURE_REASON_SOURCE_LOST,
	}
}

func liveMediaOutputAPIFixture() types.LiveMediaStreamOutputInfo {
	return types.LiveMediaStreamOutputInfo{
		OutputId: 900, RoomId: 1201, LiveSessionId: 33, TaskId: 501,
		BitrateLevel: 3, Protocol: 1,
		Bucket: "live-hls", ObjectKey: "1201/33/hd.m3u8", CdnDomain: "cdn.example.com",
		Width: 1280, Height: 720, BitrateKbps: 2000, Fps: 30,
		State: 2, OnlineAt: 1700000100, OfflineAt: 1700000400, OnlineExpireAt: 1893456000,
		RequestId: "req-upsert-1", Ctime: 1700000000, Mtime: 1700000400, Reason: 2,
	}
}

func liveMediaRecordInfoFixture() *livemediarpc.LiveRecordTaskInfo {
	return &livemediarpc.LiveRecordTaskInfo{
		RecordId: 601, RoomId: 1201, LiveSessionId: 33, SourceTaskId: 0,
		State:   livemediarpc.LiveRecordState_LIVE_RECORD_STATE_RECORDING,
		StartAt: 100, EndAt: 200, RecordStartAt: 105, RecordEndAt: 0,
		SegmentSeconds: 6, LastSeq: 42, SegmentCount: 40, GapCount: 2, RecordedDurationMs: 228000,
		OutputBucket: "live-record", OutputPrefix: "1201/33/",
		HeartbeatAt: 1700000200, TimeoutAt: 1700000300, Version: 4,
		Reason: livemediarpc.FailureReason_FAILURE_REASON_STORAGE,
		Errno:  7, ErrMsg: "对象存储写入重试中", RequestId: "req-recstart-1", TraceId: "trace-recstart",
		Ctime: 1700000000, Mtime: 1700000200,
	}
}

func liveMediaRecordAPIFixture() types.LiveMediaRecordTaskInfo {
	return types.LiveMediaRecordTaskInfo{
		RecordId: 601, RoomId: 1201, LiveSessionId: 33, SourceTaskId: 0,
		State: 2, StartAt: 100, EndAt: 200, RecordStartAt: 105, RecordEndAt: 0,
		SegmentSeconds: 6, LastSeq: 42, SegmentCount: 40, GapCount: 2, RecordedDurationMs: 228000,
		OutputBucket: "live-record", OutputPrefix: "1201/33/",
		HeartbeatAt: 1700000200, TimeoutAt: 1700000300, Version: 4, Reason: 4,
		Errno:     7,
		ErrMsg:    "对象存储写入重试中",
		RequestId: "req-recstart-1",
		TraceId:   "trace-recstart",
		Ctime:     1700000000,
		Mtime:     1700000200,
	}
}

func liveMediaSegmentInfoFixture() *livemediarpc.RecordSegmentInfo {
	return &livemediarpc.RecordSegmentInfo{
		Id: 8801, RecordId: 601, RoomId: 1201, LiveSessionId: 33, Seq: 11,
		StartAt: 165, EndAt: 171, DurationMs: 6000,
		State:  livemediarpc.SegmentState_SEGMENT_STATE_MISSING,
		Bucket: "live-record", ObjectKey: "1201/33/seg-11.ts",
		SizeBytes: 0, Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4",
		WorkerId: "worker-a", RegisteredAt: 1700000200, Mtime: 1700000210,
	}
}

func liveMediaSegmentAPIFixture() types.LiveMediaRecordSegmentInfo {
	return types.LiveMediaRecordSegmentInfo{
		Id: 8801, RecordId: 601, RoomId: 1201, LiveSessionId: 33, Seq: 11,
		StartAt: 165, EndAt: 171, DurationMs: 6000, State: 4,
		Bucket: "live-record", ObjectKey: "1201/33/seg-11.ts",
		SizeBytes: 0, Checksum: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4",
		WorkerId: "worker-a", RegisteredAt: 1700000200, Mtime: 1700000210,
	}
}

func liveMediaReplayInfoFixture() *livemediarpc.LiveReplayTaskInfo {
	return &livemediarpc.LiveReplayTaskInfo{
		ReplayId: 701, RoomId: 1201, LiveSessionId: 33, RecordId: 601,
		State:   livemediarpc.ReplayState_REPLAY_STATE_COMPLETED,
		FromSeq: 2, ToSeq: 88, SegmentCount: 86, GapCount: 1,
		StartAt: 100, EndAt: 200, DurationMs: 600000, AllowGaps: true,
		OutputBucket: "live-replay", OutputKey: "replay/701.mp4",
		AssetId: 4001, Aid: 0, Bvid: "",
		AnchorMid: 99, Title: "回放：某场直播全场", Version: 12,
		Reason:    livemediarpc.FailureReason_FAILURE_REASON_DATA_GAP,
		Errno:     0,
		ErrMsg:    "",
		RequestId: "req-replaysubmit-1", TraceId: "trace-replaysubmit",
		Ctime: 1700000000, Mtime: 1700000900,
	}
}

func liveMediaReplayAPIFixture() types.LiveMediaReplayTaskInfo {
	return types.LiveMediaReplayTaskInfo{
		ReplayId: 701, RoomId: 1201, LiveSessionId: 33, RecordId: 601, State: 6,
		FromSeq: 2, ToSeq: 88, SegmentCount: 86, GapCount: 1,
		StartAt: 100, EndAt: 200, DurationMs: 600000, AllowGaps: true,
		OutputBucket: "live-replay", OutputKey: "replay/701.mp4",
		AssetId: 4001, Aid: 0, Bvid: "",
		AnchorMid: 99, Title: "回放：某场直播全场", Version: 12, Reason: 8,
		RequestId: "req-replaysubmit-1",
		TraceId:   "trace-replaysubmit",
		Ctime:     1700000000,
		Mtime:     1700000900,
	}
}

func liveMediaRefInfoFixture() *livemediarpc.ReplayAssetRefInfo {
	return &livemediarpc.ReplayAssetRefInfo{
		Id: 9901, RoomId: 1201, LiveSessionId: 33, ReplayId: 701, RecordId: 601,
		AssetId: 4001, Aid: 0, Bvid: "BV1xx411c7mu", AnchorMid: 99,
		Bucket: "live-replay", ObjectKey: "replay/701.mp4", DurationMs: 600000,
		SegmentFromSeq: 2, SegmentToSeq: 88, GapCount: 1,
		ReviewState:   livemediarpc.ReviewState_REVIEW_STATE_PUBLISHED,
		ReviewStateAt: 1700000800, RetentionState: 1, PublishedAt: 1893456000,
		Ctime: 1700000000, Mtime: 1700000800,
	}
}

func liveMediaRefAPIFixture() types.LiveMediaReplayAssetRefInfo {
	return types.LiveMediaReplayAssetRefInfo{
		Id: 9901, RoomId: 1201, LiveSessionId: 33, ReplayId: 701, RecordId: 601,
		AssetId: 4001, Aid: 0, Bvid: "BV1xx411c7mu", AnchorMid: 99,
		Bucket: "live-replay", ObjectKey: "replay/701.mp4", DurationMs: 600000,
		SegmentFromSeq: 2, SegmentToSeq: 88, GapCount: 1,
		ReviewState: 3, ReviewStateAt: 1700000800, RetentionState: 1, PublishedAt: 1893456000,
		Ctime: 1700000000, Mtime: 1700000800,
	}
}

func liveMediaRetentionInfoFixture() *livemediarpc.LiveRetentionTaskInfo {
	return &livemediarpc.LiveRetentionTaskInfo{
		RetentionId: 801, TargetKind: livemediarpc.RetentionTargetKind_RETENTION_TARGET_KIND_SEGMENT,
		RoomId: 1201, TargetId: 0, ExpireBefore: 1893456000, Purge: false, BatchLimit: 100,
		State:   livemediarpc.RetentionState_RETENTION_STATE_SUCCEEDED,
		Scanned: 100, Deleted: 0, Skipped: 12,
		Reason:   "切片已被回放合并吸收，按保留期回收",
		Operator: liveMediaOperatorFixture,
		Version:  3, FailReason: livemediarpc.FailureReason_FAILURE_REASON_DATA_GAP,
		Errno: 0, ErrMsg: "",
		RequestId: "req-retention-1", TraceId: "trace-retention",
		Ctime: 1700000000, Mtime: 1700000500,
	}
}

func liveMediaRetentionAPIFixture() types.LiveMediaRetentionTaskInfo {
	return types.LiveMediaRetentionTaskInfo{
		RetentionId: 801, TargetKind: 1, RoomId: 1201, TargetId: 0, ExpireBefore: 1893456000,
		Purge: false, BatchLimit: 100, State: 3, Scanned: 100, Deleted: 0, Skipped: 12,
		Reason: "切片已被回放合并吸收，按保留期回收", Operator: liveMediaOperatorFixture,
		Version: 3, FailReason: 8, Errno: 0, ErrMsg: "",
		RequestId: "req-retention-1", TraceId: "trace-retention",
		Ctime: 1700000000, Mtime: 1700000500,
	}
}

// --- 路由表：23 条 = 11 读 + 12 写 ---

type liveMediaRoute struct {
	name    string
	write   bool
	newBody func() any
	call    func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error
}

func liveMediaRoutes() []liveMediaRoute {
	return []liveMediaRoute{
		{"transcodeGet", false, func() any { return liveMediaTranscodeGetBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeGet
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeGet](t, req)
				}
				_, err := NewLiveMediaTranscodeGetLogic(ctx, s).LiveMediaTranscodeGet(p)
				return err
			}},
		{"transcodeList", false, func() any { return liveMediaTranscodeListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeList](t, req)
				}
				_, err := NewLiveMediaTranscodeListLogic(ctx, s).LiveMediaTranscodeList(p)
				return err
			}},
		{"outputList", false, func() any { return liveMediaOutputListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaOutputList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaOutputList](t, req)
				}
				_, err := NewLiveMediaOutputListLogic(ctx, s).LiveMediaOutputList(p)
				return err
			}},
		{"recordGet", false, func() any { return liveMediaRecordGetBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRecordGet
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRecordGet](t, req)
				}
				_, err := NewLiveMediaRecordGetLogic(ctx, s).LiveMediaRecordGet(p)
				return err
			}},
		{"recordList", false, func() any { return liveMediaRecordListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRecordList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRecordList](t, req)
				}
				_, err := NewLiveMediaRecordListLogic(ctx, s).LiveMediaRecordList(p)
				return err
			}},
		{"segmentList", false, func() any { return liveMediaSegmentListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRecordSegmentList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRecordSegmentList](t, req)
				}
				_, err := NewLiveMediaRecordSegmentListLogic(ctx, s).LiveMediaRecordSegmentList(p)
				return err
			}},
		{"replayGet", false, func() any { return liveMediaReplayGetBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplayGet
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplayGet](t, req)
				}
				_, err := NewLiveMediaReplayGetLogic(ctx, s).LiveMediaReplayGet(p)
				return err
			}},
		{"replayList", false, func() any { return liveMediaReplayListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplayList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplayList](t, req)
				}
				_, err := NewLiveMediaReplayListLogic(ctx, s).LiveMediaReplayList(p)
				return err
			}},
		{"replayAssetList", false, func() any { return liveMediaReplayAssetListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplayAssetList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplayAssetList](t, req)
				}
				_, err := NewLiveMediaReplayAssetListLogic(ctx, s).LiveMediaReplayAssetList(p)
				return err
			}},
		{"retentionGet", false, func() any { return liveMediaRetentionGetBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRetentionGet
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRetentionGet](t, req)
				}
				_, err := NewLiveMediaRetentionGetLogic(ctx, s).LiveMediaRetentionGet(p)
				return err
			}},
		{"retentionList", false, func() any { return liveMediaRetentionListBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRetentionList
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRetentionList](t, req)
				}
				_, err := NewLiveMediaRetentionListLogic(ctx, s).LiveMediaRetentionList(p)
				return err
			}},
		{"transcodeStart", true, func() any { return liveMediaTranscodeStartBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeStart
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeStart](t, req)
				}
				_, err := NewLiveMediaTranscodeStartLogic(ctx, s).LiveMediaTranscodeStart(p)
				return err
			}},
		{"transcodeStop", true, func() any { return liveMediaTranscodeStopBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeStop
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeStop](t, req)
				}
				_, err := NewLiveMediaTranscodeStopLogic(ctx, s).LiveMediaTranscodeStop(p)
				return err
			}},
		{"transcodeRetry", true, func() any { return liveMediaTranscodeRetryBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeRetry
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeRetry](t, req)
				}
				_, err := NewLiveMediaTranscodeRetryLogic(ctx, s).LiveMediaTranscodeRetry(p)
				return err
			}},
		{"transcodeCancel", true, func() any { return liveMediaTranscodeCancelBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaTranscodeCancel
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaTranscodeCancel](t, req)
				}
				_, err := NewLiveMediaTranscodeCancelLogic(ctx, s).LiveMediaTranscodeCancel(p)
				return err
			}},
		{"outputUpsert", true, func() any { return liveMediaOutputUpsertBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaOutputUpsert
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaOutputUpsert](t, req)
				}
				_, err := NewLiveMediaOutputUpsertLogic(ctx, s).LiveMediaOutputUpsert(p)
				return err
			}},
		{"outputOffline", true, func() any { return liveMediaOutputOfflineBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaOutputOffline
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaOutputOffline](t, req)
				}
				_, err := NewLiveMediaOutputOfflineLogic(ctx, s).LiveMediaOutputOffline(p)
				return err
			}},
		{"recordStart", true, func() any { return liveMediaRecordStartBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRecordStart
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRecordStart](t, req)
				}
				_, err := NewLiveMediaRecordStartLogic(ctx, s).LiveMediaRecordStart(p)
				return err
			}},
		{"recordStop", true, func() any { return liveMediaRecordStopBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRecordStop
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRecordStop](t, req)
				}
				_, err := NewLiveMediaRecordStopLogic(ctx, s).LiveMediaRecordStop(p)
				return err
			}},
		{"replaySubmit", true, func() any { return liveMediaReplaySubmitBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplaySubmit
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplaySubmit](t, req)
				}
				_, err := NewLiveMediaReplaySubmitLogic(ctx, s).LiveMediaReplaySubmit(p)
				return err
			}},
		{"replayAssetBind", true, func() any { return liveMediaReplayBindBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplayAssetBind
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplayAssetBind](t, req)
				}
				_, err := NewLiveMediaReplayAssetBindLogic(ctx, s).LiveMediaReplayAssetBind(p)
				return err
			}},
		{"replayContentState", true, func() any { return liveMediaReplayContentStateBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaReplayContentState
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaReplayContentState](t, req)
				}
				_, err := NewLiveMediaReplayContentStateLogic(ctx, s).LiveMediaReplayContentState(p)
				return err
			}},
		{"retentionSubmit", true, func() any { return liveMediaRetentionSubmitBody() },
			func(t *testing.T, ctx context.Context, s *svc.ServiceContext, req any) error {
				var p *types.ParamLiveMediaRetentionSubmit
				if req != nil {
					p = liveMediaMust[types.ParamLiveMediaRetentionSubmit](t, req)
				}
				_, err := NewLiveMediaRetentionSubmitLogic(ctx, s).LiveMediaRetentionSubmit(p)
				return err
			}},
	}
}

func liveMediaRouteByName(t *testing.T, name string) liveMediaRoute {
	t.Helper()
	for _, r := range liveMediaRoutes() {
		if r.name == name {
			return r
		}
	}
	t.Fatalf("路由 %s 不在测试表里", name)
	return liveMediaRoute{}
}

func TestLiveMediaRouteTableCoversAllTwentyThreeRoutes(t *testing.T) {
	routes := liveMediaRoutes()
	if len(routes) != 23 {
		t.Fatalf("路由表条目 = %d, want 23", len(routes))
	}
	seen := make(map[string]bool, len(routes))
	var writes, reads int
	for _, r := range routes {
		if seen[r.name] {
			t.Fatalf("路由名重复：%s", r.name)
		}
		seen[r.name] = true
		if r.write {
			writes++
		} else {
			reads++
		}
		if r.newBody == nil || r.call == nil {
			t.Fatalf("路由 %s 缺 newBody/call", r.name)
		}
	}
	if writes != 12 || reads != 11 {
		t.Fatalf("读写配比 = %d 写 / %d 读, want 12/11", writes, reads)
	}
}

// 未配置 LiveMediaRPC（客户端 nil）时 23 条一律失败关闭，绝不回空成功。
func TestLiveMediaRoutesFailClosedWithoutClient(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		ctx := context.Background()
		if r.write {
			ctx = liveMediaSessionCtx()
		}
		err := r.call(t, ctx, liveMediaSvc(nil), r.newBody())
		if !errors.Is(err, errLiveMediaNotConfigured) {
			t.Fatalf("%s 未配置下游时 err = %v, want errLiveMediaNotConfigured", r.name, err)
		}
	}
}

func TestLiveMediaRoutesRejectMissingBody(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		ctx := context.Background()
		if r.write {
			ctx = liveMediaSessionCtx()
		}
		err := r.call(t, ctx, liveMediaSvc(&liveMediaAdminFake{}), nil)
		if !errors.Is(err, errLiveRequestMissing) {
			t.Fatalf("%s 请求体缺失时 err = %v, want errLiveRequestMissing", r.name, err)
		}
	}
}

// 十二条写入口没有会话身份即 fail-closed：这挡住「路由忘了挂 AdminPermission」的漂移。
func TestLiveMediaWriteRoutesRequireSessionIdentity(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		if !r.write {
			continue
		}
		fake := &liveMediaAdminFake{}
		err := r.call(t, context.Background(), liveMediaSvc(fake), r.newBody())
		if !errors.Is(err, errLiveSessionRequired) {
			t.Fatalf("%s 无会话身份时 err = %v, want errLiveSessionRequired", r.name, err)
		}
		if fake.calls != 0 {
			t.Fatalf("%s 在无会话身份时仍打了下游 %d 次", r.name, fake.calls)
		}
	}
}

// 只读面不要求会话身份（与 audit/ops-config/cron/live 其余三服务/recommend 读面同一口径）。
func TestLiveMediaReadRoutesDoNotRequireSession(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		if r.write {
			continue
		}
		fake := &liveMediaAdminFake{}
		if err := r.call(t, context.Background(), liveMediaSvc(fake), r.newBody()); err != nil {
			t.Fatalf("%s 只读路由不应要求会话身份: %v", r.name, err)
		}
		if fake.calls != 1 {
			t.Fatalf("%s 下游调用次数 = %d, want 1", r.name, fake.calls)
		}
	}
}

// 下游错误必须原样透传：不兜成 code=0 的空数据，也不改写成「没找到」。
func TestLiveMediaRoutesPassDownstreamErrorsThrough(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		fake := &liveMediaAdminFake{err: errLiveMediaFakeDownstream}
		err := r.call(t, liveMediaSessionCtx(), liveMediaSvc(fake), r.newBody())
		if !errors.Is(err, errLiveMediaFakeDownstream) {
			t.Fatalf("%s 下游错误未透传: %v", r.name, err)
		}
		if fake.calls != 1 {
			t.Fatalf("%s 下游调用次数 = %d, want 1", r.name, fake.calls)
		}
	}
}

// 幂等键只去空格判空，绝不改写：TrimSpace 后回写会让服务侧去重失去语义。
func TestLiveMediaWriteRoutesRequireNonBlankRequestId(t *testing.T) {
	for _, r := range liveMediaRoutes() {
		if !r.write || r.name == "replayContentState" {
			// ApplyReplayContentState 契约里没有 request_id（缺口见 admin.api/README）。
			continue
		}
		body := r.newBody()
		v := reflect.ValueOf(body).Elem().FieldByName("RequestId")
		if !v.IsValid() || v.Kind() != reflect.String {
			t.Fatalf("%s 入参没有 RequestId 字段", r.name)
		}
		v.SetString("   ")
		fake := &liveMediaAdminFake{}
		err := r.call(t, liveMediaSessionCtx(), liveMediaSvc(fake), body)
		liveMediaErrString(t, err, "request_id required")
		if fake.calls != 0 {
			t.Fatalf("%s 空白幂等键仍打了下游 %d 次", r.name, fake.calls)
		}
	}
}

// 会话门槛先于形态门槛：身份缺失时的错误必须是身份错误，不能退化成「参数不对」。
func TestLiveMediaWriteSessionGateRunsBeforeShapeGate(t *testing.T) {
	body := liveMediaTranscodeStopBody()
	body.TaskId = 0
	body.Reason = 0
	fake := &liveMediaAdminFake{}
	r := liveMediaRouteByName(t, "transcodeStop")
	err := r.call(t, context.Background(), liveMediaSvc(fake), body)
	liveMediaErrString(t, err, "admin session identity required")
	if fake.calls != 0 {
		t.Fatalf("无会话身份时不应打下游，实际 %d 次", fake.calls)
	}
}

// --- 形态门槛 ---

type liveMediaGateCase struct {
	route  string
	want   string
	mutate func(t *testing.T, body any)
}

func TestLiveMediaShapeGates(t *testing.T) {
	cases := []liveMediaGateCase{
		// 必填主键：传 0 只会落到不存在的主键上，回给后台的是零值行而不是「少传参数」。
		{"transcodeGet", "task_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeGet](t, b).TaskId = 0
		}},
		{"transcodeList", "room_id must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeList](t, b).RoomId = -1
		}},
		{"transcodeList", "state must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeList](t, b).State = -1
		}},
		{"transcodeList", "pn must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeList](t, b).Pn = -1
		}},
		{"transcodeList", "ps must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeList](t, b).Ps = -1
		}},
		{"outputList", "room_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputList](t, b).RoomId = 0
		}},
		{"outputList", "live_session_id must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputList](t, b).SessionId = -1
		}},
		{"recordGet", "record_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordGet](t, b).RecordId = 0
		}},
		{"recordList", "live_session_id must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordList](t, b).SessionId = -1
		}},
		{"segmentList", "record_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordSegmentList](t, b).RecordId = 0
		}},
		{"segmentList", "after_seq must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordSegmentList](t, b).AfterSeq = -1
		}},
		{"segmentList", "limit must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordSegmentList](t, b).Limit = -1
		}},
		{"replayGet", "replay_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayGet](t, b).ReplayId = 0
		}},
		{"replayList", "state must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayList](t, b).State = -1
		}},
		{"replayAssetList", "anchor_mid must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayAssetList](t, b).AnchorMid = -1
		}},
		{"replayAssetList", "review_state must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayAssetList](t, b).ReviewState = -1
		}},
		{"retentionGet", "retention_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionGet](t, b).RetentionId = 0
		}},
		{"retentionList", "target_kind must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionList](t, b).TargetKind = -1
		}},

		// 写入口：枚举目标位不许 UNSPECIFIED、主键必填、幂等键非空。
		{"transcodeStart", "room_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).RoomId = 0
		}},
		{"transcodeStart", "template_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).TemplateId = 0
		}},
		{"transcodeStart", "source_ref required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).SourceRef = "   "
		}},
		{"transcodeStart", "bitrate_level required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).BitrateLevel = 0
		}},
		{"transcodeStart", "protocol required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).Protocol = 0
		}},
		{"transcodeStart", "max_attempts must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStart](t, b).MaxAttempts = -1
		}},
		{"transcodeStop", "task_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStop](t, b).TaskId = 0
		}},
		{"transcodeStop", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStop](t, b).Reason = 0
		}},
		{"transcodeStop", "expected_version must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeStop](t, b).ExpectedVersion = -1
		}},
		{"transcodeRetry", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeRetry](t, b).Reason = "   "
		}},
		{"transcodeCancel", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaTranscodeCancel](t, b).Reason = 0
		}},
		{"outputUpsert", "bucket and object_key must be given together", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputUpsert](t, b).ObjectKey = ""
		}},
		{"outputUpsert", "bitrate_level required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputUpsert](t, b).BitrateLevel = 0
		}},
		{"outputUpsert", "width must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputUpsert](t, b).Width = -1
		}},
		{"outputOffline", "output_id or (room_id, bitrate_level, protocol) required", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaOutputOffline](t, b)
			p.OutputId = 0
		}},
		{"outputOffline", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaOutputOffline](t, b).Reason = 0
		}},
		{"recordStart", "output_bucket and output_prefix must be given together", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordStart](t, b).OutputPrefix = ""
		}},
		{"recordStart", "start_at must be <= end_at", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaRecordStart](t, b)
			p.StartAt, p.EndAt = 300, 200
		}},
		{"recordStart", "end_at must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordStart](t, b).EndAt = -1
		}},
		{"recordStart", "segment_seconds must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordStart](t, b).SegmentSeconds = -1
		}},
		{"recordStop", "record_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordStop](t, b).RecordId = 0
		}},
		{"recordStop", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRecordStop](t, b).Reason = 0
		}},
		{"replaySubmit", "record_id required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplaySubmit](t, b).RecordId = 0
		}},
		{"replaySubmit", "title required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplaySubmit](t, b).Title = "   "
		}},
		{"replaySubmit", "from_seq must be <= to_seq", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaReplaySubmit](t, b)
			p.FromSeq, p.ToSeq = 90, 88
		}},
		{"replaySubmit", "start_at must be <= end_at", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaReplaySubmit](t, b)
			p.StartAt, p.EndAt = 300, 200
		}},
		{"replayAssetBind", "asset_id or aid required", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaReplayAssetBind](t, b)
			p.AssetId, p.Aid = 0, 0
		}},
		{"replayAssetBind", "bucket and object_key must be given together", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayAssetBind](t, b).Bucket = ""
		}},
		{"replayContentState", "replay_id or asset_id required", func(t *testing.T, b any) {
			p := liveMediaMust[types.ParamLiveMediaReplayContentState](t, b)
			p.ReplayId, p.AssetId = 0, 0
		}},
		{"replayContentState", "review_state required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayContentState](t, b).ReviewState = 0
		}},
		{"replayContentState", "published_at must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaReplayContentState](t, b).PublishedAt = -1
		}},
		{"retentionSubmit", "target_kind required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionSubmit](t, b).TargetKind = 0
		}},
		{"retentionSubmit", "reason required", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionSubmit](t, b).Reason = "   "
		}},
		{"retentionSubmit", "expire_before must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionSubmit](t, b).ExpireBefore = -1
		}},
		{"retentionSubmit", "batch_limit must be >= 0", func(t *testing.T, b any) {
			liveMediaMust[types.ParamLiveMediaRetentionSubmit](t, b).BatchLimit = -1
		}},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		key := c.route + "|" + c.want
		if seen[key] {
			t.Fatalf("门槛用例重复：%s", key)
		}
		seen[key] = true
		r := liveMediaRouteByName(t, c.route)
		body := r.newBody()
		c.mutate(t, body)
		fake := &liveMediaAdminFake{}
		err := r.call(t, liveMediaSessionCtx(), liveMediaSvc(fake), body)
		liveMediaErrString(t, err, c.want)
		if fake.calls != 0 {
			t.Fatalf("%s 形态非法时仍打了下游 %d 次（want 0）", r.name, fake.calls)
		}
	}
}

// --- 逐字段投影 ---

func TestLiveMediaTranscodeGetProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{transcodeInfo: liveMediaTranscodeInfoFixture()}
	resp, err := NewLiveMediaTranscodeGetLogic(context.Background(), liveMediaSvc(fake)).
		LiveMediaTranscodeGet(liveMediaTranscodeGetBody())
	if err != nil {
		t.Fatalf("读取转码任务失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaTranscodeAPIFixture()) {
		t.Fatalf("转码任务投影丢字段:\n got %+v\nwant %+v", resp.Data, liveMediaTranscodeAPIFixture())
	}
	if fake.getTranscodeReq.GetTaskId() != 501 {
		t.Fatalf("task_id 未原样透传: %d", fake.getTranscodeReq.GetTaskId())
	}
}

func TestLiveMediaOutputUpsertProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{outputInfo: liveMediaOutputInfoFixture()}
	resp, err := NewLiveMediaOutputUpsertLogic(liveMediaSessionCtx(), liveMediaSvc(fake)).
		LiveMediaOutputUpsert(liveMediaOutputUpsertBody())
	if err != nil {
		t.Fatalf("登记档位失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaOutputAPIFixture()) {
		t.Fatalf("档位投影丢字段:\n got %+v\nwant %+v", resp.Data, liveMediaOutputAPIFixture())
	}
	got := fake.upsertOutputReq
	want := liveMediaOutputUpsertBody()
	if got.GetRoomId() != want.RoomId || got.GetLiveSessionId() != want.SessionId ||
		got.GetTaskId() != want.TaskId || int32(got.GetBitrateLevel()) != want.BitrateLevel ||
		int32(got.GetProtocol()) != want.Protocol || got.GetBucket() != want.Bucket ||
		got.GetObjectKey() != want.ObjectKey || got.GetCdnDomain() != want.CdnDomain ||
		got.GetWidth() != want.Width || got.GetHeight() != want.Height ||
		got.GetBitrateKbps() != want.BitrateKbps || got.GetFps() != want.Fps ||
		got.GetOnlineExpireAt() != want.OnlineExpireAt || got.GetRequestId() != want.RequestId ||
		got.GetTraceId() != want.TraceId {
		t.Fatalf("档位表单未原样透传: %+v", got)
	}
}

func TestLiveMediaRecordGetProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{recordInfo: liveMediaRecordInfoFixture()}
	resp, err := NewLiveMediaRecordGetLogic(context.Background(), liveMediaSvc(fake)).
		LiveMediaRecordGet(liveMediaRecordGetBody())
	if err != nil {
		t.Fatalf("读取录制任务失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaRecordAPIFixture()) {
		t.Fatalf("录制任务投影丢字段（last_seq/gap_count 是断点续录证据）:\n got %+v\nwant %+v",
			resp.Data, liveMediaRecordAPIFixture())
	}
}

func TestLiveMediaSegmentListProjectsAndKeepsKeyset(t *testing.T) {
	fake := &liveMediaAdminFake{listSegmentReply: &livemediarpc.ListRecordSegmentsReply{
		Segments:     []*livemediarpc.RecordSegmentInfo{liveMediaSegmentInfoFixture()},
		NextAfterSeq: 11,
		HasMore:      true,
		Total:        4000000001, // int64：证明网关没把它窄化成 int32
	}}
	resp, err := NewLiveMediaRecordSegmentListLogic(context.Background(), liveMediaSvc(fake)).
		LiveMediaRecordSegmentList(liveMediaSegmentListBody())
	if err != nil {
		t.Fatalf("读取切片失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if len(resp.Data.List) != 1 || !reflect.DeepEqual(resp.Data.List[0], liveMediaSegmentAPIFixture()) {
		t.Fatalf("切片投影丢字段: %+v", resp.Data.List)
	}
	if resp.Data.Total != 4000000001 || resp.Data.NextAfterSeq != 11 || !resp.Data.HasMore {
		t.Fatalf("keyset 读数未原样回传: %+v", resp.Data)
	}
	got := fake.listSegmentReq
	if got.GetRecordId() != 601 || int32(got.GetState()) != 4 || got.GetAfterSeq() != 10 || got.GetLimit() != 200 {
		t.Fatalf("切片查询入参未原样透传: %+v", got)
	}
}

func TestLiveMediaReplaySubmitProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{replayInfo: liveMediaReplayInfoFixture()}
	resp, err := NewLiveMediaReplaySubmitLogic(liveMediaSessionCtx(), liveMediaSvc(fake)).
		LiveMediaReplaySubmit(liveMediaReplaySubmitBody())
	if err != nil {
		t.Fatalf("提交回放失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaReplayAPIFixture()) {
		t.Fatalf("回放任务投影丢字段:\n got %+v\nwant %+v", resp.Data, liveMediaReplayAPIFixture())
	}
	got := fake.submitReplayReq
	want := liveMediaReplaySubmitBody()
	if !got.GetAllowGaps() || got.GetFromSeq() != want.FromSeq || got.GetToSeq() != want.ToSeq ||
		got.GetRecordId() != want.RecordId || got.GetTitle() != want.Title ||
		got.GetDescription() != want.Description || got.GetAnchorMid() != want.AnchorMid {
		t.Fatalf("回放入参未原样透传: %+v", got)
	}
}

func TestLiveMediaReplayAssetBindProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{refInfo: liveMediaRefInfoFixture()}
	resp, err := NewLiveMediaReplayAssetBindLogic(liveMediaSessionCtx(), liveMediaSvc(fake)).
		LiveMediaReplayAssetBind(liveMediaReplayBindBody())
	if err != nil {
		t.Fatalf("回填引用失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaRefAPIFixture()) {
		t.Fatalf("引用行投影丢字段（review_state/published_at 是 video 只读投影）:\n got %+v\nwant %+v",
			resp.Data, liveMediaRefAPIFixture())
	}
}

func TestLiveMediaRetentionSubmitProjectsEveryField(t *testing.T) {
	fake := &liveMediaAdminFake{retentionInfo: liveMediaRetentionInfoFixture()}
	resp, err := NewLiveMediaRetentionSubmitLogic(liveMediaSessionCtx(), liveMediaSvc(fake)).
		LiveMediaRetentionSubmit(liveMediaRetentionSubmitBody())
	if err != nil {
		t.Fatalf("提交回收失败: %v", err)
	}
	liveMediaAssertEnvelope(t, resp.Code, resp.Message, resp.TTL)
	if !reflect.DeepEqual(resp.Data, liveMediaRetentionAPIFixture()) {
		t.Fatalf("回收任务投影丢字段（scanned/deleted/skipped 是凭证）:\n got %+v\nwant %+v",
			resp.Data, liveMediaRetentionAPIFixture())
	}
}

// --- 列表分页与过滤位 ---

func TestLiveMediaListRoutesPassFiltersAndPageVerbatim(t *testing.T) {
	fake := &liveMediaAdminFake{
		listTranscodeReply: &livemediarpc.ListLiveTranscodeTasksReply{
			Page:  &livemediarpc.PageResult{Total: 3},
			Tasks: []*livemediarpc.LiveTranscodeTaskInfo{liveMediaTranscodeInfoFixture()},
		},
		listOutputReply: &livemediarpc.ListStreamOutputsReply{
			Page:    &livemediarpc.PageResult{Total: 2},
			Outputs: []*livemediarpc.StreamOutputInfo{liveMediaOutputInfoFixture()},
		},
		listRecordReply: &livemediarpc.ListLiveRecordTasksReply{
			Page:  &livemediarpc.PageResult{Total: 1},
			Tasks: []*livemediarpc.LiveRecordTaskInfo{liveMediaRecordInfoFixture()},
		},
		listReplayReply: &livemediarpc.ListReplayTasksReply{
			Page:  &livemediarpc.PageResult{Total: 4},
			Tasks: []*livemediarpc.LiveReplayTaskInfo{liveMediaReplayInfoFixture()},
		},
		listRefReply: &livemediarpc.ListReplayAssetRefsReply{
			Page: &livemediarpc.PageResult{Total: 5},
			Refs: []*livemediarpc.ReplayAssetRefInfo{liveMediaRefInfoFixture()},
		},
		listRetentionReply: &livemediarpc.ListRetentionTasksReply{
			Page:  &livemediarpc.PageResult{Total: 6},
			Tasks: []*livemediarpc.LiveRetentionTaskInfo{liveMediaRetentionInfoFixture()},
		},
	}
	s := liveMediaSvc(fake)

	transcodeResp, err := NewLiveMediaTranscodeListLogic(context.Background(), s).
		LiveMediaTranscodeList(liveMediaTranscodeListBody())
	if err != nil {
		t.Fatalf("转码列表失败: %v", err)
	}
	if transcodeResp.Data.Total != 3 || len(transcodeResp.Data.List) != 1 {
		t.Fatalf("转码列表回包异常: total=%d len=%d", transcodeResp.Data.Total, len(transcodeResp.Data.List))
	}
	gotT := fake.listTranscodeReq
	if gotT.GetRoomId() != 1201 || gotT.GetLiveSessionId() != 33 || int32(gotT.GetState()) != 2 ||
		gotT.GetTemplateId() != 7 || gotT.GetPage().GetPn() != 2 || gotT.GetPage().GetPs() != 20 {
		t.Fatalf("转码列表过滤位/分页未原样透传: %+v", gotT)
	}

	outputResp, err := NewLiveMediaOutputListLogic(context.Background(), s).
		LiveMediaOutputList(liveMediaOutputListBody())
	if err != nil {
		t.Fatalf("档位列表失败: %v", err)
	}
	if outputResp.Data.Total != 2 || !reflect.DeepEqual(outputResp.Data.List[0], liveMediaOutputAPIFixture()) {
		t.Fatalf("档位列表投影异常: %+v", outputResp.Data)
	}
	if !fake.listOutputReq.GetIncludeOffline() || fake.listOutputReq.GetRoomId() != 1201 {
		t.Fatalf("档位列表入参未原样透传: %+v", fake.listOutputReq)
	}

	recordResp, err := NewLiveMediaRecordListLogic(context.Background(), s).
		LiveMediaRecordList(liveMediaRecordListBody())
	if err != nil {
		t.Fatalf("录制列表失败: %v", err)
	}
	if recordResp.Data.Total != 1 || !reflect.DeepEqual(recordResp.Data.List[0], liveMediaRecordAPIFixture()) {
		t.Fatalf("录制列表投影异常: %+v", recordResp.Data)
	}

	replayResp, err := NewLiveMediaReplayListLogic(context.Background(), s).
		LiveMediaReplayList(liveMediaReplayListBody())
	if err != nil {
		t.Fatalf("回放列表失败: %v", err)
	}
	if replayResp.Data.Total != 4 || int32(fake.listReplayReq.GetState()) != 6 {
		t.Fatalf("回放列表投影/透传异常: total=%d req=%+v", replayResp.Data.Total, fake.listReplayReq)
	}

	refResp, err := NewLiveMediaReplayAssetListLogic(context.Background(), s).
		LiveMediaReplayAssetList(liveMediaReplayAssetListBody())
	if err != nil {
		t.Fatalf("引用列表失败: %v", err)
	}
	if refResp.Data.Total != 5 || int32(fake.listRefReq.GetReviewState()) != 3 ||
		fake.listRefReq.GetAnchorMid() != 99 {
		t.Fatalf("引用列表投影/透传异常: total=%d req=%+v", refResp.Data.Total, fake.listRefReq)
	}

	retentionResp, err := NewLiveMediaRetentionListLogic(context.Background(), s).
		LiveMediaRetentionList(liveMediaRetentionListBody())
	if err != nil {
		t.Fatalf("回收列表失败: %v", err)
	}
	if retentionResp.Data.Total != 6 || int32(fake.listRetentionReq.GetTargetKind()) != 1 ||
		int32(fake.listRetentionReq.GetState()) != 3 {
		t.Fatalf("回收列表投影/透传异常: total=%d req=%+v", retentionResp.Data.Total, fake.listRetentionReq)
	}
}

// 空列表必须是 []（非 null），且 total 缺失按 0 回传而不是伪造。
func TestLiveMediaEmptyListsAreNotNilSlices(t *testing.T) {
	fake := &liveMediaAdminFake{
		listTranscodeReply: &livemediarpc.ListLiveTranscodeTasksReply{},
		listOutputReply:    &livemediarpc.ListStreamOutputsReply{},
		listRecordReply:    &livemediarpc.ListLiveRecordTasksReply{},
		listSegmentReply:   &livemediarpc.ListRecordSegmentsReply{},
		listReplayReply:    &livemediarpc.ListReplayTasksReply{},
		listRefReply:       &livemediarpc.ListReplayAssetRefsReply{},
		listRetentionReply: &livemediarpc.ListRetentionTasksReply{},
	}
	s := liveMediaSvc(fake)

	tc, err := NewLiveMediaTranscodeListLogic(context.Background(), s).LiveMediaTranscodeList(liveMediaTranscodeListBody())
	if err != nil || tc.Data.List == nil || len(tc.Data.List) != 0 || tc.Data.Total != 0 {
		t.Fatalf("转码空列表应投影成 []: resp=%+v err=%v", tc, err)
	}
	ou, err := NewLiveMediaOutputListLogic(context.Background(), s).LiveMediaOutputList(liveMediaOutputListBody())
	if err != nil || ou.Data.List == nil {
		t.Fatalf("档位空列表应投影成 []: resp=%+v err=%v", ou, err)
	}
	rc, err := NewLiveMediaRecordListLogic(context.Background(), s).LiveMediaRecordList(liveMediaRecordListBody())
	if err != nil || rc.Data.List == nil {
		t.Fatalf("录制空列表应投影成 []: resp=%+v err=%v", rc, err)
	}
	sg, err := NewLiveMediaRecordSegmentListLogic(context.Background(), s).LiveMediaRecordSegmentList(liveMediaSegmentListBody())
	if err != nil || sg.Data.List == nil || sg.Data.HasMore || sg.Data.NextAfterSeq != 0 {
		t.Fatalf("切片空列表/keyset 读数异常: resp=%+v err=%v", sg, err)
	}
	rp, err := NewLiveMediaReplayListLogic(context.Background(), s).LiveMediaReplayList(liveMediaReplayListBody())
	if err != nil || rp.Data.List == nil {
		t.Fatalf("回放空列表应投影成 []: resp=%+v err=%v", rp, err)
	}
	af, err := NewLiveMediaReplayAssetListLogic(context.Background(), s).LiveMediaReplayAssetList(liveMediaReplayAssetListBody())
	if err != nil || af.Data.List == nil {
		t.Fatalf("引用空列表应投影成 []: resp=%+v err=%v", af, err)
	}
	rt, err := NewLiveMediaRetentionListLogic(context.Background(), s).LiveMediaRetentionList(liveMediaRetentionListBody())
	if err != nil || rt.Data.List == nil {
		t.Fatalf("回收空列表应投影成 []: resp=%+v err=%v", rt, err)
	}
}

// --- 操作者与来源口径 ---

// 有 operator 位的五个方法：operator 只能是会话渲染值，表单里没有这个字段。
func TestLiveMediaOperatorRenderedFromSession(t *testing.T) {
	fake := &liveMediaAdminFake{}
	s := liveMediaSvc(fake)
	ctx := middleware.WithAdmin(context.Background(), middleware.AdminIdentity{AdminID: 4242})

	if _, err := NewLiveMediaTranscodeStopLogic(ctx, s).LiveMediaTranscodeStop(liveMediaTranscodeStopBody()); err != nil {
		t.Fatalf("停止转码失败: %v", err)
	}
	if got := fake.stopTranscodeReq.GetOperator(); got != "admin:4242" {
		t.Fatalf("StopLiveTranscode operator = %q, want admin:4242", got)
	}
	if _, err := NewLiveMediaTranscodeRetryLogic(ctx, s).LiveMediaTranscodeRetry(liveMediaTranscodeRetryBody()); err != nil {
		t.Fatalf("重试转码失败: %v", err)
	}
	if got := fake.retryTranscodeReq.GetOperator(); got != "admin:4242" {
		t.Fatalf("RetryLiveTranscode operator = %q", got)
	}
	if _, err := NewLiveMediaTranscodeCancelLogic(ctx, s).LiveMediaTranscodeCancel(liveMediaTranscodeCancelBody()); err != nil {
		t.Fatalf("取消转码失败: %v", err)
	}
	if got := fake.cancelTranscodeReq.GetOperator(); got != "admin:4242" {
		t.Fatalf("CancelLiveTranscode operator = %q", got)
	}
	if _, err := NewLiveMediaRecordStopLogic(ctx, s).LiveMediaRecordStop(liveMediaRecordStopBody()); err != nil {
		t.Fatalf("停止录制失败: %v", err)
	}
	if got := fake.stopRecordReq.GetOperator(); got != "admin:4242" {
		t.Fatalf("StopLiveRecord operator = %q", got)
	}
	if _, err := NewLiveMediaRetentionSubmitLogic(ctx, s).LiveMediaRetentionSubmit(liveMediaRetentionSubmitBody()); err != nil {
		t.Fatalf("提交回收失败: %v", err)
	}
	if got := fake.submitRetentionReq.GetOperator(); got != "admin:4242" {
		t.Fatalf("SubmitRetentionTask operator = %q", got)
	}
}

// ApplyReplayContentState 的 source 由网关固定成 manual，且它是唯一没有 request_id 的写路由。
func TestLiveMediaContentStateFixesSourceAndNeedsNoRequestId(t *testing.T) {
	fake := &liveMediaAdminFake{refInfo: liveMediaRefInfoFixture()}
	body := liveMediaReplayContentStateBody()
	resp, err := NewLiveMediaReplayContentStateLogic(liveMediaSessionCtx(), liveMediaSvc(fake)).
		LiveMediaReplayContentState(body)
	if err != nil {
		t.Fatalf("刷新投影失败: %v", err)
	}
	if resp.Data.ReviewState != 3 {
		t.Fatalf("投影回包异常: %+v", resp.Data)
	}
	got := fake.applyStateReq
	if got.GetSource() != "manual" {
		t.Fatalf("source = %q, want manual（后台点出来的刷新不可能是事件或 video 回调）", got.GetSource())
	}
	if got.GetEventId() != "evt-content-published-1" {
		t.Fatalf("event_id 未原样透传: %q", got.GetEventId())
	}
	if got.GetReplayId() != 701 || int32(got.GetReviewState()) != 3 || got.GetPublishedAt() != 1893456000 {
		t.Fatalf("投影入参未原样透传: %+v", got)
	}
	// event_id 留空也必须放行：契约里它是「驱动本次同步的事件 ID」，人工刷新没有事件可引，
	// 网关不伪造随机值（见 admin.api 契约缺口）。
	body.EventId = ""
	fake2 := &liveMediaAdminFake{refInfo: liveMediaRefInfoFixture()}
	if _, err := NewLiveMediaReplayContentStateLogic(liveMediaSessionCtx(), liveMediaSvc(fake2)).
		LiveMediaReplayContentState(body); err != nil {
		t.Fatalf("event_id 为空时人工刷新应放行: %v", err)
	}
	if fake2.applyStateReq.GetEventId() != "" {
		t.Fatalf("网关伪造了 event_id: %q", fake2.applyStateReq.GetEventId())
	}
}

// 幂等键与哨兵值原样透传：不改写、不补默认值、不把 0 当缺参。
func TestLiveMediaSentinelsPassThroughVerbatim(t *testing.T) {
	fake := &liveMediaAdminFake{}
	s := liveMediaSvc(fake)
	ctx := liveMediaSessionCtx()

	startBody := liveMediaTranscodeStartBody()
	startBody.SessionId = 0
	startBody.AnchorMid = 0
	startBody.MaxAttempts = 0
	startBody.TimeoutSeconds = 0
	startBody.RequestId = "  req-with-spaces  " // 只判空、不改值
	if _, err := NewLiveMediaTranscodeStartLogic(ctx, s).LiveMediaTranscodeStart(startBody); err != nil {
		t.Fatalf("登记转码失败: %v", err)
	}
	got := fake.startTranscodeReq
	if got.GetRequestId() != "  req-with-spaces  " {
		t.Fatalf("幂等键被改写了: %q", got.GetRequestId())
	}
	if got.GetLiveSessionId() != 0 || got.GetAnchorMid() != 0 || got.GetMaxAttempts() != 0 || got.GetTimeoutSeconds() != 0 {
		t.Fatalf("0 哨兵被网关补了默认值: %+v", got)
	}

	recBody := liveMediaRecordStartBody()
	recBody.OutputBucket = ""
	recBody.OutputPrefix = "" // 两个都不给 = 用服务默认，合法
	if _, err := NewLiveMediaRecordStartLogic(ctx, s).LiveMediaRecordStart(recBody); err != nil {
		t.Fatalf("登记录制失败（引用全空应放行）: %v", err)
	}
	if fake.startRecordReq.GetOutputBucket() != "" || fake.startRecordReq.GetOutputPrefix() != "" {
		t.Fatalf("录制输出引用被网关改写: %+v", fake.startRecordReq)
	}

	offBody := liveMediaOutputOfflineBody()
	offBody.OutputId = 0
	offBody.RoomId, offBody.BitrateLevel, offBody.Protocol = 1201, 3, 1 // 三元组寻址
	if _, err := NewLiveMediaOutputOfflineLogic(ctx, s).LiveMediaOutputOffline(offBody); err != nil {
		t.Fatalf("按三元组下线失败: %v", err)
	}
	if fake.offlineOutpReq.GetRoomId() != 1201 || int32(fake.offlineOutpReq.GetBitrateLevel()) != 3 ||
		int32(fake.offlineOutpReq.GetProtocol()) != 1 {
		t.Fatalf("三元组寻址未透传: %+v", fake.offlineOutpReq)
	}

	bindBody := liveMediaReplayBindBody()
	bindBody.AssetId, bindBody.Aid = 0, 5002 // 两阶段回填：先建稿后登记媒资
	if _, err := NewLiveMediaReplayAssetBindLogic(ctx, s).LiveMediaReplayAssetBind(bindBody); err != nil {
		t.Fatalf("按 aid 回填引用失败: %v", err)
	}
	if fake.bindAssetReq.GetAid() != 5002 || fake.bindAssetReq.GetAssetId() != 0 {
		t.Fatalf("引用主体被改写: %+v", fake.bindAssetReq)
	}
}
