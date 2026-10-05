package model

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/live-room/rpc"
)

// 本文件覆盖 model 层不碰数据库就能判对错的三类规则：
//  1. 状态机矩阵与 proto 枚举编号的一致性（迁移与 RPC 契约都靠这套编号）；
//  2. SQL 片段构造器（WHERE / ORDER BY / SET）的拼接结果——这些字符串直接决定
//     「列表与总数口径一致」「未知排序值不会被注入」；
//  3. 入参校验先于 SQL：用 nil 连接调用，只允许拿到哨兵错误。
//     若某个方法把校验放到 SQL 之后，这里会 panic 而不是返回错误，
//     这正是要的效果（AGENTS.md §9：不许用假实现让测试变绿）。

// --- 1. 状态机矩阵 ---

// TestRoomTransitionMatrixIsClosed 校验房间状态机的封闭性与「未知即拒绝」。
func TestRoomTransitionMatrixIsClosed(t *testing.T) {
	defined := []int32{
		RoomStatePending, RoomStateReady, RoomStateLiving, RoomStateFinished,
		RoomStateBanned, RoomStateDisabled,
	}
	for _, s := range defined {
		if !ValidRoomState(s) {
			t.Errorf("RoomState=%d 已定义但 ValidRoomState 判为非法，矩阵漏了该状态", s)
		}
	}
	for _, s := range []int32{RoomStateUnspecified, 7, 99, -1} {
		if ValidRoomState(s) {
			t.Errorf("RoomState=%d 不该被判为合法（禁止未知取值进矩阵后被当成可迁移）", s)
		}
		// UNSPECIFIED 与未知值都不得作为迁移起点或终点。
		for _, other := range defined {
			if CanRoomTransition(s, other) || CanRoomTransition(other, s) {
				if s == RoomStateUnspecified && other == RoomStateUnspecified {
					continue
				}
				t.Errorf("CanRoomTransition 允许了涉及非法状态 %d 的迁移", s)
			}
		}
	}
	// FINISHED 是终态：任何路径都不得离开（CloseRoom 之后只读）。
	if got := RoomTransitionTargets(RoomStateFinished); len(got) != 0 {
		t.Errorf("FINISHED 的出边应为空，实际 %v", got)
	}
	// LIVING 只能回 READY / 被禁播 / 被关闭，不得直接回 PENDING 或 DISABLED
	// （直播中「退回待完善」会让正在看的观众看到一个不存在的房间）。
	targets := RoomTransitionTargets(RoomStateLiving)
	if containsInt32(targets, RoomStatePending) || containsInt32(targets, RoomStateDisabled) {
		t.Errorf("LIVING 不应能直接迁到 PENDING/DISABLED，实际 %v", targets)
	}
	// 解禁落点取决于 verify_state，所以 PENDING 与 READY 两条边都必须在。
	bannedTargets := RoomTransitionTargets(RoomStateBanned)
	if !containsInt32(bannedTargets, RoomStatePending) || !containsInt32(bannedTargets, RoomStateReady) {
		t.Errorf("BANNED 解禁必须同时可落 PENDING 与 READY，实际 %v", bannedTargets)
	}
	// RoomTransitionTargets 必须升序且不得暴露矩阵内部状态。
	if !sortedAsc(targets) {
		t.Errorf("RoomTransitionTargets 必须升序返回，实际 %v", targets)
	}
	if len(RoomTransitionTargets(RoomStateReady)) == 0 {
		t.Error("READY 无出边，说明矩阵没被正确填充")
	}
}

// TestVerifyStateMatrixForbidsDirectPass 锁死审核侧最容易走捷径的一条边：
// REJECTED 只能经「重新送审」离开，禁止直接写 PASSED（AGENTS.md §8）。
func TestVerifyStateMatrixForbidsDirectPass(t *testing.T) {
	if CanVerifyTransition(VerifyStateRejected, VerifyStatePassed) {
		t.Error("REJECTED 直接迁到 PASSED 等于绕过审核，必须禁止")
	}
	if !CanVerifyTransition(VerifyStateRejected, VerifyStateReviewing) {
		t.Error("REJECTED 必须可以重新送审")
	}
	if !CanVerifyTransition(VerifyStatePassed, VerifyStateReviewing) {
		t.Error("改资料后重新送审（PASSED→REVIEWING）必须允许")
	}
	if CanVerifyTransition(VerifyStateUnspecified, VerifyStatePassed) {
		t.Error("UNSPECIFIED 不得作为迁移起点")
	}
	for _, s := range []int32{VerifyStateNone, VerifyStateReviewing, VerifyStatePassed, VerifyStateRejected} {
		if !ValidVerifyState(s) {
			t.Errorf("VerifyState=%d 已定义但判为非法", s)
		}
	}
}

// TestSessionMatrixTerminalStates 校验场次终态不可回改，以及「进行中」集合的口径。
func TestSessionMatrixTerminalStates(t *testing.T) {
	for _, term := range []int32{SessionStateEnded, SessionStateTerminated} {
		if !SessionStateIsTerminal(term) {
			t.Errorf("SessionState=%d 应为终态", term)
		}
		for _, to := range []int32{SessionStatePending, SessionStateLiving, SessionStateEnded, SessionStateTerminated} {
			if CanSessionTransition(term, to) {
				t.Errorf("终态 %d 不得迁到 %d：时长簿记已落定", term, to)
			}
		}
	}
	for _, active := range ActiveSessionStates {
		if SessionStateIsTerminal(active) {
			t.Errorf("ActiveSessionStates 含终态 %d，FindActiveByRoom 会捞到已结束场次", active)
		}
	}
	if len(ActiveSessionStates) != 2 {
		t.Errorf("进行中场次应只有 PENDING/LIVING 两态，实际 %v", ActiveSessionStates)
	}
	if SessionStateIsTerminal(SessionStateUnspecified) {
		t.Error("UNSPECIFIED 不是终态")
	}
	// PENDING 可以不经 LIVING 直接结束（建档后推流从未到达）。
	if !CanSessionTransition(SessionStatePending, SessionStateEnded) {
		t.Error("PENDING 必须可以直接 ENDED，否则空建档场次永远关不掉")
	}
	if got := SessionTransitionTargets(SessionStatePending); !containsInt32(got, SessionStateLiving) {
		t.Errorf("PENDING 应可迁到 LIVING，实际 %v", got)
	}
}

