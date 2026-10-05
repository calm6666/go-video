package logic

// roominfo_logic_test.go 覆盖写侧方法 UpdateRoomInfo（updateroominfologic.go）。
//
// 锁的结论：
//  1. **「未传即不改」的部分更新**——本方法与 UpdateRoomSetting 的整段覆盖口径相反，
//     这条对比是两份文件各钉一半：这里是「空串/0 的列保持库里原值」，
//     roomsetting_logic_test.go 里是「false 是显式关闭而不是未传」；
//  2. 差量为空时**一行都不写**：不送审、不抢键、不回读，只把当前投影原样回出去
//     （空改动去建审核任务会污染队列，这是 updateroominfologic.go:32 写明的口径）；
//  3. live_room.UpdateProfile 这条 UPDATE **不推进 state_version**：
//     改资料不是状态迁移，所以 CAS 版本必须原地不动（model/live_room.go:320-367）；
//  4. 守卫顺序：入参守卫全在触库前；字段级守卫（标题/封面长度、分区可用性）与
//     可编辑状态判定都在 Anchors.IsEnabled **之后**、claimDedup **之前**——
//     也就是「权限判定先于任何写，且被拒绝的请求不烧幂等键」；
//  5. 本方法全程**不开事务**（wantTxCount 0）：送审是改行之前的外部往返，
//     审计日志是改行之后的 best-effort（写失败只记错误日志），
//     所以「资料已改但审计缺行」是可观察形态而不是假设；
//  6. request_id 三态：首次 / 命中重放（回库里存的旧投影，不是当前值）/ 键被别人用过。
//
// 本文件顺带钉住的两条当前行为（已登记为 README 已知缺口，编号见 README）：
//     a) 房间已在 REVIEWING 时再改资料，审计行记的是 t2:2->2 这条**原地迁移**，
//        同时 reject_reason 被清——见 TestUpdateRoomInfoResubmitOnReviewingRoomLogsNoOpMigration；
//     b) UpdateProfile 的 RowsAffected==0 只报 ErrConcurrentUpdate，
//        但审核任务已经提交出去了（下游多一条任务），残留形态见
//        TestUpdateRoomInfoStateChangedUnderneathReportsConcurrentUpdate。

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
	roomInfoNow      int64 = 1_700_000_000
	roomInfoRoom     int64 = 4001
	roomInfoMid      int64 = 9001 // 生效房主
	roomInfoCohost   int64 = 9003 // 生效联合主播（可改资料）
	roomInfoManager  int64 = 9002 // 房管（不可改资料）
	roomInfoStranger int64 = 9004 // 没有绑定
	roomInfoArea     int64 = 7002 // 新建用例要切去的启用分区
	roomInfoAreaCur  int64 = 7001 // baseRoom 自带的分区
)

// roomInfoReq 是一条「除 request_id 外都合法」的改资料请求：只改标题。
func roomInfoReq(reqID string) *rpc.UpdateRoomInfoReq {
	return &rpc.UpdateRoomInfoReq{
		RoomId:      roomInfoRoom,
		OperatorMid: roomInfoMid,
		Title:       "深夜电台 第二季",
		RequestId:   reqID,
		TraceId:     "trace-info",
	}
}

// newRoomInfoLogic 装配一个接了 moderation 的上下文。
// 需要「未接线」形态的用例请直接用 NewUpdateRoomInfoLogic(ctx, st.svcCtx())。
func newRoomInfoLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *UpdateRoomInfoLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewUpdateRoomInfoLogic(context.Background(), st.wireModeration().svcCtxWith(c))
}

// seedRoomInfoScene 布一个「PENDING + 已过审 + 有封面 + 房主绑定」的房间。
// cover/reject_reason 都要给非空值，否则「未传即不改」与「空串清空」两种形态分不开。
func seedRoomInfoScene(t *testing.T, st *store, state, verifyState int32) *model.LiveRoom {
	t.Helper()
	r := baseRoom(roomInfoRoom, roomInfoMid, state)
	r.Title = "深夜电台"
	r.Cover = "cover/old.jpg"
	r.AreaID = roomInfoAreaCur
	r.VerifyState = verifyState
	r.BanUntil = 0
	if verifyState == model.VerifyStateRejected {
		r.RejectReason = "封面不合规"
	}
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(601, roomInfoRoom, roomInfoMid, model.AnchorRoleOwner))
	return r
}

