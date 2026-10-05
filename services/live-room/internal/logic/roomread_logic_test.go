package logic

// roomread_logic_test.go 覆盖房间维度的两个读方法：GetRoom / ListRooms。
//
// 侧重点是「接线」而不是投影本身：`roomInfo` 等投影函数由 conv_projection_test.go 逐字段锁定，
// 这里锁的是守卫顺序（拒绝时一次依赖调用都不许发生）、取数路径（按 room_id 还是按房主、
// 是否走绑定表回表）、查询条件是否原样到达 model、以及 total 与页内容的口径关系。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// --- GetRoom ---

func TestGetRoomByRoomIDProjectsRow(t *testing.T) {
	st := newStore()
	row := st.seedRoom(&model.LiveRoom{
		RoomID: 101, OwnerMid: 9001, Title: "深夜电台", Cover: "cover/101.jpg", AreaID: 7002,
		State: model.RoomStateLiving, VerifyState: model.VerifyStatePassed,
		ActiveSessionID: 5001, ActiveStreamID: "stream-abc", StateVersion: 4,
		BanUntil: 0, Platform: model.PlatformIOS, AppVersion: "1.2.3",
		Ctime: 111, Mtime: 222,
	})
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101})

	wantNoErr(t, "GetRoom(room_id=101)", err)
	info := reply.GetRoom()
	if info == nil {
		t.Fatalf("GetRoom 返回空 Room：reply=%#v", reply)
	}
	wantEQ(t, "投影", "room_id", info.GetRoomId(), row.RoomID)
	wantEQ(t, "投影", "owner_mid", info.GetOwnerMid(), row.OwnerMid)
	wantEQ(t, "投影", "title", info.GetTitle(), "深夜电台")
	wantEQ(t, "投影", "cover", info.GetCover(), "cover/101.jpg")
	wantEQ(t, "投影", "area_id", info.GetAreaId(), row.AreaID)
	wantEQ(t, "投影", "state", info.GetState(), rpc.RoomState_ROOM_STATE_LIVING)
	wantEQ(t, "投影", "verify_state", info.GetVerifyState(), rpc.VerifyState_VERIFY_STATE_PASSED)
	wantEQ(t, "投影", "active_session_id", info.GetActiveSessionId(), row.ActiveSessionID)
	wantEQ(t, "投影", "active_stream_id", info.GetActiveStreamId(), "stream-abc")
	wantEQ(t, "投影", "state_version", info.GetStateVersion(), row.StateVersion)
	wantEQ(t, "投影", "ctime", info.GetCtime(), int64(111))
	wantEQ(t, "投影", "mtime", info.GetMtime(), int64(222))
	// 纯房间读：既不该读配置表，也不该读场次表。
	wantSeq(t, "GetRoom 纯房间读", st.log, 0, "live_room.FindOne:101")
	wantNoDirectSQL(t, "GetRoom", st.conn)
}

func TestGetRoomRejectsMissingKeysBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.GetRoomReq
	}{
		{"nil 请求", nil},
		{"两个键都是 0", &rpc.GetRoomReq{}},
		{"room_id 为负且无 owner_mid", &rpc.GetRoomReq{RoomId: -7}},
		{"owner_mid 为负", &rpc.GetRoomReq{OwnerMid: -1}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
		logic := NewGetRoomLogic(context.Background(), st.svcCtx())

		reply, err := logic.GetRoom(tc.in)

		wantErrIs(t, "GetRoom("+tc.name+")", err, model.ErrInvalidRoomID)
		wantEQ(t, "GetRoom("+tc.name+")", "reply", reply, nil)
		// 空条件查询不能退化成「返回某一行」或「返回空 reply」。
		wantNoCallAfter(t, "GetRoom("+tc.name+")", st.log, 0)
	}
}

func TestGetRoomMissingRowIsNotFoundNotZeroReply(t *testing.T) {
	st := newStore()
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 404})

	wantErrIs(t, "GetRoom(不存在)", err, model.ErrRoomNotFound)
	wantEQ(t, "GetRoom(不存在)", "reply", reply, nil)
	wantSeq(t, "GetRoom(不存在)", st.log, 0, "live_room.FindOne:404")
}