// TestReplayAndBanMatrices 校验回放与禁播记录的状态机边界。
func TestReplayAndBanMatrices(t *testing.T) {
	// AVAILABLE → REMOVED 是唯一出口（版权撤回/违规），且 REMOVED 不可逆。
	if !CanReplayTransition(ReplayStateAvailable, ReplayStateRemoved) {
		t.Error("AVAILABLE 必须可被 REMOVED（版权撤回必须有落点）")
	}
	if CanReplayTransition(ReplayStateRemoved, ReplayStateAvailable) {
		t.Error("REMOVED 不得回到 AVAILABLE：撤回后自行复活会绕过版权决定")
	}
	if CanReplayTransition(ReplayStateNone, ReplayStateRemoved) {
		t.Error("无回放的场次不该迁到 REMOVED")
	}
	for _, to := range []int32{ReplayStateProcessing, ReplayStateAvailable, ReplayStateRemoved} {
		if !ValidReplayState(to) {
			t.Errorf("ReplayState=%d 已定义但判为非法", to)
		}
	}
	// 禁播记录：解除与到期都是终态，且二者互不可转（区分「人解除的」与「自己到期的」）。
	if CanBanRecordTransition(BanStateLifted, BanStateExpired) || CanBanRecordTransition(BanStateExpired, BanStateLifted) {
		t.Error("禁播记录的 LIFTED/EXPIRED 必须互不可转，否则审计归因会失真")
	}
	if !CanBanRecordTransition(BanStateActive, BanStateLifted) || !CanBanRecordTransition(BanStateActive, BanStateExpired) {
		t.Error("生效禁播必须可被解除或到期")
	}
	if ValidBanState(BanStateUnspecifiedAlias) {
		t.Error("禁播记录状态 0 不应被判为合法")
	}
}

// BanStateUnspecifiedAlias 显式写出「0 不是合法禁播状态」这个前提。
const BanStateUnspecifiedAlias int32 = 0

// TestValidatorRejectsUnspecifiedAndUnknown 校验全部枚举判定函数不接受 0 与越界值。
func TestValidatorRejectsUnspecifiedAndUnknown(t *testing.T) {
	type vc struct {
		name    string
		valid   func(int32) bool
		okays   []int32
		badness []int32
	}
	for _, c := range []vc{
		{"AnchorRole", ValidAnchorRole, []int32{AnchorRoleOwner, AnchorRoleCohost, AnchorRoleManager},
			[]int32{AnchorRoleUnspecified, 4, -1}},
		{"Platform", ValidPlatform, []int32{PlatformAndroid, PlatformIOS, PlatformHarmony, PlatformDesktop},
			[]int32{PlatformUnspecified, 5, -1}},
		{"LiveType", ValidLiveType, []int32{LiveTypeVideo, LiveTypeAudio, LiveTypeScreen},
			[]int32{0, 4, -1}},
		{"StreamState", ValidStreamState, []int32{StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped},
			[]int32{0, 5, -1}},
	} {
		for _, v := range c.okays {
			if !c.valid(v) {
				t.Errorf("%s(%d) 应为合法", c.name, v)
			}
		}
		for _, v := range c.badness {
			if c.valid(v) {
				t.Errorf("%s(%d) 应被拒绝（未知即放行会让非法值写进状态列）", c.name, v)
			}
		}
	}
}