func TestUpdateRoomInfoHappyPathChangesOnlyTheSentColumn(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	lg := newRoomInfoLogic(t, st)

	reply, err := lg.UpdateRoomInfo(roomInfoReq("req-info-1"))
	wantNoErr(t, "UpdateRoomInfo", err)
	defer st.checkRaces(t)

	// 完整轨迹：读房间 → 判权限 → 抢键 → 送审 → 改资料 → 审计 → 回读 → 回填结果。
	wantSeq(t, "UpdateRoomInfo 首次执行", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:req-info-1",
		"moderation.SubmitForReview:4001",
		"live_room.UpdateProfile:4001:v2/t9001",
		"live_room_state_log.Insert:t2:3->2:profile_resubmitted",
		"live_room.FindOne:4001",
		"live_room_idempotency.SaveResult:req-info-1",
	)
	// 本方法一句事务都不开：改资料是一条 UPDATE，审计是它之后的独立写。
	wantTxCount(t, "UpdateRoomInfo 全程", st.conn, 0)

	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "只改标题", "title", row.Title, "深夜电台 第二季")
	// 未传的列保持库里原值——这就是「未传即不改」，与 UpdateRoomSetting 的整段覆盖相反。
	wantEQ(t, "未传即不改", "cover", row.Cover, "cover/old.jpg")
	wantEQ(t, "未传即不改", "area_id", row.AreaID, int64(roomInfoAreaCur))
	wantEQ(t, "未传即不改", "state", row.State, model.RoomStatePending)
	// 改资料必须重新过审：verify_state 走 PASSED→REVIEWING。
	wantEQ(t, "重新送审", "verify_state", row.VerifyState, model.VerifyStateReviewing)
	wantEQ(t, "重新送审", "moderation_task_id", row.ModerationTaskID, int64(9001))
	// 本方法的核心口径：这条 UPDATE 里没有 state_version，改资料不推进状态版本。
	wantEQ(t, "改资料不动版本", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "改资料不动创建时间", "ctime", row.Ctime, int64(1000+roomInfoRoom))
	wantEQ(t, "改资料刷修改时间", "mtime", row.Mtime, roomInfoNow)

	wantEQ(t, "应答", "replayed", reply.GetReplayed(), false)
	wantEQ(t, "应答", "moderation_task_id", reply.GetModerationTaskId(), int64(9001))
	wantEQ(t, "应答", "回读后的 title", reply.GetRoom().GetTitle(), "深夜电台 第二季")
	wantEQ(t, "应答", "回读后的 state_version", reply.GetRoom().GetStateVersion(), int32(1))

	// 送审入参：reason 是「资料变更」专用串，不能复用建房那条。
	sub := st.mod.submits[0]
	wantEQ(t, "送审入参", "submission_id", sub.GetSubmissionId(), roomInfoRoom)
	wantEQ(t, "送审入参", "business", sub.GetBusiness(), "live")
	wantEQ(t, "送审入参", "reason", sub.GetReason(), "live_room_profile_updated")
	wantEQ(t, "送审入参", "mid", sub.GetMid(), roomInfoMid)

	logs := st.logsOf(roomInfoRoom)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1（资料态迁移）", len(logs))
	}
	wantEQ(t, "资料审计", "state_type", logs[0].StateType, model.LogTypeVerifyState)
	wantEQ(t, "资料审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "3->2")
	wantEQ(t, "资料审计", "source", logs[0].Source, model.SourceRPCClient)
	wantEQ(t, "资料审计", "operator_mid", logs[0].OperatorMid, roomInfoMid)
	wantEQ(t, "资料审计", "request_id", logs[0].RequestID, "req-info-1")
	wantEQ(t, "资料审计", "trace_id", logs[0].TraceID, "trace-info")
	wantEQ(t, "资料审计", "reason", logs[0].Reason, "profile_resubmitted")

	// 没有新建/删除任何行：只有那一条房间与绑定，加一条审计和一个键。
	wantDeepEQ(t, "落库面", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, logs: 1, idem: 1})
}

func TestUpdateRoomInfoAllSentColumnsAndTrimmingReachTheRow(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	st.seedArea(baseArea(roomInfoArea, 0, model.AreaStateEnabled))
	lg := newRoomInfoLogic(t, st)

	reply, err := lg.UpdateRoomInfo(&rpc.UpdateRoomInfoReq{
		RoomId: roomInfoRoom, OperatorMid: roomInfoMid,
		Title: "  三列全改  ", Cover: "cover/new.jpg", AreaId: roomInfoArea,
		RequestId: "req-info-all",
	})
	wantNoErr(t, "三列全改", err)
	wantEQ(t, "三列全改", "应答 task_id", reply.GetModerationTaskId(), int64(9001))

	// 分区可用性是差量成立才查的：这一条 IsUsable 出现在抢键之前。
	wantSeq(t, "三列全改", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		fmt.Sprintf("live_area.IsUsable:%d", roomInfoArea),
		"live_room_idempotency.Claim:req-info-all",
		"moderation.SubmitForReview:4001",
		"live_room.UpdateProfile:4001:v2/t9001",
		"live_room_state_log.Insert:t2:3->2:profile_resubmitted",
		"live_room.FindOne:4001",
		"live_room_idempotency.SaveResult:req-info-all",
	)
	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "三列全改", "title 去首尾空白", row.Title, "三列全改")
	wantEQ(t, "三列全改", "cover", row.Cover, "cover/new.jpg")
	wantEQ(t, "三列全改", "area_id", row.AreaID, int64(roomInfoArea))
	wantEQ(t, "三列全改", "state_version 仍是 1", row.StateVersion, int32(1))
}

// TestUpdateRoomInfoRejectedProfileClearsRejectReason 锁「重新送审顺带清 reject_reason」：
// model 的 UpdateProfile 在 verify_state 非 UNSPECIFIED 时恒写 reject_reason=”，
// 而 model 侧的 CanVerifyTransition 允许 REJECTED→REVIEWING（禁止的是直接写 PASSED）。
func TestUpdateRoomInfoRejectedProfileClearsRejectReason(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	r := seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStateRejected)
	wantEQ(t, "布数据", "reject_reason 初值", r.RejectReason, "封面不合规")

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-rej"))
	wantNoErr(t, "驳回后改资料", err)

	wantSeq(t, "驳回后改资料", st.log, 2,
		"live_room_idempotency.Claim:req-info-rej",
		"moderation.SubmitForReview:4001",
		"live_room.UpdateProfile:4001:v2/t9001",
		"live_room_state_log.Insert:t2:4->2:profile_resubmitted",
		"live_room.FindOne:4001",
		"live_room_idempotency.SaveResult:req-info-rej",
	)
	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "驳回后改资料", "verify_state", row.VerifyState, model.VerifyStateReviewing)
	wantEQ(t, "驳回后改资料", "reject_reason 已清", row.RejectReason, "")
}

