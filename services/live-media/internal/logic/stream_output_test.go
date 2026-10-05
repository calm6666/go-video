package logic

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 分发档位三个方法：UpsertStreamOutput / OfflineStreamOutput / ListStreamOutputs。
//
// 这组方法与转码/录制生命周期的最大差别是**没有 version 列**，因此每个用例都要同时钉住：
//  1. 幂等只来自 uniq_output_natural(room_id, live_session_id, bitrate_level, protocol)：
//     同键重登记必须复用同一行（不新增行），而 request_id 上没有唯一索引 —— 它只是调用方
//     归因字段，重复 request_id 绝不允许被当成幂等键（否则第二次「重新上线」会被静默吞掉，
//     观众侧档位永远回不来）；
//  2. state 本身就是 CAS 条件：MarkOfflineTx 命中 0 行不是错误，logic 必须回读归因，
//     并且「同一次下线只留一份证据」（0 行不补第二条事件）；
//  3. 响应必须等于提交后的行（回读 FindOne），不是入参回显：online_at/ctime 这些审计列
//     由 model 决定是否刷新，logic 若自己拼响应就会报出库里不存在的时刻。
//
// 事件 payload 只允许出现标识与档位编号（bucket/object_key/cdn 不进事件），
// 因此每个写路径用例都带 wantNoLeak。
// 交错用 db.onHit 构造「logic 读快照之后、写入之前别人抢改」；回滚会撤销钩子造出的态，
// 所以这里断言的是归因与副作用，不是交错后的最终库态（见 fakes_test.go 头注释）。

// ---------------------------------------------------------------- 请求构造与小工具

// upsertOutputReq 一份合法的档位登记请求（用例按需在副本上改字段）。
func upsertOutputReq(requestID string) *rpc.UpsertStreamOutputReq {
	return &rpc.UpsertStreamOutputReq{
		RoomId: testRoomID, LiveSessionId: testSession,
		BitrateLevel: rpc.BitrateLevel(testLevel), Protocol: rpc.StreamProtocol(testProtocol),
		Bucket: "live-hls", ObjectKey: "live/71001/88001/hd.m3u8", CdnDomain: "pull.example.com",
		Width: 1280, Height: 720, BitrateKbps: 3000, Fps: 30,
		RequestId: requestID, TraceId: "trace-output-1",
	}
}

func offlineOutputReq(outputID int64, reason rpc.FailureReason) *rpc.OfflineStreamOutputReq {
	return &rpc.OfflineStreamOutputReq{
		OutputId: outputID, Reason: reason,
		RequestId: "req-offline-1", TraceId: "trace-offline-1",
	}
}

func naturalOfflineReq(level, protocol int32) *rpc.OfflineStreamOutputReq {
	return &rpc.OfflineStreamOutputReq{
		RoomId: testRoomID, BitrateLevel: rpc.BitrateLevel(level), Protocol: rpc.StreamProtocol(protocol),
		Reason: rpc.FailureReason_FAILURE_REASON_SOURCE_LOST,
		// request_id 与定位方式无关：即使按自然键下线也必须带归因键（logic 先校验后定位）。
		RequestId: "req-offline-natural", TraceId: "trace-offline-natural",
	}
}

func listOutputsReq(pn, ps int32) *rpc.ListStreamOutputsReq {
	return &rpc.ListStreamOutputsReq{
		RoomId: testRoomID,
		Page:   &rpc.PageParam{Pn: pn, Ps: ps},
	}
}

func mustOutput(t *testing.T, db *store, outputID int64) *model.LiveStreamOutput {
	t.Helper()
	row, ok := db.outputs[outputID]
	if !ok {
		t.Fatalf("分发档位 %d 不存在（现有 %+v）", outputID, outputIDs(db))
	}
	return row
}

func outputIDs(db *store) []int64 {
	ids := make([]int64, 0, len(db.outputs))
	for id := range db.outputs {
		ids = append(ids, id)
	}
	return ids
}

func assertOutputRowUnchanged(t *testing.T, db *store, outputID int64, want model.LiveStreamOutput) {
	t.Helper()
	got := mustOutput(t, db, outputID)
	if *got != want {
		t.Fatalf("非法路径改动了档位行：\n got=%+v\nwant=%+v", *got, want)
	}
}

// wantOutputEchoesRow 钉住「响应 == 库里的行」：这是档位三个方法共同的契约，
// 逐列比对而不是只看 output_id —— 一旦投影串列（例如把 offline_reason 报成 request 的 reason），
// 观众侧就会拿到一个库里不存在的档位结论。
func wantOutputEchoesRow(t *testing.T, label string, info *rpc.StreamOutputInfo, row *model.LiveStreamOutput) {
	t.Helper()
	wantField(t, label, "output_id", info.GetOutputId(), row.OutputId)
	wantField(t, label, "room_id", info.GetRoomId(), row.RoomId)
	wantField(t, label, "live_session_id", info.GetLiveSessionId(), row.LiveSession)
	wantField(t, label, "task_id", info.GetTaskId(), row.TaskId)
	wantField(t, label, "bitrate_level", int32(info.GetBitrateLevel()), row.BitrateLevel)
	wantField(t, label, "protocol", int32(info.GetProtocol()), row.Protocol)
	wantField(t, label, "bucket", info.GetBucket(), row.Bucket)
	wantField(t, label, "object_key", info.GetObjectKey(), row.ObjectKey)
	wantField(t, label, "cdn_domain", info.GetCdnDomain(), row.CdnDomain)
	wantField(t, label, "width", info.GetWidth(), row.Width)
	wantField(t, label, "height", info.GetHeight(), row.Height)
	wantField(t, label, "bitrate_kbps", info.GetBitrateKbps(), row.BitrateKbps)
	wantField(t, label, "fps", info.GetFps(), row.Fps)
	wantField(t, label, "state", info.GetState(), row.State)
	wantField(t, label, "online_at", info.GetOnlineAt(), row.OnlineAt)
	wantField(t, label, "offline_at", info.GetOfflineAt(), row.OfflineAt)
	wantField(t, label, "online_expire_at", info.GetOnlineExpireAt(), row.OnlineExpire)
	wantField(t, label, "request_id", info.GetRequestId(), row.RequestId)
	wantField(t, label, "reason", int32(info.GetReason()), row.OfflineReason)
	wantField(t, label, "ctime", info.GetCtime(), row.Ctime)
	wantField(t, label, "mtime", info.GetMtime(), row.Mtime)
}

// ---------------------------------------------------------------- UpsertStreamOutput