func TestGetRoomPropagatesStoreError(t *testing.T) {
	st := newStore()
	st.rooms.failWith("FindOne", errors.New("db down"))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101})

	wantErrContains(t, "GetRoom(故障)", err, "db down")
	if errors.Is(err, model.ErrRoomNotFound) {
		t.Fatal("store 故障不得被翻译成「房间不存在」——那会让调用方以为可以安全建新房")
	}
	wantEQ(t, "GetRoom(故障)", "reply", reply, nil)
}

func TestGetRoomRoomIDWinsOverOwnerMid(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(102, 9002, model.RoomStateReady))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, OwnerMid: 9002})

	wantNoErr(t, "GetRoom(两个键都给)", err)
	wantEQ(t, "GetRoom(两个键都给)", "room_id", reply.GetRoom().GetRoomId(), int64(101))
	// room_id 优先：不得再按 owner_mid 反查（那是两条 SQL 的开销与两个答案）。
	wantSeq(t, "GetRoom(两个键都给)", st.log, 0, "live_room.FindOne:101")
}

func TestGetRoomByOwnerTakesNewestNonFinishedRoom(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(102, 9001, model.RoomStateLiving))
	// 终态房不进「主播的现用房间」：ListByOwner 的 SQL 带 state <> FINISHED。
	st.seedRoom(baseRoom(103, 9001, model.RoomStateFinished))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{OwnerMid: 9001})

	wantNoErr(t, "GetRoom(owner_mid)", err)
	wantEQ(t, "GetRoom(owner_mid)", "room_id", reply.GetRoom().GetRoomId(), int64(102))
	wantSeq(t, "GetRoom(owner_mid)", st.log, 0, "live_room.ListByOwner:9001/1")
	wantCount(t, "GetRoom(owner_mid)", st.log, "live_room.FindOne", 0)
}

func TestGetRoomByOwnerWithoutRoomReportsWhichMid(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9002, model.RoomStateReady))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{OwnerMid: 9001})

	wantErrIs(t, "GetRoom(该主播没房)", err, model.ErrRoomNotFound)
	wantErrContains(t, "GetRoom(该主播没房)", err, "owner_mid=9001")
	wantEQ(t, "GetRoom(该主播没房)", "reply", reply, nil)
}

func TestGetRoomWithSettingUsesServerDefaultsWhenRowMissing(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithSetting: true})

	wantNoErr(t, "GetRoom(with_setting, 无配置行)", err)
	setting := reply.GetSetting()
	if setting == nil {
		t.Fatal("with_setting=true 必须回一个 Setting（缺行套默认，不能缺席）")
	}
	wantEQ(t, "默认配置", "danmaku_enabled", setting.GetDanmakuEnabled(), true)
	wantEQ(t, "默认配置", "reply_enabled", setting.GetReplyEnabled(), true)
	// 录制与连麦是显式 opt-in：缺配置行时必须是关，不能替用户开录制。
	wantEQ(t, "默认配置", "record_enabled", setting.GetRecordEnabled(), false)
	wantEQ(t, "默认配置", "linkmic_enabled", setting.GetLinkmicEnabled(), false)
	wantEQ(t, "默认配置", "live_type", setting.GetLiveType(), model.LiveTypeVideo)
	wantEQ(t, "默认配置", "min_client_version_code", setting.GetMinClientVersionCode(), int32(0))
	wantSeq(t, "GetRoom(with_setting)", st.log, 0, "live_room.FindOne:101", "live_room_setting.FindOne:101")

	// 缺陷（已登记 README）：缺配置行时 setting_info 用 defaultSettingRow(0) 兜底，
	// 于是 setting.room_id 回的是 0 而不是被查询的 101。客户端若以 setting.room_id 归因
	// 就会拿到一个「不属于任何房间」的配置。这里锁住当前真实值，防止无声变化。
	wantEQ(t, "缺陷待修：缺行时默认配置的 room_id", "setting.room_id", setting.GetRoomId(), int64(0))
}