// TestModelStateConstantsMatchProtoEnums 把 model 的状态常量与生成的 proto 枚举编号钉在一起。
//
// 这两套编号必须逐值相等：model 直接往状态列里写自己的常量，logic 再把列值转成 rpc 枚举。
// 任何一侧「顺手重排」都不会编译失败，只会让客户端读到错位状态（把 FINISHED 显示成 BANNED）。
func TestModelStateConstantsMatchProtoEnums(t *testing.T) {
	cases := []struct {
		name  string
		model int32
		proto int32
	}{
		{"RoomState.PENDING", RoomStatePending, int32(rpc.RoomState_ROOM_STATE_PENDING)},
		{"RoomState.READY", RoomStateReady, int32(rpc.RoomState_ROOM_STATE_READY)},
		{"RoomState.LIVING", RoomStateLiving, int32(rpc.RoomState_ROOM_STATE_LIVING)},
		{"RoomState.FINISHED", RoomStateFinished, int32(rpc.RoomState_ROOM_STATE_FINISHED)},
		{"RoomState.BANNED", RoomStateBanned, int32(rpc.RoomState_ROOM_STATE_BANNED)},
		{"RoomState.DISABLED", RoomStateDisabled, int32(rpc.RoomState_ROOM_STATE_DISABLED)},

		{"VerifyState.NONE", VerifyStateNone, int32(rpc.VerifyState_VERIFY_STATE_NONE)},
		{"VerifyState.REVIEWING", VerifyStateReviewing, int32(rpc.VerifyState_VERIFY_STATE_REVIEWING)},
		{"VerifyState.PASSED", VerifyStatePassed, int32(rpc.VerifyState_VERIFY_STATE_PASSED)},
		{"VerifyState.REJECTED", VerifyStateRejected, int32(rpc.VerifyState_VERIFY_STATE_REJECTED)},

		{"SessionState.PENDING", SessionStatePending, int32(rpc.SessionState_SESSION_STATE_PENDING)},
		{"SessionState.LIVING", SessionStateLiving, int32(rpc.SessionState_SESSION_STATE_LIVING)},
		{"SessionState.ENDED", SessionStateEnded, int32(rpc.SessionState_SESSION_STATE_ENDED)},
		{"SessionState.TERMINATED", SessionStateTerminated, int32(rpc.SessionState_SESSION_STATE_TERMINATED)},

		{"EndReason.ANCHOR_STOP", EndReasonAnchorStop, int32(rpc.EndReason_END_REASON_ANCHOR_STOP)},
		{"EndReason.BANNED", EndReasonBanned, int32(rpc.EndReason_END_REASON_BANNED)},
		{"EndReason.ROOM_CLOSED", EndReasonRoomClosed, int32(rpc.EndReason_END_REASON_ROOM_CLOSED)},
		{"EndReason.STREAM_TIMEOUT", EndReasonStreamTimeout, int32(rpc.EndReason_END_REASON_STREAM_TIMEOUT)},
		{"EndReason.STREAM_REPLAY", EndReasonStreamReplay, int32(rpc.EndReason_END_REASON_STREAM_REPLAY)},

		{"ReplayState.NONE", ReplayStateNone, int32(rpc.ReplayState_REPLAY_STATE_NONE)},
		{"ReplayState.PROCESSING", ReplayStateProcessing, int32(rpc.ReplayState_REPLAY_STATE_PROCESSING)},
		{"ReplayState.AVAILABLE", ReplayStateAvailable, int32(rpc.ReplayState_REPLAY_STATE_AVAILABLE)},
		{"ReplayState.REMOVED", ReplayStateRemoved, int32(rpc.ReplayState_REPLAY_STATE_REMOVED)},

		{"AnchorRole.OWNER", AnchorRoleOwner, int32(rpc.AnchorRole_ANCHOR_ROLE_OWNER)},
		{"AnchorRole.COHOST", AnchorRoleCohost, int32(rpc.AnchorRole_ANCHOR_ROLE_COHOST)},
		{"AnchorRole.MANAGER", AnchorRoleManager, int32(rpc.AnchorRole_ANCHOR_ROLE_MANAGER)},

		{"BanType.TEMPORARY", BanTypeTemporary, int32(rpc.BanType_BAN_TYPE_TEMPORARY)},
		{"BanType.PERMANENT", BanTypePermanent, int32(rpc.BanType_BAN_TYPE_PERMANENT)},

		{"Platform.ANDROID", PlatformAndroid, int32(rpc.Platform_PLATFORM_ANDROID)},
		{"Platform.IOS", PlatformIOS, int32(rpc.Platform_PLATFORM_IOS)},
		{"Platform.HARMONY", PlatformHarmony, int32(rpc.Platform_PLATFORM_HARMONY)},
		{"Platform.DESKTOP", PlatformDesktop, int32(rpc.Platform_PLATFORM_DESKTOP)},

		{"RoomOrder.LIVING_FIRST", RoomOrderLivingFirst, int32(rpc.RoomOrder_ROOM_ORDER_LIVING_FIRST)},
		{"RoomOrder.CTIME_DESC", RoomOrderCtimeDesc, int32(rpc.RoomOrder_ROOM_ORDER_CTIME_DESC)},
	}
	for _, c := range cases {
		if c.model != c.proto {
			t.Errorf("%s: model=%d 但 proto=%d，两边编号必须逐值相等", c.name, c.model, c.proto)
		}
	}
}

// TestEveryProtoEnumValueHasModelConstant 反向兜底：proto 里新增枚举值而 model 没跟上时报警。
// 漏接的后果是 logic 把新状态原样写进列，而矩阵里没有该状态，房间会卡死在无人识别的状态上。
func TestEveryProtoEnumValueHasModelConstant(t *testing.T) {
	cases := []struct {
		name   string
		values map[int32]string
		valid  func(int32) bool
	}{
		{"RoomState", rpc.RoomState_name, ValidRoomState},
		{"VerifyState", rpc.VerifyState_name, ValidVerifyState},
		{"SessionState", rpc.SessionState_name, ValidSessionState},
		{"ReplayState", rpc.ReplayState_name, ValidReplayState},
		{"BanState", map[int32]string{1: "ACTIVE", 2: "LIFTED", 3: "EXPIRED"}, ValidBanState},
		{"AnchorRole", rpc.AnchorRole_name, ValidAnchorRole},
		{"Platform", rpc.Platform_name, ValidPlatform},
	}
	for _, c := range cases {
		for v, label := range c.values {
			if v == 0 || strings.Contains(label, "UNSPECIFIED") {
				continue // 0 一律是「未指定」，按设计不属于合法状态
			}
			if !c.valid(v) {
				t.Errorf("%s=%d(%s) 在 proto 里已定义，但 model 没有对应常量或校验函数不认它", c.name, v, label)
			}
		}
	}
}

// --- 2. SQL 片段构造器 ---