func TestUpsertStreamOutputRegistersOnlineRowAndEmitsOneEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)

	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).
		UpsertStreamOutput(upsertOutputReq("req-out-1"))
	info = wantOK(t, info, err, "首次登记档位")

	if info.GetOutputId() <= 0 {
		t.Fatalf("登记必须返回自增 output_id，实际 %d", info.GetOutputId())
	}
	if len(db.outputs) != 1 {
		t.Fatalf("首次登记应恰好 1 行，实际 %d", len(db.outputs))
	}
	row := mustOutput(t, db, info.GetOutputId())
	wantOutputEchoesRow(t, "首次登记", info, row)

	// 落库真值：state 由 model 写成常量在线（入参的 state 根本不进 SQL），
	// offline_at/offline_reason 在 INSERT 的 VALUES 里写死 0。
	wantField(t, "首次登记", "state", row.State, model.StreamOutputStateOnline)
	wantField(t, "首次登记", "offline_at", row.OfflineAt, int64(0))
	wantField(t, "首次登记", "offline_reason", row.OfflineReason, int32(0))
	wantField(t, "首次登记", "request_id", row.RequestId, "req-out-1")
	wantField(t, "首次登记", "trace_id", row.TraceId, "trace-output-1")
	wantField(t, "首次登记", "bucket", row.Bucket, "live-hls")
	wantField(t, "首次登记", "object_key", row.ObjectKey, "live/71001/88001/hd.m3u8")
	if row.OnlineAt <= 0 || row.Ctime <= 0 {
		t.Errorf("上线时刻与登记时刻必须由服务端取时钟写入：online_at=%d ctime=%d", row.OnlineAt, row.Ctime)
	}

	wantEvents(t, db, []string{model.EventTypeStreamOutputOnline}, "首次登记")
	ev := eventAt(t, db, 0)
	wantField(t, "首次登记事件", "aggregate_type", ev.AggregateType, model.AggregateStreamOutput)
	wantField(t, "首次登记事件", "aggregate_id", ev.AggregateId, strconv.FormatInt(row.OutputId, 10))
	wantField(t, "首次登记事件", "room_id", ev.RoomId, testRoomID)
	p := eventPayload(t, ev)
	wantField(t, "首次登记事件", "payload.output_id", toInt64(t, p, "output_id"), row.OutputId)
	wantField(t, "首次登记事件", "payload.room_id", toInt64(t, p, "room_id"), row.RoomId)
	wantField(t, "首次登记事件", "payload.live_session_id", toInt64(t, p, "live_session_id"), row.LiveSession)
	wantField(t, "首次登记事件", "payload.bitrate_level", toInt64(t, p, "bitrate_level"), int64(testLevel))
	wantField(t, "首次登记事件", "payload.protocol", toInt64(t, p, "protocol"), int64(testProtocol))
	// 新行的 online_at 就是这次写入的值，事件与行必须同源（否则消费者按事件建投影会与主表打架）。
	wantField(t, "首次登记事件", "payload.online_at", toInt64(t, p, "online_at"), row.OnlineAt)
	// 可播引用绝不进事件。
	for _, forbidden := range []string{"bucket", "object_key", "cdn_domain"} {
		if _, ok := p[forbidden]; ok {
			t.Errorf("事件 payload 不得携带可播引用 %q：%v", forbidden, p)
		}
	}
	wantNoLeak(t, db, "首次登记")
}

// uniq_output_natural 的 upsert 语义：同键再登记必须**复用同一行**并复位下线结论，
// 同时保住两条审计事实（ctime 首次登记时刻、online_at 首次上线时刻）。
// 少任何一条都会破坏排障：档位行被反复新增后无法回答「这个档位第一次是什么时候上线的」。
func TestUpsertStreamOutputRefreshesSameNaturalKeyWithoutNewRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	first := nowTS() - 900
	seeded := seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.OnlineAt, o.Ctime, o.Mtime = first, first-100, first-100
		o.OfflineAt, o.OfflineReason = first+300, model.ReasonSourceLost
		o.Width, o.Height, o.BitrateKbps, o.Fps = 640, 360, 900, 15
		o.OnlineExpire = 12345
	})

	in := upsertOutputReq("req-out-re-online")
	in.ObjectKey = "live/71001/88001/hd-v2.m3u8"
	in.BitrateKbps, in.Fps = 4500, 60
	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
	info = wantOK(t, info, err, "同键再登记")

	wantField(t, "同键再登记", "复用主键", info.GetOutputId(), seeded.OutputId)
	if len(db.outputs) != 1 {
		t.Fatalf("同一天然键产生了第二行（uniq_output_natural 语义被破坏）：现有 %+v", outputIDs(db))
	}
	row := mustOutput(t, db, seeded.OutputId)
	wantOutputEchoesRow(t, "同键再登记", info, row)

	// 刷新的是「产物引用 + 参数快照」。
	wantField(t, "同键再登记", "object_key", row.ObjectKey, "live/71001/88001/hd-v2.m3u8")
	wantField(t, "同键再登记", "bitrate_kbps", row.BitrateKbps, int32(4500))
	wantField(t, "同键再登记", "fps", row.Fps, int32(60))
	wantField(t, "同键再登记", "request_id", row.RequestId, "req-out-re-online")
	// 复位下线结论 = 「重新在线」的全部语义。
	wantField(t, "同键再登记", "state", row.State, model.StreamOutputStateOnline)
	wantField(t, "同键再登记", "offline_at", row.OfflineAt, int64(0))
	wantField(t, "同键再登记", "offline_reason", row.OfflineReason, int32(0))
	// 审计事实不回写：online_at 仍是首次上线时刻（model 的 ON DUPLICATE 子句里没有 online_at）。
	// 注意：事件 payload 的 online_at 取本次请求时钟，与行的这一列在「再上线」时天然不同 ——
	// 行才是事实源（与 OfflineStreamOutput 不带 offline_at 的口径一致），故此处只钉行。
	wantField(t, "同键再登记", "online_at（首次上线时刻不回写）", row.OnlineAt, first)
	wantField(t, "同键再登记", "ctime（首次登记时刻不回写）", row.Ctime, first-100)
	if row.Mtime <= first-100 {
		t.Errorf("再上线必须刷新 mtime，实际 %d", row.Mtime)
	}
	// 未在入参里显式给的列不得被清空成别的值：online_expire_at 由入参覆盖（本次没给 → 0）。
	wantField(t, "同键再登记", "online_expire_at", row.OnlineExpire, int64(0))

	wantEvents(t, db, []string{model.EventTypeStreamOutputOnline}, "同键再登记")
	wantNoLeak(t, db, "同键再登记")
}

// 档位表的幂等键**不是** request_id：它上面没有唯一索引，同一天然键会反复上下线，
// 每次都刷新它。因此重复 request_id 不得被当成重放而吞掉第二次「重新上线」。
func TestUpsertStreamOutputDoesNotTreatRequestIDAsIdempotencyKey(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	first, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).
		UpsertStreamOutput(upsertOutputReq("req-out-shared"))
	first = wantOK(t, first, err, "第一次登记")
	// 同 request_id、同天然键：先把档位下线，再用同一个 request_id 上线。
	offReq := offlineOutputReq(first.GetOutputId(), rpc.FailureReason_FAILURE_REASON_MANUAL)
	if _, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(offReq); err != nil {
		t.Fatalf("下线失败（用例前提失效）：%v", err)
	}

	second, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).
		UpsertStreamOutput(upsertOutputReq("req-out-shared"))
	second = wantOK(t, second, err, "重复 request_id 的再登记")

	wantField(t, "重复 request_id", "复用同一行", second.GetOutputId(), first.GetOutputId())
	if got := mustOutput(t, db, second.GetOutputId()).State; got != model.StreamOutputStateOnline {
		t.Fatalf("复用 request_id 时第二次上线被当成幂等重放吞掉了：state=%d", got)
	}
	// 两次登记各自留一条上线事件（下线事件夹在中间），共 3 条。
	wantEvents(t, db, []string{
		model.EventTypeStreamOutputOnline,
		model.EventTypeStreamOutputOffline,
		model.EventTypeStreamOutputOnline,
	}, "重复 request_id 仍是两次真实上线")
	wantCalls(t, db, "StreamOutputs.UpsertTx", 0, 2, "重复 request_id 必须照常写")

	// 同 request_id 落在**另一个**天然键上同样不被拒（进一步证明它没有唯一约束）。
	other := upsertOutputReq("req-out-shared")
	other.BitrateLevel = rpc.BitrateLevel_BITRATE_LEVEL_SD
	third, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(other)
	third = wantOK(t, third, err, "跨档位复用同一 request_id")
	if third.GetOutputId() == first.GetOutputId() || len(db.outputs) != 2 {
		t.Fatalf("不同档位必须各成一行：output_id=%d/%d 现有 %d 行",
			first.GetOutputId(), third.GetOutputId(), len(db.outputs))
	}
	wantNoLeak(t, db, "request_id 非幂等键")
}

