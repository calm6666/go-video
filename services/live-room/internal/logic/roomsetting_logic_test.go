package logic

// roomsetting_logic_test.go 覆盖写侧方法 UpdateRoomSetting（updateroomsettinglogic.go）。
//
// 本文件与 roominfo_logic_test.go 是一对**镜像**：同一个房间的两类写入口，
// 差量口径完全相反，两边各钉一半，谁被改坏都会红一条：
//   - UpdateRoomInfo：空串/0 = 未传 = 保持库里原值（「未传即不改」）；
//   - UpdateRoomSetting：整段覆盖，false = 显式关闭，nil setting 直接拒绝
//     （「未传」在这里无法与「全部关闭」区分，让一次漏传把房间功能全关死比拒绝危险）。
//
// 其余锁的结论：
//  1. live_room_setting.Upsert 的冲突键是 PRIMARY KEY(room_id)：有行走更新分支、无行走插入分支，
//     更新分支**保 ctime 只刷 mtime**（本方法最容易被写错的一条）；
//  2. 落库的行是**整段**：库里原有的 record=1 会被这次的 false 抹掉；
//  3. 归属判定用 FindOwner，只有生效房主可以改（房主以外的角色一律 ErrAnchorNotOwner），
//     与 UpdateRoomInfo 的「房主或生效联合主播」不同——这条差异是契约里没有 admin 位造成的，
//     已登记为 README 已知缺口；
//  4. 本方法**全程不开事务**（wantTxCount 0），且抢到键之后的写不带任何房间状态 CAS：
//     见 TestUpdateRoomSettingWritesThroughAfterRoomClosedUnderneath（房间在读取之后被关闭，
//     配置行照样写进去），这是本方法相对其它写方法的结构性差异；
//  5. request_id 三态与其它写方法一致；setting 的字段校验在**任何触库之前**。

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
	setNow     int64 = 1_700_000_000
	setCTime   int64 = setNow - 5000 // 库里那条配置行的创建时间
	setRoom    int64 = 4101
	setMid     int64 = 9101 // 生效房主
	setCohost  int64 = 9103 // 生效联合主播
	setManager int64 = 9102 // 房管
	setStran   int64 = 9104 // 无绑定
	setOther   int64 = 4102 // 另一个房间
)

// setReq 组一条整段覆盖请求；setting 为 nil 用来打「缺 setting」那条守卫。
func setReq(reqID string, s *rpc.RoomSetting) *rpc.UpdateRoomSettingReq {
	return &rpc.UpdateRoomSettingReq{
		RoomId: setRoom, OperatorMid: setMid, Setting: s, RequestId: reqID, TraceId: "trace-set",
	}
}

// allOn 是一条「四开关全开 + 语音直播 + 最低版本 5」的整段配置。
func allOn() *rpc.RoomSetting {
	return &rpc.RoomSetting{
		RoomId: setRoom, DanmakuEnabled: true, ReplyEnabled: true,
		RecordEnabled: true, LinkmicEnabled: true,
		LiveType: model.LiveTypeAudio, MinClientVersionCode: 5,
	}
}

// allOff 是 allOn 的「全部显式关闭」镜像：布尔全 false、live_type 未填（0）、版本不限（0）。
// 这三个 0 值各有语义：live_type=0 归一为视频直播，min=0 是合法的「不限制」。
func allOff() *rpc.RoomSetting {
	return &rpc.RoomSetting{RoomId: setRoom}
}

func newSettingLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *UpdateRoomSettingLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewUpdateRoomSettingLogic(context.Background(), st.svcCtxWith(c))
}

// seedSettingScene 布「非终态房间 + 生效房主 + 一行已有配置」。
// 已有配置行刻意把 record/linkmic 打开、live_type 设成语音、min 设成 5，
// 这样「整段覆盖」才会留下可观察的差异。
func seedSettingScene(t *testing.T, st *store, state int32) (*model.LiveRoom, *model.LiveRoomSetting) {
	t.Helper()
	r := baseRoom(setRoom, setMid, state)
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(701, setRoom, setMid, model.AnchorRoleOwner))
	s := baseSetting(setRoom, setCTime)
	s.RecordEnabled = model.BoolToInt32(true)
	s.LinkmicEnabled = model.BoolToInt32(true)
	s.LiveType = model.LiveTypeAudio
	s.MinClientVersionCode = 5
	s.Mtime = setCTime
	st.seedSetting(s)
	return r, s
}