// TestRoomListWhereKeepsListAndCountAligned 校验 List 与 Count 共用同一个 WHERE 构造器，
// 并且「零值 = 不过滤」的约定成立：否则页码与总数会各说各话。
func TestRoomListWhereKeepsListAndCountAligned(t *testing.T) {
	cases := []struct {
		name      string
		query     RoomListQuery
		wantWhere string
		wantArgs  []interface{}
	}{
		{
			name:      "无条件也只按 state>0 收敛",
			query:     RoomListQuery{},
			wantWhere: "state > ?",
			wantArgs:  []interface{}{RoomStateUnspecified},
		},
		{
			name:      "按房主过滤",
			query:     RoomListQuery{OwnerMid: 42},
			wantWhere: "state > ? AND owner_mid = ?",
			wantArgs:  []interface{}{RoomStateUnspecified, int64(42)},
		},
		{
			name:      "按分区过滤",
			query:     RoomListQuery{AreaID: 7},
			wantWhere: "state > ? AND area_id = ?",
			wantArgs:  []interface{}{RoomStateUnspecified, int64(7)},
		},
		{
			name:      "按状态过滤",
			query:     RoomListQuery{State: RoomStateLiving},
			wantWhere: "state > ? AND state = ?",
			wantArgs:  []interface{}{RoomStateUnspecified, RoomStateLiving},
		},
		{
			name:      "三个维度叠加顺序稳定",
			query:     RoomListQuery{OwnerMid: 42, AreaID: 7, State: RoomStateReady},
			wantWhere: "state > ? AND owner_mid = ? AND area_id = ? AND state = ?",
			wantArgs:  []interface{}{RoomStateUnspecified, int64(42), int64(7), RoomStateReady},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			where, args := roomListWhere(c.query)
			if where != c.wantWhere {
				t.Errorf("WHERE 不符\n got=%s\nwant=%s", where, c.wantWhere)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("参数个数 %d，期望 %d（占位符与参数错位数会直接写错 SQL）", len(args), len(c.wantArgs))
			}
			for i := range args {
				if args[i] != c.wantArgs[i] {
					t.Errorf("args[%d]=%v，期望 %v", i, args[i], c.wantArgs[i])
				}
			}
			// 占位符数量必须与参数数量相等：否则 MySQL 报参数个数错误或静默错位。
			if got, want := strings.Count(where, "?"), len(args); got != want {
				t.Errorf("占位符 %d 个，参数 %d 个", got, want)
			}
		})
	}
}

// TestRoomListOrderIsClosedSet 校验排序片段取自常量表，未知取值退回默认排序。
// 这是「不把任何外部输入拼进 ORDER BY」的唯一防线。
func TestRoomListOrderIsClosedSet(t *testing.T) {
	if got := roomListOrder(RoomOrderLivingFirst); !strings.Contains(got, "(state = 3) DESC") || !strings.Contains(got, "room_id DESC") {
		t.Errorf("LIVING_FIRST 排序片段异常: %s", got)
	}
	if got := roomListOrder(RoomOrderCtimeDesc); got != "ctime DESC, room_id DESC" {
		t.Errorf("CTIME_DESC 排序片段异常: %s", got)
	}
	for _, bad := range []int32{RoomOrderIDDesc, 99, -1} {
		if got := roomListOrder(bad); got != "room_id DESC" {
			t.Errorf("roomListOrder(%d) 应退回默认排序，实际 %s", bad, got)
		}
	}
	// 任何取值都不得引入分号、注释或子查询。
	for _, order := range []int32{0, 1, 2, 3, 999, -999} {
		got := roomListOrder(order)
		for _, bad := range []string{";", "--", "/*", " UNION ", "DROP "} {
			if strings.Contains(strings.ToUpper(got), bad) {
				t.Errorf("排序片段含危险字符 %q: %s", bad, got)
			}
		}
	}
}

// TestAreaWhereUsesMinusOneAsNoFilter 校验分区过滤的哨兵取值。
// 0 对 parent_area_id（一级分区）和 state（停用）都是合法业务值，
// 所以「不过滤」只能用 -1；把哨兵写回 0 会让「只取一级分区」变成不可能表达。
func TestAreaWhereUsesMinusOneAsNoFilter(t *testing.T) {
	cases := []struct {
		name      string
		q         AreaListQuery
		wantWhere string
		wantArgs  int
	}{
		{"全不过滤", AreaListQuery{ParentAreaID: -1, State: -1}, "area_id > 0", 0},
		{"只取一级分区", AreaListQuery{ParentAreaID: 0, State: -1}, "area_id > 0 AND parent_area_id = ?", 1},
		{"只取停用分区", AreaListQuery{ParentAreaID: -1, State: AreaStateDisabled}, "area_id > 0 AND state = ?", 1},
		{"启用且指定父", AreaListQuery{ParentAreaID: 3, State: AreaStateEnabled}, "area_id > 0 AND parent_area_id = ? AND state = ?", 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			where, args := areaWhere(c.q)
			if where != c.wantWhere {
				t.Errorf("WHERE 不符\n got=%s\nwant=%s", where, c.wantWhere)
			}
			if len(args) != c.wantArgs || strings.Count(where, "?") != len(args) {
				t.Errorf("参数个数 %d，期望 %d", len(args), c.wantArgs)
			}
		})
	}
	// 默认零值（两个维度都是 0）会被误读成「只取一级且只取停用」——
	// 调用方必须显式传 -1，这里把这个陷阱固化成断言，防止有人「顺手」复用零值。
	where, args := areaWhere(AreaListQuery{})
	if where != "area_id > 0 AND parent_area_id = ? AND state = ?" || len(args) != 2 {
		t.Errorf("AreaListQuery 零值应展开为两个过滤条件（见注释里的陷阱），实际 %s / %d 个参数", where, len(args))
	}
}

// TestAnchorAndBanWhereFilters 校验绑定与禁播列表的过滤拼装。
func TestAnchorAndBanWhereFilters(t *testing.T) {
	where, args := anchorWhere(AnchorListQuery{RoomID: 5})
	if where != "room_id = ?" || len(args) != 1 || args[0] != int64(5) {
		t.Errorf("只给 RoomID 时不该多出条件: %s / %v", where, args)
	}
	where, args = anchorWhere(AnchorListQuery{RoomID: 5, Role: AnchorRoleManager, OnlyEnabled: true})
	if where != "room_id = ? AND role = ? AND state = ?" || len(args) != 3 {
		t.Errorf("角色 + 仅生效应展开三个条件: %s / %v", where, args)
	}

	where, args = banWhere(BanListQuery{})
	if where != "ban_id > 0" || len(args) != 0 {
		t.Errorf("空查询应只剩恒真占位: %s / %v", where, args)
	}
	where, args = banWhere(BanListQuery{RoomID: 1, Mid: 2, State: BanStateExpired})
	if where != "ban_id > 0 AND room_id = ? AND mid = ? AND state = ?" || len(args) != 3 {
		t.Errorf("三维度过滤拼装异常: %s / %v", where, args)
	}
}