// task_id 只校验存在性与房间归属（模板与源地址的事实源在转码侧，不得复制主数据）。
func TestUpsertStreamOutputBindsTaskOfSameRoomWithoutCopyingItsData(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
		r.SourceRef = "rtmp://origin.example.com/live/room71001"
	})

	in := upsertOutputReq("req-out-with-task")
	in.TaskId = task.TaskId
	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
	info = wantOK(t, info, err, "绑定本房间转码任务")

	row := mustOutput(t, db, info.GetOutputId())
	wantField(t, "绑定任务", "task_id", row.TaskId, task.TaskId)
	// 档位行不得复制转码任务的源引用/模板（这几列在档位表里根本不存在，只能靠入参落库）。
	wantField(t, "绑定任务", "object_key 不是 source_ref", row.ObjectKey, in.GetObjectKey())
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "绑定任务", "payload.task_id", toInt64(t, p, "task_id"), task.TaskId)

	// task_id=0 是合法形态（源流直出），必须能登记。
	direct := upsertOutputReq("req-out-direct")
	direct.BitrateLevel = rpc.BitrateLevel_BITRATE_LEVEL_SOURCE
	direct.TaskId = 0
	info2, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(direct)
	info2 = wantOK(t, info2, err, "源流直出")
	wantField(t, "源流直出", "task_id", mustOutput(t, db, info2.GetOutputId()).TaskId, int64(0))
}

func TestUpsertStreamOutputRejectsInvalidTaskRefAndWritesNothing(t *testing.T) {
	t.Run("跨房间任务", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		foreign := seedTranscode(db, model.TranscodeStateRunning, func(r *model.LiveTranscodeTask) {
			r.RoomId = testRoomOther
		})
		in := upsertOutputReq("req-out-foreign-task")
		in.TaskId = foreign.TaskId
		before := snapshotWrites(db)

		info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
		wantFail(t, info, err, model.ErrInvalidRoomID, "跨房间任务")
		if !strings.Contains(err.Error(), "room") {
			t.Fatalf("错误里必须能看出是哪个房间的归属问题：%v", err)
		}
		wantNoWrites(t, db, before, "跨房间任务")
		wantEvents(t, db, nil, "跨房间任务")
	})

	t.Run("悬空或不存在的任务", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		before := snapshotWrites(db)
		in := upsertOutputReq("req-out-ghost-task")
		in.TaskId = 999999
		info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
		wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "悬空任务引用")
		if !strings.Contains(err.Error(), "task_id") {
			t.Fatalf("错误必须带上可归因的 task_id：%v", err)
		}

		negative := upsertOutputReq("req-out-negative-task")
		negative.TaskId = -1
		info, err = NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(negative)
		wantFail(t, info, err, model.ErrTranscodeTaskNotFound, "负数任务")
		wantNoWrites(t, db, before, "悬空任务引用")
	})
}

// 「登记即已过期」不是拒绝条件：Worker 可能在重放旧指令，拒绝会让它无限重投。
// 但过期值必须原样落库，否则清扫器（MarkExpiredOffline）看不到 to 点就没有依据。
func TestUpsertStreamOutputAcceptsAlreadyExpiredOnlineExpire(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	expired := nowTS() - 60
	in := upsertOutputReq("req-out-expired")
	in.OnlineExpireAt = expired

	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
	info = wantOK(t, info, err, "登记即过期")
	row := mustOutput(t, db, info.GetOutputId())
	wantField(t, "登记即过期", "online_expire_at", row.OnlineExpire, expired)
	wantField(t, "登记即过期", "state", row.State, model.StreamOutputStateOnline)
	wantEvents(t, db, []string{model.EventTypeStreamOutputOnline}, "登记即过期")
}