// TestUpdateRoomSettingWholeRowOverwriteIsTheContract 是与
// TestUpdateRoomInfoEmptyDeltaWritesNothing 成对的核心用例：
// 没在 rpc 里出现的字段（布尔零值）在这里被当成「显式关闭」写进库，
// 而且更新分支只刷 mtime、ctime 必须留住。
func TestUpdateRoomSettingWholeRowOverwriteIsTheContract(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	lg := newSettingLogic(t, st)

	reply, err := lg.UpdateRoomSetting(setReq("req-set-off", allOff()))
	wantNoErr(t, "UpdateRoomSetting", err)
	defer st.checkRaces(t)

	wantSeq(t, "整段覆盖首次执行", st.log, 0,
		"live_room.FindOne:4101",
		fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
		"live_room_idempotency.Claim:req-set-off",
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom),
		fmt.Sprintf("live_room_setting.FindOne:%d", setRoom),
		"live_room_idempotency.SaveResult:req-set-off",
	)
	// 一条事务都没有：这正是它与 BanRoom / CloseRoom 的结构性差别。
	wantTxCount(t, "UpdateRoomSetting 全程", st.conn, 0)

	row := st.settingAt(t, setRoom)
	wantEQ(t, "整段覆盖", "danmaku 由 1 被写成 0", row.DanmakuEnabled, model.BoolToInt32(false))
	wantEQ(t, "整段覆盖", "reply 由 1 被写成 0", row.ReplyEnabled, model.BoolToInt32(false))
	wantEQ(t, "整段覆盖", "record 由 1 被抹成 0", row.RecordEnabled, model.BoolToInt32(false))
	wantEQ(t, "整段覆盖", "linkmic 由 1 被抹成 0", row.LinkmicEnabled, model.BoolToInt32(false))
	// live_type=0（客户端未填）归一为视频直播，不是「保持原值 2」。
	wantEQ(t, "整段覆盖", "live_type 归一", row.LiveType, model.LiveTypeVideo)
	// min_client_version_code=0 是合法值「不限制」，同样被覆盖掉原来的 5。
	wantEQ(t, "整段覆盖", "min 被覆盖为 0", row.MinClientVersionCode, int32(0))
	// ON DUPLICATE KEY UPDATE 的更新分支里没有 ctime：首次创建时间必须留住。
	wantEQ(t, "整段覆盖", "ctime 保住", row.Ctime, setCTime)
	wantEQ(t, "整段覆盖", "mtime 刷新", row.Mtime, setNow)
	// 冲突键是 room_id：同一房间只可能有一行，第二次写不许插出新行。
	wantEQ(t, "整段覆盖", "配置行数", len(st.settings.rows), 1)

	wantEQ(t, "整段覆盖应答", "replayed", reply.GetReplayed(), false)
	wantEQ(t, "整段覆盖应答", "回读后的 danmaku", reply.GetSetting().GetDanmakuEnabled(), false)
	wantEQ(t, "整段覆盖应答", "回读后的 live_type", reply.GetSetting().GetLiveType(), model.LiveTypeVideo)
	wantEQ(t, "整段覆盖应答", "回读后的 room_id", reply.GetSetting().GetRoomId(), setRoom)
	wantEQ(t, "整段覆盖应答", "回读后的 mtime", reply.GetSetting().GetMtime(), setNow)
	// 房间行一格未动：改配置不推进状态、不动 state_version。
	room := st.roomAt(t, setRoom)
	wantEQ(t, "整段覆盖", "房间 state", room.State, model.RoomStatePending)
	wantEQ(t, "整段覆盖", "房间 state_version", room.StateVersion, int32(1))
	wantMethodCount(t, "改配置不动房间行", st.log, "live_room.UpdateProfile", 0)
	wantMethodCount(t, "改配置不动房间行", st.log, "live_room.TransitionTx", 0)
	// 本方法不写审计：配置变更没有状态迁移可记（与 README 里的归因口径一致）。
	wantMethodCount(t, "改配置不写审计", st.log, "live_room_state_log.Insert", 0)
	wantMethodCount(t, "改配置不写审计", st.log, "live_room_state_log.InsertTx", 0)
	wantDeepEQ(t, "落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, settings: 1, idem: 1})
}

