package logic

// createroom_logic_test.go 覆盖写侧方法 CreateRoom。
//
// 这里锁的四类结论：
//  1. 入参守卫**全部**在触库之前（表驱动，每条都断言「此后一次依赖调用都不许发生」）；
//  2. 落库形态：房间行 + 房主绑定 + 配置行 + 审计日志在同一个 TransactCtx 里按固定顺序写，
//     四行都得读回来看（假件不回滚，所以中途失败时「还剩哪几行」是可观察的事实）；
//  3. 送审是事务提交**之后**的外部往返：失败必须吞掉并保持 verify_state=NONE 的事实，
//     成功才回写 REVIEWING + 任务 ID；
//  4. request_id 的三种形态：首次受理 / 命中重放（零新写）/ 键被别人用过（拒绝）。
//
// 生成的 room_id 是 AUTO_INCREMENT，不进期望序列的字面量：用例先读 reply.GetRoomId()
// 再把它拼进 wantSeq，这等价于「断言配置行确实挂在刚建出来的那个房间上」，比写死数字更强。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

const (
	createroomTestNow  int64 = 1_700_000_000
	createroomTestMid  int64 = 9001
	createroomTestArea       = 7001
)

// createRoomReq 是一条「除 request_id 外都合法」的创建请求，用例只改自己关心的字段。
func createRoomReq(reqID string) *rpc.CreateRoomReq {
	return &rpc.CreateRoomReq{
		Mid:       createroomTestMid,
		Title:     "深夜电台",
		Cover:     "cover/night.jpg",
		AreaId:    createroomTestArea,
		Platform:  rpc.Platform_PLATFORM_IOS,
		RequestId: reqID,
		TraceId:   "trace-1",
	}
}

// newCreateLogic 装配一个「接了 moderation、其余下游为 nil」的上下文。
// 不传 conf 时用与 etc/live-room.yaml 对齐的默认配置。
func newCreateLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *CreateRoomLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewCreateRoomLogic(context.Background(), st.wireModeration().svcCtxWith(c))
}