func TestUpsertStreamOutputValidatesInputAndWritesNothing(t *testing.T) {
	longRequestID := strings.Repeat("q", maxRequestIDRunes+1)
	longObjectKey := strings.Repeat("k", maxObjectKeyRunes+1)
	longCdn := strings.Repeat("a", maxCdnRunes+1) + ".example.com"

	cases := []struct {
		name   string
		mutate func(*rpc.UpsertStreamOutputReq)
		// want 为 nil 表示「必须被拒绝，但不保证包装了哨兵」（缺陷 #4：helpers.go 的列宽
		// 超限分支没有 %w，见 record_lifecycle_test.go 同一处标注）。
		want error
	}{
		{"房间号为零", func(r *rpc.UpsertStreamOutputReq) { r.RoomId = 0 }, model.ErrInvalidRoomID},
		{"房间号为负", func(r *rpc.UpsertStreamOutputReq) { r.RoomId = -1 }, model.ErrInvalidRoomID},
		{"场次号为零", func(r *rpc.UpsertStreamOutputReq) { r.LiveSessionId = 0 }, model.ErrInvalidSessionID},
		{"档位越界（0=UNSPECIFIED）", func(r *rpc.UpsertStreamOutputReq) { r.BitrateLevel = rpc.BitrateLevel_BITRATE_LEVEL_UNSPECIFIED }, model.ErrInvalidTransition},
		{"档位越界（7）", func(r *rpc.UpsertStreamOutputReq) { r.BitrateLevel = rpc.BitrateLevel(7) }, model.ErrInvalidTransition},
		{"协议越界", func(r *rpc.UpsertStreamOutputReq) { r.Protocol = rpc.StreamProtocol_STREAM_PROTOCOL_UNSPECIFIED }, model.ErrInvalidTransition},
		{"协议越界（5）", func(r *rpc.UpsertStreamOutputReq) { r.Protocol = rpc.StreamProtocol(5) }, model.ErrInvalidTransition},
		{"缺幂等键", func(r *rpc.UpsertStreamOutputReq) { r.RequestId = "   " }, model.ErrEmptyRequestID},
		{"幂等键超列宽", func(r *rpc.UpsertStreamOutputReq) { r.RequestId = longRequestID }, model.ErrEmptyRequestID},
		{"object_key 缺失", func(r *rpc.UpsertStreamOutputReq) { r.ObjectKey = "" }, model.ErrInvalidBucketRef},
		{"object_key 带签名参数", func(r *rpc.UpsertStreamOutputReq) { r.ObjectKey = leakObjectKey }, model.ErrInvalidBucketRef},
		{"object_key 写成绝对地址", func(r *rpc.UpsertStreamOutputReq) { r.ObjectKey = "https://cdn.example.com/a.m3u8" }, model.ErrInvalidBucketRef},
		{"object_key 以斜杠开头", func(r *rpc.UpsertStreamOutputReq) { r.ObjectKey = "/live/a.m3u8" }, model.ErrInvalidBucketRef},
		{"object_key 超列宽", func(r *rpc.UpsertStreamOutputReq) { r.ObjectKey = longObjectKey }, nil},
		{"桶名写成 URL", func(r *rpc.UpsertStreamOutputReq) { r.Bucket = "https://live-hls" }, model.ErrInvalidBucketRef},
		{"桶名为空", func(r *rpc.UpsertStreamOutputReq) { r.Bucket = "" }, model.ErrInvalidBucketRef},
		{"cdn_domain 带 scheme", func(r *rpc.UpsertStreamOutputReq) { r.CdnDomain = "https://pull.example.com" }, model.ErrInvalidBucketRef},
		{"cdn_domain 带路径", func(r *rpc.UpsertStreamOutputReq) { r.CdnDomain = "pull.example.com/live/a.m3u8" }, model.ErrInvalidBucketRef},
		{"cdn_domain 带签名参数", func(r *rpc.UpsertStreamOutputReq) { r.CdnDomain = "pull.example.com?x-amz-signature=" + leakMarker }, model.ErrInvalidBucketRef},
		{"cdn_domain 超列宽", func(r *rpc.UpsertStreamOutputReq) { r.CdnDomain = longCdn }, nil},
		{"宽为负", func(r *rpc.UpsertStreamOutputReq) { r.Width = -1 }, model.ErrInvalidBucketRef},
		{"高为负", func(r *rpc.UpsertStreamOutputReq) { r.Height = -1 }, model.ErrInvalidBucketRef},
		{"码率为负", func(r *rpc.UpsertStreamOutputReq) { r.BitrateKbps = -1 }, model.ErrInvalidBucketRef},
		{"帧率为负", func(r *rpc.UpsertStreamOutputReq) { r.Fps = -1 }, model.ErrInvalidBucketRef},
		{"在线有效期为负", func(r *rpc.UpsertStreamOutputReq) { r.OnlineExpireAt = -1 }, model.ErrInvalidTransition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			in := upsertOutputReq("req-out-bad")
			tc.mutate(in)
			before := snapshotWrites(db)

			info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
			if err == nil {
				t.Fatalf("%s：应被拒绝，实际返回 %+v", tc.name, info)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("%s：应报 %v，实际 err=%v", tc.name, tc.want, err)
			}
			if !isNilPtr(info) {
				t.Fatalf("%s：失败路径不得带回响应体：%+v", tc.name, info)
			}
			wantNoWrites(t, db, before, tc.name)
			wantEvents(t, db, nil, tc.name)
			wantNoLeak(t, db, tc.name)
		})
	}

	t.Run("cdn_domain 允许为空（未接 CDN 的直出档位）", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		in := upsertOutputReq("req-out-no-cdn")
		in.CdnDomain = "  "
		info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
		info = wantOK(t, info, err, "空 CDN 域名")
		wantField(t, "空 CDN 域名", "cdn_domain", mustOutput(t, db, info.GetOutputId()).CdnDomain, "")
	})
}

// 档位行与事件同事务：事件写失败必须连行一起回滚，否则观众侧出现「有档位无事件」的悬空分发。
func TestUpsertStreamOutputRollsBackRowWhenEventWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	db.failOn("Outbox.Insert", errOutboxDown)

	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).
		UpsertStreamOutput(upsertOutputReq("req-out-tx"))
	wantFail(t, info, err, errOutboxDown, "事件写失败")
	if len(db.outputs) != 0 {
		t.Fatalf("事务回滚后仍留下档位行：%+v", outputIDs(db))
	}
	wantEvents(t, db, nil, "事件写失败")
	wantCalls(t, db, "StreamOutputs.UpsertTx", 0, 1, "事务内确实尝试过写入")
	// 回滚掉的只是库态：入参快照仍是可断言的事实（state 由 model 写常量在线）。
	wantField(t, "事件写失败", "尝试写入的档位", triedOutput(t, db, 0).State, model.StreamOutputStateOnline)
}

// 并发交错：本事务的预读发生在对手提交之前，写入时天然键已被别人占。
// ON DUPLICATE KEY 分支必须收敛到既有行（复活它），既不新增第二行也不报错。
func TestUpsertStreamOutputConcurrentNaturalKeyWriteConvergesOnExistingRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	in := upsertOutputReq("req-out-race")
	var raced *model.LiveStreamOutput
	db.onHit("StreamOutputs.UpsertTx", func() {
		// 对手在本次 upsert 之前提交同一天然键的行，且处于已下线态。
		raced = seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
			o.RequestId = "req-out-by-other-worker"
		})
	})

	info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
	info = wantOK(t, info, err, "天然键竞态")

	if len(db.outputs) != 1 {
		t.Fatalf("竞态写出了第二行：现有 %+v", outputIDs(db))
	}
	wantField(t, "天然键竞态", "复用对手的行", info.GetOutputId(), raced.OutputId)
	row := mustOutput(t, db, raced.OutputId)
	wantField(t, "天然键竞态", "state", row.State, model.StreamOutputStateOnline)
	wantField(t, "天然键竞态", "offline_at 复位", row.OfflineAt, int64(0))
	wantField(t, "天然键竞态", "request_id 刷新", row.RequestId, "req-out-race")
	// 事件恰好一条，且指向复用的那一行（不留下指向不存在行的半条事件）。
	wantEvents(t, db, []string{model.EventTypeStreamOutputOnline}, "天然键竞态")
	if got := eventAt(t, db, 0).AggregateId; got != strconv.FormatInt(raced.OutputId, 10) {
		t.Fatalf("事件聚合主键必须指向被复用的行，实际 %q vs %d", got, raced.OutputId)
	}
}

func TestUpsertStreamOutputFailsClosedOnDependencyErrors(t *testing.T) {
	// StreamOutputs.FindByNaturalKey 不在列表里：它只在 ON DUPLICATE 分支被调用，
	// 首次登记（本用例的前提）走不到它，注入错误反而测不到任何东西。
	for _, op := range []string{"TranscodeTasks.FindOne", "StreamOutputs.UpsertTx",
		"StreamOutputs.FindOne", "Outbox.Insert", "DB.TransactCtx"} {
		t.Run(op, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			db.failOn(op, errModelDown)
			in := upsertOutputReq("req-out-down")
			if op == "TranscodeTasks.FindOne" {
				in.TaskId = 11 // 只有给了转码任务才会走这条读
			}
			before := snapshotWrites(db)

			info, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(in)
			if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
				t.Fatalf("%s 故障必须原样抛出（不得伪造成功）：%v", op, err)
			}
			if !isNilPtr(info) {
				t.Errorf("%s 故障路径不得带回响应体：%+v", op, info)
			}
			if op == "StreamOutputs.FindOne" {
				// 唯一「报错但写入仍在库」的读：回读发生在事务提交之后，撤销不了已提交的写。
				// 这里必须承认真实形态 —— 行和事件都已提交，只是响应失败；调用方重试会按天然键
				// 收敛到同一行（见 TestUpsertStreamOutputRefreshesSameNaturalKeyWithoutNewRow）。
				if len(db.outputs) != 1 {
					t.Fatalf("提交后回读故障应当保留 1 行（实际 %d），否则说明事务边界理解有误", len(db.outputs))
				}
				wantEvents(t, db, []string{model.EventTypeStreamOutputOnline}, "提交后回读故障：事件已随事务提交")
				return
			}
			wantEvents(t, db, nil, op+" 之后不得留下事件")
			if len(db.outputs) != 0 {
				t.Fatalf("%s 故障后仍留下档位行：%+v", op, outputIDs(db))
			}
			if op != "StreamOutputs.UpsertTx" && op != "Outbox.Insert" && op != "DB.TransactCtx" {
				wantNoWrites(t, db, before, op)
			}
		})
	}
}