func TestGetRoomWithSettingProjectsStoredRow(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedSetting(&model.LiveRoomSetting{
		RoomID: 101, DanmakuEnabled: model.BoolToInt32(false), ReplyEnabled: model.BoolToInt32(true),
		RecordEnabled: model.BoolToInt32(true), LinkmicEnabled: model.BoolToInt32(false),
		LiveType: model.LiveTypeAudio, MinClientVersionCode: 42, Ctime: 100, Mtime: 300,
	})
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithSetting: true})

	wantNoErr(t, "GetRoom(with_setting, 有配置行)", err)
	setting := reply.GetSetting()
	wantEQ(t, "库值", "room_id", setting.GetRoomId(), int64(101))
	wantEQ(t, "库值", "danmaku_enabled", setting.GetDanmakuEnabled(), false)
	wantEQ(t, "库值", "record_enabled", setting.GetRecordEnabled(), true)
	wantEQ(t, "库值", "live_type", setting.GetLiveType(), model.LiveTypeAudio)
	wantEQ(t, "库值", "min_client_version_code", setting.GetMinClientVersionCode(), int32(42))
	wantEQ(t, "库值", "mtime", setting.GetMtime(), int64(300))
}

func TestGetRoomSettingErrorPropagates(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.settings.failWith("FindOne", errors.New("setting query failed"))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithSetting: true})

	wantEQ(t, "GetRoom(配置读失败)", "reply", reply, nil)
	wantErrContains(t, "GetRoom(配置读失败)", err, "setting query failed")
}

// TestGetRoomWithActiveSessionCoversBothStates 一次覆盖「有进行中场次」和「没有」两种落点：
// 没有场次时不是错误，ActiveSession 缺席即是答案（房间 READY 也能查附带场次）。
func TestGetRoomWithActiveSessionCoversBothStates(t *testing.T) {
	t.Run("有进行中场次", func(t *testing.T) {
		st := newStore()
		st.seedRoom(baseRoom(101, 9001, model.RoomStateLiving))
		st.seedSession(&model.LiveSession{
			SessionID: 5001, RoomID: 101, Mid: 9001, State: model.SessionStateLiving,
			TitleSnapshot: "开播快照", AreaIDSnapshot: 7002, StreamID: "stream-abc",
			StartedAt: 900, LastStreamSeq: 12, ReplayState: model.ReplayStateNone,
			Ctime: 880, Mtime: 900,
		})
		// 终态场次不能被当成「当前场次」。
		st.seedSession(baseSession(5002, 101, 9001, model.SessionStateEnded))
		logic := NewGetRoomLogic(context.Background(), st.svcCtx())

		reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithActiveSession: true})

		wantNoErr(t, "GetRoom(with_active_session)", err)
		active := reply.GetActiveSession()
		wantEQ(t, "当前场次", "session_id", active.GetSessionId(), int64(5001))
		wantEQ(t, "当前场次", "state", active.GetState(), rpc.SessionState_SESSION_STATE_LIVING)
		wantEQ(t, "当前场次", "title_snapshot", active.GetTitleSnapshot(), "开播快照")
		wantEQ(t, "当前场次", "stream_id", active.GetStreamId(), "stream-abc")
		wantSeq(t, "GetRoom(with_active_session)", st.log, 0,
			"live_room.FindOne:101", "live_session.FindActiveByRoom:101")
	})

	t.Run("无进行中场次", func(t *testing.T) {
		st := newStore()
		st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
		logic := NewGetRoomLogic(context.Background(), st.svcCtx())

		reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithActiveSession: true})

		wantNoErr(t, "GetRoom(with_active_session, 无场次)", err)
		wantEQ(t, "无场次", "active_session 缺席", reply.GetActiveSession() == nil, true)
		wantSeq(t, "GetRoom(with_active_session, 无场次)", st.log, 0,
			"live_room.FindOne:101", "live_session.FindActiveByRoom:101")
	})
}

// TestGetRoomBothAttachmentsReadInFixedOrder 锁定「房间 → 配置 → 场次」的读取顺序，
// 并验证 with_* 任一为真时整个房间读都不走缓存分支（否则会把「本轮没查」答成「没有」）。
func TestGetRoomBothAttachmentsReadInFixedOrder(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateLiving))
	st.seedSetting(&model.LiveRoomSetting{
		RoomID: 101, DanmakuEnabled: model.BoolToInt32(true), ReplyEnabled: model.BoolToInt32(true),
		RecordEnabled: model.BoolToInt32(true), LinkmicEnabled: model.BoolToInt32(true),
		LiveType: model.LiveTypeScreen, Ctime: 100, Mtime: 400,
	})
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStatePending))
	logic := NewGetRoomLogic(context.Background(), st.svcCtx())

	reply, err := logic.GetRoom(&rpc.GetRoomReq{RoomId: 101, WithSetting: true, WithActiveSession: true})

	wantNoErr(t, "GetRoom(两个附带)", err)
	wantEQ(t, "GetRoom(两个附带)", "room_id", reply.GetRoom().GetRoomId(), int64(101))
	wantEQ(t, "GetRoom(两个附带)", "setting.live_type", reply.GetSetting().GetLiveType(), model.LiveTypeScreen)
	wantEQ(t, "GetRoom(两个附带)", "session_state", reply.GetActiveSession().GetState(),
		rpc.SessionState_SESSION_STATE_PENDING)
	wantSeq(t, "GetRoom(两个附带)", st.log, 0,
		"live_room.FindOne:101", "live_room_setting.FindOne:101", "live_session.FindActiveByRoom:101")
}