// TestUpdateRoomInfoResubmitOnReviewingRoomLogsNoOpMigration 钉住 updateroominfologic.go:125
// 的当前行为：房间已在 REVIEWING 时再改资料，verifyTargetAfterResubmit 判定「不能再迁」而回原值，
// 于是审计行记的是一条 2->2 的原地迁移，同时 reject_reason 被清掉。
// 状态没变却写了一条迁移日志——这与 AGENTS.md §8「审计与状态变更同事务」的精神冲突，
// 且本方法根本不在事务里，已登记为 README 已知缺口（本用例是把当前行为钉住的哨兵）。
func TestUpdateRoomInfoResubmitOnReviewingRoomLogsNoOpMigration(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	r := seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStateReviewing)
	r.RejectReason = "上一轮驳回意见"
	// seedRoom 会拒绝生产写不出来的行，但不改 verify 与 reason 的组合，
	// 这里直接改内存里的行：REVIEWING + reject_reason 是 ApplyRoomModerationResult 之前的合法形态。

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-twice"))
	wantNoErr(t, "审核中再改资料", err)

	wantSeq(t, "审核中再改资料", st.log, 2,
		"live_room_idempotency.Claim:req-info-twice",
		"moderation.SubmitForReview:4001",
		// key 里 v2 就是「目标态还是 REVIEWING」，即一次空迁移。
		"live_room.UpdateProfile:4001:v2/t9001",
		"live_room_state_log.Insert:t2:2->2:profile_resubmitted",
		"live_room.FindOne:4001",
		"live_room_idempotency.SaveResult:req-info-twice",
	)
	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "审核中再改资料", "verify_state 不变", row.VerifyState, model.VerifyStateReviewing)
	wantEQ(t, "审核中再改资料", "reject_reason 仍被清", row.RejectReason, "")

	logs := st.logsOf(roomInfoRoom)
	wantEQ(t, "空迁移也留了审计行", "行数", len(logs), 1)
	wantEQ(t, "空迁移也留了审计行", "from->to",
		fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "2->2")
}

func TestUpdateRoomInfoLivingRoomAllowsTitleChangeButKeepsState(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStateLiving, model.VerifyStatePassed)

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-live"))
	wantNoErr(t, "在播房间改标题", err)

	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "在播改资料", "state 仍是 LIVING", row.State, model.RoomStateLiving)
	wantEQ(t, "在播改资料", "title", row.Title, "深夜电台 第二季")
	wantEQ(t, "在播改资料", "state_version", row.StateVersion, int32(1))
	// 允许集合是 {PENDING, READY, LIVING}，logic 侧判定通过后交给 SQL 的 IN 条件。
	wantMethodCount(t, "在播改资料", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "在播改资料", st.log, "live_room.Transition", 0)
}

// TestUpdateRoomInfoAreaChangeIsStricterThanTitleChange 锁 changing_area 对允许状态集合的影响：
// 切分区只允许 PENDING/READY（roomInfoEditableStates），在播房间切分区必须在抢键之前被拒。
func TestUpdateRoomInfoAreaChangeIsStricterThanTitleChange(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStateLiving, model.VerifyStatePassed)
	st.seedArea(baseArea(roomInfoArea, 0, model.AreaStateEnabled))

	reply, err := newRoomInfoLogic(t, st).UpdateRoomInfo(&rpc.UpdateRoomInfoReq{
		RoomId: roomInfoRoom, OperatorMid: roomInfoMid, AreaId: roomInfoArea,
		RequestId: "req-info-area-living",
	})
	wantErrIs(t, "在播切分区", err, model.ErrRoomStateNotEditable)
	wantErrContains(t, "在播切分区", err, "state=3, changing_area=true")
	if reply != nil {
		t.Fatalf("在播切分区应答 = %+v, want nil", reply)
	}

	// 分区可用性已经查了，但可编辑判定在抢键之前：键不能烧。
	wantSeq(t, "在播切分区", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		fmt.Sprintf("live_area.IsUsable:%d", roomInfoArea),
	)
	wantKeyUnburned(t, "在播切分区", "req-info-area-living", st)
	wantMethodCount(t, "在播切分区", st.log, "live_room.UpdateProfile", 0)
	wantMethodCount(t, "在播切分区", st.log, "moderation.SubmitForReview", 0)
}

// TestUpdateRoomInfoNonEditableStates table 锁「哪些状态不能改资料」以及错误消息里的两个参数。
func TestUpdateRoomInfoNonEditableStates(t *testing.T) {
	cases := []struct {
		name  string
		state int32
		want  string
	}{
		{"banned", model.RoomStateBanned, "state=5, changing_area=false"},
		{"disabled", model.RoomStateDisabled, "state=6, changing_area=false"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, tc.state, model.VerifyStatePassed)

			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-state"))
			wantErrIs(t, tc.name, err, model.ErrRoomStateNotEditable)
			wantErrContains(t, tc.name, err, tc.want)
			// 判定发生在 claimDedup 之前：一次写都没有，键也没烧。
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
			)
			wantKeyUnburned(t, tc.name, "req-info-state", st)
		})
	}
}