// ---------------------------------------------------------------- OfflineStreamOutput

func TestOfflineStreamOutputByOutputIDWritesConclusionAndEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	online := seedOutput(db, model.StreamOutputStateOnline, nil)
	before := *mustOutput(t, db, online.OutputId)

	in := offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_SOURCE_LOST)
	in.RequestId = "req-offline-src"
	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(in)
	info = wantOK(t, info, err, "按主键下线")

	row := mustOutput(t, db, online.OutputId)
	wantOutputEchoesRow(t, "按主键下线", info, row)
	wantField(t, "按主键下线", "state", row.State, model.StreamOutputStateOffline)
	wantField(t, "按主键下线", "offline_reason", row.OfflineReason, model.ReasonSourceLost)
	wantField(t, "按主键下线", "trace_id", row.TraceId, "trace-offline-1")
	if row.OfflineAt <= 0 {
		t.Fatalf("offline_at 必须由服务端取时钟写入，实际 %d", row.OfflineAt)
	}
	// 下线不是删除：产物引用与档位参数必须原样保留，供回收任务与排障使用。
	wantField(t, "按主键下线", "bucket 保留", row.Bucket, before.Bucket)
	wantField(t, "按主键下线", "object_key 保留", row.ObjectKey, before.ObjectKey)
	wantField(t, "按主键下线", "bitrate_kbps 保留", row.BitrateKbps, before.BitrateKbps)
	wantField(t, "按主键下线", "ctime 保留", row.Ctime, before.Ctime)
	// 下线不覆写 request_id（那是 Upsert 侧的登记归因），也不碰 online_at。
	wantField(t, "按主键下线", "request_id 不被覆写", row.RequestId, before.RequestId)
	wantField(t, "按主键下线", "online_at 不被改写", row.OnlineAt, before.OnlineAt)

	wantEvents(t, db, []string{model.EventTypeStreamOutputOffline}, "按主键下线")
	ev := eventAt(t, db, 0)
	wantField(t, "下线事件", "aggregate_type", ev.AggregateType, model.AggregateStreamOutput)
	wantField(t, "下线事件", "aggregate_id", ev.AggregateId, strconv.FormatInt(online.OutputId, 10))
	p := eventPayload(t, ev)
	wantField(t, "下线事件", "payload.prev_state", toInt64(t, p, "prev_state"), int64(model.StreamOutputStateOnline))
	wantField(t, "下线事件", "payload.state", toInt64(t, p, "state"), int64(model.StreamOutputStateOffline))
	wantField(t, "下线事件", "payload.offline_reason", toInt64(t, p, "offline_reason"), int64(model.ReasonSourceLost))
	wantField(t, "下线事件", "payload.bitrate_level", toInt64(t, p, "bitrate_level"), int64(testLevel))
	wantField(t, "下线事件", "payload.request_id", p["request_id"], "req-offline-src")
	// offline_at 不进事件：时钟由 MarkOfflineTx 内部取，行本身才是事实源（避免同秒两处时间戳打架）。
	if _, ok := p["offline_at"]; ok {
		t.Errorf("事件不该带 offline_at（与行的时钟来源不同）：%v", p)
	}
	for _, forbidden := range []string{"bucket", "object_key", "cdn_domain"} {
		if _, ok := p[forbidden]; ok {
			t.Errorf("事件 payload 不得携带可播引用 %q：%v", forbidden, p)
		}
	}
	wantNoLeak(t, db, "按主键下线")
}

// 自然键定位（不带场次）：只有一场在线时必须精确命中该行。
func TestOfflineStreamOutputByNaturalKeyLocatesOnlineRow(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	target := seedOutput(db, model.StreamOutputStateOnline, nil)
	// 已下线的同档位历史行不得被选中（state 条件）。
	// 场次必须与在线行不同：同 (room,session,level,protocol) 的两行在 uniq_output_natural 下不存在。
	seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.LiveSession = testSession2
	})
	// 另一个档位的在线行也不得被顺手下线。
	other := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
	})

	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(naturalOfflineReq(testLevel, testProtocol))
	info = wantOK(t, info, err, "自然键下线")

	wantField(t, "自然键下线", "命中的行", info.GetOutputId(), target.OutputId)
	wantField(t, "自然键下线", "state", mustOutput(t, db, target.OutputId).State, model.StreamOutputStateOffline)
	wantField(t, "自然键下线", "另一个档位不受影响",
		mustOutput(t, db, other.OutputId).State, model.StreamOutputStateOnline)
	wantEvents(t, db, []string{model.EventTypeStreamOutputOffline}, "自然键下线")
	p := eventPayload(t, eventAt(t, db, 0))
	wantField(t, "自然键下线", "payload.live_session_id", toInt64(t, p, "live_session_id"), target.LiveSession)
}

// 同档位两场同时在线属数据异常：猜错对象会把还在分发的档位摘掉，必须报歧义而不是任选一行。
func TestOfflineStreamOutputRejectsAmbiguousNaturalKey(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedOutput(db, model.StreamOutputStateOnline, nil)
	b := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.OutputId, o.LiveSession = a.OutputId+1, testSession2
	})
	before := snapshotWrites(db)

	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(naturalOfflineReq(testLevel, testProtocol))
	wantFail(t, info, err, model.ErrStreamOutputAmbiguous, "同档位两场在线")
	if !strings.Contains(err.Error(), "output_id") {
		t.Errorf("歧义错误必须告诉调用方改用 output_id：%v", err)
	}
	wantNoWrites(t, db, before, "同档位两场在线")
	wantEvents(t, db, nil, "同档位两场在线")
	assertOutputRowUnchanged(t, db, a.OutputId, *a)
	assertOutputRowUnchanged(t, db, b.OutputId, *b)
}