func TestCreateRoomHappyPathWritesFourRowsInOneTransaction(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	lg := newCreateLogic(t, st)

	reply, err := lg.CreateRoom(createRoomReq("req-create-1"))
	wantNoErr(t, "CreateRoom", err)
	defer st.checkRaces(t)

	roomID := reply.GetRoomId()
	wantEQ(t, "CreateRoom", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_PENDING)
	wantEQ(t, "CreateRoom", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REVIEWING)
	wantEQ(t, "CreateRoom", "moderation_task_id", reply.GetModerationTaskId(), int64(9001))
	wantEQ(t, "CreateRoom", "replayed", reply.GetReplayed(), false)

	// 完整轨迹：分区可用性 → 抢键 → 上限 → 事务（4 条写）→ 送审 → 回写资料态 → 回填结果。
	wantSeq(t, "CreateRoom 首次执行", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-create-1",
		"live_room.CountByOwner:9001",
		"db.TransactCtx",
		"live_room.InsertTx",
		"live_room_anchor.BindTx:m9001",
		fmt.Sprintf("live_room_setting.UpsertTx:%d", roomID),
		"live_room_state_log.InsertTx:t1:0->1:create_room",
		fmt.Sprintf("moderation.SubmitForReview:%d", roomID),
		fmt.Sprintf("live_room.UpdateProfile:%d:v2/t9001", roomID),
		"live_room_state_log.Insert:t2:1->2:profile_submitted",
		"live_room_idempotency.SaveResult:req-create-1",
	)
	wantTxCount(t, "CreateRoom 落库", st.conn, 1)

	row := st.roomAt(t, roomID)
	wantEQ(t, "房间行", "owner_mid", row.OwnerMid, createroomTestMid)
	wantEQ(t, "房间行", "title", row.Title, "深夜电台")
	wantEQ(t, "房间行", "cover", row.Cover, "cover/night.jpg")
	wantEQ(t, "房间行", "area_id", row.AreaID, int64(createroomTestArea))
	wantEQ(t, "房间行", "state", row.State, model.RoomStatePending)
	wantEQ(t, "房间行", "verify_state", row.VerifyState, model.VerifyStateReviewing)
	wantEQ(t, "房间行", "moderation_task_id", row.ModerationTaskID, int64(9001))
	wantEQ(t, "房间行", "platform", row.Platform, model.PlatformIOS)
	// 创建不是状态迁移：state_version 保持 1（只有 Transition 会 ++）。
	wantEQ(t, "房间行", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "房间行", "ctime", row.Ctime, createroomTestNow)
	wantEQ(t, "房间行", "mtime", row.Mtime, createroomTestNow)

	anchor := st.anchorRow(t, roomID, createroomTestMid, model.AnchorRoleOwner)
	wantEQ(t, "房主绑定", "state", anchor.State, model.BindStateEnabled)
	wantEQ(t, "房主绑定", "owner_room_id", anchor.OwnerRoomID.Int64, roomID)

	setting := st.settingAt(t, roomID)
	wantEQ(t, "配置行", "danmaku_enabled", setting.DanmakuEnabled, model.BoolToInt32(true))
	wantEQ(t, "配置行", "record_enabled", setting.RecordEnabled, model.BoolToInt32(false))
	wantEQ(t, "配置行", "live_type", setting.LiveType, model.LiveTypeVideo)
	wantEQ(t, "配置行", "ctime", setting.Ctime, createroomTestNow)

	logs := st.logsOf(roomID)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（建房 + 送审）", len(logs))
	}
	wantEQ(t, "建房日志", "state_type", logs[0].StateType, model.LogTypeRoomState)
	wantEQ(t, "建房日志", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "0->1")
	wantEQ(t, "建房日志", "source", logs[0].Source, model.SourceRPCClient)
	wantEQ(t, "建房日志", "request_id", logs[0].RequestID, "req-create-1")
	wantEQ(t, "建房日志", "operator_mid", logs[0].OperatorMid, createroomTestMid)
	wantEQ(t, "送审日志", "state_type", logs[1].StateType, model.LogTypeVerifyState)
	wantEQ(t, "送审日志", "from->to", fmt.Sprintf("%d->%d", logs[1].FromState, logs[1].ToState), "1->2")
	wantEQ(t, "送审日志", "reason", logs[1].Reason, "profile_submitted")

	// 送审入参：business 取配置、submission_id 就是刚建出来的房间。
	sub := st.mod.submits[0]
	wantEQ(t, "送审入参", "submission_id", sub.GetSubmissionId(), roomID)
	wantEQ(t, "送审入参", "business", sub.GetBusiness(), "live")
	wantEQ(t, "送审入参", "reason", sub.GetReason(), "live_room_created")
	wantEQ(t, "送审入参", "mid", sub.GetMid(), createroomTestMid)

	// 结果快照已回填，且回填的是首次执行的 reply（重放要拿到同一个 room_id）。
	rec := st.idemAt("req-create-1")
	if rec == nil {
		t.Fatal("去重键未登记")
	}
	wantEQ(t, "去重键", "rpc", rec.Rpc, "CreateRoom")
	wantEQ(t, "去重键", "room_id", rec.RoomID, int64(0))
	if !strings.Contains(rec.ResultJSON, fmt.Sprintf("\"room_id\":%d", roomID)) {
		t.Errorf("去重结果快照 = %q, want 含刚分配的 room_id", rec.ResultJSON)
	}
	wantDeepEQ(t, "落库面", "counts", st.counts(), storeCounts{
		// areas 计 1：用例种子了一条启用分区（CreateRoom 要校验它），不是被测代码写的行。
		rooms: 1, settings: 1, anchors: 1, areas: 1, logs: 2, idem: 1,
	})
}