// TestUpdateRoomInfoEmptyDeltaWritesNothing 锁 updateroominfologic.go:85 的早退分支：
// 未传、传空串、传纯空白、传与现值相同的值，四种都算「无差量」——
// 不抢键、不送审、不改行、不回读，只把当前投影原样回出去。
func TestUpdateRoomInfoEmptyDeltaWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*rpc.UpdateRoomInfoReq)
	}{
		{"全零值", func(*rpc.UpdateRoomInfoReq) {}},
		{"纯空白标题", func(in *rpc.UpdateRoomInfoReq) { in.Title = "   \t " }},
		{"与现值同的标题", func(in *rpc.UpdateRoomInfoReq) { in.Title = "深夜电台" }},
		{"与现值同的封面", func(in *rpc.UpdateRoomInfoReq) { in.Cover = "cover/old.jpg" }},
		{"与现值同的分区", func(in *rpc.UpdateRoomInfoReq) { in.AreaId = roomInfoAreaCur }},
		{"封面留空串", func(in *rpc.UpdateRoomInfoReq) { in.Cover = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			// 同值分区那一条要求 area 行存在吗？logic 只在「>0 且 != 现值」时才查库，
			// 所以这里连分区表都不该被碰——不 seed 任何 area，让越界写自己暴露。
			lg := newRoomInfoLogic(t, st)

			in := &rpc.UpdateRoomInfoReq{
				RoomId: roomInfoRoom, OperatorMid: roomInfoMid, RequestId: "req-info-nodiff",
			}
			tc.mut(in)

			reply, err := lg.UpdateRoomInfo(in)
			wantNoErr(t, tc.name, err)
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
			)
			wantEQ(t, tc.name, "moderation_task_id=0 表示没送审", reply.GetModerationTaskId(), int64(0))
			wantEQ(t, tc.name, "replayed", reply.GetReplayed(), false)
			wantEQ(t, tc.name, "回的是当前投影", reply.GetRoom().GetTitle(), "深夜电台")
			wantEQ(t, tc.name, "回的是当前投影", reply.GetRoom().GetCover(), "cover/old.jpg")
			wantKeyUnburned(t, tc.name, "req-info-nodiff", st)
			wantTxCount(t, tc.name, st.conn, 0)
			// 精确到方法名：空差量不得产生任何一条写 SQL（前缀匹配会把别的表算成命中）。
			for _, m := range []string{
				"live_room.UpdateProfile", "live_room.Transition", "live_room.TransitionTx",
				"live_room_state_log.Insert", "live_room_state_log.InsertTx",
				"live_room_idempotency.Claim", "live_area.IsUsable", "moderation.SubmitForReview",
			} {
				wantMethodCount(t, tc.name, st.log, m, 0)
			}
		})
	}
}

func TestUpdateRoomInfoNilRequestIsRejected(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	lg := newRoomInfoLogic(t, st)
	before := st.log.snapshot()

	reply, err := lg.UpdateRoomInfo(nil)
	wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
	if reply != nil {
		t.Fatalf("nil 请求应答 = %+v, want nil", reply)
	}
	wantNoCallAfter(t, "nil 请求", st.log, before)
}

// TestUpdateRoomInfoGuardsRejectBeforeAnyDependencyCall 锁入参守卫的**顺序**与**哨兵**：
// room_id → operator_mid → request_id，三条都在第一次触库之前。
// 注意 operator 用的是 checkMid（ErrInvalidMid）而不是 checkOperator（ErrOperatorRequired），
// 这条不对称是契约里「操作者是主播」的语义，必须钉住而不是靠读代码确认。
func TestUpdateRoomInfoGuardsRejectBeforeAnyDependencyCall(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*rpc.UpdateRoomInfoReq)
		want  error
		frag  string
		guard bool // 只查哨兵，不查消息片段
	}{
		{"房间号 0", func(in *rpc.UpdateRoomInfoReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, "", true},
		{"房间号负数", func(in *rpc.UpdateRoomInfoReq) { in.RoomId = -1 }, model.ErrInvalidRoomID, "", true},
		{"操作者 0", func(in *rpc.UpdateRoomInfoReq) { in.OperatorMid = 0 }, model.ErrInvalidMid, "", true},
		{"操作者负数", func(in *rpc.UpdateRoomInfoReq) { in.OperatorMid = -3 }, model.ErrInvalidMid, "", true},
		{"幂等键空", func(in *rpc.UpdateRoomInfoReq) { in.RequestId = "" }, model.ErrRequestIDRequired, "", true},
		{"幂等键纯空白", func(in *rpc.UpdateRoomInfoReq) { in.RequestId = "  " }, model.ErrRequestIDRequired, "", true},
		{"幂等键超长", func(in *rpc.UpdateRoomInfoReq) { in.RequestId = strings.Repeat("r", 65) },
			model.ErrDedupIDTooLong, "65 bytes, max 64", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			lg := newRoomInfoLogic(t, st)
			before := st.log.snapshot()

			in := roomInfoReq("req-info-guard")
			tc.mut(in)
			reply, err := lg.UpdateRoomInfo(in)
			wantErrIs(t, tc.name, err, tc.want)
			if !tc.guard {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			if reply != nil {
				t.Fatalf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantNoCallAfter(t, tc.name, st.log, before)
		})
	}
}

// TestUpdateRoomInfoGuardOrder 锁「同时给两个坏入参时先报哪个」：
// 顺序错一位，客户端拿到的哨兵就换了一个，重试策略跟着错。
func TestUpdateRoomInfoGuardOrder(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	lg := newRoomInfoLogic(t, st)
	before := st.log.snapshot()

	// 房间号与操作者都非法：先报 room_id。
	_, err := lg.UpdateRoomInfo(&rpc.UpdateRoomInfoReq{RequestId: "req-info-order"})
	wantErrIs(t, "room_id 先于 operator_mid", err, model.ErrInvalidRoomID)
	// 操作者非法且幂等键也非法：先报 operator_mid。
	_, err = lg.UpdateRoomInfo(&rpc.UpdateRoomInfoReq{RoomId: roomInfoRoom, Title: "x"})
	wantErrIs(t, "operator_mid 先于 request_id", err, model.ErrInvalidMid)
	wantNoCallAfter(t, "守卫顺序", st.log, before)
}

// TestUpdateRoomInfoFieldGuardsRunAfterThePermissionRead 钉住本方法与别的写方法不同的一处顺序：
// 标题/封面/分区的**字段级**校验发生在读房间与判权限之后，
// 所以「权限不足」永远盖过「标题太长」——两种失败各自的触库面也不同。
func TestUpdateRoomInfoFieldGuardsRunAfterThePermissionRead(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)

	cases := []struct {
		name string
		mut  func(*rpc.UpdateRoomInfoReq)
		want error
		frag string
	}{
		{"标题超列宽", func(in *rpc.UpdateRoomInfoReq) { in.Title = strings.Repeat("标", 81) },
			model.ErrTitleInvalid, "81 > 80"},
		{"封面是绝对 URL", func(in *rpc.UpdateRoomInfoReq) { in.Title = ""; in.Cover = "https://cdn/x.jpg" },
			model.ErrCoverTooLong, "只允许 object key"},
		{"封面超列宽", func(in *rpc.UpdateRoomInfoReq) { in.Title = ""; in.Cover = "cover/" + strings.Repeat("x", 512) },
			model.ErrCoverTooLong, "518 > 512"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := st.log.snapshot()
			in := roomInfoReq("req-info-field-" + tc.name)
			tc.mut(in)
			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(in)
			wantErrIs(t, tc.name, err, tc.want)
			wantErrContains(t, tc.name, err, tc.frag)
			wantSeq(t, tc.name, st.log, before,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
			)
			wantKeyUnburned(t, tc.name, "req-info-field-"+tc.name, st)
		})
	}
}