// TestUpdateRoomSettingExplicitTrueAndValuesReachTheRow 补上镜像的另一半：
// 传 true 与传具体枚举/数字都按给定值落库（不是「与库里相同就跳过」）。
func TestUpdateRoomSettingExplicitTrueAndValuesReachTheRow(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStateLiving)

	s := allOn()
	s.LiveType = model.LiveTypeScreen
	s.MinClientVersionCode = 42
	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-on", s))
	wantNoErr(t, "全开 + 屏幕分享", err)

	row := st.settingAt(t, setRoom)
	wantEQ(t, "全开", "danmaku", row.DanmakuEnabled, model.BoolToInt32(true))
	wantEQ(t, "全开", "record", row.RecordEnabled, model.BoolToInt32(true))
	wantEQ(t, "全开", "live_type 按传入", row.LiveType, model.LiveTypeScreen)
	wantEQ(t, "全开", "min 按传入", row.MinClientVersionCode, int32(42))
	wantEQ(t, "在播改配置", "房间 state 不动", st.roomAt(t, setRoom).State, model.RoomStateLiving)
}

// TestUpdateRoomSettingInsertsWhenRoomHasNoRow 锁「房间从没写过配置」这一分支：
// Upsert 走插入，ctime 与 mtime 都是本次 now。
func TestUpdateRoomSettingInsertsWhenRoomHasNoRow(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	r := baseRoom(setRoom, setMid, model.RoomStateReady)
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(702, setRoom, setMid, model.AnchorRoleOwner))

	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-insert", allOn()))
	wantNoErr(t, "无配置行时首次落库", err)

	wantSeq(t, "无配置行时首次落库", st.log, 3,
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom),
		fmt.Sprintf("live_room_setting.FindOne:%d", setRoom),
		"live_room_idempotency.SaveResult:req-set-insert",
	)
	row := st.settingAt(t, setRoom)
	wantEQ(t, "新建配置行", "ctime 是本次 now", row.Ctime, setNow)
	wantEQ(t, "新建配置行", "mtime 是本次 now", row.Mtime, setNow)
	wantDeepEQ(t, "新建配置行", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, settings: 1, idem: 1})
}

// TestUpdateRoomSettingRoomIDComesFromTheRequestNotTheSetting 锁「整段落谁的 room_id」：
// 契约里 RoomSetting.room_id 与顶层 room_id 重复，本方法以顶层为准，
// 否则调用方改 A 房间的 setting 结构体就能写到 B 房间上。
func TestUpdateRoomSettingRoomIDComesFromTheRequestNotTheSetting(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)

	s := allOn()
	s.RoomId = setOther // 与顶层 room_id 不一致：必须被忽略。
	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-swap", s))
	wantNoErr(t, "内层 room_id 不一致", err)

	wantSeq(t, "内层 room_id 不一致", st.log, 2,
		"live_room_idempotency.Claim:req-set-swap",
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom), // 落的是顶层那个房间
		fmt.Sprintf("live_room_setting.FindOne:%d", setRoom),
		"live_room_idempotency.SaveResult:req-set-swap",
	)
	wantEQ(t, "内层 room_id 不一致", "配置行数仍是 1", len(st.settings.rows), 1)
	wantEQ(t, "内层 room_id 不一致", "行的归属", st.settingAt(t, setRoom).RoomID, setRoom)
	wantEQ(t, "内层 room_id 不一致", "room_id=4102 没被插出来", len(st.settings.rows), 1)
}