func TestCreateRoomGuardsRejectBeforeAnyDependencyCall(t *testing.T) {
	longTitle := strings.Repeat("标", 81)
	cases := []struct {
		name     string
		mutate   func(*rpc.CreateRoomReq)
		wantErr  error
		wantFrag string
	}{
		{"mid 为 0", func(r *rpc.CreateRoomReq) { r.Mid = 0 }, model.ErrInvalidMid, ""},
		{"mid 为负", func(r *rpc.CreateRoomReq) { r.Mid = -1 }, model.ErrInvalidMid, ""},
		{"request_id 空", func(r *rpc.CreateRoomReq) { r.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"request_id 全空白", func(r *rpc.CreateRoomReq) { r.RequestId = "   " }, model.ErrRequestIDRequired, ""},
		{"request_id 超 64 字节", func(r *rpc.CreateRoomReq) { r.RequestId = strings.Repeat("x", 65) },
			model.ErrDedupIDTooLong, "max 64"},
		{"标题空", func(r *rpc.CreateRoomReq) { r.Title = "" }, model.ErrTitleInvalid, ""},
		{"标题只有空格", func(r *rpc.CreateRoomReq) { r.Title = "  " }, model.ErrTitleInvalid, ""},
		{"标题 81 字符", func(r *rpc.CreateRoomReq) { r.Title = longTitle }, model.ErrTitleInvalid, "81 > 80"},
		{"封面是绝对 URL", func(r *rpc.CreateRoomReq) { r.Cover = "https://cdn/x.jpg" },
			model.ErrCoverTooLong, "只允许 object key"},
		{"封面超 512 字符", func(r *rpc.CreateRoomReq) { r.Cover = strings.Repeat("c", 513) },
			model.ErrCoverTooLong, "513 > 512"},
		{"area_id 为 0", func(r *rpc.CreateRoomReq) { r.AreaId = 0 }, model.ErrInvalidAreaID, ""},
		{"area_id 为负", func(r *rpc.CreateRoomReq) { r.AreaId = -3 }, model.ErrInvalidAreaID, ""},
		{"未定义平台", func(r *rpc.CreateRoomReq) { r.Platform = rpc.Platform(99) },
			model.ErrPlatformInvalid, "99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
			in := createRoomReq("req-guard")
			tc.mutate(in)

			reply, err := newCreateLogic(t, st).CreateRoom(in)

			if tc.wantFrag == "" {
				wantErrIs(t, tc.name, err, tc.wantErr)
			} else {
				wantErrContains(t, tc.name, err, tc.wantFrag)
				wantErrIs(t, tc.name, err, tc.wantErr)
			}
			wantEQ(t, tc.name, "reply", reply, nil)
			// 守卫拒绝必须一次依赖调用都不发生（含抢键）。
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantKeyUnburned(t, tc.name, "req-guard", st)
		})
	}
}

func TestCreateRoomNilRequestIsRejected(t *testing.T) {
	st := newStore()
	reply, err := newCreateLogic(t, st).CreateRoom(nil)
	wantErrIs(t, "CreateRoom(nil)", err, model.ErrInvalidMid)
	wantEQ(t, "CreateRoom(nil)", "reply", reply, nil)
	wantNoCallAfter(t, "CreateRoom(nil)", st.log, 0)
}

func TestCreateRoomSettingValidationRejectsBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name     string
		setting  *rpc.RoomSetting
		wantFrag string
	}{
		{"未定义直播类型", &rpc.RoomSetting{LiveType: 99}, "live_type=99"},
		{"最低版本号负数", &rpc.RoomSetting{MinClientVersionCode: -1}, "min_client_version_code=-1"},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
		in := createRoomReq("req-setting-guard")
		in.Setting = tc.setting

		reply, err := newCreateLogic(t, st).CreateRoom(in)

		wantErrContains(t, tc.name, err, tc.wantFrag)
		wantErrIs(t, tc.name, err, model.ErrSettingInvalid)
		wantEQ(t, tc.name, "reply", reply, nil)
		wantNoCallAfter(t, tc.name, st.log, 0)
	}
}

func TestCreateRoomNilSettingUsesServerDefaults(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-default-setting"))
	wantNoErr(t, "CreateRoom(默认配置)", err)

	s := st.settingAt(t, reply.GetRoomId())
	wantEQ(t, "默认配置", "danmaku", s.DanmakuEnabled, model.BoolToInt32(true))
	wantEQ(t, "默认配置", "reply", s.ReplyEnabled, model.BoolToInt32(true))
	wantEQ(t, "默认配置", "record", s.RecordEnabled, model.BoolToInt32(false))
	wantEQ(t, "默认配置", "linkmic", s.LinkmicEnabled, model.BoolToInt32(false))
	wantEQ(t, "默认配置", "live_type", s.LiveType, model.LiveTypeVideo)
	wantEQ(t, "默认配置", "min_client_version_code", s.MinClientVersionCode, int32(0))
}