// TestGetRoomPlainReadWithoutCacheAlwaysGoesToMySQL 锁 AGENTS.md §5 的读侧口径：
// CacheRedis 未接入（svc.Cache=nil）时只回源 MySQL，不得返回空结果或缓存里的假命中。
// 因此两次同样的读必须产生两次 FindOne。
func TestGetRoomPlainReadWithoutCacheAlwaysGoesToMySQL(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	sctx := st.svcCtx()
	if sctx.Cache != nil {
		t.Fatal("本用例的前提是 Cache=nil（纯内存用例没有 Redis 实例）")
	}

	for i := 1; i <= 2; i++ {
		reply, err := NewGetRoomLogic(context.Background(), sctx).GetRoom(&rpc.GetRoomReq{RoomId: 101})
		wantNoErr(t, "GetRoom 第二次读", err)
		wantEQ(t, "第二次读仍回源", "room_id", reply.GetRoom().GetRoomId(), int64(101))
		wantCount(t, "GetRoom 回读次数", st.log, "live_room.FindOne", i)
	}
	wantNoDirectSQL(t, "GetRoom 两次读", st.conn)
}

// --- ListRooms ---

func TestListRoomsNilRequestReturnsEmptyReplyWithoutQuery(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(nil)

	wantNoErr(t, "ListRooms(nil)", err)
	if reply == nil {
		t.Fatal("ListRooms(nil) 约定回空 reply（列表类接口与 GetRoom 的空条件拒绝不同口径）")
	}
	wantEQ(t, "ListRooms(nil)", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "ListRooms(nil)", "page", reply.GetPage(), int32(0))
	wantEQ(t, "ListRooms(nil)", "page_size", reply.GetPageSize(), int32(0))
	wantNoCallAfter(t, "ListRooms(nil)", st.log, 0)
}

func TestListRoomsDefaultPagingSharesOneQueryWithCount(t *testing.T) {
	st := newStore()
	for _, id := range []int64{101, 102, 103} {
		st.seedRoom(baseRoom(id, 9001, model.RoomStateReady))
	}
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{})

	wantNoErr(t, "ListRooms(默认分页)", err)
	wantEQ(t, "默认分页", "page", reply.GetPage(), int32(1))
	wantEQ(t, "默认分页", "page_size", reply.GetPageSize(), int32(20))
	wantEQ(t, "默认分页", "total", reply.GetTotal(), int32(3))
	wantInt64sEQ(t, "默认分页", "rooms", roomIDs(reply.GetRooms()), []int64{103, 102, 101})

	// total 与页内容同口径的唯一实现方式：List 与 Count 收到**完全相同**的查询条件。
	wantSeq(t, "List 与 Count 的顺序", st.log, 0, "live_room.List", "live_room.Count")
	if len(st.rooms.listQueries) != 1 || len(st.rooms.countQueries) != 1 {
		t.Fatalf("List/Count 调用次数异常：%d / %d", len(st.rooms.listQueries), len(st.rooms.countQueries))
	}
	wantDeepEQ(t, "发现页", "query", st.rooms.listQueries[0], model.RoomListQuery{Limit: 20})
	wantDeepEQ(t, "total 与页内容同口径", "count 收到的 query",
		st.rooms.countQueries[0], st.rooms.listQueries[0])
	wantNoDirectSQL(t, "ListRooms", st.conn)
}