// TestRoomPatchSetClauses 校验状态迁移附带列的 SET 片段与参数顺序严格对应。
// 顺序错一位就会把「禁播到期时间」写进「当前场次 ID」这类静默脏数据。
func TestRoomPatchSetClauses(t *testing.T) {
	var (
		verify = VerifyStateRejected
		reject = "标题含违规词"
		ban    = int64(1800000000)
		sess   = int64(777)
		stream = "stream-abc"
		task   = int64(9001)
	)
	args := []interface{}{}
	set := RoomPatch{
		VerifyState: &verify, RejectReason: &reject, BanUntil: &ban,
		ActiveSessionID: &sess, ActiveStreamID: &stream, ModerationTaskID: &task,
	}.setClauses(&args)

	wantSet := []string{
		"verify_state = ?", "reject_reason = ?", "ban_until = ?",
		"active_session_id = ?", "active_stream_id = ?", "moderation_task_id = ?",
	}
	if strings.Join(set, ",") != strings.Join(wantSet, ",") {
		t.Errorf("SET 片段不符\n got=%v\nwant=%v", set, wantSet)
	}
	wantArgs := []interface{}{verify, reject, ban, sess, stream, task}
	if len(args) != len(wantArgs) {
		t.Fatalf("参数个数 %d，期望 %d", len(args), len(wantArgs))
	}
	for i := range args {
		if args[i] != wantArgs[i] {
			t.Errorf("args[%d]=%v，期望 %v", i, args[i], wantArgs[i])
		}
	}
	// 全 nil 的 patch 不得产生任何 SET 片段（否则会拼出 "SET , mtime = ?" 这种非法 SQL）。
	args = nil
	if got := (RoomPatch{}).setClauses(&args); len(got) != 0 || len(args) != 0 {
		t.Errorf("空 patch 应不产生 SET 片段，实际 %v / %v", got, args)
	}
}

// TestPlaceholdersNeverProducesEmptyInList 校验 IN (...) 永不拼成空列表。
// 调用方传空切片时 placeholders 仍要给出一个占位符，否则 SQL 语法直接错。
func TestPlaceholdersNeverProducesEmptyInList(t *testing.T) {
	for _, n := range []int{-5, 0} {
		if got := placeholders(n); got != "?" {
			t.Errorf("placeholders(%d)=%q，非正数必须退化为单个占位符", n, got)
		}
	}
	if got := placeholders(3); got != "?, ?, ?" {
		t.Errorf("placeholders(3)=%q", got)
	}
	// 活跃/终态集合都靠它拼 IN，元素数与占位符数必须相等。
	for _, list := range [][]int32{ActiveSessionStates, terminalSessionStates} {
		if got, want := strings.Count(placeholders(len(list)), "?"), len(list); got != want {
			t.Errorf("占位符 %d 与参数 %d 不等", got, want)
		}
	}
}

// TestRuneLenCountsCharactersNotBytes 校验长度上限按字符计。
// 按字节算会让中文标题被 3 倍截断（AGENTS.md §6 多端一致）。
func TestRuneLenCountsCharactersNotBytes(t *testing.T) {
	const zh = "直播间标题上限校验" // 9 个汉字、27 个字节
	if got := runeLen(zh); got != 9 {
		t.Errorf("runeLen=%d，期望 9 个字符", got)
	}
	if len(zh) == runeLen(zh) {
		t.Error("该用例依赖中英文字节数不同，中文字符串已不满足前提")
	}
	// 32 个汉字是分区名上限的边界：按字节算会提前拒绝合法名称。
	if runeLen(strings.Repeat("游", 32)) != AreaNameMaxRunes {
		t.Errorf("32 个汉字应正好等于 AreaNameMaxRunes=%d", AreaNameMaxRunes)
	}
	if runeLen(strings.Repeat("游", 33)) <= AreaNameMaxRunes {
		t.Error("33 个汉字必须超限")
	}
}

// TestBoolToInt32RoundTrip 校验开关列的读写转换。
// 这里锁定的是「非 0 一律视为开启」，否则 TINYINT 列被人工写成 2 时会静默变关闭。
func TestBoolToInt32RoundTrip(t *testing.T) {
	if BoolToInt32(true) != 1 || BoolToInt32(false) != 0 {
		t.Error("bool 转列值必须严格 0/1")
	}
	for _, v := range []int32{0, 1, 2, -1} {
		if got, want := Int32ToBool(v), v != 0; got != want {
			t.Errorf("Int32ToBool(%d)=%v，期望 %v", v, got, want)
		}
	}
	if !Int32ToBool(BoolToInt32(true)) || Int32ToBool(BoolToInt32(false)) {
		t.Error("往返不一致")
	}
}

// --- 3. 入参校验先于 SQL ---