func TestUpdateRoomInfoTerminalRoomRejected(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStateFinished, model.VerifyStatePassed)
	// 终态房间连权限都不该查：房间状态判定在绑定读之前。
	st.seedAnchor(baseAnchor(602, roomInfoRoom, roomInfoCohost, model.AnchorRoleCohost))

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-term"))
	wantErrIs(t, "已关闭房间改资料", err, model.ErrRoomFinished)
	wantSeq(t, "已关闭房间改资料", st.log, 0, "live_room.FindOne:4001")
	wantKeyUnburned(t, "已关闭房间改资料", "req-info-term", st)
	wantMethodCount(t, "已关闭房间改资料", st.log, "live_room_anchor.IsEnabled", 0)
}

func TestUpdateRoomInfoMissingRoomRejected(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-none"))
	wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
	wantSeq(t, "房间不存在", st.log, 0, "live_room.FindOne:4001")
	wantKeyUnburned(t, "房间不存在", "req-info-none", st)
}

// TestUpdateRoomInfoWhoMayEditProfile 锁角色判定：房主与生效联合主播可以，
// 房管与无绑定的人不行；三种拒绝都发生在任何写之前。
func TestUpdateRoomInfoWhoMayEditProfile(t *testing.T) {
	fixClock(t, roomInfoNow)

	t.Run("联合主播可以改", func(t *testing.T) {
		st := newStore()
		seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
		st.seedAnchor(baseAnchor(603, roomInfoRoom, roomInfoCohost, model.AnchorRoleCohost))

		_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(&rpc.UpdateRoomInfoReq{
			RoomId: roomInfoRoom, OperatorMid: roomInfoCohost, Title: "联合主播改的",
			RequestId: "req-info-cohost",
		})
		wantNoErr(t, "联合主播改资料", err)
		wantEQ(t, "联合主播改资料", "title", st.roomAt(t, roomInfoRoom).Title, "联合主播改的")
	})

	for _, tc := range []struct {
		name string
		mid  int64
		seed func(*testing.T, *store)
	}{
		{"房管被拒", roomInfoManager, func(t *testing.T, st *store) {
			st.seedAnchor(baseAnchor(604, roomInfoRoom, roomInfoManager, model.AnchorRoleManager))
		}},
		{"无绑定被拒", roomInfoStranger, func(*testing.T, *store) {}},
		{"绑定已停用被拒", roomInfoCohost, func(t *testing.T, st *store) {
			a := baseAnchor(605, roomInfoRoom, roomInfoCohost, model.AnchorRoleCohost)
			a.State = model.BindStateDisabled
			st.seedAnchor(a)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			tc.seed(t, st)

			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(&rpc.UpdateRoomInfoReq{
				RoomId: roomInfoRoom, OperatorMid: tc.mid, Title: "不该落库", RequestId: "req-info-forbid",
			})
			wantErrIs(t, tc.name, err, model.ErrAnchorForbidden)
			// 权限判定在任何写之前，而且它自己就是第二次读——之后一次调用都不许有。
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, tc.mid),
			)
			wantKeyUnburned(t, tc.name, "req-info-forbid", st)
			wantEQ(t, tc.name, "title 未改", st.roomAt(t, roomInfoRoom).Title, "深夜电台")
		})
	}
}