// TestListRoomsTotalCountsFilteredRowsNotPageRows 打实「total 是过滤后的总数」：
// 页大小 1 时 total 仍是全部 READY 房数，且过滤条件同时到达 List 与 Count。
func TestListRoomsTotalCountsFilteredRowsNotPageRows(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(102, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(103, 9001, model.RoomStateLiving))
	st.seedRoom(baseRoom(104, 9002, model.RoomStateDisabled))
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{
		State: rpc.RoomState_ROOM_STATE_READY, Page: 1, PageSize: 1,
	})

	wantNoErr(t, "ListRooms(state=READY, size=1)", err)
	wantEQ(t, "过滤后的总数", "total", reply.GetTotal(), int32(2))
	wantInt64sEQ(t, "一页只装一条", "rooms", roomIDs(reply.GetRooms()), []int64{102})
	wantEQ(t, "回显", "page_size", reply.GetPageSize(), int32(1))

	wantDeepEQ(t, "state 过滤", "query", st.rooms.listQueries[0],
		model.RoomListQuery{State: model.RoomStateReady, Limit: 1})
	wantDeepEQ(t, "state 过滤同口径", "count query", st.rooms.countQueries[0], st.rooms.listQueries[0])
}

func TestListRoomsAreaAndOrderFiltersReachModel(t *testing.T) {
	st := newStore()
	a := baseRoom(101, 9001, model.RoomStateReady)
	a.AreaID = 7002
	st.seedRoom(a)
	b := baseRoom(102, 9001, model.RoomStateReady)
	b.AreaID = 7003
	st.seedRoom(b)
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{
		AreaId: 7003, Order: rpc.RoomOrder_ROOM_ORDER_CTIME_DESC, Page: 2, PageSize: 10,
	})

	wantNoErr(t, "ListRooms(area+order)", err)
	wantInt64sEQ(t, "分区过滤", "rooms", roomIDs(reply.GetRooms()), []int64{102})
	wantEQ(t, "分区过滤", "total", reply.GetTotal(), int32(1))
	wantDeepEQ(t, "分区与排序", "query", st.rooms.listQueries[0], model.RoomListQuery{
		AreaID: 7003, Order: model.RoomOrderCtimeDesc, Offset: 10, Limit: 10,
	})
}

func TestListRoomsRejectsInvalidParamsBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListRoomsReq
		want error
	}{
		{"负页码", &rpc.ListRoomsReq{Page: -1}, model.ErrInvalidPage},
		{"页大小越界", &rpc.ListRoomsReq{PageSize: 101}, model.ErrPageSizeTooLarge},
		{"未知房间状态", &rpc.ListRoomsReq{State: rpc.RoomState(9)}, model.ErrInvalidRoomTransition},
		{"未知排序", &rpc.ListRoomsReq{Order: rpc.RoomOrder(7)}, model.ErrRoomOrderInvalid},
		{"负分区", &rpc.ListRoomsReq{AreaId: -1}, model.ErrInvalidAreaID},
		{"负房主", &rpc.ListRoomsReq{OwnerMid: -5}, model.ErrInvalidMid},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
		logic := NewListRoomsLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListRooms(tc.in)

		wantErrIs(t, "ListRooms("+tc.name+")", err, tc.want)
		wantEQ(t, "ListRooms("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListRooms("+tc.name+")", st.log, 0)
	}
}

// TestListRoomsValidatesPaginationAheadOfEnums 锁校验优先级：
// 页大小 → 页码 → 状态 → 排序 → 分区。全错时只能报第一个，否则客户端改了一个还是报错、
// 却不知道自己真正错在哪。
func TestListRoomsValidatesPaginationAheadOfEnums(t *testing.T) {
	st := newStore()
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	_, err := logic.ListRooms(&rpc.ListRoomsReq{
		PageSize: 500, Page: -1, State: rpc.RoomState(9), Order: rpc.RoomOrder(7), AreaId: -1,
	})
	wantErrIs(t, "同时越界", err, model.ErrPageSizeTooLarge)

	_, err = logic.ListRooms(&rpc.ListRoomsReq{Page: -1, State: rpc.RoomState(9)})
	wantErrIs(t, "页码先于状态", err, model.ErrInvalidPage)

	_, err = logic.ListRooms(&rpc.ListRoomsReq{State: rpc.RoomState(9), Order: rpc.RoomOrder(7)})
	wantErrIs(t, "状态先于排序", err, model.ErrInvalidRoomTransition)
}