// TestValidationPrecedesSQL 用 nil 连接调用所有带前置校验的 model 方法。
//
// nil 连接意味着「一旦真的去执行 SQL 就会 panic」，所以本用例通过只可能是
// 「先校验参数、拿到哨兵错误就返回」。这拦住的是一类很具体的回归：
// 有人为了「让代码更短」把 mid<=0 / 状态非法 的判断挪到查询之后或干脆删掉，
// 编译与既有单测都不会报警，只有在这里会以 panic 形式暴露。
//
// 同时断言返回的不是 ErrNotImplemented —— 模型层本轮就要能真跑。
func TestValidationPrecedesSQL(t *testing.T) {
	var (
		ctx      = context.Background()
		room     = NewLiveRoomModel(nil)
		setting  = NewLiveRoomSettingModel(nil)
		anchor   = NewLiveRoomAnchorModel(nil)
		session  = NewLiveSessionModel(nil)
		ban      = NewLiveRoomBanModel(nil)
		area     = NewLiveAreaModel(nil)
		stateLog = NewLiveRoomStateLogModel(nil)
		idem     = NewLiveRoomIdempotencyModel(nil)
	)
	cases := []struct {
		name string
		want error
		call func() error
	}{
		// live_room
		{"Room/ListByOwner", ErrInvalidMid, func() error { _, err := room.ListByOwner(ctx, 0, 10); return err }},
		{"Room/CountByOwner", ErrInvalidMid, func() error { _, err := room.CountByOwner(ctx, 0, nil); return err }},
		{"Room/CountByArea", ErrInvalidAreaID, func() error { _, err := room.CountByArea(ctx, 0, nil); return err }},
		{"Room/Transition", ErrInvalidRoomTransition, func() error {
			_, err := room.Transition(ctx, 1, RoomStateFinished, RoomStateReady, 0, RoomPatch{})
			return err
		}},
		{"Room/TransitionTx", ErrInvalidRoomTransition, func() error {
			_, err := room.TransitionTx(ctx, nil, 1, RoomStatePending, RoomStateLiving, 0, RoomPatch{})
			return err
		}},
		{"Room/UpdateProfile", ErrRoomStateNotEditable, func() error {
			_, err := room.UpdateProfile(ctx, 1, "新标题", "", 0, VerifyStateUnspecified, 0, nil)
			return err
		}},
		// live_room_setting
		{"Setting/UpsertRoomID", ErrInvalidRoomID, func() error {
			return setting.Upsert(ctx, &LiveRoomSetting{LiveType: LiveTypeVideo})
		}},
		{"Setting/UpsertLiveType", ErrSettingInvalid, func() error {
			return setting.Upsert(ctx, &LiveRoomSetting{RoomID: 1, LiveType: 99})
		}},
		{"Setting/UpsertMinVersion", ErrSettingInvalid, func() error {
			return setting.Upsert(ctx, &LiveRoomSetting{RoomID: 1, LiveType: LiveTypeAudio, MinClientVersionCode: -1})
		}},
		{"Setting/FindOne", ErrInvalidRoomID, func() error { _, err := setting.FindOne(ctx, 0); return err }},
		// live_room_anchor
		{"Anchor/BindRoomID", ErrInvalidRoomID, func() error { _, err := anchor.Bind(ctx, 0, 5, AnchorRoleOwner); return err }},
		{"Anchor/BindMid", ErrInvalidMid, func() error { _, err := anchor.Bind(ctx, 1, 0, AnchorRoleOwner); return err }},
		{"Anchor/BindRole", ErrAnchorRoleInvalid, func() error { _, err := anchor.Bind(ctx, 1, 5, AnchorRoleUnspecified); return err }},
		{"Anchor/IsEnabled", ErrInvalidMid, func() error { _, _, err := anchor.IsEnabled(ctx, 0, 5); return err }},
		{"Anchor/List", ErrInvalidRoomID, func() error { _, err := anchor.List(ctx, AnchorListQuery{}); return err }},
		{"Anchor/Count", ErrInvalidRoomID, func() error { _, err := anchor.Count(ctx, AnchorListQuery{}); return err }},
		{"Anchor/CountActiveRoomsByMid", ErrInvalidMid, func() error {
			_, err := anchor.CountActiveRoomsByMid(ctx, 0, AnchorRoleOwner)
			return err
		}},
		{"Anchor/UnbindRoomID", ErrInvalidRoomID, func() error { _, err := anchor.Unbind(ctx, 0, 5, AnchorRoleManager); return err }},
		{"Anchor/UnbindMid", ErrInvalidMid, func() error { _, err := anchor.Unbind(ctx, 1, 0, AnchorRoleManager); return err }},
		{"Anchor/UnbindRole", ErrAnchorRoleInvalid, func() error { _, err := anchor.Unbind(ctx, 1, 5, 77); return err }},
		{"Anchor/TransferOwnerSameMid", ErrInvalidMid, func() error { _, err := anchor.TransferOwner(ctx, 1, 5, 5); return err }},
		{"Anchor/TransferOwnerZero", ErrInvalidMid, func() error { _, err := anchor.TransferOwner(ctx, 1, 0, 6); return err }},
		{"Anchor/ListRoomsByMid", ErrInvalidMid, func() error { _, err := anchor.ListRoomsByMid(ctx, 0, AnchorRoleOwner, 10); return err }},
		// live_session
		{"Session/InsertRoomID", ErrInvalidRoomID, func() error { _, err := session.Insert(ctx, &LiveSession{Mid: 5}); return err }},
		{"Session/InsertMid", ErrInvalidMid, func() error { _, err := session.Insert(ctx, &LiveSession{RoomID: 1}); return err }},
		{"Session/ListActiveByRoom", ErrInvalidRoomID, func() error { _, err := session.ListActiveByRoom(ctx, 0, 2); return err }},
		{"Session/FindLatest", ErrInvalidRoomID, func() error { _, err := session.FindLatest(ctx, 0, 0); return err }},
		{"Session/List", ErrInvalidRoomID, func() error { _, err := session.List(ctx, SessionListModel{}); return err }},
		{"Session/Transition", ErrInvalidSessionTransition, func() error {
			_, err := session.Transition(ctx, 1, SessionStateEnded, SessionStateLiving, EndReasonAnchorStop, 0)
			return err
		}},
		{"Session/TransitionNoEndReason", ErrEndReasonInvalid, func() error {
			_, err := session.TransitionTx(ctx, nil, 1, SessionStatePending, SessionStateTerminated, EndReasonUnspecified, 0)
			return err
		}},
		{"Session/SetStreamIDEmpty", ErrStreamRefMismatch, func() error { _, err := session.SetStreamID(ctx, 1, ""); return err }},
		{"Session/AdvanceStreamSeqTransition", ErrInvalidSessionTransition, func() error {
			_, err := session.AdvanceStreamSeq(ctx, 1, 5, SessionStateEnded, SessionStatePending, EndReasonAnchorStop, 0)
			return err
		}},
		{"Session/AdvanceStreamSeqStale", ErrStreamSeqStale, func() error {
			_, err := session.AdvanceStreamSeq(ctx, 1, 0, SessionStatePending, SessionStateLiving, EndReasonUnspecified, 0)
			return err
		}},
		{"Session/AdvanceStreamSeqNoEndReason", ErrEndReasonInvalid, func() error {
			_, err := session.AdvanceStreamSeq(ctx, 1, 5, SessionStateLiving, SessionStateEnded, EndReasonUnspecified, 0)
			return err
		}},
		{"Session/AttachReplaySessionID", ErrInvalidSessionID, func() error {
			_, err := session.AttachReplay(ctx, 0, 1, ReplayStateNone, ReplayStateProcessing, 1, 0, 0)
			return err
		}},
		{"Session/AttachReplayRoomID", ErrInvalidRoomID, func() error {
			_, err := session.AttachReplay(ctx, 1, 0, ReplayStateNone, ReplayStateProcessing, 1, 0, 0)
			return err
		}},
		{"Session/AttachReplayTransition", ErrInvalidReplayTransition, func() error {
			_, err := session.AttachReplay(ctx, 1, 2, ReplayStateNone, ReplayStateRemoved, 1, 0, 0)
			return err
		}},
		{"Session/SetModerationTaskID", ErrTaskMismatch, func() error { return session.SetModerationTaskID(ctx, 1, 0) }},
		// live_room_ban
		{"Ban/InsertRoomID", ErrInvalidRoomID, func() error { _, err := ban.Insert(ctx, &LiveRoomBan{OperatorMid: 2}); return err }},
		{"Ban/InsertOperator", ErrOperatorRequired, func() error {
			_, err := ban.Insert(ctx, &LiveRoomBan{RoomID: 1, BanType: BanTypeTemporary})
			return err
		}},
		{"Ban/InsertType", ErrBanTypeInvalid, func() error {
			_, err := ban.Insert(ctx, &LiveRoomBan{RoomID: 1, OperatorMid: 2, BanType: BanTypeUnspecified})
			return err
		}},
		{"Ban/PermanentWithEndAt", ErrBanTypeInvalid, func() error {
			_, err := ban.Insert(ctx, &LiveRoomBan{RoomID: 1, OperatorMid: 2, BanType: BanTypePermanent, EndAt: 123})
			return err
		}},
		{"Ban/TemporaryNoDuration", ErrBanDurationRequired, func() error {
			_, err := ban.Insert(ctx, &LiveRoomBan{RoomID: 1, OperatorMid: 2, BanType: BanTypeTemporary, StartAt: 100, EndAt: 100})
			return err
		}},
		{"Ban/HasActiveByMid", ErrInvalidMid, func() error { _, err := ban.HasActiveByMid(ctx, 0, 100); return err }},
		{"Ban/LiftNotFound", ErrBanNotFound, func() error { _, err := ban.Lift(ctx, 0, 2, "理由", 0); return err }},
		{"Ban/LiftOperator", ErrOperatorRequired, func() error { _, err := ban.Lift(ctx, 1, 0, "理由", 0); return err }},
		// live_area
		{"Area/InsertEmptyName", ErrAreaNameInvalid, func() error { _, err := area.Insert(ctx, &LiveArea{OperatorMid: 1}); return err }},
		{"Area/InsertLongName", ErrAreaNameInvalid, func() error {
			_, err := area.Insert(ctx, &LiveArea{AreaName: strings.Repeat("游", AreaNameMaxRunes+1), OperatorMid: 1})
			return err
		}},
		{"Area/InsertOperator", ErrOperatorRequired, func() error {
			_, err := area.Insert(ctx, &LiveArea{AreaName: "游戏", State: AreaStateEnabled})
			return err
		}},
		{"Area/InsertState", ErrAreaStateInvalid, func() error {
			_, err := area.Insert(ctx, &LiveArea{AreaName: "游戏", OperatorMid: 1, State: 7})
			return err
		}},
		{"Area/UpdateAreaID", ErrInvalidAreaID, func() error {
			_, err := area.Update(ctx, &LiveArea{OperatorMid: 1, State: AreaStateEnabled})
			return err
		}},
		{"Area/UpdateOperator", ErrOperatorRequired, func() error {
			_, err := area.Update(ctx, &LiveArea{AreaID: 1, State: AreaStateEnabled})
			return err
		}},
		{"Area/UpdateState", ErrAreaStateInvalid, func() error {
			_, err := area.Update(ctx, &LiveArea{AreaID: 1, OperatorMid: 2, State: 3})
			return err
		}},
		{"Area/FindByName", ErrAreaNameInvalid, func() error { _, err := area.FindByName(ctx, ""); return err }},
		{"Area/IsUsable", ErrInvalidAreaID, func() error { _, err := area.IsUsable(ctx, 0); return err }},
		{"Area/CountChildren", ErrInvalidAreaID, func() error { _, err := area.CountChildren(ctx, 0); return err }},
		// live_room_state_log
		{"StateLog/InsertRoomID", ErrInvalidRoomID, func() error {
			_, err := stateLog.Insert(ctx, &LiveRoomStateLog{StateType: LogTypeRoomState})
			return err
		}},
		{"StateLog/InsertStateType", ErrStateTypeInvalid, func() error {
			_, err := stateLog.Insert(ctx, &LiveRoomStateLog{RoomID: 1, StateType: 0})
			return err
		}},
		{"StateLog/InsertTxStateType", ErrStateTypeInvalid, func() error {
			_, err := stateLog.InsertTx(ctx, nil, &LiveRoomStateLog{RoomID: 1, StateType: 9})
			return err
		}},
		{"StateLog/ListByRoom", ErrInvalidRoomID, func() error { _, err := stateLog.ListByRoom(ctx, 0, LogTypeRoomState, 10); return err }},
		{"StateLog/RangeMissing", ErrQueryRangeRequired, func() error {
			_, err := stateLog.ListByTimeRange(ctx, 0, 0, "", 10)
			return err
		}},
		{"StateLog/RangeInverted", ErrQueryRangeRequired, func() error {
			_, err := stateLog.ListByTimeRange(ctx, 200, 100, "", 10)
			return err
		}},
		// live_room_idempotency
		{"Idem/ClaimEmptyKey", ErrRequestIDRequired, func() error {
			_, err := idem.Claim(ctx, &LiveRoomIdempotency{Kind: IdempotencyKindRequest})
			return err
		}},
		{"Idem/ClaimKind", ErrIdempotencyKindInvalid, func() error {
			_, err := idem.Claim(ctx, &LiveRoomIdempotency{DedupKey: "req-1", Kind: 0})
			return err
		}},
		{"Idem/FindEmptyKey", ErrRequestIDRequired, func() error { _, err := idem.Find(ctx, ""); return err }},
		{"Idem/SaveResultEmptyKey", ErrRequestIDRequired, func() error { _, err := idem.SaveResult(ctx, "", "{}"); return err }},
		{"Idem/DeleteExpired", ErrQueryRangeRequired, func() error { _, err := idem.DeleteExpired(ctx, 0, 100); return err }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatalf("非法入参竟返回了 nil（校验被跳过或 SQL 被执行了）")
			}
			if errors.Is(err, ErrNotImplemented) {
				t.Fatalf("model 层不该返回 ErrNotImplemented，本轮就要能真跑: %v", err)
			}
			if !errors.Is(err, c.want) {
				t.Errorf("错误类型不符\n got=%v\nwant=%v", err, c.want)
			}
		})
	}
}