// 跨房间拿错 ID 必须是归属错误，而不是「下线成功」。
func TestOfflineStreamOutputRejectsCrossRoomOutputID(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	foreign := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.RoomId = testRoomOther
	})
	before := snapshotWrites(db)

	in := offlineOutputReq(foreign.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
	in.RoomId = testRoomID
	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(in)
	wantFail(t, info, err, model.ErrInvalidRoomID, "跨房间下线")
	wantNoWrites(t, db, before, "跨房间下线")
	wantEvents(t, db, nil, "跨房间下线")
	assertOutputRowUnchanged(t, db, foreign.OutputId, *foreign)

	// 归属校验通过（room 与行一致）时不得因带 room 而被拒。
	okIn := offlineOutputReq(foreign.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
	okIn.RoomId = testRoomOther
	info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(okIn)
	info = wantOK(t, info, err, "同房间归属校验")
	wantField(t, "同房间归属校验", "state", info.GetState(), model.StreamOutputStateOffline)
}

// 「一次下线结论只记一次」：对已下线的行重复下线返回首次的结论，不覆写、不再发事件。
func TestOfflineStreamOutputReplayKeepsFirstConclusion(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seeded := seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.OfflineAt, o.OfflineReason = nowTS()-500, model.ReasonSourceLost
	})
	snapshot := *mustOutput(t, db, seeded.OutputId)
	before := snapshotWrites(db)

	in := offlineOutputReq(seeded.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
	in.RequestId = "req-offline-replay"
	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(in)
	info = wantOK(t, info, err, "重复下线")

	wantOutputEchoesRow(t, "重复下线", info, &snapshot)
	// 首次结论就是审计事实：人工重放不得把「源流丢失」改写成「人工」。
	wantField(t, "重复下线", "offline_reason 不被覆写", info.GetReason(),
		rpc.FailureReason_FAILURE_REASON_SOURCE_LOST)
	wantField(t, "重复下线", "offline_at 不被改写", info.GetOfflineAt(), snapshot.OfflineAt)
	wantNoWrites(t, db, before, "重复下线")
	wantEvents(t, db, nil, "重复下线不得补第二条事件")

	// 下线 → 再上线 → 再下线 是**新的一次下线**：必须写新的结论并再发一条事件。
	upIn := upsertOutputReq("req-offline-cycle") // 天然键与 seeded 行一致（room/session/level/protocol 同默认值）
	if _, err := NewUpsertStreamOutputLogic(context.Background(), svcCtx).UpsertStreamOutput(upIn); err != nil {
		t.Fatalf("再上线失败：%v", err)
	}
	reOff := offlineOutputReq(seeded.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
	reOff.RequestId = "req-offline-second"
	info2, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(reOff)
	info2 = wantOK(t, info2, err, "下线后重上线再下线")
	wantField(t, "再下线", "reason", int32(info2.GetReason()), model.ReasonManual)
	if info2.GetOfflineAt() <= snapshot.OfflineAt {
		t.Errorf("再下线必须写新的 offline_at：%d 应大于 %d", info2.GetOfflineAt(), snapshot.OfflineAt)
	}
	wantEvents(t, db, []string{
		model.EventTypeStreamOutputOnline,
		model.EventTypeStreamOutputOffline,
	}, "下线-上线-下线的事件顺序")
	wantNoLeak(t, db, "重复下线")
}

func TestOfflineStreamOutputUnknownRowAndValidation(t *testing.T) {
	t.Run("不存在的档位", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		before := snapshotWrites(db)
		info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
			OfflineStreamOutput(offlineOutputReq(4242, rpc.FailureReason_FAILURE_REASON_MANUAL))
		wantFail(t, info, err, model.ErrStreamOutputNotFound, "不存在的档位")
		wantNoWrites(t, db, before, "不存在的档位")
		wantEvents(t, db, nil, "不存在的档位")

		// 只走自然键、不给 room：等价于「全房间下线」的口子，必须拒绝。
		noRoom := naturalOfflineReq(testLevel, testProtocol)
		noRoom.RoomId = 0
		info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(noRoom)
		wantFail(t, info, err, model.ErrInvalidRoomID, "自然键缺房间")
		// 档位/协议非法：不得走到 FindOnlineByLevel（否则 room 条件之外的越界值会进 SQL）。
		for _, mutate := range []func(*rpc.OfflineStreamOutputReq){
			func(r *rpc.OfflineStreamOutputReq) { r.BitrateLevel = rpc.BitrateLevel_BITRATE_LEVEL_UNSPECIFIED },
			func(r *rpc.OfflineStreamOutputReq) { r.Protocol = rpc.StreamProtocol_STREAM_PROTOCOL_UNSPECIFIED },
		} {
			bad := naturalOfflineReq(testLevel, testProtocol)
			mutate(bad)
			info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(bad)
			wantFail(t, info, err, model.ErrInvalidTransition, "自然键枚举越界")
		}
		wantNoWrites(t, db, before, "定位入口校验")
	})

	t.Run("入参校验", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		online := seedOutput(db, model.StreamOutputStateOnline, nil)
		before := snapshotWrites(db)

		negativeID := offlineOutputReq(-1, rpc.FailureReason_FAILURE_REASON_MANUAL)
		info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(negativeID)
		wantFail(t, info, err, model.ErrStreamOutputNotFound, "负的 output_id")

		noReason := offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_UNSPECIFIED)
		info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(noReason)
		wantFail(t, info, err, model.ErrInvalidTransition, "下线原因未指定")
		if !strings.Contains(err.Error(), "reason") {
			t.Errorf("原因缺失的错误必须说明要具体原因：%v", err)
		}
		outOfRange := offlineOutputReq(online.OutputId, rpc.FailureReason(99))
		info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(outOfRange)
		wantFail(t, info, err, model.ErrInvalidTransition, "下线原因越界")

		noKey := offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
		noKey.RequestId = ""
		info, err = NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(noKey)
		wantFail(t, info, err, model.ErrEmptyRequestID, "缺幂等键")

		wantNoWrites(t, db, before, "入参校验")
		wantEvents(t, db, nil, "入参校验")
	})

	t.Run("未知状态行不是已知档位态", func(t *testing.T) {
		db := newStore()
		svcCtx := newTestSvc(db)
		// 脏数据（迁移或人工修表造出的第三种 state）：既不是在线也不是可幂等重放的离线。
		dirty := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.State = 9
		})
		before := snapshotWrites(db)

		info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
			OfflineStreamOutput(offlineOutputReq(dirty.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL))
		wantFail(t, info, err, model.ErrInvalidTransition, "未知档位状态")
		wantNoWrites(t, db, before, "未知档位状态")
		wantEvents(t, db, nil, "未知档位状态")
		assertOutputRowUnchanged(t, db, dirty.OutputId, *dirty)
	})
}

// 竞态：CAS 命中 0 行且回读确认已下线 → 这是「别人也下线了」，
// 必须返回成功且不补第二条事件（同一次下线只留一份证据）。
func TestOfflineStreamOutputLosesRaceWithoutSecondEvent(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	online := seedOutput(db, model.StreamOutputStateOnline, nil)
	db.onHit("StreamOutputs.MarkOfflineTx", func() {
		row := db.outputs[online.OutputId]
		row.State, row.OfflineAt, row.OfflineReason = model.StreamOutputStateOffline, nowTS()-5, model.ReasonSourceLost
	})

	in := offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(in)
	info = wantOK(t, info, err, "下线竞态")

	wantField(t, "下线竞态", "state", info.GetState(), model.StreamOutputStateOffline)
	wantField(t, "下线竞态", "别人的结论不被覆写", int32(info.GetReason()), model.ReasonSourceLost)
	wantCalls(t, db, "StreamOutputs.MarkOfflineTx", 0, 1, "CAS 只发生一次")
	wantEvents(t, db, nil, "CAS 0 行不得补第二条下线事件")
	wantCalls(t, db, "Outbox.Insert", 0, 0, "CAS 0 行不得尝试写事件")
}