// TestUpdateRoomSettingOwnerOnlyGuard 锁归属判定：只有生效房主能改，
// 且判定在任何写之前、不烧键。房主缺行与「有房主但不是他」共用同一个哨兵。
func TestUpdateRoomSettingOwnerOnlyGuard(t *testing.T) {
	fixClock(t, setNow)

	cases := []struct {
		name string
		mid  int64
		seed func(*testing.T, *store)
	}{
		{"联合主播不行", setCohost, func(t *testing.T, st *store) {
			st.seedAnchor(baseAnchor(711, setRoom, setCohost, model.AnchorRoleCohost))
		}},
		{"房管不行", setManager, func(t *testing.T, st *store) {
			st.seedAnchor(baseAnchor(712, setRoom, setManager, model.AnchorRoleManager))
		}},
		{"无绑定的人不行", setStran, func(*testing.T, *store) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedSettingScene(t, st, model.RoomStatePending)
			tc.seed(t, st)

			before := *st.settingAt(t, setRoom)
			in := setReq("req-set-owner", allOff())
			in.OperatorMid = tc.mid // 三条都是「非房主」：联合主播 / 房管 / 无绑定
			_, err := newSettingLogic(t, st).UpdateRoomSetting(in)
			wantErrIs(t, tc.name, err, model.ErrAnchorNotOwner)
			wantErrContains(t, tc.name, err, fmt.Sprintf("operator_mid=%d 不是该房间生效房主", tc.mid))
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4101",
				fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
			)
			wantKeyUnburned(t, tc.name, "req-set-owner", st)
			wantMethodCount(t, tc.name, st.log, "live_room_setting.Upsert", 0)
			wantDeepEQ(t, tc.name, "配置行一字未动", *st.settingAt(t, setRoom), before)
		})
	}

	t.Run("房间没有生效房主时同哨兵", func(t *testing.T) {
		st := newStore()
		r := baseRoom(setRoom, setMid, model.RoomStatePending)
		st.seedRoom(r)
		// 只剩一条联合主播绑定：live_room.owner_mid 还指着 setMid，
		// 但 role=OWNER AND state=1 的行没有——FindOwner 回 (nil, nil)。
		st.seedAnchor(baseAnchor(713, setRoom, setMid, model.AnchorRoleCohost))
		st.seedSetting(baseSetting(setRoom, setCTime))

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-noowner", allOn()))
		wantErrIs(t, "无生效房主", err, model.ErrAnchorNotOwner)
		wantErrContains(t, "无生效房主", err, "不是该房间生效房主")
		wantSeq(t, "无生效房主", st.log, 0,
			"live_room.FindOne:4101",
			fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
		)
		wantKeyUnburned(t, "无生效房主", "req-set-noowner", st)
	})
}

func TestUpdateRoomSettingRoomStateAndExistence(t *testing.T) {
	t.Run("终态房间直接拒", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStateFinished)

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-term", allOn()))
		wantErrIs(t, "已关闭房间改配置", err, model.ErrRoomFinished)
		// 终态判定在归属判定之前：连 FindOwner 都不该发。
		wantSeq(t, "已关闭房间改配置", st.log, 0, "live_room.FindOne:4101")
		wantMethodCount(t, "已关闭房间改配置", st.log, "live_room_anchor.FindOwner", 0)
		wantKeyUnburned(t, "已关闭房间改配置", "req-set-term", st)
	})

	t.Run("BANNED 房间仍然可以改配置", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStateBanned)

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-banned", allOff()))
		wantNoErr(t, "禁播房间改配置", err)
		wantEQ(t, "禁播房间改配置", "配置已落",
			st.settingAt(t, setRoom).DanmakuEnabled, model.BoolToInt32(false))
	})

	t.Run("房间不存在", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-none", allOn()))
		wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
		wantSeq(t, "房间不存在", st.log, 0, "live_room.FindOne:4101")
		wantKeyUnburned(t, "房间不存在", "req-set-none", st)
	})

	t.Run("房间读失败原样上抛", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStatePending)
		boom := errors.New("test: room read failed")
		st.rooms.failWith("FindOne", boom)

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-readfail", allOn()))
		if !errors.Is(err, boom) {
			t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
		}
		wantSeq(t, "房间读失败", st.log, 0, "live_room.FindOne:4101")
		wantKeyUnburned(t, "房间读失败", "req-set-readfail", st)
	})
}