func TestUpdateRoomInfoReadFailuresPropagateVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		arm   func(*store, error)
		from  int
		tail  string
		fails string
	}{
		{"房间读失败", func(st *store, e error) { st.rooms.failWith("FindOne", e) }, 0,
			"live_room.FindOne:4001", "live_room_anchor.IsEnabled"},
		{"绑定读失败", func(st *store, e error) { st.anchors.failWith("IsEnabled", e) }, 1,
			fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid), "live_area.IsUsable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			st.seedArea(baseArea(roomInfoArea, 0, model.AreaStateEnabled))
			boom := errors.New("test: db down")
			tc.arm(st, boom)

			in := roomInfoReq("req-info-readfail")
			in.AreaId = roomInfoArea
			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(in)
			if !errors.Is(err, boom) {
				t.Fatalf("%s：错误 = %v, want errors.Is(%v)", tc.name, err, boom)
			}
			wantSeq(t, tc.name, st.log, tc.from, tc.tail)
			wantMethodCount(t, tc.name, st.log, tc.fails, 0)
			wantKeyUnburned(t, tc.name, "req-info-readfail", st)
		})
	}
}

// TestUpdateRoomInfoAreaAvailability 锁分区差的三条口径：
// 停用分区、缺行分区都归一成 ErrAreaDisabled（缺行等同停用），
// 且错误消息必须带上 area_id，否则调用方分不清自己传的是哪一个。
func TestUpdateRoomInfoAreaAvailability(t *testing.T) {
	cases := []struct {
		name  string
		seed  bool
		state int32
	}{
		{"停用分区", false, model.AreaStateDisabled},
		{"分区行不存在", true, model.AreaStateEnabled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			if !tc.seed {
				st.seedArea(baseArea(roomInfoArea, 0, tc.state))
			}

			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(&rpc.UpdateRoomInfoReq{
				RoomId: roomInfoRoom, OperatorMid: roomInfoMid, AreaId: roomInfoArea,
				RequestId: "req-info-area-bad",
			})
			wantErrIs(t, tc.name, err, model.ErrAreaDisabled)
			wantErrContains(t, tc.name, err, fmt.Sprintf("area_id=%d", roomInfoArea))
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
				fmt.Sprintf("live_area.IsUsable:%d", roomInfoArea),
			)
			wantKeyUnburned(t, tc.name, "req-info-area-bad", st)
			wantEQ(t, tc.name, "area_id 未改", st.roomAt(t, roomInfoRoom).AreaID, int64(roomInfoAreaCur))
		})
	}
}

// TestUpdateRoomInfoWithoutModerationDoesNotBurnKey 锁「下游未配置」这一降级分支的位置：
// 判定在 claimDedup **之前**，所以接线之后同一条 request_id 还能重试成功。
// （这是全仓唯一一处「守卫比 model 更严」的顺序，必须钉住。）
func TestUpdateRoomInfoWithoutModerationDoesNotBurnKey(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	// 故意不调 wireModeration：svcCtx.Moderation 保持 nil，即本环境未接入审核。
	lg := NewUpdateRoomInfoLogic(context.Background(), st.svcCtx())

	_, err := lg.UpdateRoomInfo(roomInfoReq("req-info-nomod"))
	wantErrIs(t, "审核未接线", err, model.ErrModerationNotConfigured)
	wantSeq(t, "审核未接线", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
	)
	wantKeyUnburned(t, "审核未接线", "req-info-nomod", st)
	wantEQ(t, "审核未接线", "title 未改", st.roomAt(t, roomInfoRoom).Title, "深夜电台")
}

// TestUpdateRoomInfoSubmitFailureLeavesRowAndKey 锁「先送审再改行」的代价与收益：
// 送审失败时本地一行未写（可以安全重试），但键已经被 Claim 消费掉——
// 重试同一条 request_id 拿到的是 ErrIdempotencyResultMissing 而不是重新执行。
func TestUpdateRoomInfoSubmitFailureLeavesRowAndKey(t *testing.T) {
	cases := []struct {
		name string
		arm  func(*store)
	}{
		{"下游报错", func(st *store) { st.mod.failWith(errors.New("test: moderation down")) }},
		{"下游回了空任务", func(st *store) { st.mod.taskReturns(0) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, roomInfoNow)
			st := newStore()
			seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
			tc.arm(st)

			_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-submitfail"))
			wantErrIs(t, tc.name, err, model.ErrDownstreamUnavailable)
			wantSeq(t, tc.name, st.log, 0,
				"live_room.FindOne:4001",
				fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
				"live_room_idempotency.Claim:req-info-submitfail",
				"moderation.SubmitForReview:4001",
			)
			row := st.roomAt(t, roomInfoRoom)
			wantEQ(t, tc.name, "title 未改", row.Title, "深夜电台")
			wantEQ(t, tc.name, "verify_state 未动", row.VerifyState, model.VerifyStatePassed)
			wantEQ(t, tc.name, "task_id 未落库", row.ModerationTaskID, int64(0))
			wantMethodCount(t, tc.name, st.log, "live_room.UpdateProfile", 0)
			wantMethodCount(t, tc.name, st.log, "live_room_state_log.Insert", 0)
			wantKeyBurnedNoResult(t, tc.name, "req-info-submitfail", st)

			// 重试同一条键：拿不到结果，只能报「结果还没写出来」。
			_, err = newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-submitfail"))
			wantErrIs(t, tc.name+" 重试", err, model.ErrIdempotencyResultMissing)
		})
	}
}