// 竞态之后行落在「既非在线也非下线」的第三种态：数据异常，必须报错且整笔事务回滚，
// 绝不能既下线又发事件（回滚后库里仍是在线行，归因来自回读）。
func TestOfflineStreamOutputRaceToUnknownStateFailsAndRollsBack(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	online := seedOutput(db, model.StreamOutputStateOnline, nil)
	snapshot := *mustOutput(t, db, online.OutputId)
	db.onHit("StreamOutputs.MarkOfflineTx", func() {
		db.outputs[online.OutputId].State = 9
	})

	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL))
	wantFail(t, info, err, model.ErrInvalidTransition, "竞态后状态未知")
	if !strings.Contains(err.Error(), "lost race") {
		t.Errorf("错误必须说明是竞态失败：%v", err)
	}
	wantEvents(t, db, nil, "竞态失败不得留下事件")
	assertOutputRowUnchanged(t, db, online.OutputId, snapshot)
}

// 事件行不存在=不在线（FindOnlineByLevel 查无此行）：必须报不存在而不是成功。
func TestOfflineStreamOutputNaturalKeyMissIsNotFound(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	before := snapshotWrites(db)
	// 库里只有一行**已下线**的档位：自然键路径按在线找，必然找不到。
	seedOutput(db, model.StreamOutputStateOffline, nil)

	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(naturalOfflineReq(testLevel, testProtocol))
	wantFail(t, info, err, model.ErrStreamOutputNotFound, "无在线档位可下线")
	wantNoWrites(t, db, before, "无在线档位可下线")
	wantEvents(t, db, nil, "无在线档位可下线")
}

func TestOfflineStreamOutputRollsBackWhenEventWriteFails(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	online := seedOutput(db, model.StreamOutputStateOnline, nil)
	snapshot := *mustOutput(t, db, online.OutputId)
	db.failOn("Outbox.Insert", errOutboxDown)

	info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).
		OfflineStreamOutput(offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_CDN))
	wantFail(t, info, err, errOutboxDown, "下线事件写失败")
	// 观众侧不允许出现「库里已下线但没有事件」的分发口径分裂。
	assertOutputRowUnchanged(t, db, online.OutputId, snapshot)
	wantCalls(t, db, "StreamOutputs.MarkOfflineTx", 0, 1, "事务内确实尝试过 CAS")
	wantEvents(t, db, nil, "下线事件写失败")
}

func TestOfflineStreamOutputFailsClosedOnDependencyErrors(t *testing.T) {
	for _, op := range []string{"StreamOutputs.FindOne", "StreamOutputs.FindOnlineByLevel",
		"StreamOutputs.MarkOfflineTx", "Outbox.Insert", "DB.TransactCtx"} {
		t.Run(op, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			online := seedOutput(db, model.StreamOutputStateOnline, nil)
			snapshot := *mustOutput(t, db, online.OutputId)
			db.failOn(op, errModelDown)

			in := offlineOutputReq(online.OutputId, rpc.FailureReason_FAILURE_REASON_MANUAL)
			if op == "StreamOutputs.FindOnlineByLevel" {
				in = naturalOfflineReq(testLevel, testProtocol)
				in.RequestId = "req-offline-natural"
			}
			info, err := NewOfflineStreamOutputLogic(context.Background(), svcCtx).OfflineStreamOutput(in)
			if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
				t.Fatalf("%s 故障必须原样抛出：%v", op, err)
			}
			if !isNilPtr(info) {
				t.Errorf("%s 故障路径不得带回响应体：%+v", op, info)
			}
			assertOutputRowUnchanged(t, db, online.OutputId, snapshot)
			wantEvents(t, db, nil, op+" 之后不得留下事件")
		})
	}
}

// ---------------------------------------------------------------- ListStreamOutputs

// 默认口径：只要该房间的在线档位（播放器选档），按 (bitrate_level, protocol) 稳定升序。
func TestListStreamOutputsReturnsOnlineRowsOfTheRoom(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	// 故意按乱序播种，验证排序来自查询口径而不是 map 遍历顺序。
	sdOnline := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SD)
	})
	hlsOnline := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.Protocol = int32(rpc.StreamProtocol_STREAM_PROTOCOL_RTMP)
	})
	offOnline := seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_UHD)
	})
	otherRoom := seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
		o.RoomId = testRoomOther
		o.BitrateLevel = int32(rpc.BitrateLevel_BITRATE_LEVEL_SOURCE)
	})
	before := snapshotWrites(db)

	reply, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(listOutputsReq(1, 10))
	reply = wantOK(t, reply, err, "列档位")

	got := reply.GetOutputs()
	if len(got) != 2 {
		t.Fatalf("应恰好返回本房间 2 个在线档位，实际 %d 条：%+v", len(got), got)
	}
	wantField(t, "列档位", "总数（不含下线与其它房间）", reply.GetPage().GetTotal(), int32(2))
	// ORDER BY bitrate_level ASC, protocol ASC：HD(3) 排在 SD(4) 之前，与播种顺序无关。
	wantField(t, "列档位", "第 1 条档位", int32(got[0].GetBitrateLevel()), testLevel)
	wantField(t, "列档位", "第 2 条档位", int32(got[1].GetBitrateLevel()), int32(rpc.BitrateLevel_BITRATE_LEVEL_SD))
	wantField(t, "列档位", "第 1 条协议", int32(got[0].GetProtocol()), int32(rpc.StreamProtocol_STREAM_PROTOCOL_RTMP))
	wantOutputEchoesRow(t, "列档位第 1 条", got[0], mustOutput(t, db, hlsOnline.OutputId))
	wantOutputEchoesRow(t, "列档位第 2 条", got[1], mustOutput(t, db, sdOnline.OutputId))

	for _, o := range got {
		if o.GetOutputId() == offOnline.OutputId || o.GetOutputId() == otherRoom.OutputId {
			t.Fatalf("下线行或别房间的行泄漏到默认口径：%+v", o)
		}
		// 只回引用，不回签名地址（可播地址由 live-gateway 侧签名）。
		if strings.Contains(o.GetObjectKey(), "x-amz") || strings.Contains(o.GetCdnDomain(), "://") {
			t.Errorf("档位投影带出了可播地址形态：%s/%s", o.GetCdnDomain(), o.GetObjectKey())
		}
	}
	// 只读方法：零写、零事件，并且缓存未配置时必须回源主表（不得把「读不到缓存」当成空结果）。
	wantNoWrites(t, db, before, "列档位")
	wantEvents(t, db, nil, "列档位")
	wantCalls(t, db, "StreamOutputs.ListByRoom", 0, 1, "缓存缺失必须回源")
}