// TestListRoomsOffsetIsComputedButDroppedByModelSQL 缺陷锁定：
// logic 把 page/page_size 折算成 RoomListQuery.Offset 交给 model，
// 但 `model/live_room.go:213-230` 的 List（与 :494-521 的 ListByRoomIDs）
// 拼出的 SQL 只有 `LIMIT ?`，**没有 OFFSET 子句**，RoomListQuery.Offset 从未进 SQL。
// 结果：第 2 页与第 1 页是同一批房间，超出页大小的房间永远翻不到。
// 这里按当前真实行为断言（假件照抄真实 SQL，不替生产补 OFFSET），
// 修复时本用例即红——修完应改成「第二页拿到 101」。
func TestListRoomsOffsetIsComputedButDroppedByModelSQL(t *testing.T) {
	st := newStore()
	for _, id := range []int64{101, 102, 103} {
		st.seedRoom(baseRoom(id, 9001, model.RoomStateReady))
	}
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	first, err := logic.ListRooms(&rpc.ListRoomsReq{Page: 1, PageSize: 2})
	wantNoErr(t, "第一页", err)
	second, err := logic.ListRooms(&rpc.ListRoomsReq{Page: 2, PageSize: 2})
	wantNoErr(t, "第二页", err)

	// logic 侧的折算是对的：offset = (page-1)*size。
	wantDeepEQ(t, "第一页条件", "offset", st.rooms.listQueries[0].Offset, int32(0))
	wantDeepEQ(t, "第二页条件", "offset", st.rooms.listQueries[1].Offset, int32(2))

	// 但两页内容相同，且第二页的 tail（room 101）没有任何一页能翻到。
	wantInt64sEQ(t, "第一页内容", "rooms", roomIDs(first.GetRooms()), []int64{103, 102})
	wantInt64sEQ(t, "缺陷：第二页与第一页同内容", "rooms", roomIDs(second.GetRooms()), []int64{103, 102})
	wantEQ(t, "总数说明还有下一页", "total", second.GetTotal(), int32(3))
	wantEQ(t, "回显页码", "page", second.GetPage(), int32(2))
}

// TestListRoomsByOwnerGoesThroughBindingTable 打实「按绑定表取 id 再回表」：
// 房管/连麦主播也要看到自己所在的房间，所以可见集合来自 live_room_anchor，
// 且回表条件里 OwnerMid 必须留空——否则房管又被 owner_mid 筛掉一次。
func TestListRoomsByOwnerGoesThroughBindingTable(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(102, 9001, model.RoomStateLiving))
	st.seedRoom(baseRoom(103, 9002, model.RoomStateReady))
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.seedAnchor(baseAnchor(12, 102, 9001, model.AnchorRoleCohost))

	logic := NewListRoomsLogic(context.Background(), st.svcCtx())
	reply, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 1, PageSize: 10})

	wantNoErr(t, "ListRooms(可见房间)", err)
	wantInt64sEQ(t, "可见房间", "rooms", roomIDs(reply.GetRooms()), []int64{102, 101})
	wantEQ(t, "可见房间", "total", reply.GetTotal(), int32(2))
	wantSeq(t, "绑定表 → 回表", st.log, 0,
		"live_room_anchor.ListRoomsByMid:9001", "live_room.ListByRoomIDs:n=2")
	wantMethodCount(t, "可见路径不得走发现页", st.log, "live_room.List", 0)
	wantMethodCount(t, "可见路径不得走 Rooms.Count", st.log, "live_room.Count", 0)
	if len(st.rooms.listByIDsQueries) != 1 {
		t.Fatalf("回表次数 = %d, want 1", len(st.rooms.listByIDsQueries))
	}
	call := st.rooms.listByIDsQueries[0]
	wantEQ(t, "回表条件", "OwnerMid 必须留空", call.q.OwnerMid, int64(0))
	wantInt64sEQ(t, "回表 id 集合", "ids", call.ids, []int64{102, 101})
	wantDeepEQ(t, "回表条件", "其余过滤", call.q, model.RoomListQuery{Limit: 10})
	wantNoDirectSQL(t, "ListRooms(可见房间)", st.conn)
}

func TestListRoomsByOwnerWithoutBindingSkipsRoomTable(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9002, model.RoomStateReady))
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001})

	wantNoErr(t, "ListRooms(没有绑定)", err)
	wantEQ(t, "没有绑定", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "没有绑定", "rooms 为空", len(reply.GetRooms()), 0)
	wantSeq(t, "空可见集合", st.log, 0, "live_room_anchor.ListRoomsByMid:9001")
	wantCount(t, "空可见集合不得回表", st.log, "live_room.ListByRoomIDs", 0)
}