func TestCreateRoomExplicitSettingIsStoredAsGiven(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	in := createRoomReq("req-explicit-setting")
	in.Setting = &rpc.RoomSetting{
		DanmakuEnabled: false, ReplyEnabled: true, RecordEnabled: true, LinkmicEnabled: true,
		LiveType: model.LiveTypeAudio, MinClientVersionCode: 10234,
	}

	reply, err := newCreateLogic(t, st).CreateRoom(in)
	wantNoErr(t, "CreateRoom(显式配置)", err)

	s := st.settingAt(t, reply.GetRoomId())
	wantEQ(t, "显式配置", "danmaku", s.DanmakuEnabled, model.BoolToInt32(false))
	wantEQ(t, "显式配置", "record", s.RecordEnabled, model.BoolToInt32(true))
	wantEQ(t, "显式配置", "linkmic", s.LinkmicEnabled, model.BoolToInt32(true))
	wantEQ(t, "显式配置", "live_type", s.LiveType, model.LiveTypeAudio)
	wantEQ(t, "显式配置", "min_client_version_code", s.MinClientVersionCode, int32(10234))
}

func TestCreateRoomUnspecifiedPlatformRecordsAndroid(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	in := createRoomReq("req-platform")
	in.Platform = rpc.Platform_PLATFORM_UNSPECIFIED

	reply, err := newCreateLogic(t, st).CreateRoom(in)
	wantNoErr(t, "CreateRoom(未指定端)", err)
	wantEQ(t, "未指定端归一", "platform", st.roomAt(t, reply.GetRoomId()).Platform, model.PlatformAndroid)
}

func TestCreateRoomTitleTrimmedAndRefsTruncatedToColumnWidth(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	in := createRoomReq("req-trim")
	in.Title = "  深夜电台  "
	in.AppVersion = strings.Repeat("版", 100) // 100 rune > maxRefRunes(64)
	in.TraceId = strings.Repeat("t", 100)    // 100 byte > maxTraceIDBytes(64)

	reply, err := newCreateLogic(t, st).CreateRoom(in)
	wantNoErr(t, "CreateRoom(裁剪)", err)

	row := st.roomAt(t, reply.GetRoomId())
	wantEQ(t, "标题", "title", row.Title, "深夜电台")
	wantEQ(t, "app_version 截断", "runes", runeLen(row.AppVersion), 64)

	log := st.logsOf(row.RoomID)[0]
	wantEQ(t, "trace_id 裁剪", "bytes", len(log.TraceID), 64)
	wantEQ(t, "审计里的 request_id", "request_id", log.RequestID, "req-trim")
}

func TestCreateRoomDisabledAreaRejectedWithZeroWrites(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateDisabled))

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-disabled-area"))

	wantErrIs(t, "停用分区", err, model.ErrAreaDisabled)
	wantErrContains(t, "停用分区", err, "area_id=7001")
	wantEQ(t, "停用分区", "reply", reply, nil)
	wantSeq(t, "停用分区只读了一次分区表", st.log, 0, "live_area.IsUsable:7001")
	wantKeyUnburned(t, "停用分区", "req-disabled-area", st)
	wantTxCount(t, "停用分区", st.conn, 0)
	wantDeepEQ(t, "停用分区残留", "counts", st.counts(), storeCounts{areas: 1})
}

func TestCreateRoomUnknownAreaRowIsNotUsable(t *testing.T) {
	st := newStore() // 库里一个分区都没有

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-no-area"))

	wantErrIs(t, "分区不存在", err, model.ErrAreaDisabled)
	wantEQ(t, "分区不存在", "reply", reply, nil)
	wantNoCallAfter(t, "分区不存在", st.log, 1)
	wantKeyUnburned(t, "分区不存在", "req-no-area", st)
}

func TestCreateRoomAreaLookupFailurePropagatesUnchanged(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: area select")
	st.areas.failWith("IsUsable", boom)

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-area-boom"))

	// 依赖错误必须原样抛出，不能被降级成「分区不可用」。
	wantErrIs(t, "分区查询失败", err, boom)
	if errors.Is(err, model.ErrAreaDisabled) {
		t.Errorf("分区查询失败被伪装成 ErrAreaDisabled：%v", err)
	}
	wantEQ(t, "分区查询失败", "reply", reply, nil)
	wantSeq(t, "分区查询失败", st.log, 0, "live_area.IsUsable:7001")
	wantKeyUnburned(t, "分区查询失败", "req-area-boom", st)
	wantDeepEQ(t, "分区查询失败残留", "counts", st.counts(), storeCounts{areas: 1})
}