func TestUpdateRoomInfoProfileWriteFailurePropagates(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	boom := errors.New("test: update failed")
	st.rooms.failWith("UpdateProfile", boom)

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-writefail"))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	wantSeq(t, "改资料写失败", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:req-info-writefail",
		"moderation.SubmitForReview:4001",
		"live_room.UpdateProfile:4001:v2/t9001",
	)
	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "改资料写失败", "title", row.Title, "深夜电台")
	wantEQ(t, "改资料写失败", "verify_state", row.VerifyState, model.VerifyStatePassed)
	// 送审任务已提交给下游，本地一行未写：审核队列里多一条孤儿任务，
	// 这是「先送审再改行」的既定代价，用 counts 把「本地确实没写」钉成事实。
	wantDeepEQ(t, "改资料写失败残留", "counts", st.counts(), storeCounts{rooms: 1, anchors: 1, idem: 1})
	wantTxCount(t, "改资料写失败", st.conn, 0)
}

// TestUpdateRoomInfoStateChangedUnderneathReportsConcurrentUpdate 锁 RowsAffected==0 分支：
// UPDATE 的 WHERE 带 `state IN (allowStates)`，读到 PENDING 之后被别的入口改成 DISABLED，
// 这条 UPDATE 就命中 0 行 → ErrConcurrentUpdate。
// 残留形态是本用例的重点：审核任务**已经提交**，但资料一行未改。
func TestUpdateRoomInfoStateChangedUnderneathReportsConcurrentUpdate(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	st.raceBefore("live_room.UpdateProfile", func() {
		for _, r := range st.rooms.rows {
			if r.RoomID == roomInfoRoom {
				r.State = model.RoomStateDisabled // 等价于「运营在两次语句之间停用了房间」
			}
		}
	})

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-cas"))
	wantErrIs(t, "状态被并发改走", err, model.ErrConcurrentUpdate)
	st.checkRaces(t)

	wantSeq(t, "状态被并发改走", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:req-info-cas",
		"moderation.SubmitForReview:4001",
		"live_room.UpdateProfile:4001:v2/t9001",
	)
	// 钩子把 state 改成了 DISABLED，这是前提而不是结论；资料列必须一行未动。
	row := st.roomAt(t, roomInfoRoom)
	wantEQ(t, "状态被并发改走", "title 未改", row.Title, "深夜电台")
	wantEQ(t, "状态被并发改走", "verify_state 未动", row.VerifyState, model.VerifyStatePassed)
	wantEQ(t, "状态被并发改走", "state_version 未动", row.StateVersion, int32(1))
	wantMethodCount(t, "状态被并发改走", st.log, "live_room_state_log.Insert", 0)
	wantKeyBurnedNoResult(t, "状态被并发改走", "req-info-cas", st)
	wantEQ(t, "送审已经发生", "submit 次数", st.mod.submitCount(), 1)
}

// TestUpdateRoomInfoAuditFailureIsSwallowed 锁 updateroominfologic.go:140-144：
// 资料已经改完，审计日志写失败只记错误日志、不对外报错。
// 于是库里出现「verify_state 已迁移但没有对应审计行」的分裂形态——
// 本用例把这个形态钉成**当前行为**，它违反 AGENTS.md §8「审计与状态变更同事务」，
// 已登记为 README 已知缺口（补法要么把这条 Insert 挪进事务，要么承认资料变更不是状态变更）。
func TestUpdateRoomInfoAuditFailureIsSwallowed(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	boom := errors.New("test: audit insert failed")
	st.stateLogs.failWith("Insert", boom)

	reply, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-auditfail"))
	wantNoErr(t, "审计写失败不推翻资料变更", err)
	wantEQ(t, "审计写失败", "应答仍带 task_id", reply.GetModerationTaskId(), int64(9001))

	// 失败的 Insert 仍然进了 callLog（语句发了），后面的回读与回填照常。
	wantSeq(t, "审计写失败", st.log, 4,
		"live_room.UpdateProfile:4001:v2/t9001",
		"live_room_state_log.Insert:t2:3->2:profile_resubmitted",
		"live_room.FindOne:4001",
		"live_room_idempotency.SaveResult:req-info-auditfail",
	)
	wantEQ(t, "审计写失败", "title 已改", st.roomAt(t, roomInfoRoom).Title, "深夜电台 第二季")
	wantDeepEQ(t, "审计写失败残留", "counts", st.counts(),
		storeCounts{rooms: 1, anchors: 1, logs: 0, idem: 1})
	if len(st.logsOf(roomInfoRoom)) != 0 {
		t.Fatalf("审计行残留 = %d, want 0", len(st.logsOf(roomInfoRoom)))
	}
}