// TestListRoomsByOwnerVisibleWindowIsBounded 锁 anchorVisibleFetchCap=500：
// 越界必须拒绝而不是悄悄返回不完整的一页。守卫发生在取绑定之前。
func TestListRoomsByOwnerVisibleWindowIsBounded(t *testing.T) {
	st := newStore()
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	// offset(500)+size(100) > 500 → 拒绝。
	_, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 6, PageSize: 100})
	wantErrIs(t, "ListRooms(越出可见窗口)", err, model.ErrInvalidPage)
	wantErrContains(t, "ListRooms(越出可见窗口)", err, "500")
	wantNoCallAfter(t, "ListRooms(越出可见窗口)", st.log, 0)

	// 恰好压线（offset 400 + size 100 == 500）放行。
	reply, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 5, PageSize: 100})
	wantNoErr(t, "ListRooms(窗口边界)", err)
	wantEQ(t, "ListRooms(窗口边界)", "total", reply.GetTotal(), int32(0))
	wantSeq(t, "ListRooms(窗口边界)", st.log, 0, "live_room_anchor.ListRoomsByMid:9001")
}

// TestListRoomsByOwnerSlicesPageInsideWindow 是上一条的正面：可见路径的分页
// 在 Go 侧切片完成（不像 Rooms.List 那样丢 offset），所以第二页真的换了一批房间。
func TestListRoomsByOwnerSlicesPageInsideWindow(t *testing.T) {
	st := newStore()
	for _, id := range []int64{101, 102, 103, 104, 105} {
		st.seedRoom(baseRoom(id, 9001, model.RoomStateReady))
		st.seedAnchor(baseAnchor(20+id-100, id, 9001, model.AnchorRoleManager))
	}
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	first, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 1, PageSize: 2})
	wantNoErr(t, "可见第一页", err)
	second, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 2, PageSize: 2})
	wantNoErr(t, "可见第二页", err)
	third, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, Page: 3, PageSize: 2})
	wantNoErr(t, "可见第三页", err)

	wantInt64sEQ(t, "第一页", "rooms", roomIDs(first.GetRooms()), []int64{105, 104})
	wantInt64sEQ(t, "第二页", "rooms", roomIDs(second.GetRooms()), []int64{103, 102})
	wantInt64sEQ(t, "第三页（不满页）", "rooms", roomIDs(third.GetRooms()), []int64{101})
	wantEQ(t, "总数恒定", "total", third.GetTotal(), int32(5))
}

// TestListRoomsByOwnerTotalCountsBindingsNotRooms 缺陷锁定：
// 可见路径的 total 取 `len(ListRoomsByMid 的返回)`，而该 SQL 是 `SELECT room_id`、
// **没有 DISTINCT**（model/live_room_anchor.go:317-341）。同一主播在同一房间占两个角色
// （联合主播 + 房管）时这里会出现两次同一 room_id，total 被计成 2；
// 回表用的是 `room_id IN (...)`，同一房间只回一行。于是「总数」比实际可见房间多。
// 按当前真实行为断言，修复（加 DISTINCT 或按 id 去重）时本用例即红。
func TestListRoomsByOwnerTotalCountsBindingsNotRooms(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9002, model.RoomStateReady))
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(12, 101, 9001, model.AnchorRoleManager))
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{OwnerMid: 9001, PageSize: 10})

	wantNoErr(t, "ListRooms(双角色绑定)", err)
	wantEQ(t, "缺陷：total 按绑定行数计", "total", reply.GetTotal(), int32(2))
	wantInt64sEQ(t, "页内容按房间去重", "rooms", roomIDs(reply.GetRooms()), []int64{101})
	wantSeq(t, "回表收到重复 id", st.log, 0,
		"live_room_anchor.ListRoomsByMid:9001", "live_room.ListByRoomIDs:n=2")
}