// TestUpdateRoomSettingNilRequestAndGuards 锁入参守卫：
// 全部在第一次触库之前，且 operator 用的是 checkOperator（ErrOperatorRequired），
// 与 UpdateRoomInfo 的 checkMid（ErrInvalidMid）不同——两个方法的「操作者」语义不一样。
func TestUpdateRoomSettingNilRequestAndGuards(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.UpdateRoomSettingReq
		want error
		frag string
	}{
		{"nil 请求", nil, model.ErrInvalidRoomID, ""},
		{"房间号 0", setReq("g", allOn()), model.ErrInvalidRoomID, ""},
		{"操作者 0", setReq("g", allOn()), model.ErrOperatorRequired, ""},
		{"幂等键空", setReq("", allOn()), model.ErrRequestIDRequired, ""},
		{"幂等键超长", setReq(strings.Repeat("k", 65), allOn()), model.ErrDedupIDTooLong, "max 64"},
		{"缺 setting 整段", setReq("g", nil), model.ErrSettingInvalid,
			"setting 必填（整段覆盖，缺字段无法与显式关闭区分）"},
		{"live_type 未定义", setReq("g", &rpc.RoomSetting{RoomId: setRoom, LiveType: 9}),
			model.ErrSettingInvalid, "live_type=9"},
		{"最低版本为负", setReq("g", &rpc.RoomSetting{RoomId: setRoom, MinClientVersionCode: -1}),
			model.ErrSettingInvalid, "min_client_version_code=-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, setNow)
			st := newStore()
			seedSettingScene(t, st, model.RoomStatePending)
			lg := newSettingLogic(t, st)
			before := st.log.snapshot()

			in := tc.in
			if in != nil {
				switch tc.name {
				case "房间号 0":
					in.RoomId = 0
				case "操作者 0":
					in.OperatorMid = 0
				}
			}
			reply, err := lg.UpdateRoomSetting(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			if reply != nil {
				t.Fatalf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantNoCallAfter(t, tc.name, st.log, before)
		})
	}
}

// TestUpdateRoomSettingGuardOrder 单独锁「缺 setting 与非法 live_type 都在触库之前」：
// 这两条是**字段级**守卫，UpdateRoomInfo 的同类守卫却在权限读之后，
// 整段覆盖的语义要求先把「要写成什么」定下来再去碰库。
func TestUpdateRoomSettingGuardOrder(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	lg := newSettingLogic(t, st)
	before := st.log.snapshot()

	// 房间号非法 + 缺 setting：先报房间号。
	_, err := lg.UpdateRoomSetting(&rpc.UpdateRoomSettingReq{OperatorMid: setMid, RequestId: "g1"})
	wantErrIs(t, "room_id 先于 setting", err, model.ErrInvalidRoomID)
	// 操作者非法 + live_type 非法：先报操作者。
	_, err = lg.UpdateRoomSetting(&rpc.UpdateRoomSettingReq{
		RoomId: setRoom, RequestId: "g2", Setting: &rpc.RoomSetting{LiveType: 7}})
	wantErrIs(t, "operator 先于 setting 内容", err, model.ErrOperatorRequired)
	// 房间不存在 + live_type 非法：先报 live_type（一个 DB 调用都没有）。
	_, err = lg.UpdateRoomSetting(&rpc.UpdateRoomSettingReq{
		RoomId: 999999, OperatorMid: setMid, RequestId: "g3", Setting: &rpc.RoomSetting{LiveType: 7}})
	wantErrIs(t, "setting 内容先于读房间", err, model.ErrSettingInvalid)
	wantNoCallAfter(t, "UpdateRoomSetting 守卫顺序", st.log, before)
}