func TestCreateRoomWithoutModerationWritesNothing(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	// 不 wireModeration：这就是「本环境没接 moderation」的真实形态。
	lg := NewCreateRoomLogic(context.Background(), st.svcCtx())

	reply, err := lg.CreateRoom(createRoomReq("req-no-mod"))

	// 必须是显式错误而不是静默成功：建一个永远停在未送审的房间更糟。
	wantErrIs(t, "未接 moderation", err, model.ErrModerationNotConfigured)
	wantEQ(t, "未接 moderation", "reply", reply, nil)
	wantSeq(t, "未接 moderation", st.log, 0, "live_area.IsUsable:7001")
	wantKeyUnburned(t, "未接 moderation", "req-no-mod", st)
	wantTxCount(t, "未接 moderation", st.conn, 0)
	wantDeepEQ(t, "未接 moderation 残留", "counts", st.counts(), storeCounts{areas: 1})
}

func TestCreateRoomOwnerLimitIncludesFinishedRooms(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.seedRoom(baseRoom(101, createroomTestMid, model.RoomStateFinished))

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-limit"))

	wantErrIs(t, "上限（含终态）", err, model.ErrRoomLimitExceeded)
	wantErrContains(t, "上限（含终态）", err, "已有 1 个房间，上限 1")
	wantEQ(t, "上限（含终态）", "reply", reply, nil)
	wantSeq(t, "上限（含终态）", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-limit",
		"live_room.CountByOwner:9001",
	)
	// 「关掉再建不能返还额度」只能从 states 集合读出来。
	calls := st.rooms.countOwnerCalls
	if len(calls) != 1 {
		t.Fatalf("CountByOwner 调用次数 = %d, want 1", len(calls))
	}
	wantDeepEQ(t, "上限计入的状态集合", "states", calls[0].states, ownerLimitStates())
	withFinished := false
	for _, s := range calls[0].states {
		if s == model.RoomStateFinished {
			withFinished = true
		}
	}
	if !withFinished {
		t.Errorf("上限计入的状态集合 %v 必须含 FINISHED：关掉再建不能返还额度", calls[0].states)
	}
	wantKeyBurnedNoResult(t, "上限（含终态）", "req-limit", st)
	wantTxCount(t, "上限（含终态）", st.conn, 0)
}

func TestCreateRoomOwnerLimitZeroDisablesTheCheck(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.seedRoom(baseRoom(101, createroomTestMid, model.RoomStatePending))
	st.seedRoom(baseRoom(102, createroomTestMid, model.RoomStateLiving))
	// MaxRoomsPerOwner=0 表示不限：上限查询这一步根本不该发生（少一次 COUNT，也不会误拒）。
	lg := newCreateLogic(t, st, testLiveRoomConfDo(func(c *config.LiveRoomConf) { c.MaxRoomsPerOwner = 0 }))

	reply, err := lg.CreateRoom(createRoomReq("req-nolimit"))
	wantNoErr(t, "上限=0 表示不限", err)
	if reply.GetRoomId() <= 102 {
		t.Fatalf("上限=0 时新房间 ID = %d，应大于两间种子房间", reply.GetRoomId())
	}
	wantMethodCount(t, "上限=0 不查房间数", st.log, "live_room.CountByOwner", 0)
}

func TestCreateRoomOwnerCountFailureLeavesBurnedKey(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: count rooms")
	st.rooms.failWith("CountByOwner", boom)

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-count-boom"))

	wantErrIs(t, "上限查询失败", err, boom)
	wantEQ(t, "上限查询失败", "reply", reply, nil)
	wantTxCount(t, "上限查询失败", st.conn, 0)
	wantKeyBurnedNoResult(t, "上限查询失败", "req-count-boom", st)
}