// include_offline 与 live_session_id 两条过滤口径必须原样透传给 model（返回值看不出区别）。
func TestListStreamOutputsPassesFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	cur := seedOutput(db, model.StreamOutputStateOnline, nil)
	hist := seedOutput(db, model.StreamOutputStateOffline, func(o *model.LiveStreamOutput) {
		o.LiveSession = testSession2
	})

	in := listOutputsReq(1, 10)
	in.LiveSessionId = testSession
	in.IncludeOffline = true
	before := snapshotWrites(db)
	reply, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(in)
	reply = wantOK(t, reply, err, "按场次含下线列档位")

	q := db.lastOutputList
	if q == nil {
		t.Fatal("ListByRoom 未被调用，过滤口径无法验证")
	}
	wantField(t, "含下线列档位", "透传 room_id", q.roomID, testRoomID)
	wantField(t, "含下线列档位", "透传 session_id", q.sessionID, testSession)
	wantField(t, "含下线列档位", "透传 include_offline", q.includeOffline, true)
	// 本房间在线行属于该场次，历史行的 session 是 testSession2：只应返回一行。
	wantField(t, "含下线列档位", "命中行", reply.GetOutputs()[0].GetOutputId(), cur.OutputId)
	wantField(t, "含下线列档位", "total", reply.GetPage().GetTotal(), int32(1))

	// 不给 session、含下线：两场的历史都带出来（排障口径）。
	in2 := listOutputsReq(1, 10)
	in2.IncludeOffline = true
	reply2, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(in2)
	reply2 = wantOK(t, reply2, err, "含下线列档位")
	wantField(t, "含下线列档位", "透传 session_id（不过滤）", db.lastOutputList.sessionID, int64(0))
	wantField(t, "含下线列档位", "行数", int32(len(reply2.GetOutputs())), int32(2))
	wantField(t, "含下线列档位", "total", reply2.GetPage().GetTotal(), int32(2))
	// 两行都要出现（含下线的意义就是把历史带出来）；同 (档位,协议) 的行序生产 SQL 不保证，
	// 因此这里只按集合断言，不把 fake 的 output_id 兜底排序当成契约钉住。
	seen := map[int64]bool{}
	for _, o := range reply2.GetOutputs() {
		seen[o.GetOutputId()] = true
		wantField(t, "含下线列档位", "state 如实返回", o.GetState(), outputStateOf(db, o.GetOutputId()))
	}
	if !seen[cur.OutputId] || !seen[hist.OutputId] {
		t.Errorf("含下线的列表缺少行：got=%v want 含 %d 与 %d", seen, cur.OutputId, hist.OutputId)
	}
	wantNoWrites(t, db, before, "含下线列档位")
	wantEvents(t, db, nil, "含下线列档位")
}

// outputStateOf 读库里该行的 state 列：用于「投影不得自己推断在线态」的比对。
func outputStateOf(db *store, outputID int64) int32 {
	return db.outputs[outputID].State
}

// 分页口径：ps 越界夹到 MaxListPageSize（与 model.clampPage 同式，因此透传的 limit 就是 SQL 的 LIMIT），
// 页大小 0 取默认 20；pn 超深翻页窗口是拒绝而不是扫全表。
func TestListStreamOutputsPagingAndClamping(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < 3; i++ {
		seedOutput(db, model.StreamOutputStateOnline, func(o *model.LiveStreamOutput) {
			o.BitrateLevel = int32(2 + i) // UHD/HD/SD 三档
		})
	}

	reply, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(listOutputsReq(1, 2))
	reply = wantOK(t, reply, err, "第一页")
	wantField(t, "第一页", "行数", int32(len(reply.GetOutputs())), int32(2))
	wantField(t, "第一页", "total 是全量数而非本页数", reply.GetPage().GetTotal(), int32(3))
	wantField(t, "第一页", "透传 pn", db.lastOutputList.pn, int32(1))
	wantField(t, "第一页", "透传 ps", db.lastOutputList.ps, int32(2))
	wantField(t, "第一页", "透传 maxPS", db.lastOutputList.maxPS, int32(50))

	reply2, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(listOutputsReq(2, 2))
	reply2 = wantOK(t, reply2, err, "第二页")
	wantField(t, "第二页", "行数", int32(len(reply2.GetOutputs())), int32(1))
	if reply2.GetOutputs()[0].GetOutputId() == reply.GetOutputs()[0].GetOutputId() {
		t.Errorf("第二页与第一页首行相同：OFFSET 没生效")
	}

	// ps 越界 → 夹到默认 20（proto 承诺「超限取默认」）。
	overSized := listOutputsReq(1, 999)
	if _, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(overSized); err != nil {
		t.Fatalf("ps 越界应夹取而不是报错：%v", err)
	}
	wantField(t, "ps 越界", "夹取后的 ps", db.lastOutputList.ps, int32(20))
	// ps 为 0/负 → 同样取默认 20；pn<1 视为 1。
	if _, err := NewListStreamOutputsLogic(context.Background(), svcCtx).
		ListStreamOutputs(listOutputsReq(0, -5)); err != nil {
		t.Fatalf("pn/ps 非正数应归一而不是报错：%v", err)
	}
	wantField(t, "pn/ps 归一", "pn", db.lastOutputList.pn, int32(1))
	wantField(t, "pn/ps 归一", "ps", db.lastOutputList.ps, int32(20))

	// 深翻页：offset=(pn-1)*ps 超出 maxListOffset 必须拒绝（且不调用 model）。
	before := snapshotWrites(db)
	listCalls := db.count("StreamOutputs.ListByRoom")
	deep := listOutputsReq(int32(maxListOffset/20+2), 20)
	info, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(deep)
	wantFail(t, info, err, model.ErrInvalidPage, "深翻页")
	if !strings.Contains(err.Error(), "OFFSET") {
		t.Errorf("错误必须说明是 OFFSET 代价失控：%v", err)
	}
	wantCalls(t, db, "StreamOutputs.ListByRoom", listCalls, 0, "深翻页不得扫库")
	wantNoWrites(t, db, before, "深翻页")

	// 窗口边缘（offset == maxListOffset）仍允许，证明判定用的是夹取后的 ps。
	edge := listOutputsReq(int32(maxListOffset/20+1), 999)
	if _, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(edge); err != nil {
		t.Fatalf("恰好落在窗口边缘不应拒绝：%v", err)
	}
}

func TestListStreamOutputsRejectsMissingRoomAndFailsClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedOutput(db, model.StreamOutputStateOnline, nil)
	before := snapshotWrites(db)

	for _, roomID := range []int64{0, -1} {
		in := listOutputsReq(1, 10)
		in.RoomId = roomID
		reply, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(in)
		wantFail(t, reply, err, model.ErrInvalidRoomID, "房间号为零的列档位")
	}
	// 本方法不提供「全局列档位」入口：一个 model 方法都不许被调用。
	wantNoWrites(t, db, before, "房间号为零的列档位")
	wantCalls(t, db, "StreamOutputs.ListByRoom", 0, 0, "缺 room 不得查库")

	db.failOn("StreamOutputs.ListByRoom", errModelDown)
	reply, err := NewListStreamOutputsLogic(context.Background(), svcCtx).ListStreamOutputs(listOutputsReq(1, 10))
	if err == nil || !strings.Contains(err.Error(), errModelDown.Error()) {
		t.Fatalf("列档位读故障必须原样抛出（不得伪装成空列表）：%v", err)
	}
	if !isNilPtr(reply) {
		t.Errorf("读故障路径不得带回响应体：%+v", reply)
	}
	wantEvents(t, db, nil, "列档位读故障")
}