// TestListRoomsByOwnerRespectsStateFilter 说明可见路径与发现页共用同一套过滤条件：
// state/area 会带到回表查询里，被过滤掉的房间不算进页内容（但按上面的缺陷仍算进 total）。
func TestListRoomsByOwnerRespectsStateFilter(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	st.seedRoom(baseRoom(102, 9001, model.RoomStateFinished))
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.seedAnchor(baseAnchor(12, 102, 9001, model.AnchorRoleOwner))
	logic := NewListRoomsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRooms(&rpc.ListRoomsReq{
		OwnerMid: 9001, State: rpc.RoomState_ROOM_STATE_READY, PageSize: 10,
	})

	wantNoErr(t, "ListRooms(可见 + 状态过滤)", err)
	wantInt64sEQ(t, "只回 READY 房", "rooms", roomIDs(reply.GetRooms()), []int64{101})
	call := st.rooms.listByIDsQueries[0]
	wantDeepEQ(t, "状态过滤带到回表", "query", call.q,
		model.RoomListQuery{State: model.RoomStateReady, Limit: 10})
}

// TestReadSideDoesNotRequireDownstreamClients 是本轮的不变量测试：
// creator / risk-control / moderation 三个 client 与 Redis 全部未接入时，
// 7 个读方法必须照常工作（读侧不触达下游）。真触达了就是 nil panic，本用例会崩。
// 反过来也说明：写侧那些「未配置必须显式失败」的分支不在读侧，见 README。
func TestReadSideDoesNotRequireDownstreamClients(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateLiving))
	st.seedSetting(&model.LiveRoomSetting{
		RoomID: 101, DanmakuEnabled: model.BoolToInt32(true), ReplyEnabled: model.BoolToInt32(true),
		RecordEnabled: model.BoolToInt32(false), LinkmicEnabled: model.BoolToInt32(false),
		LiveType: model.LiveTypeVideo, Ctime: 100, Mtime: 200,
	})
	st.seedSession(baseSession(5001, 101, 9001, model.SessionStateLiving))
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.seedBan(baseBan(301, 101, model.BanStateLifted))
	st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))

	sctx := st.svcCtx()
	if sctx.Creator != nil || sctx.RiskControl != nil || sctx.Moderation != nil || sctx.Cache != nil {
		t.Fatal("本用例的前提是三个下游 client 与 Cache 都未接入")
	}
	ctx := context.Background()

	room, err := NewGetRoomLogic(ctx, sctx).GetRoom(&rpc.GetRoomReq{RoomId: 101})
	wantNoErr(t, "GetRoom", err)
	wantEQ(t, "GetRoom", "room_id", room.GetRoom().GetRoomId(), int64(101))

	rooms, err := NewListRoomsLogic(ctx, sctx).ListRooms(&rpc.ListRoomsReq{})
	wantNoErr(t, "ListRooms", err)
	wantEQ(t, "ListRooms", "total", rooms.GetTotal(), int32(1))

	anchors, err := NewListAnchorsLogic(ctx, sctx).ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})
	wantNoErr(t, "ListAnchors", err)
	wantEQ(t, "ListAnchors", "total", anchors.GetTotal(), int32(1))

	// 分区：-1 才是「不过滤」，0 是真实取值（state=0 表示只看停用分区），
	// 所以这里必须显式给 -1，否则查的是「停用分区」这一子集。
	areas, err := NewListAreasLogic(ctx, sctx).ListAreas(&rpc.ListAreasReq{
		ParentAreaId: areaCacheNoFilter, State: areaCacheNoFilter,
	})
	wantNoErr(t, "ListAreas", err)
	wantEQ(t, "ListAreas", "total", areas.GetTotal(), int32(1))

	bans, err := NewListRoomBansLogic(ctx, sctx).ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555})
	wantNoErr(t, "ListRoomBans", err)
	wantEQ(t, "ListRoomBans", "total", bans.GetTotal(), int32(1))

	sessions, err := NewListSessionsLogic(ctx, sctx).ListSessions(&rpc.ListSessionsReq{RoomId: 101})
	wantNoErr(t, "ListSessions", err)
	wantEQ(t, "ListSessions", "条数", len(sessions.GetSessions()), 1)

	session, err := NewGetSessionLogic(ctx, sctx).GetSession(&rpc.GetSessionReq{SessionId: 5001})
	wantNoErr(t, "GetSession", err)
	wantEQ(t, "GetSession", "session_id", session.GetSession().GetSessionId(), int64(5001))

	// 全程没有触发任何未实现的写侧方法，也没有绕过 model 直连库。
	wantCount(t, "读侧不得触达幂等表", st.log, "live_room_idempotency.", 0)
	wantCount(t, "读侧不得触达状态日志表", st.log, "live_room_state_log.", 0)
	wantNoDirectSQL(t, "读侧 7 个方法", st.conn)
}