func TestCreateRoomInsertFailureLeavesOnlyBurnedKey(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: insert room")
	st.rooms.failWith("InsertTx", boom)

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-insert-boom"))

	wantErrIs(t, "建房 INSERT 失败", err, boom)
	wantEQ(t, "建房 INSERT 失败", "reply", reply, nil)
	wantSeq(t, "建房 INSERT 失败", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-insert-boom",
		"live_room.CountByOwner:9001",
		"db.TransactCtx",
		"live_room.InsertTx",
	)
	wantDeepEQ(t, "建房 INSERT 失败残留", "counts", st.counts(),
		storeCounts{areas: 1, idem: 1})

	// 键已烧但结果永远回填不上：同一 request_id 重试不再是「安全重试」。
	_, err2 := newCreateLogic(t, st).CreateRoom(createRoomReq("req-insert-boom"))
	wantErrIs(t, "建房失败后重试", err2, model.ErrIdempotencyResultMissing)
}

func TestCreateRoomSettingWriteFailureLeavesPartialRows(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: upsert setting")
	st.settings.failWith("UpsertTx", boom)

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-setting-boom"))

	wantErrIs(t, "配置行写入失败", err, boom)
	wantEQ(t, "配置行写入失败", "reply", reply, nil)
	wantSeq(t, "配置行写入失败", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-setting-boom",
		"live_room.CountByOwner:9001",
		"db.TransactCtx",
		"live_room.InsertTx",
		"live_room_anchor.BindTx:m9001",
		"live_room_setting.UpsertTx:1",
	)
	// 假件不回滚：这两行确实是「事务中途失败后库里还剩的东西」。
	// 真实 MySQL 会整笔回滚，本用例锁的是**语句顺序与所属事务**，不是回滚能力。
	wantDeepEQ(t, "配置行写入失败残留", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, areas: 1, idem: 1})
	wantTxCount(t, "配置行写入失败", st.conn, 1)
}

func TestCreateRoomBindFailureAbortsBeforeSetting(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: bind owner")
	st.anchors.failWith("BindTx", boom)

	_, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-bind-boom"))
	wantErrIs(t, "房主绑定失败", err, boom)

	wantMethodCount(t, "绑定失败后不再写配置", st.log, "live_room_setting.UpsertTx", 0)
	wantMethodCount(t, "绑定失败后不再写审计", st.log, "live_room_state_log.InsertTx", 0)
	wantMethodCount(t, "绑定失败后不送审", st.log, "moderation.SubmitForReview", 0)
	wantCount(t, "绑定失败后不回填结果", st.log, "live_room_idempotency.SaveResult", 0)
}

func TestCreateRoomAuditFailureInsideTxAbortsWholeWrite(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: audit insert")
	st.stateLogs.failWith("InsertTx", boom)

	_, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-audit-boom"))
	wantErrIs(t, "事务内审计失败", err, boom)
	// 审计与状态同事务：审计写不进去就不该送审（否则会出现「没房却有任务」）。
	wantMethodCount(t, "审计失败后不送审", st.log, "moderation.SubmitForReview", 0)
}

func TestCreateRoomSubmitFailureIsSwallowedAndKeepsNone(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.mod.failWith(errors.New("boom: moderation down"))

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-submit-boom"))

	// 房间已建好，不能假装失败：错误被吞，reply 停在「未送审」的真实形态。
	wantNoErr(t, "送审失败", err)
	wantEQ(t, "送审失败", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_NONE)
	wantEQ(t, "送审失败", "moderation_task_id", reply.GetModerationTaskId(), int64(0))
	wantEQ(t, "送审失败", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_PENDING)

	roomID := reply.GetRoomId()
	wantSeq(t, "送审失败", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-submit-boom",
		"live_room.CountByOwner:9001",
		"db.TransactCtx",
		"live_room.InsertTx",
		"live_room_anchor.BindTx:m9001",
		fmt.Sprintf("live_room_setting.UpsertTx:%d", roomID),
		"live_room_state_log.InsertTx:t1:0->1:create_room",
		fmt.Sprintf("moderation.SubmitForReview:%d", roomID),
		"live_room_idempotency.SaveResult:req-submit-boom",
	)
	row := st.roomAt(t, roomID)
	wantEQ(t, "送审失败落库事实", "verify_state", row.VerifyState, model.VerifyStateNone)
	wantEQ(t, "送审失败落库事实", "moderation_task_id", row.ModerationTaskID, int64(0))
	wantEQ(t, "送审失败落库事实", "state", row.State, model.RoomStatePending)
	if n := len(st.logsOf(roomID)); n != 1 {
		t.Errorf("审计行数 = %d, want 1（只有建房那条，没有送审那条）", n)
	}
}