// TestSentinelErrorsAreDistinct 校验哨兵错误没有互相覆盖。
// 两个语义不同的失败共用一个 error，logic 就无法映射成不同的 gRPC code。
func TestSentinelErrorsAreDistinct(t *testing.T) {
	all := []error{
		ErrInvalidMid, ErrInvalidRoomID, ErrInvalidSessionID, ErrInvalidAreaID,
		ErrRequestIDRequired, ErrEventIDRequired, ErrTitleInvalid, ErrCoverTooLong,
		ErrRoomNotFound, ErrSessionNotFound, ErrNoActiveSession, ErrRoomFinished,
		ErrRoomStateNotEditable, ErrRoomBanned, ErrRoomNotBanned, ErrRoomLimitExceeded,
		ErrConcurrentUpdate, ErrInvalidRoomTransition, ErrInvalidVerifyTransition,
		ErrInvalidSessionTransition, ErrInvalidReplayTransition, ErrInvalidBanTransition,
		ErrNotVerified, ErrAnchorNotFound, ErrAnchorForbidden, ErrCannotUnbindOwner,
		ErrDuplicateOwner, ErrAnchorRoleInvalid, ErrAnchorActionInvalid, ErrAnchorLimitExceeded,
		ErrAreaNotFound, ErrAreaDisabled, ErrAreaNameConflict, ErrAreaNameInvalid,
		ErrAreaParentInvalid, ErrAreaInUse, ErrAreaStateInvalid, ErrStateTypeInvalid,
		ErrQueryRangeRequired, ErrIdempotencyKindInvalid, ErrIdempotencyResultMissing,
		ErrOperatorRequired, ErrBanTypeInvalid, ErrBanDurationRequired, ErrBanNotFound,
		ErrBanNotActive, ErrEndReasonInvalid, ErrSessionNotTerminal, ErrSessionRoomMismatch,
		ErrReplayStateInvalid, ErrStreamStateInvalid, ErrStreamSeqStale, ErrStreamRefMismatch,
		ErrSettingInvalid, ErrNoSettingRow, ErrVerdictInvalid, ErrTaskMismatch,
		ErrInvalidPage, ErrPageSizeTooLarge, ErrCursorInvalid,
		ErrCreatorNotConfigured, ErrRiskControlNotConfigured, ErrModerationNotConfigured,
		ErrDownstreamUnavailable,
	}
	seen := map[string]error{}
	for _, a := range all {
		if prev, dup := seen[a.Error()]; dup && prev != a {
			t.Errorf("两个不同哨兵共用了消息 %q，logic 无法区分", a.Error())
		}
		seen[a.Error()] = a
	}
	// 错误报文里不得出现 SQL 片段、密钥或明文地址（AGENTS.md §7/§9）。
	for _, a := range all {
		low := strings.ToLower(a.Error())
		for _, bad := range []string{"select ", "insert ", "update ", "delete ", "password", "token", "secret", "127.0.0.1", "key"} {
			if strings.Contains(low, bad) {
				t.Errorf("哨兵错误 %q 含不该外泄的内容片段 %q", a.Error(), bad)
			}
		}
	}
}

func sortedAsc(s []int32) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] > s[i] {
			return false
		}
	}
	return true
}

// containsInt32 用于断言状态机出边集合，避免为了一个判断引入 slices 依赖。
func containsInt32(list []int32, v int32) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