// TestUpdateRoomSettingUpsertFailureLeavesRowUntouched 锁「唯一那条写语句失败」的形态：
// 没有事务，所以「回滚」根本不是这里的语义——库里剩的就是失败前写进去的东西。
// 本例失败点在写之前，所以配置行一字未动，但键已经烧掉。
func TestUpdateRoomSettingUpsertFailureLeavesRowUntouched(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	before := *st.settingAt(t, setRoom) // 值拷贝：下面要比的是「失败前后各读一次」，不是同一指针自比
	boom := errors.New("test: upsert failed")
	st.settings.failWith("Upsert", boom)

	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-writefail", allOff()))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	wantSeq(t, "配置写失败", st.log, 0,
		"live_room.FindOne:4101",
		fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
		"live_room_idempotency.Claim:req-set-writefail",
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom),
	)
	wantMethodCount(t, "配置写失败", st.log, "live_room_setting.FindOne", 0)
	wantKeyBurnedNoResult(t, "配置写失败", "req-set-writefail", st)
	wantDeepEQ(t, "配置写失败残留", "整行", *st.settingAt(t, setRoom), before)
	wantTxCount(t, "配置写失败", st.conn, 0)

	// 重试同一条键拿不到结果：整段覆盖的写失败必须换新键重试。
	_, err = newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-writefail", allOff()))
	wantErrIs(t, "配置写失败后重试", err, model.ErrIdempotencyResultMissing)
}

// TestUpdateRoomSettingReadBackFailureReportsErrorWithCommittedWrite 锁「已写成功但对外报错」：
// 回读失败时配置已经落库，键也没回填。
func TestUpdateRoomSettingReadBackFailureReportsErrorWithCommittedWrite(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	boom := errors.New("test: read back failed")
	st.settings.failWith("FindOne", boom)

	reply, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-readback", allOff()))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	if reply != nil {
		t.Fatalf("应答 = %+v, want nil", reply)
	}
	wantSeq(t, "配置回读失败", st.log, 3,
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom),
		fmt.Sprintf("live_room_setting.FindOne:%d", setRoom),
	)
	wantEQ(t, "配置回读失败", "写已经落库",
		st.settingAt(t, setRoom).DanmakuEnabled, model.BoolToInt32(false))
	wantMethodCount(t, "配置回读失败", st.log, "live_room_idempotency.SaveResult", 0)
	wantKeyBurnedNoResult(t, "配置回读失败", "req-set-readback", st)
}

// TestUpdateRoomSettingClaimFailurePropagates 锁抢键失败：不写配置。
func TestUpdateRoomSettingClaimFailurePropagates(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	boom := errors.New("test: claim failed")
	st.idem.failWith("Claim", boom)

	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-claim", allOff()))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	wantSeq(t, "抢键失败", st.log, 2,
		"live_room_idempotency.Claim:req-set-claim",
	)
	wantMethodCount(t, "抢键失败", st.log, "live_room_setting.Upsert", 0)
	wantEQ(t, "抢键失败", "配置未动",
		st.settingAt(t, setRoom).DanmakuEnabled, model.BoolToInt32(true))
}

// TestUpdateRoomSettingOwnerReadFailurePropagates 锁归属读失败：既不写也不烧键。
func TestUpdateRoomSettingOwnerReadFailurePropagates(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	boom := errors.New("test: owner read failed")
	st.anchors.failWith("FindOwner", boom)

	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-ownerfail", allOff()))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	wantSeq(t, "归属读失败", st.log, 0,
		"live_room.FindOne:4101",
		fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
	)
	wantKeyUnburned(t, "归属读失败", "req-set-ownerfail", st)
	wantMethodCount(t, "归属读失败", st.log, "live_room_setting.Upsert", 0)
}