func TestCreateRoomEmptyTaskFromDownstreamCountsAsFailure(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.mod.taskReturns(0) // 下游成功应答但没给任务

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-empty-task"))

	wantNoErr(t, "空任务", err)
	wantEQ(t, "空任务", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_NONE)
	wantEQ(t, "空任务", "moderation_task_id", reply.GetModerationTaskId(), int64(0))
	wantEQ(t, "空任务", "库内 verify_state", st.roomAt(t, reply.GetRoomId()).VerifyState, model.VerifyStateNone)
	wantMethodCount(t, "空任务不写资料态", st.log, "live_room.UpdateProfile", 0)
}

func TestCreateRoomVerifyAuditFailureAfterCommitIsSwallowed(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: verify audit insert")
	st.stateLogs.failWith("Insert", boom) // 只打事务外那条

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-verify-audit-boom"))

	wantNoErr(t, "送审审计失败", err)
	wantEQ(t, "送审审计失败", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REVIEWING)
	row := st.roomAt(t, reply.GetRoomId())
	wantEQ(t, "送审审计失败", "库内 verify_state", row.VerifyState, model.VerifyStateReviewing)
	// 事实已落房间行，审计缺行：本用例把「日志失败不推翻既成事实」这条口径钉住。
	if n := len(st.logsOf(row.RoomID)); n != 1 {
		t.Errorf("审计行数 = %d, want 1（送审那条没写进去）", n)
	}
	wantCount(t, "送审审计失败仍回填结果", st.log, "live_room_idempotency.SaveResult:req-verify-audit-boom", 1)
}

func TestCreateRoomCasMissOnVerifyBackfillStillReportsReviewing(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	// 在回写资料态之前，别的入口把房间推进到 LIVING：allowStates=[PENDING] 的 UPDATE 命中 0 行。
	st.raceBefore("live_room.UpdateProfile", func() {
		for _, r := range st.rooms.rows {
			r.State = model.RoomStateLiving
			r.StateVersion++
		}
	})

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-cas-miss"))
	wantNoErr(t, "回写资料态 CAS 未命中", err)
	st.checkRaces(t)

	// createroomlogic.go:168 丢掉了 UpdateProfile 的 ok：reply 照样声称 REVIEWING。
	wantEQ(t, "CAS 未命中时的 reply", "verify_state", reply.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_REVIEWING)
	wantEQ(t, "CAS 未命中时的 reply", "moderation_task_id", reply.GetModerationTaskId(), int64(9001))
	row := st.roomAt(t, reply.GetRoomId())
	wantEQ(t, "CAS 未命中后的库值", "verify_state", row.VerifyState, model.VerifyStateNone)
	wantEQ(t, "CAS 未命中后的库值", "moderation_task_id", row.ModerationTaskID, int64(0))
	wantEQ(t, "CAS 未命中后的库值", "state", row.State, model.RoomStateLiving)
	// 而且照样追加了一条「1->2 profile_submitted」的审计行 —— 与库内事实矛盾。
	logs := st.logsOf(row.RoomID)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2", len(logs))
	}
	wantEQ(t, "矛盾的审计行", "to_state", logs[1].ToState, model.VerifyStateReviewing)
}