// TestUpdateRoomInfoPostWriteReadFailure 锁提交后回读的两处失败形态：
// 资料已改，但回读报错 / 回读发现行没了，两种都对外报错且不回填结果。
func TestUpdateRoomInfoPostWriteReadFailure(t *testing.T) {
	t.Run("回读报错", func(t *testing.T) {
		fixClock(t, roomInfoNow)
		st := newStore()
		seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
		boom := errors.New("test: read back failed")
		// 钩子挂在审计写之前：审计写完才有第二次 FindOne。
		st.raceBefore("live_room_state_log.Insert", func() { st.rooms.failWith("FindOne", boom) })

		_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-readback"))
		if !errors.Is(err, boom) {
			t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
		}
		st.checkRaces(t)
		wantSeq(t, "回读报错", st.log, 5,
			"live_room_state_log.Insert:t2:3->2:profile_resubmitted",
			"live_room.FindOne:4001",
		)
		wantEQ(t, "回读报错", "title 已改", st.roomAt(t, roomInfoRoom).Title, "深夜电台 第二季")
		wantMethodCount(t, "回读报错", st.log, "live_room_idempotency.SaveResult", 0)
		wantKeyBurnedNoResult(t, "回读报错", "req-info-readback", st)
	})

	t.Run("回读缺行", func(t *testing.T) {
		fixClock(t, roomInfoNow)
		st := newStore()
		seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
		// 房间行在审计写之前被删掉（等价于别的入口关了房并清理），
		// 于是第二次 FindOne 回 (nil, nil) → ErrRoomNotFound。
		st.raceBefore("live_room_state_log.Insert", func() { st.rooms.rows = nil })

		_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-vanish"))
		wantErrIs(t, "回读缺行", err, model.ErrRoomNotFound)
		st.checkRaces(t)
		wantSeq(t, "回读缺行", st.log, 5,
			"live_room_state_log.Insert:t2:3->2:profile_resubmitted",
			"live_room.FindOne:4001",
		)
		wantMethodCount(t, "回读缺行", st.log, "live_room_idempotency.SaveResult", 0)
		wantDeepEQ(t, "回读缺行残留", "counts", st.counts(), storeCounts{anchors: 1, logs: 1, idem: 1})
	})
}

func TestUpdateRoomInfoClaimFailurePropagates(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	boom := errors.New("test: claim failed")
	st.idem.failWith("Claim", boom)

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-claim"))
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want errors.Is(%v)", err, boom)
	}
	wantSeq(t, "抢键失败", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:req-info-claim",
	)
	wantMethodCount(t, "抢键失败", st.log, "moderation.SubmitForReview", 0)
	wantEQ(t, "抢键失败", "title 未改", st.roomAt(t, roomInfoRoom).Title, "深夜电台")
}

// TestUpdateRoomInfoReplayReturnsStoredProfileNotCurrentOne 锁重放口径：
// 第二次同键调用回的是**首次执行时存的投影**，即使库里已经又变了一次。
func TestUpdateRoomInfoReplayReturnsStoredProfileNotCurrentOne(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	st.seedIdem("req-info-replay", "UpdateRoomInfo",
		`{"room":{"room_id":4001,"owner_mid":9001,"title":"首次执行时的标题","state":1,"verify_state":2,"state_version":1},"moderation_task_id":9001}`)
	// 再往库里写一条更新的标题：重放绝不能回它。
	if _, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-newer")); err != nil {
		t.Fatalf("布数据用的第二次改资料失败：%v", err)
	}
	from := st.log.snapshot()
	st.rooms.rows[0].Title = "库里当前更新的标题"

	reply, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("req-info-replay"))
	wantNoErr(t, "重放", err)
	wantSeq(t, "重放", st.log, from,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:req-info-replay",
		"live_room_idempotency.Find:req-info-replay",
	)
	wantEQ(t, "重放", "replayed", reply.GetReplayed(), true)
	wantEQ(t, "重放", "回的是存的标题", reply.GetRoom().GetTitle(), "首次执行时的标题")
	wantEQ(t, "重放", "回的是存的 task_id", reply.GetModerationTaskId(), int64(9001))
	wantEQ(t, "重放", "库里标题没被重放改掉", st.roomAt(t, roomInfoRoom).Title, "库里当前更新的标题")
	// 零新写：上面那段完整序列已经把「不送审、不改行、不写审计、不回填」锁死了。
	wantMethodCount(t, "重放", st.log, "moderation.SubmitForReview", 1)
	wantMethodCount(t, "重放", st.log, "live_room.UpdateProfile", 1)
	wantMethodCount(t, "重放", st.log, "live_room_state_log.Insert", 1)
	wantMethodCount(t, "重放", st.log, "live_room_idempotency.SaveResult", 1)
}

func TestUpdateRoomInfoKeyUsedByAnotherRpcIsRejected(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	st.seedIdem("shared-key", "BanRoom", `{"ban_id":1}`)

	_, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("shared-key"))
	wantErrIs(t, "键被别的 RPC 用过", err, model.ErrRequestIDReused)
	wantErrContains(t, "键被别的 RPC 用过", err, "used by BanRoom")
	wantSeq(t, "键被别的 RPC 用过", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:shared-key",
		"live_room_idempotency.Find:shared-key",
	)
	wantMethodCount(t, "键被别的 RPC 用过", st.log, "live_room.UpdateProfile", 0)
	// 既有行的归因不被改写。
	wantEQ(t, "键被别的 RPC 用过", "rpc", st.idemAt("shared-key").Rpc, "BanRoom")
}

func TestUpdateRoomInfoBurnedKeyWithoutResult(t *testing.T) {
	fixClock(t, roomInfoNow)
	st := newStore()
	seedRoomInfoScene(t, st, model.RoomStatePending, model.VerifyStatePassed)
	st.seedIdem("burned", "UpdateRoomInfo", "")

	reply, err := newRoomInfoLogic(t, st).UpdateRoomInfo(roomInfoReq("burned"))
	wantErrIs(t, "烧过的键没有结果", err, model.ErrIdempotencyResultMissing)
	if reply != nil {
		t.Fatalf("应答 = %+v, want nil", reply)
	}
	wantSeq(t, "烧过的键没有结果", st.log, 0,
		"live_room.FindOne:4001",
		fmt.Sprintf("live_room_anchor.IsEnabled:%d/%d", roomInfoRoom, roomInfoMid),
		"live_room_idempotency.Claim:burned",
		"live_room_idempotency.Find:burned",
	)
	wantMethodCount(t, "烧过的键没有结果", st.log, "live_room.UpdateProfile", 0)
}