// TestUpdateRoomSettingWritesThroughAfterRoomClosedUnderneath 钉住当前行为：
// 读到「非终态」与写配置行之间没有任何复核（不在事务里、UPDATE 也不带房间状态条件），
// 所以房间在中间被关掉时配置照样落库，且本方法回成功。
// BanRoom / CloseRoom 都在自己的事务里用 `state=from` 守住这一秒，本方法没有——
// 已登记为 README 已知缺口。
func TestUpdateRoomSettingWritesThroughAfterRoomClosedUnderneath(t *testing.T) {
	fixClock(t, setNow)
	st := newStore()
	seedSettingScene(t, st, model.RoomStatePending)
	st.raceBefore("live_room_setting.Upsert", func() {
		for _, r := range st.rooms.rows {
			if r.RoomID == setRoom {
				r.State = model.RoomStateFinished // 等价于「房主在两条语句之间关了房」
				r.StateVersion++
			}
		}
	})

	_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-toctou", allOff()))
	wantNoErr(t, "关房竞态下的配置写", err) // 当前行为：成功，不报错
	st.checkRaces(t)

	wantSeq(t, "关房竞态下的配置写", st.log, 2,
		"live_room_idempotency.Claim:req-set-toctou",
		fmt.Sprintf("live_room_setting.Upsert:%d", setRoom),
		fmt.Sprintf("live_room_setting.FindOne:%d", setRoom),
		"live_room_idempotency.SaveResult:req-set-toctou",
	)
	// 终态房间里多了一行覆盖后的配置：danmaku 已被写成 0。
	wantEQ(t, "关房竞态", "房间已终态", st.roomAt(t, setRoom).State, model.RoomStateFinished)
	wantEQ(t, "关房竞态", "配置仍被改写",
		st.settingAt(t, setRoom).DanmakuEnabled, model.BoolToInt32(false))
	wantTxCount(t, "关房竞态", st.conn, 0)
}

func TestUpdateRoomSettingReplayIdempotencyTriad(t *testing.T) {
	t.Run("命中重放回存过的整段", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStatePending)
		st.seedIdem("req-set-replay", "UpdateRoomSetting",
			`{"setting":{"room_id":4101,"danmaku_enabled":true,"reply_enabled":true,"live_type":3,"min_client_version_code":7,"mtime":111}}`)

		reply, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("req-set-replay", allOff()))
		wantNoErr(t, "配置重放", err)
		wantSeq(t, "配置重放", st.log, 0,
			"live_room.FindOne:4101",
			fmt.Sprintf("live_room_anchor.FindOwner:%d", setRoom),
			"live_room_idempotency.Claim:req-set-replay",
			"live_room_idempotency.Find:req-set-replay",
		)
		wantEQ(t, "配置重放", "replayed", reply.GetReplayed(), true)
		// 回的是首次执行时存的整段值，不是本次请求要写的那一套。
		wantEQ(t, "配置重放", "live_type", reply.GetSetting().GetLiveType(), int32(3))
		wantEQ(t, "配置重放", "min", reply.GetSetting().GetMinClientVersionCode(), int32(7))
		wantEQ(t, "配置重放", "mtime", reply.GetSetting().GetMtime(), int64(111))
		wantEQ(t, "配置重放", "danmaku", reply.GetSetting().GetDanmakuEnabled(), true)
		// 零新写。
		wantMethodCount(t, "配置重放", st.log, "live_room_setting.Upsert", 0)
		wantMethodCount(t, "配置重放", st.log, "live_room_setting.FindOne", 0)
		wantMethodCount(t, "配置重放", st.log, "live_room_idempotency.SaveResult", 0)
	})

	t.Run("键被别的 RPC 用过", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStatePending)
		st.seedIdem("shared-set", "CloseRoom", `{"state":4}`)

		_, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("shared-set", allOff()))
		wantErrIs(t, "键被别人用过", err, model.ErrRequestIDReused)
		wantErrContains(t, "键被别人用过", err, "used by CloseRoom")
		wantSeq(t, "键被别人用过", st.log, 2,
			"live_room_idempotency.Claim:shared-set",
			"live_room_idempotency.Find:shared-set",
		)
		wantMethodCount(t, "键被别人用过", st.log, "live_room_setting.Upsert", 0)
	})

	t.Run("烧过的键没有结果", func(t *testing.T) {
		fixClock(t, setNow)
		st := newStore()
		seedSettingScene(t, st, model.RoomStatePending)
		st.seedIdem("burned-set", "UpdateRoomSetting", "")

		reply, err := newSettingLogic(t, st).UpdateRoomSetting(setReq("burned-set", allOff()))
		wantErrIs(t, "烧过的键", err, model.ErrIdempotencyResultMissing)
		if reply != nil {
			t.Fatalf("应答 = %+v, want nil", reply)
		}
		wantSeq(t, "烧过的键", st.log, 2,
			"live_room_idempotency.Claim:burned-set",
			"live_room_idempotency.Find:burned-set",
		)
		wantMethodCount(t, "烧过的键", st.log, "live_room_setting.Upsert", 0)
	})
}