func TestCreateRoomReplayReturnsStoredReplyWithZeroNewWrites(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	lg := newCreateLogic(t, st)

	first, err := lg.CreateRoom(createRoomReq("req-replay"))
	wantNoErr(t, "CreateRoom 首次", err)
	from := st.log.snapshot()

	second, err := lg.CreateRoom(createRoomReq("req-replay"))
	wantNoErr(t, "CreateRoom 重放", err)

	wantEQ(t, "重放", "replayed", second.GetReplayed(), true)
	wantEQ(t, "重放", "room_id 必须回原值", second.GetRoomId(), first.GetRoomId())
	wantEQ(t, "重放", "state", second.GetState(), first.GetState())
	wantEQ(t, "重放", "verify_state", second.GetVerifyState(), first.GetVerifyState())
	wantEQ(t, "重放", "moderation_task_id", second.GetModerationTaskId(), first.GetModerationTaskId())
	wantSeq(t, "重放只读键", st.log, from,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:req-replay",
		"live_room_idempotency.Find:req-replay",
	)
	// 第二次不得再产生任何副作用：只有一间房、一条绑定、一份结果快照。
	wantDeepEQ(t, "重放残留", "counts", st.counts(), storeCounts{
		rooms: 1, settings: 1, anchors: 1, logs: 2, areas: 1, idem: 1,
	})
	wantTxCount(t, "重放不起事务", st.conn, 1)
	wantCount(t, "重放不重复送审", st.log, "moderation.SubmitForReview", 1)
}

func TestCreateRoomReplayAfterAreaDisabledIsBlockedByGuard(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	lg := newCreateLogic(t, st)
	first, err := lg.CreateRoom(createRoomReq("req-replay-disabled"))
	wantNoErr(t, "CreateRoom 首次", err)

	// 运营把分区停掉了 —— 客户端重试同一个 request_id 现在拿不回原结果。
	for _, a := range st.areas.rows {
		a.State = model.AreaStateDisabled
	}
	from := st.log.snapshot()
	reply, err := lg.CreateRoom(createRoomReq("req-replay-disabled"))

	wantErrIs(t, "重放被分区守卫挡住", err, model.ErrAreaDisabled)
	wantEQ(t, "重放被分区守卫挡住", "reply", reply, nil)
	wantSeq(t, "重放被分区守卫挡住", st.log, from, "live_area.IsUsable:7001")
	// 首次建的房间还在，但客户端这次重试既拿不到 room_id 也没产生新房间。
	wantEQ(t, "重放被分区守卫挡住", "房间还在", st.roomAt(t, first.GetRoomId()).RoomID, first.GetRoomId())
	wantCount(t, "重放被分区守卫挡住", st.log, "live_room.InsertTx", 1)
}

func TestCreateRoomReplayOfKeyUsedByAnotherRpcIsRejected(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.seedIdem("shared-key", "StartLive", `{"session_id":55}`)

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("shared-key"))

	wantErrIs(t, "跨方法复用键", err, model.ErrRequestIDReused)
	wantErrContains(t, "跨方法复用键", err, "used by StartLive")
	wantEQ(t, "跨方法复用键", "reply", reply, nil)
	wantSeq(t, "跨方法复用键", st.log, 0,
		"live_area.IsUsable:7001",
		"live_room_idempotency.Claim:shared-key",
		"live_room_idempotency.Find:shared-key",
	)
	// 不能把别的 RPC 的结果当自己的回放出去，也不该多建房。
	wantDeepEQ(t, "跨方法复用键残留", "counts", st.counts(), storeCounts{areas: 1, idem: 1})
}

func TestCreateRoomReplayOfBurnedKeyWithoutResultErrors(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	st.seedIdem("half-done", "CreateRoom", "") // 首次执行 Claim 了但没回填

	reply, err := newCreateLogic(t, st).CreateRoom(createRoomReq("half-done"))

	wantErrIs(t, "键已烧但结果没回填", err, model.ErrIdempotencyResultMissing)
	wantEQ(t, "键已烧但结果没回填", "reply", reply, nil)
	wantCount(t, "键已烧但结果没回填", st.log, "live_room.InsertTx", 0)
	wantDeepEQ(t, "键已烧但结果没回填残留", "counts", st.counts(), storeCounts{areas: 1, idem: 1})
}

func TestCreateRoomClaimFailurePropagates(t *testing.T) {
	fixClock(t, createroomTestNow)
	st := newStore()
	st.seedArea(baseArea(createroomTestArea, 0, model.AreaStateEnabled))
	boom := errors.New("boom: claim insert")
	st.idem.failWith("Claim", boom)

	_, err := newCreateLogic(t, st).CreateRoom(createRoomReq("req-claim-boom"))
	wantErrIs(t, "抢键失败", err, boom)
	wantTxCount(t, "抢键失败", st.conn, 0)
	wantCount(t, "抢键失败", st.log, "live_room.CountByOwner", 0)
}
