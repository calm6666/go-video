package logic

// banlift_logic_test.go 覆盖写侧方法 BanRoom（banroomlogic.go）与 LiftBan（liftbanlogic.go）。
//
// 两个方法成对钉住，锁的结论（全部由实现反推，不是理想设计）：
//  1. 禁播时长只有一条进入写路径的通道：banEndAt(ban_type, duration_seconds, now) 先算出 end_at，
//     再由 Rooms.TransitionTx 的 RoomPatch{BanUntil} 落进 live_room.ban_until 投影。
//     永久禁播恒 end_at=0（即使请求带了 duration_seconds 也被吃掉），到期扫描永不放行，
//     只能由 LiftBan 解除 —— 所以 LiftBan 必须能把 ban_until 一并清成 0，两个方法合起来才闭合。
//  2. live_room_ban 是 append-only + 一次解除写三列：model/live_room_ban.go:insert 的列清单里
//     lift_operator_mid/lift_reason/lifted_at 是**字面量 0/""/0**（新记录不可能带解除信息），
//     lift 的 UPDATE 才把三列写成真值，且 WHERE 带 state=1 → 重复解除命中 0 行。
//     本文件对「插入后三列是什么」「解除后三列是什么」「reason 没传时写成空串」都读回断言。
//  3. BanRoom 无归属校验：它读 Anchors.FindOwner 只为了快照被禁主播 mid（异常数据回 0），
//     不校验 operator_mid 是不是房主；LiftBan 干脆连 FindOwner 都不调（用调用次数 0 锁死）。
//     权限口径与 UpdateRoomSetting 相反，所以必须分别钉住而不是笼统写「运营接口不校验」。
//  4. 事务边界：BanRoom 的「替换旧记录 + 插新记录 + 踢场次 + 迁房间状态 + 审计」全在同一个
//     TransactCtx 里；LiftBan 的「解除记录 + 迁状态/清投影」也在同一个事务里。
//     假件不回滚，所以失败用例断言的是**真实残留形态**（哪些写已经落库、键烧没烧）。
//  5. 已在 BANNED 的房间再禁播不走 TransitionTx（矩阵无自边），改走 SetBanUntilTx +
//     t1:5->5:ban window replaced 审计；这一分支不推进 state_version，是刻意的。
//  6. LiftBan 的落点由 verify_state 决定（PASSED→READY，其余→PENDING），且它**不信自己写的值**：
//     提交后再读一次房间，读到的状态与目标不一致时如实回当前状态并改文案。

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
	banNow        int64 = 1_700_000_000
	banRoomID     int64 = 4201
	banOwner      int64 = 9201
	banOper       int64 = 555
	banLiftOper   int64 = 666
	banSessID     int64 = 9301
	banPrevID     int64 = 8800
	banOtherRm    int64 = 4299
	banReason           = "违规内容处置"
	banLiftReason       = "申诉通过"
)

// banReq 是一条「除幂等键外都合法」的禁播请求。
func banReq(reqID string, banType rpc.BanType, duration int64) *rpc.BanRoomReq {
	return &rpc.BanRoomReq{
		RoomId:          banRoomID,
		BanType:         banType,
		DurationSeconds: duration,
		Reason:          banReason,
		OperatorMid:     banOper,
		RequestId:       reqID,
		TraceId:         "trace-ban",
	}
}

// liftReq 是「解除当前生效记录」的合法请求（ban_id=0）。
func liftReq(reqID string) *rpc.LiftBanReq {
	return &rpc.LiftBanReq{
		RoomId:      banRoomID,
		OperatorMid: banLiftOper,
		Reason:      banLiftReason,
		RequestId:   reqID,
		TraceId:     "trace-lift",
	}
}

func newBanLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *BanRoomLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewBanRoomLogic(context.Background(), st.svcCtxWith(c))
}

func newLiftLogic(t *testing.T, st *store, conf ...config.LiveRoomConf) *LiftBanLogic {
	t.Helper()
	c := testLiveRoomConf()
	if len(conf) > 0 {
		c = conf[0]
	}
	return NewLiftBanLogic(context.Background(), st.svcCtxWith(c))
}

// seedBannedRoom 布一个处于 BANNED 的房间 + 一条生效永久禁播。
// 历史已解除记录也一起布上：它既是「FindActiveByRoom 只看 state=1」的反例，
// 又让新禁播的自增主键可预测（8800 + 1 = 8801）。
func seedBannedRoom(t *testing.T, st *store, verifyState int32) *model.LiveRoom {
	t.Helper()
	r := baseRoom(banRoomID, banOwner, model.RoomStateBanned)
	r.VerifyState = verifyState
	r.BanUntil = 0
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	st.seedBan(baseBan(banPrevID-100, banRoomID, model.BanStateLifted))
	st.seedBan(activePermanentBan(banPrevID, banRoomID, banOwner))
	return r
}

func activePermanentBan(banID, roomID, mid int64) *model.LiveRoomBan {
	b := baseBan(banID, roomID, model.BanStateActive)
	b.Mid = mid
	return b
}

func temporaryBan(banID, roomID, mid, startAt, endAt int64, state int32) *model.LiveRoomBan {
	b := baseBan(banID, roomID, state)
	b.Mid = mid
	b.BanType = model.BanTypeTemporary
	b.StartAt = startAt
	b.EndAt = endAt
	return b
}

// liveRoomRow 返回库里的**活指针**，只给并发钩子改行用；断言一律走 st.roomAt 的副本。
func liveRoomRow(t *testing.T, st *store) *model.LiveRoom {
	t.Helper()
	for _, r := range st.rooms.rows {
		if r.RoomID == banRoomID {
			return r
		}
	}
	t.Fatalf("库里没有房间 %d", banRoomID)
	return nil
}

// --- BanRoom ---

func TestBanRoomTemporaryBanOnLivingRoomFullTrace(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	r := baseRoom(banRoomID, banOwner, model.RoomStateLiving)
	r.ActiveSessionID = banSessID
	r.ActiveStreamID = "stream-4201"
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	sess := baseSession(banSessID, banRoomID, banOwner, model.SessionStateLiving)
	sess.StartedAt = banNow - 300
	sess.StreamID = "stream-4201"
	st.seedSession(sess)
	// 一条早已解除的历史记录：FindActiveByRoom 不得把它当生效禁播。
	st.seedBan(baseBan(banPrevID-100, banRoomID, model.BanStateLifted))
	lg := newBanLogic(t, st)

	reply, err := lg.BanRoom(banReq("req-ban-tmp", rpc.BanType_BAN_TYPE_TEMPORARY, 3600))
	wantNoErr(t, "临时禁播", err)
	defer st.checkRaces(t)

	wantSeq(t, "临时禁播首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:req-ban-tmp",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		fmt.Sprintf("live_session.FindActiveByRoom:%d", banRoomID),
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			banSessID, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonBanned),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, banReason),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateLiving, model.RoomStateBanned, r.StateVersion),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateLiving, model.RoomStateBanned, banReason),
		"live_room_idempotency.SaveResult:req-ban-tmp",
	)
	wantTxCount(t, "临时禁播", st.conn, 1)
	// 历史已解除记录不该被替换写：整场只有一次 InsertTx、零次 LiftTx。
	wantMethodCount(t, "临时禁播", st.log, "live_room_ban.LiftTx", 0)
	wantMethodCount(t, "临时禁播", st.log, "live_room_ban.InsertTx", 1)

	wantEQ(t, "禁播应答", "ban_id", reply.GetBanId(), banPrevID-99)
	wantEQ(t, "禁播应答", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_BANNED)
	wantEQ(t, "禁播应答", "terminated_session_id", reply.GetTerminatedSessionId(), banSessID)
	wantEQ(t, "禁播应答", "end_at", reply.GetEndAt(), banNow+3600)
	wantEQ(t, "禁播应答", "replayed", reply.GetReplayed(), false)

	// 禁播时长进写路径的落点：ban_until = now + duration（由那条迁移的 patch 写进去）。
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "禁播后的房间", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "禁播后的房间", "state_version", row.StateVersion, int32(2))
	wantEQ(t, "禁播后的房间", "ban_until", row.BanUntil, banNow+3600)
	wantEQ(t, "禁播后的房间", "mtime", row.Mtime, banNow)
	wantEQ(t, "禁播后的房间", "ctime", row.Ctime, 1000+banRoomID)

	ban := st.banAt(t, reply.GetBanId())
	wantEQ(t, "禁播记录", "room_id", ban.RoomID, banRoomID)
	// mid 是**生效房主的快照**，不是操作者：operator_mid 才记运营。
	wantEQ(t, "禁播记录", "mid", ban.Mid, banOwner)
	wantEQ(t, "禁播记录", "operator_mid", ban.OperatorMid, banOper)
	wantEQ(t, "禁播记录", "ban_type", ban.BanType, model.BanTypeTemporary)
	wantEQ(t, "禁播记录", "reason", ban.Reason, banReason)
	wantEQ(t, "禁播记录", "start_at", ban.StartAt, banNow)
	wantEQ(t, "禁播记录", "end_at", ban.EndAt, banNow+3600)
	wantEQ(t, "禁播记录", "state", ban.State, model.BanStateActive)
	wantEQ(t, "禁播记录", "trace_id", ban.TraceID, "trace-ban")
	wantEQ(t, "禁播记录", "ctime", ban.Ctime, banNow)
	// 纪律 2：INSERT 的列清单把解除三列写成常量，新记录不可能带解除信息。
	wantEQ(t, "禁播记录", "lift_operator_mid", ban.LiftOperatorMid, int64(0))
	wantEQ(t, "禁播记录", "lift_reason", ban.LiftReason, "")
	wantEQ(t, "禁播记录", "lifted_at", ban.LiftedAt, int64(0))

	ss := st.sessionAt(t, banSessID)
	wantEQ(t, "被踢场次", "state", ss.State, model.SessionStateTerminated)
	wantEQ(t, "被踢场次", "end_reason", ss.EndReason, model.EndReasonBanned)
	wantEQ(t, "被踢场次", "ended_at", ss.EndedAt, banNow)

	logs := st.logsOf(banRoomID)
	if len(logs) != 2 {
		t.Fatalf("审计行数 = %d, want 2（场次态 + 房间态）", len(logs))
	}
	wantEQ(t, "场次审计", "state_type", logs[0].StateType, model.LogTypeSessionState)
	wantEQ(t, "场次审计", "from->to", fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "2->4")
	wantEQ(t, "房间审计", "state_type", logs[1].StateType, model.LogTypeRoomState)
	wantEQ(t, "房间审计", "from->to", fmt.Sprintf("%d->%d", logs[1].FromState, logs[1].ToState), "3->5")
	wantEQ(t, "房间审计", "session_id", logs[1].SessionID, banSessID)
	for i, l := range logs {
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "source", l.Source, model.SourceRPCAdmin)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "operator_mid", l.OperatorMid, banOper)
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "request_id", l.RequestID, "req-ban-tmp")
		wantEQ(t, fmt.Sprintf("审计[%d]", i), "reason", l.Reason, banReason)
	}

	// 已知缺口（README 16）：禁播把场次踢成终态、房间迁到 BANNED，
	// 但 RoomPatch 只带 ban_until —— active_session_id 仍指着那条 TERMINATED 场次，
	// 而且本方法一次都没调 ClearActiveSession。这里两侧同时钉住，改动时必红。
	wantEQ(t, "缺口现场", "active_session_id", row.ActiveSessionID, banSessID)
	wantEQ(t, "缺口现场", "active_stream_id", row.ActiveStreamID, "stream-4201")
	wantMethodCount(t, "禁播后未清挂机位", st.log, "live_room.ClearActiveSession", 0)
}

func TestBanRoomPermanentBanZeroesEndAtEvenWhenDurationGiven(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	lg := newBanLogic(t, st)

	// 请求同时给 permanent 与 duration：时长被类型吃掉，不产生「永久但有到期」的形态。
	reply, err := lg.BanRoom(banReq("req-ban-perm", rpc.BanType_BAN_TYPE_PERMANENT, 3600))
	wantNoErr(t, "永久禁播", err)
	defer st.checkRaces(t)

	wantSeq(t, "永久禁播首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:req-ban-perm",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		fmt.Sprintf("live_session.FindActiveByRoom:%d", banRoomID),
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStatePending, model.RoomStateBanned, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStatePending, model.RoomStateBanned, banReason),
		"live_room_idempotency.SaveResult:req-ban-perm",
	)
	wantEQ(t, "永久禁播应答", "end_at", reply.GetEndAt(), int64(0))
	wantEQ(t, "永久禁播应答", "terminated_session_id", reply.GetTerminatedSessionId(), int64(0))

	ban := st.banAt(t, reply.GetBanId())
	wantEQ(t, "永久禁播记录", "end_at", ban.EndAt, int64(0))
	wantEQ(t, "永久禁播记录", "ban_type", ban.BanType, model.BanTypePermanent)
	// ban_until=0 与「临时禁播已到期」在列上同形，只能靠 state 区分 —— 这里是 5。
	wantEQ(t, "永久禁播后的房间", "ban_until", st.roomAt(t, banRoomID).BanUntil, int64(0))
	// 没有进行中场次就不该出场次迁移。
	wantMethodCount(t, "永久禁播", st.log, "live_session.TransitionTx", 0)
	wantMethodCount(t, "永久禁播", st.log, "live_room_state_log.InsertTx", 1)
}

func TestBanRoomOnBannedRoomReplacesWindowWithoutFakeTransition(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	lg := newBanLogic(t, st)

	reply, err := lg.BanRoom(banReq("req-ban-again", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantNoErr(t, "重复禁播", err)
	defer st.checkRaces(t)

	wantSeq(t, "重复禁播首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:req-ban-again",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		fmt.Sprintf("live_session.FindActiveByRoom:%d", banRoomID),
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		"live_room_ban.InsertTx",
		// 已在 BANNED：矩阵无自边，改走 SetBanUntilTx（不推进 state_version）。
		fmt.Sprintf("live_room.SetBanUntilTx:%d:%d", banRoomID, banNow+600),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateBanned, "ban window replaced"),
		"live_room_idempotency.SaveResult:req-ban-again",
	)
	wantTxCount(t, "重复禁播", st.conn, 1)
	wantMethodCount(t, "不伪造状态迁移", st.log, "live_room.TransitionTx", 0)

	wantEQ(t, "重复禁播应答", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_BANNED)
	wantEQ(t, "重复禁播应答", "end_at", reply.GetEndAt(), banNow+600)
	wantEQ(t, "重复禁播应答", "terminated_session_id", reply.GetTerminatedSessionId(), int64(0))

	// 旧记录必须先落到「已解除」终态，且解除三列写的是替换语义。
	prev := st.banAt(t, banPrevID)
	wantEQ(t, "被替换的旧记录", "state", prev.State, model.BanStateLifted)
	wantEQ(t, "被替换的旧记录", "lift_operator_mid", prev.LiftOperatorMid, banOper)
	wantEQ(t, "被替换的旧记录", "lift_reason", prev.LiftReason, "replaced by a new ban")
	wantEQ(t, "被替换的旧记录", "lifted_at", prev.LiftedAt, banNow)

	row := st.roomAt(t, banRoomID)
	wantEQ(t, "替换区间的房间", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "替换区间的房间", "state_version", row.StateVersion, int32(1))
	wantEQ(t, "替换区间的房间", "ban_until", row.BanUntil, banNow+600)
	wantEQ(t, "替换区间的房间", "mtime", row.Mtime, banNow)

	// 库里同时留着「一条已解除 + 一条生效」，且只有新记录是 Active。
	counts := st.counts()
	wantEQ(t, "禁播表", "bans 行数", counts.bans, 3)
	var active int
	for _, id := range []int64{banPrevID - 100, banPrevID, reply.GetBanId()} {
		if st.banAt(t, id).State == model.BanStateActive {
			active++
		}
	}
	wantEQ(t, "生效禁播条数", "state=1 的行数", active, 1)

	logs := st.logsOf(banRoomID)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "替换区间审计", "reason", logs[0].Reason, "ban window replaced")
	wantEQ(t, "替换区间审计", "from->to",
		fmt.Sprintf("%d->%d", logs[0].FromState, logs[0].ToState), "5->5")
}

func TestBanRoomRepeatSameRequestIDReplaysStoredReply(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	st.seedIdem("req-ban-replay", "BanRoom",
		fmt.Sprintf(`{"ban_id":%d,"state":5,"terminated_session_id":%d,"end_at":%d,"replayed":true}`,
			banPrevID, banSessID, banNow+600))

	reply, err := newBanLogic(t, st).BanRoom(banReq("req-ban-replay", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantNoErr(t, "禁播重放", err)

	wantSeq(t, "禁播重放", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:req-ban-replay",
		"live_room_idempotency.Find:req-ban-replay",
	)
	wantEQ(t, "禁播重放", "replayed", reply.GetReplayed(), true)
	wantEQ(t, "禁播重放", "ban_id", reply.GetBanId(), banPrevID)
	wantEQ(t, "禁播重放", "terminated_session_id", reply.GetTerminatedSessionId(), banSessID)
	wantEQ(t, "禁播重放", "end_at", reply.GetEndAt(), banNow+600)
	// 重放不重复处置：禁播表没新增行，房间没再动，零事务。
	before := st.counts()
	wantEQ(t, "禁播重放残留", "bans", before.bans, 2)
	wantEQ(t, "禁播重放残留", "logs", before.logs, 0)
	wantTxCount(t, "禁播重放", st.conn, 0)
	wantMethodCount(t, "禁播重放", st.log, "live_room_ban.InsertTx", 0)
	wantMethodCount(t, "禁播重放", st.log, "live_room_ban.FindActiveByRoom", 0)
	wantSeq(t, "禁播重放之后不得再有任何调用", st.log, 4)
}

func TestBanRoomKeyUsedByAnotherRPCRejected(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	st.seedIdem("shared-ban", "CloseRoom", `{"state":4}`)

	_, err := newBanLogic(t, st).BanRoom(banReq("shared-ban", rpc.BanType_BAN_TYPE_PERMANENT, 0))
	wantErrIs(t, "键被别人用过", err, model.ErrRequestIDReused)
	wantErrContains(t, "键被别人用过", err, "used by CloseRoom")
	wantSeq(t, "键被别人用过", st.log, 2,
		"live_room_idempotency.Claim:shared-ban",
		"live_room_idempotency.Find:shared-ban",
	)
	wantMethodCount(t, "键被别人用过", st.log, "live_room_ban.InsertTx", 0)
}

func TestBanRoomBurnedKeyWithoutResultReportsMissing(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	st.seedIdem("burned-ban", "BanRoom", "")

	reply, err := newBanLogic(t, st).BanRoom(banReq("burned-ban", rpc.BanType_BAN_TYPE_PERMANENT, 0))
	wantErrIs(t, "烧过的键", err, model.ErrIdempotencyResultMissing)
	if reply != nil {
		t.Fatalf("应答 = %+v, want nil", reply)
	}
	wantSeq(t, "烧过的键", st.log, 2,
		"live_room_idempotency.Claim:burned-ban",
		"live_room_idempotency.Find:burned-ban",
	)
	wantMethodCount(t, "烧过的键", st.log, "live_room_ban.InsertTx", 0)
}

func TestBanRoomTerminalAndMissingRoomRejectedBeforeClaim(t *testing.T) {
	t.Run("已关闭房间不可禁播", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStateFinished))
		st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))

		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-fin", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantErrIs(t, "终态房间", err, model.ErrRoomFinished)
		wantSeq(t, "终态房间", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", banRoomID))
		wantKeyUnburned(t, "终态房间", "req-ban-fin", st)
	})

	t.Run("DISABLED 房间仍可禁播", func(t *testing.T) {
		// 只有 FINISHED 是终态；DISABLED→BANNED 是矩阵里的合法边。
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStateDisabled))
		st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))

		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-dis", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantNoErr(t, "禁用房间再禁播", err)
		wantSeq(t, "禁用房间再禁播", st.log, 7,
			fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
				banRoomID, model.RoomStateDisabled, model.RoomStateBanned, int32(1)),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeRoomState, model.RoomStateDisabled, model.RoomStateBanned, banReason),
			"live_room_idempotency.SaveResult:req-ban-dis",
		)
	})

	t.Run("房间不存在", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()

		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-none", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
		wantSeq(t, "房间不存在", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", banRoomID))
		wantKeyUnburned(t, "房间不存在", "req-ban-none", st)
	})
}

func TestBanRoomGuardTableRejectsWithZeroDependencyCalls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.BanRoomReq)
		want   error
		frag   string
	}{
		{"缺运营者", func(in *rpc.BanRoomReq) { in.OperatorMid = 0 }, model.ErrOperatorRequired, ""},
		{"运营者为负", func(in *rpc.BanRoomReq) { in.OperatorMid = -1 }, model.ErrOperatorRequired, ""},
		{"缺房间号", func(in *rpc.BanRoomReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"幂等键空", func(in *rpc.BanRoomReq) { in.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"幂等键纯空白", func(in *rpc.BanRoomReq) { in.RequestId = "   " }, model.ErrRequestIDRequired, ""},
		{"原因必填", func(in *rpc.BanRoomReq) { in.Reason = "" }, model.ErrReasonTooLong, "reason required"},
		{"原因纯空白", func(in *rpc.BanRoomReq) { in.Reason = "   " }, model.ErrReasonTooLong, "reason required"},
		{"原因超长", func(in *rpc.BanRoomReq) {
			in.Reason = strings.Repeat("违", maxReasonRunes+1)
		}, model.ErrReasonTooLong, ""},
		{"禁播类型缺省", func(in *rpc.BanRoomReq) {
			in.BanType = rpc.BanType_BAN_TYPE_UNSPECIFIED
		}, model.ErrBanTypeInvalid, ""},
		{"禁播类型未知", func(in *rpc.BanRoomReq) { in.BanType = rpc.BanType(9) }, model.ErrBanTypeInvalid, ""},
		{"临时禁播缺时长", func(in *rpc.BanRoomReq) {
			in.BanType = rpc.BanType_BAN_TYPE_TEMPORARY
			in.DurationSeconds = 0
		}, model.ErrBanDurationRequired, ""},
		{"临时禁播负时长", func(in *rpc.BanRoomReq) {
			in.BanType = rpc.BanType_BAN_TYPE_TEMPORARY
			in.DurationSeconds = -60
		}, model.ErrBanDurationRequired, ""},
		{"临时禁播时长溢出", func(in *rpc.BanRoomReq) {
			in.BanType = rpc.BanType_BAN_TYPE_TEMPORARY
			in.DurationSeconds = int64(^uint64(0) >> 1)
		}, model.ErrBanDurationRequired, "duration overflow"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, banNow)
			st := newStore()
			// 房间与房主都在库里：守卫若没生效，后面就会走通到写路径。
			st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStateLiving))
			st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
			in := banReq("req-ban-guard", rpc.BanType_BAN_TYPE_TEMPORARY, 3600)
			tc.mutate(in)

			before := st.log.snapshot()
			reply, err := newBanLogic(t, st).BanRoom(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			if reply != nil {
				t.Fatalf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantNoCallAfter(t, tc.name, st.log, before)
			wantKeyUnburned(t, tc.name, "req-ban-guard", st)
			wantTxCount(t, tc.name, st.conn, 0)
			wantNoDirectSQL(t, tc.name, st.conn)
			wantEQ(t, tc.name+"：禁播表", "bans", st.counts().bans, 0)
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		lg := newBanLogic(t, st)
		before := st.log.snapshot()
		// 直接经生成的入口传 nil，等价于「连字段都没有」的最坏入参。
		reply, err := lg.BanRoom(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		if reply != nil {
			t.Fatalf("nil 请求应答 = %+v, want nil", reply)
		}
		wantNoCallAfter(t, "nil 请求", st.log, before)
	})

	t.Run("守卫顺序：时长非法时不读房间", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		// 故意不布房间：若守卫顺序把 banEndAt 放到 FindOne 之后，这里会拿到 ErrRoomNotFound。
		in := banReq("req-ban-order", rpc.BanType_BAN_TYPE_TEMPORARY, 0)
		before := st.log.snapshot()
		_, err := newBanLogic(t, st).BanRoom(in)
		wantErrIs(t, "顺序", err, model.ErrBanDurationRequired)
		wantNoCallAfter(t, "顺序", st.log, before)
	})
}

func TestBanRoomSnapshotsOwnerMidWithoutAuthorizingOperator(t *testing.T) {
	t.Run("陌生运营同样受理，被禁 mid 仍是房主", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
		st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
		in := banReq("req-ban-stranger", rpc.BanType_BAN_TYPE_PERMANENT, 0)
		in.OperatorMid = 424242

		reply, err := newBanLogic(t, st).BanRoom(in)
		wantNoErr(t, "陌生运营", err)
		wantEQ(t, "陌生运营", "ban_id", reply.GetBanId(), int64(1))
		ban := st.banAt(t, reply.GetBanId())
		wantEQ(t, "陌生运营", "mid", ban.Mid, banOwner)
		wantEQ(t, "陌生运营", "operator_mid", ban.OperatorMid, int64(424242))
		// 归属绑定判定的痕迹：读了 FindOwner，但一次都没做 IsEnabled（不校验操作者角色）。
		wantMethodCount(t, "陌生运营", st.log, "live_room_anchor.FindOwner", 1)
		wantMethodCount(t, "陌生运营", st.log, "live_room_anchor.IsEnabled", 0)
	})

	t.Run("没有生效房主时 mid 落 0", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
		// 只有联合主播绑定：role=OWNER AND state=1 查不到 → FindOwner 回 (nil, nil)。
		st.seedAnchor(baseAnchor(802, banRoomID, banOwner, model.AnchorRoleCohost))

		reply, err := newBanLogic(t, st).BanRoom(
			banReq("req-ban-noowner", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantNoErr(t, "无生效房主", err)
		ban := st.banAt(t, reply.GetBanId())
		wantEQ(t, "无生效房主", "mid", ban.Mid, int64(0))
		wantEQ(t, "无生效房主", "operator_mid", ban.OperatorMid, banOper)
	})
}

func TestBanRoomRoomCASMissLeavesPartialWrites(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	r := baseRoom(banRoomID, banOwner, model.RoomStateLiving)
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	st.seedSession(baseSession(banSessID, banRoomID, banOwner, model.SessionStateLiving))
	boomRow := liveRoomRow(t, st)
	// 「读到房间之后、迁移之前别的入口推走了版本」：CAS 未命中，迁移返回 false。
	st.raceBefore(fmt.Sprintf("live_room.TransitionTx:%d:%d->%d",
		banRoomID, model.RoomStateLiving, model.RoomStateBanned), func() {
		boomRow.StateVersion = 7
	})

	reply, err := newBanLogic(t, st).BanRoom(banReq("req-ban-cas", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantErrIs(t, "房间 CAS 未命中", err, model.ErrConcurrentUpdate)
	if reply != nil {
		t.Fatalf("CAS 未命中应答 = %+v, want nil", reply)
	}
	defer st.checkRaces(t)

	// 序列在迁移那条就断掉：之后的审计与 SaveResult 都没执行。
	wantSeq(t, "房间 CAS 未命中", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:req-ban-cas",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		fmt.Sprintf("live_session.FindActiveByRoom:%d", banRoomID),
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			banSessID, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonBanned),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeSessionState, model.SessionStateLiving, model.SessionStateTerminated, banReason),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateLiving, model.RoomStateBanned, int32(1)),
	)
	wantTxCount(t, "房间 CAS 未命中", st.conn, 1)

	// 假件不回滚，所以残留形态是可观察事实：场次已被终止、禁播记录已入库，
	// 但房间既没进 BANNED 也没写 ban_until —— 真实 MySQL 上这个事务会整体回滚。
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "CAS 未命中的房间", "state", row.State, model.RoomStateLiving)
	wantEQ(t, "CAS 未命中的房间", "ban_until", row.BanUntil, int64(0))
	wantEQ(t, "CAS 未命中的房间", "state_version", row.StateVersion, int32(7))
	wantEQ(t, "CAS 未命中的残留场次", "state", st.sessionAt(t, banSessID).State, model.SessionStateTerminated)
	wantEQ(t, "CAS 未命中的残留禁播行", "bans", st.counts().bans, 1)
	wantKeyBurnedNoResult(t, "CAS 未命中", "req-ban-cas", st)
}

func TestBanRoomSessionCASMissAbortsBeforeRoomTransition(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStateLiving))
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	st.seedSession(baseSession(banSessID, banRoomID, banOwner, model.SessionStateLiving))
	// 迁移前主播自己正常下播了：场次已是 ENDED，禁播的 TERMINATED 迁移命中 0 行。
	st.raceBefore("live_session.TransitionTx", func() {
		row := st.sessions.rows[0]
		row.State = model.SessionStateEnded
		row.EndReason = model.EndReasonAnchorStop
		row.EndedAt = banNow - 1
	})

	_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-sess", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantErrIs(t, "场次 CAS 未命中", err, model.ErrConcurrentUpdate)
	defer st.checkRaces(t)

	wantSeq(t, "场次 CAS 未命中", st.log, 5,
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_session.TransitionTx:%d:%d->%d/r%d",
			banSessID, model.SessionStateLiving, model.SessionStateTerminated, model.EndReasonBanned),
	)
	wantMethodCount(t, "场次 CAS 未命中", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "场次 CAS 未命中", st.log, "live_room_state_log.InsertTx", 0)
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "场次 CAS 未命中的房间", "state", row.State, model.RoomStateLiving)
	wantEQ(t, "场次 CAS 未命中的房间", "ban_until", row.BanUntil, int64(0))
	// 但新禁播记录留下了（假件不回滚），且键烧了没结果。
	wantEQ(t, "场次 CAS 未命中的残留禁播行", "bans", st.counts().bans, 1)
	wantKeyBurnedNoResult(t, "场次 CAS 未命中", "req-ban-sess", st)
}

func TestBanRoomPreviousBanAlreadyLiftedConcurrentlyAborts(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	// 读到生效记录之后，别的入口把它解了 → 替换写的 LiftTx 命中 0 行。
	st.raceBefore("live_room_ban.LiftTx", func() {
		for _, b := range st.bans.rows {
			if b.BanID == banPrevID {
				b.State = model.BanStateLifted
				b.LiftOperatorMid = banLiftOper
				b.LiftReason = "别人先解的"
				b.LiftedAt = banNow
			}
		}
	})

	_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-racelift", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantErrIs(t, "旧记录被并发解除", err, model.ErrConcurrentUpdate)
	defer st.checkRaces(t)

	wantSeq(t, "旧记录被并发解除", st.log, 5,
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
	)
	wantMethodCount(t, "旧记录被并发解除", st.log, "live_room_ban.InsertTx", 0)
	wantEQ(t, "旧记录被并发解除后的行数", "bans", st.counts().bans, 2)
	wantEQ(t, "旧记录被并发解除", "lift_reason", st.banAt(t, banPrevID).LiftReason, "别人先解的")
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "旧记录被并发解除的房间", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "旧记录被并发解除的房间", "ban_until", row.BanUntil, int64(0))
	wantKeyBurnedNoResult(t, "旧记录被并发解除", "req-ban-racelift", st)
}

func TestBanRoomInsertFailureLeavesPreviousBanLifted(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	boom := errors.New("boom: live_room_ban insert 被打断")
	st.bans.failWith("InsertTx", boom)

	_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-insert", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantErrIs(t, "新记录写入失败", err, boom)
	wantErrContains(t, "新记录写入失败", err, "live_room_ban Insert")

	wantSeq(t, "新记录写入失败", st.log, 5,
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		"live_room_ban.InsertTx",
	)
	// 最坏的残留形态：旧记录已被判「已解除」，新记录没进来 —— 此刻该房间没有任何生效禁播，
	// 而状态仍是 BANNED。真实 MySQL 由事务回滚兜住，本用例只锁「假件看到什么」。
	wantEQ(t, "写入失败后的行数", "bans", st.counts().bans, 2)
	prev := st.banAt(t, banPrevID)
	wantEQ(t, "写入失败后的旧记录", "state", prev.State, model.BanStateLifted)
	wantEQ(t, "写入失败后的旧记录", "lift_reason", prev.LiftReason, "replaced by a new ban")
	wantMethodCount(t, "写入失败", st.log, "live_room.SetBanUntilTx", 0)
	wantMethodCount(t, "写入失败", st.log, "live_room_state_log.InsertTx", 0)
	wantKeyBurnedNoResult(t, "写入失败", "req-ban-insert", st)
	wantTxCount(t, "写入失败", st.conn, 1)
}

func TestBanRoomReadFailuresStopsBeforeClaim(t *testing.T) {
	boom := errors.New("boom: 读不到")
	t.Run("房间读失败", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.rooms.failWith("FindOne", boom)
		before := st.log.snapshot()
		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-r1", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantErrIs(t, "房间读失败", err, boom)
		wantNoCallAfter(t, "房间读失败", st.log, before+1)
		wantSeq(t, "房间读失败", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", banRoomID))
		wantKeyUnburned(t, "房间读失败", "req-ban-r1", st)
	})

	t.Run("房主读失败", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
		st.anchors.failWith("FindOwner", boom)
		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-r2", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantErrIs(t, "房主读失败", err, boom)
		wantSeq(t, "房主读失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		)
		wantKeyUnburned(t, "房主读失败", "req-ban-r2", st)
		wantMethodCount(t, "房主读失败", st.log, "live_room_ban.FindActiveByRoom", 0)
	})

	t.Run("抢键失败", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
		st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
		st.idem.failWith("Claim", boom)
		_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-r3", rpc.BanType_BAN_TYPE_PERMANENT, 0))
		wantErrIs(t, "抢键失败", err, boom)
		wantSeq(t, "抢键失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
			"live_room_idempotency.Claim:req-ban-r3",
		)
		wantEQ(t, "抢键失败后的行数", "bans", st.counts().bans, 0)
		wantTxCount(t, "抢键失败", st.conn, 0)
	})
}

func TestBanRoomAuditFailureAbortsAfterRoomAlreadyMoved(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	boom := errors.New("boom: 审计写不下")
	st.stateLogs.failWith("InsertTx", boom)

	_, err := newBanLogic(t, st).BanRoom(banReq("req-ban-audit", rpc.BanType_BAN_TYPE_TEMPORARY, 600))
	wantErrIs(t, "审计失败", err, boom)

	wantSeq(t, "审计失败", st.log, 5,
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStatePending, model.RoomStateBanned, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStatePending, model.RoomStateBanned, banReason),
	)
	// 审计行没落库，房间已经进了 BANNED（假件不回滚）；SaveResult 也没跑。
	wantEQ(t, "审计失败的日志行数", "logs", st.counts().logs, 0)
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "审计失败的房间", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "审计失败的房间", "ban_until", row.BanUntil, banNow+600)
	wantKeyBurnedNoResult(t, "审计失败", "req-ban-audit", st)
}

// --- LiftBan ---

func TestLiftBanPermanentBanOnPassedRoomReturnsToReady(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	lg := newLiftLogic(t, st)

	reply, err := lg.LiftBan(liftReq("req-lift-ok"))
	wantNoErr(t, "解除永久禁播", err)
	defer st.checkRaces(t)

	wantSeq(t, "解除永久禁播首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-ok",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady, banLiftReason),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-ok",
	)
	wantTxCount(t, "解除永久禁播", st.conn, 1)
	wantEQ(t, "解除应答", "ban_id", reply.GetBanId(), banPrevID)
	wantEQ(t, "解除应答", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "解除应答", "message", reply.GetMessage(), "禁播已解除")
	wantEQ(t, "解除应答", "replayed", reply.GetReplayed(), false)

	// 解除三列写的是**真值**，且 lifted_at 由 NowUnix() 给（与 start_at 无关）。
	ban := st.banAt(t, banPrevID)
	wantEQ(t, "被解除记录", "state", ban.State, model.BanStateLifted)
	wantEQ(t, "被解除记录", "lift_operator_mid", ban.LiftOperatorMid, banLiftOper)
	wantEQ(t, "被解除记录", "lift_reason", ban.LiftReason, banLiftReason)
	wantEQ(t, "被解除记录", "lifted_at", ban.LiftedAt, banNow)
	// 处置事实不被解除覆写：mid/类型/原因/起止/操作者/ctime 全部保持原值。
	wantEQ(t, "被解除记录", "mid", ban.Mid, banOwner)
	wantEQ(t, "被解除记录", "ban_type", ban.BanType, model.BanTypePermanent)
	wantEQ(t, "被解除记录", "reason", ban.Reason, "违规内容")
	wantEQ(t, "被解除记录", "end_at", ban.EndAt, int64(0))
	wantEQ(t, "被解除记录", "operator_mid", ban.OperatorMid, 555)
	wantEQ(t, "被解除记录", "ctime", ban.Ctime, 5000+banPrevID)

	row := st.roomAt(t, banRoomID)
	wantEQ(t, "解除后的房间", "state", row.State, model.RoomStateReady)
	wantEQ(t, "解除后的房间", "state_version", row.StateVersion, int32(2))
	// 与 BanRoom 配对：RoomPatch{BanUntil: 0} 把投影清回 0。
	wantEQ(t, "解除后的房间", "ban_until", row.BanUntil, int64(0))
	wantEQ(t, "解除后的房间", "ctime", row.Ctime, 1000+banRoomID)

	logs := st.logsOf(banRoomID)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "解除审计", "source", logs[0].Source, model.SourceRPCAdmin)
	wantEQ(t, "解除审计", "operator_mid", logs[0].OperatorMid, banLiftOper)
	wantEQ(t, "解除审计", "request_id", logs[0].RequestID, "req-lift-ok")
	wantEQ(t, "解除审计", "session_id", logs[0].SessionID, int64(0))
	wantEQ(t, "解除审计", "reason", logs[0].Reason, banLiftReason)

	// LiftBan 既不做归属判定，也不动场次/配置。
	wantMethodCount(t, "解除禁播", st.log, "live_room_anchor.FindOwner", 0)
	wantMethodCount(t, "解除禁播", st.log, "live_room_anchor.IsEnabled", 0)
	wantMethodCount(t, "解除禁播", st.log, "live_session.TransitionTx", 0)
	wantMethodCount(t, "解除禁播", st.log, "live_room.SetBanUntilTx", 0)
	// 已解除的历史记录不该被再解一次。
	wantEQ(t, "禁播表行数不变", "bans", st.counts().bans, 2)
}

func TestLiftBanTargetStateFollowsVerifyState(t *testing.T) {
	cases := []struct {
		name        string
		verifyState int32
		wantState   int32
		wantRPC     rpc.RoomState
	}{
		{"审核未开始时回待完善", model.VerifyStateNone, model.RoomStatePending, rpc.RoomState_ROOM_STATE_PENDING},
		{"审核中回待完善", model.VerifyStateReviewing, model.RoomStatePending, rpc.RoomState_ROOM_STATE_PENDING},
		{"审核被驳回回待完善", model.VerifyStateRejected, model.RoomStatePending, rpc.RoomState_ROOM_STATE_PENDING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, banNow)
			st := newStore()
			seedBannedRoom(t, st, tc.verifyState)

			reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-" + tc.name))
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "state", reply.GetState(), tc.wantRPC)
			wantSeq(t, tc.name, st.log, 0,
				fmt.Sprintf("live_room.FindOne:%d", banRoomID),
				fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
				"live_room_idempotency.Claim:req-lift-"+tc.name,
				"db.TransactCtx",
				fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
				fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
					banRoomID, model.RoomStateBanned, tc.wantState, int32(1)),
				fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
					model.LogTypeRoomState, model.RoomStateBanned, tc.wantState, banLiftReason),
				fmt.Sprintf("live_room.FindOne:%d", banRoomID),
				"live_room_idempotency.SaveResult:req-lift-"+tc.name,
			)
			wantEQ(t, tc.name, "verify_state 不被解除改动",
				st.roomAt(t, banRoomID).VerifyState, tc.verifyState)
		})
	}
}

func TestLiftBanOnNonBannedRoomOnlyClearsBanUntilProjection(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	r := baseRoom(banRoomID, banOwner, model.RoomStateReady)
	r.BanUntil = banNow + 600
	st.seedRoom(r)
	st.seedBan(activePermanentBan(banPrevID, banRoomID, banOwner))

	reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-nonban"))
	wantNoErr(t, "非 BANNED 房间解除", err)

	wantSeq(t, "非 BANNED 房间解除", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-nonban",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.SetBanUntilTx:%d:%d", banRoomID, int64(0)),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-nonban",
	)
	wantEQ(t, "非 BANNED 房间解除", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "非 BANNED 房间解除", "message", reply.GetMessage(), "禁播已解除")
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "非 BANNED 房间", "state", row.State, model.RoomStateReady)
	wantEQ(t, "非 BANNED 房间", "ban_until", row.BanUntil, int64(0))
	// SetBanUntilTx 不是状态迁移：版本不推进，也不写审计。
	wantEQ(t, "非 BANNED 房间", "state_version", row.StateVersion, int32(1))
	wantMethodCount(t, "非 BANNED 房间解除", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "非 BANNED 房间解除", st.log, "live_room_state_log.InsertTx", 0)
}

func TestLiftBanWithNothingToClearWritesOnlyTheRecord(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	// 房间 READY、ban_until 本来就是 0，但确实留着一条按主播下发的生效记录。
	st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStateReady))
	st.seedBan(activePermanentBan(banPrevID, banRoomID, banOwner))

	reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-nopatch"))
	wantNoErr(t, "无投影可清", err)
	wantSeq(t, "无投影可清", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-nopatch",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-nopatch",
	)
	wantEQ(t, "无投影可清", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_READY)
	wantEQ(t, "无投影可清", "ban_id", reply.GetBanId(), banPrevID)
	wantMethodCount(t, "无投影可清", st.log, "live_room.SetBanUntilTx", 0)
	wantMethodCount(t, "无投影可清", st.log, "live_room.TransitionTx", 0)
	wantEQ(t, "无投影可清后的房间", "state", st.roomAt(t, banRoomID).State, model.RoomStateReady)
	wantEQ(t, "被解除记录", "state", st.banAt(t, banPrevID).State, model.BanStateLifted)
}

func TestLiftBanWithoutActiveBanAnswersZeroAndBurnsNoKey(t *testing.T) {
	t.Run("库里一条记录都没有", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		st.seedRoom(baseRoom(banRoomID, banOwner, model.RoomStatePending))

		reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-none"))
		wantNoErr(t, "无生效记录", err)
		wantSeq(t, "无生效记录", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		)
		wantEQ(t, "无生效记录", "ban_id", reply.GetBanId(), int64(0))
		wantEQ(t, "无生效记录", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_PENDING)
		wantEQ(t, "无生效记录", "message", reply.GetMessage(), "该房间当前没有生效禁播记录")
		wantEQ(t, "无生效记录", "replayed", reply.GetReplayed(), false)
		wantKeyUnburned(t, "无生效记录", "req-lift-none", st)
		wantTxCount(t, "无生效记录", st.conn, 0)
		wantNoDirectSQL(t, "无生效记录", st.conn)
		wantEQ(t, "无生效记录", "logs", st.counts().logs, 0)
	})

	t.Run("临时禁播已到期不算生效", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		r := baseRoom(banRoomID, banOwner, model.RoomStateBanned)
		r.BanUntil = banNow - 10
		st.seedRoom(r)
		st.seedBan(temporaryBan(banPrevID, banRoomID, banOwner, banNow-100, banNow-10, model.BanStateActive))

		reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-expired"))
		wantNoErr(t, "到期记录", err)
		// 键里带 now：SQL 的 `end_at = 0 OR end_at > ?` 用的就是同一个时刻，与 ban_until 判定同源。
		wantSeq(t, "到期记录", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		)
		wantEQ(t, "到期记录", "ban_id", reply.GetBanId(), int64(0))
		wantEQ(t, "到期记录", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_BANNED)
		// 房间还停在 BANNED、到期记录还停在 Active：到期放行是扫描任务的事，LiftBan 不代劳。
		wantEQ(t, "到期记录", "记录状态", st.banAt(t, banPrevID).State, model.BanStateActive)
		wantEQ(t, "到期记录", "房间状态", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
		wantKeyUnburned(t, "到期记录", "req-lift-expired", st)
	})
}

func TestLiftBanAlreadyIneffectiveRecordAnswersNoticeWithoutClaim(t *testing.T) {
	cases := []struct {
		name  string
		state int32
	}{
		{"已解除", model.BanStateLifted},
		{"已过期", model.BanStateExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, banNow)
			st := newStore()
			r := baseRoom(banRoomID, banOwner, model.RoomStateBanned)
			r.BanUntil = banNow + 600
			st.seedRoom(r)
			b := baseBan(banPrevID, banRoomID, tc.state)
			b.BanType = model.BanTypeTemporary
			b.StartAt = banNow - 600
			b.EndAt = banNow + 600
			if tc.state == model.BanStateLifted {
				b.LiftOperatorMid = banLiftOper
				b.LiftReason = "上一次就解了"
				b.LiftedAt = banNow - 60
			}
			st.seedBan(b)

			in := liftReq("req-lift-inactive")
			in.BanId = banPrevID
			reply, err := newLiftLogic(t, st).LiftBan(in)
			wantNoErr(t, tc.name, err)
			wantSeq(t, tc.name, st.log, 0,
				fmt.Sprintf("live_room.FindOne:%d", banRoomID),
				fmt.Sprintf("live_room_ban.FindOne:%d", banPrevID),
			)
			wantEQ(t, tc.name, "ban_id", reply.GetBanId(), banPrevID)
			wantEQ(t, tc.name, "state", reply.GetState(), rpc.RoomState_ROOM_STATE_BANNED)
			wantEQ(t, tc.name, "message", reply.GetMessage(),
				fmt.Sprintf("禁播记录已不生效（state=%d）", tc.state))
			wantEQ(t, tc.name, "replayed", reply.GetReplayed(), false)
			wantKeyUnburned(t, tc.name, "req-lift-inactive", st)
			wantTxCount(t, tc.name, st.conn, 0)
			wantMethodCount(t, tc.name, st.log, "live_room_ban.LiftTx", 0)
			// 房间仍停在 BANNED：不生效记录不驱动任何状态推进。
			wantEQ(t, tc.name, "房间状态", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
		})
	}
}

func TestLiftBanExplicitBanIDCrossRoomAndMissingRejected(t *testing.T) {
	t.Run("记录不存在", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		in := liftReq("req-lift-missing")
		in.BanId = 9999

		reply, err := newLiftLogic(t, st).LiftBan(in)
		wantErrIs(t, "记录不存在", err, model.ErrBanNotFound)
		wantErrContains(t, "记录不存在", err, "ban_id=9999")
		if reply != nil {
			t.Fatalf("记录不存在应答 = %+v, want nil", reply)
		}
		wantSeq(t, "记录不存在", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			"live_room_ban.FindOne:9999",
		)
		wantKeyUnburned(t, "记录不存在", "req-lift-missing", st)
	})

	t.Run("记录属于别的房间", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		st.seedRoom(baseRoom(banOtherRm, banOwner+1, model.RoomStateBanned))
		st.seedBan(activePermanentBan(banPrevID+50, banOtherRm, banOwner+1))
		in := liftReq("req-lift-cross")
		in.BanId = banPrevID + 50

		_, err := newLiftLogic(t, st).LiftBan(in)
		wantErrIs(t, "跨房间", err, model.ErrBanNotFound)
		wantErrContains(t, "跨房间", err,
			fmt.Sprintf("ban_id=%d 属于房间 %d，不属于 room_id=%d", banPrevID+50, banOtherRm, banRoomID))
		wantSeq(t, "跨房间", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_ban.FindOne:%d", banPrevID+50),
		)
		// 越界尝试没碰到任何一间的写。
		wantEQ(t, "跨房间后的房间", "state", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
		wantEQ(t, "跨房间后的别间", "state", st.roomAt(t, banOtherRm).State, model.RoomStateBanned)
		wantEQ(t, "跨房间后的记录", "state", st.banAt(t, banPrevID+50).State, model.BanStateActive)
		wantKeyUnburned(t, "跨房间", "req-lift-cross", st)
	})

	t.Run("ban_id 负数按缺省处理", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		in := liftReq("req-lift-neg")
		in.BanId = -7

		reply, err := newLiftLogic(t, st).LiftBan(in)
		wantNoErr(t, "负 ban_id", err)
		// resolveBan 的条件是 `ban_id <= 0`，负数走「解除当前生效记录」而不是报错。
		wantSeq(t, "负 ban_id", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
			"live_room_idempotency.Claim:req-lift-neg",
			"db.TransactCtx",
			fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
			fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
				banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
			fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
				model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady, banLiftReason),
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			"live_room_idempotency.SaveResult:req-lift-neg",
		)
		wantEQ(t, "负 ban_id", "ban_id", reply.GetBanId(), banPrevID)
	})
}

func TestLiftBanBlankReasonWritesEmptyLiftReason(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	in := liftReq("req-lift-blank")
	in.Reason = "   "

	reply, err := newLiftLogic(t, st).LiftBan(in)
	wantNoErr(t, "原因可缺省", err)
	wantSeq(t, "原因可缺省", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-blank",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
		// 审计键尾部为空串：reason 被 TrimSpace 成 "" 后原样写进审计与 lift_reason。
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-blank",
	)
	ban := st.banAt(t, banPrevID)
	wantEQ(t, "缺省原因的解除记录", "lift_reason", ban.LiftReason, "")
	wantEQ(t, "缺省原因的解除记录", "state", ban.State, model.BanStateLifted)
	wantEQ(t, "缺省原因的解除记录", "lift_operator_mid", ban.LiftOperatorMid, banLiftOper)
	logs := st.logsOf(banRoomID)
	if len(logs) != 1 {
		t.Fatalf("审计行数 = %d, want 1", len(logs))
	}
	wantEQ(t, "缺省原因的审计", "reason", logs[0].Reason, "")
	wantEQ(t, "缺省原因的应答", "message", reply.GetMessage(), "禁播已解除")
}

func TestLiftBanGuardTableRejectsWithZeroDependencyCalls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.LiftBanReq)
		want   error
	}{
		{"缺运营者", func(in *rpc.LiftBanReq) { in.OperatorMid = 0 }, model.ErrOperatorRequired},
		{"缺房间号", func(in *rpc.LiftBanReq) { in.RoomId = -1 }, model.ErrInvalidRoomID},
		{"幂等键空", func(in *rpc.LiftBanReq) { in.RequestId = "" }, model.ErrRequestIDRequired},
		{"幂等键纯空白", func(in *rpc.LiftBanReq) { in.RequestId = "  " }, model.ErrRequestIDRequired},
		{"原因超长", func(in *rpc.LiftBanReq) {
			in.Reason = strings.Repeat("诉", maxReasonRunes+1)
		}, model.ErrReasonTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, banNow)
			st := newStore()
			seedBannedRoom(t, st, model.VerifyStatePassed)
			in := liftReq("req-lift-guard")
			tc.mutate(in)

			before := st.log.snapshot()
			reply, err := newLiftLogic(t, st).LiftBan(in)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Fatalf("%s：应答 = %+v, want nil", tc.name, reply)
			}
			wantNoCallAfter(t, tc.name, st.log, before)
			wantKeyUnburned(t, tc.name, "req-lift-guard", st)
			wantTxCount(t, tc.name, st.conn, 0)
			wantEQ(t, tc.name+"：房间未动", "state", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
			wantEQ(t, tc.name+"：记录未动", "state", st.banAt(t, banPrevID).State, model.BanStateActive)
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		lg := newLiftLogic(t, st)
		before := st.log.snapshot()
		_, err := lg.LiftBan(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, before)
	})

	t.Run("原因缺省不属守卫拒绝", func(t *testing.T) {
		// 与 BanRoom 相反：LiftBan 的 reason 是可选的，空值必须继续往下走。
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		in := liftReq("req-lift-opt")
		in.Reason = ""
		_, err := newLiftLogic(t, st).LiftBan(in)
		wantNoErr(t, "可选原因", err)
		wantMethodCount(t, "可选原因", st.log, "live_room_ban.LiftTx", 1)
	})

	t.Run("房间不存在时先报错再查记录", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		before := st.log.snapshot()
		_, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-noroom"))
		wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
		wantNoCallAfter(t, "房间不存在", st.log, before+1)
		wantKeyUnburned(t, "房间不存在", "req-lift-noroom", st)
	})
}

func TestLiftBanConcurrentLiftAnswersReplayedWithoutSecondWrite(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	// 两条并发 LiftBan 里落后的一条：Lift 的 UPDATE 命中 0 行。
	st.raceBefore("live_room_ban.LiftTx", func() {
		for _, b := range st.bans.rows {
			if b.BanID == banPrevID {
				b.State = model.BanStateLifted
				b.LiftOperatorMid = banLiftOper + 1
				b.LiftReason = "并发先解的"
				b.LiftedAt = banNow
			}
		}
	})

	reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-race"))
	wantNoErr(t, "并发解除", err)
	defer st.checkRaces(t)

	wantSeq(t, "并发解除", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-race",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-race",
	)
	wantEQ(t, "并发解除", "ban_id", reply.GetBanId(), banPrevID)
	wantEQ(t, "并发解除", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_BANNED)
	wantEQ(t, "并发解除", "replayed", reply.GetReplayed(), true)
	wantEQ(t, "并发解除", "message", reply.GetMessage(), "记录已被并发解除，本次未重复写入")
	// 两次解除不是两次成功：房间一点没动（errNoOp 在迁移动作之前把事务收住）。
	row := st.roomAt(t, banRoomID)
	wantEQ(t, "并发解除的房间", "state", row.State, model.RoomStateBanned)
	wantEQ(t, "并发解除的房间", "state_version", row.StateVersion, int32(1))
	wantMethodCount(t, "并发解除", st.log, "live_room.TransitionTx", 0)
	wantMethodCount(t, "并发解除", st.log, "live_room_state_log.InsertTx", 0)
	wantEQ(t, "并发解除的记录", "lift_reason", st.banAt(t, banPrevID).LiftReason, "并发先解的")
	// 与写入失败用例相反：这条路径走完了，所以键是有结果的。
	if rec := st.idemAt("req-lift-race"); rec == nil || rec.ResultJSON == "" {
		t.Fatalf("并发解除：键应已回填结果，实际 %+v", rec)
	}
}

func TestLiftBanRoomCASMissLeavesLiftedRecordAndUnmovedRoom(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	row := liveRoomRow(t, st)
	st.raceBefore(fmt.Sprintf("live_room.TransitionTx:%d:%d->%d",
		banRoomID, model.RoomStateBanned, model.RoomStateReady), func() {
		row.StateVersion = 42
	})

	_, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-cas"))
	wantErrIs(t, "解除时房间 CAS 未命中", err, model.ErrConcurrentUpdate)
	defer st.checkRaces(t)

	wantSeq(t, "解除时房间 CAS 未命中", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-cas",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
	)
	wantTxCount(t, "解除时房间 CAS 未命中", st.conn, 1)
	// 假件不回滚：记录已判「已解除」而房间还在 BANNED —— 真实 MySQL 由事务回滚兜住。
	wantEQ(t, "CAS 未命中的记录", "state", st.banAt(t, banPrevID).State, model.BanStateLifted)
	wantEQ(t, "CAS 未命中的房间", "state", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
	wantMethodCount(t, "CAS 未命中", st.log, "live_room_state_log.InsertTx", 0)
	wantKeyBurnedNoResult(t, "解除时房间 CAS 未命中", "req-lift-cas", st)
}

func TestLiftBanReportsRealStateWhenRoomMovedUnderneath(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	// 钩子挂在审计那条写之前：等价于「迁移提交后、回读前，别的入口把房间关了」。
	st.raceBefore("live_room_state_log.InsertTx", func() {
		row := liveRoomRow(t, st)
		row.State = model.RoomStateFinished
		row.StateVersion++
		row.Mtime = banNow
	})

	reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-moved"))
	wantNoErr(t, "状态被并发推进", err)
	defer st.checkRaces(t)

	wantSeq(t, "状态被并发推进", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-moved",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady, banLiftReason),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:req-lift-moved",
	)
	// 应答回的是**读回来的**状态，不声称迁到了 target。
	wantEQ(t, "状态被并发推进", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_FINISHED)
	wantEQ(t, "状态被并发推进", "message", reply.GetMessage(), "房间状态已被并发推进，返回当前真实状态")
	wantEQ(t, "状态被并发推进", "replayed", reply.GetReplayed(), false)
	wantEQ(t, "状态被并发推进", "ban_id", reply.GetBanId(), banPrevID)
}

func TestLiftBanPostCommitReadFailureReportsErrorWithCommittedWrites(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	seedBannedRoom(t, st, model.VerifyStatePassed)
	boom := errors.New("boom: 回读不到")
	// 钩子挂在解除那条写之前：之后房间的回读全部失败（第二次 FindOne 才吃到它）。
	st.raceBefore("live_room_ban.LiftTx", func() { st.rooms.failWith("FindOne", boom) })

	reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-reread"))
	wantErrIs(t, "提交后回读失败", err, boom)
	if reply != nil {
		t.Fatalf("提交后回读失败应答 = %+v, want nil", reply)
	}
	defer st.checkRaces(t)

	wantSeq(t, "提交后回读失败", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:req-lift-reread",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", banPrevID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady, banLiftReason),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
	)
	// 写全部落地了，但调用方拿到错误且键没有结果 → 重试会撞上「烧过的键」。
	wantEQ(t, "提交后回读失败的房间", "state", st.roomAt(t, banRoomID).State, model.RoomStateReady)
	wantEQ(t, "提交后回读失败的记录", "state", st.banAt(t, banPrevID).State, model.BanStateLifted)
	wantKeyBurnedNoResult(t, "提交后回读失败", "req-lift-reread", st)
	wantMethodCount(t, "提交后回读失败", st.log, "live_room_idempotency.SaveResult", 0)
}

func TestLiftBanReplayIdempotencyTriad(t *testing.T) {
	t.Run("命中重放回存过的说明", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		st.seedIdem("req-lift-replay", "LiftBan",
			fmt.Sprintf(`{"ban_id":%d,"state":2,"replayed":true,"message":"禁播已解除"}`, banPrevID))

		reply, err := newLiftLogic(t, st).LiftBan(liftReq("req-lift-replay"))
		wantNoErr(t, "解除重放", err)
		wantSeq(t, "解除重放", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", banRoomID),
			fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
			"live_room_idempotency.Claim:req-lift-replay",
			"live_room_idempotency.Find:req-lift-replay",
		)
		wantEQ(t, "解除重放", "replayed", reply.GetReplayed(), true)
		wantEQ(t, "解除重放", "ban_id", reply.GetBanId(), banPrevID)
		wantEQ(t, "解除重放", "state", reply.GetState(), rpc.RoomState_ROOM_STATE_READY)
		wantEQ(t, "解除重放", "message", reply.GetMessage(), "禁播已解除")
		wantMethodCount(t, "解除重放", st.log, "live_room_ban.LiftTx", 0)
		wantMethodCount(t, "解除重放", st.log, "live_room.TransitionTx", 0)
		wantTxCount(t, "解除重放", st.conn, 0)
		wantEQ(t, "解除重放后房间", "state", st.roomAt(t, banRoomID).State, model.RoomStateBanned)
		wantSeq(t, "解除重放之后不得再有任何调用", st.log, 4)
	})

	t.Run("键被 BanRoom 用过", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		st.seedIdem("shared-lift", "BanRoom", `{"ban_id":7}`)

		_, err := newLiftLogic(t, st).LiftBan(liftReq("shared-lift"))
		wantErrIs(t, "解除键被别人用过", err, model.ErrRequestIDReused)
		wantErrContains(t, "解除键被别人用过", err, "used by BanRoom")
		wantMethodCount(t, "解除键被别人用过", st.log, "live_room_ban.LiftTx", 0)
	})

	t.Run("烧过的键没有结果", func(t *testing.T) {
		fixClock(t, banNow)
		st := newStore()
		seedBannedRoom(t, st, model.VerifyStatePassed)
		st.seedIdem("burned-lift", "LiftBan", "")

		reply, err := newLiftLogic(t, st).LiftBan(liftReq("burned-lift"))
		wantErrIs(t, "解除烧过的键", err, model.ErrIdempotencyResultMissing)
		if reply != nil {
			t.Fatalf("解除烧过的键应答 = %+v, want nil", reply)
		}
		wantMethodCount(t, "解除烧过的键", st.log, "live_room_ban.LiftTx", 0)
	})
}

// --- 成对往返 ---

func TestBanAndLiftPairRoundTrip(t *testing.T) {
	fixClock(t, banNow)
	st := newStore()
	r := baseRoom(banRoomID, banOwner, model.RoomStateReady)
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(801, banRoomID, banOwner, model.AnchorRoleOwner))
	_, err := newBanLogic(t, st).BanRoom(banReq("pair-ban", rpc.BanType_BAN_TYPE_TEMPORARY, 1800))
	wantNoErr(t, "成对：先禁播", err)

	banned := st.roomAt(t, banRoomID)
	wantEQ(t, "成对：禁播后", "state", banned.State, model.RoomStateBanned)
	wantEQ(t, "成对：禁播后", "state_version", banned.StateVersion, int32(2))
	wantEQ(t, "成对：禁播后", "ban_until", banned.BanUntil, banNow+1800)
	newID := st.bans.rows[len(st.bans.rows)-1].BanID

	// 解除时不指定 ban_id：解析出来的必须正是刚才那条，而不是任何历史行。
	reply, err := newLiftLogic(t, st).LiftBan(liftReq("pair-lift"))
	wantNoErr(t, "成对：再解除", err)
	wantEQ(t, "成对：解除目标", "ban_id", reply.GetBanId(), newID)
	wantEQ(t, "成对：解除目标记录", "state", st.banAt(t, newID).State, model.BanStateLifted)
	wantEQ(t, "成对：解除目标记录", "lift_reason", st.banAt(t, newID).LiftReason, banLiftReason)

	after := st.roomAt(t, banRoomID)
	wantEQ(t, "成对：解除后", "state", after.State, model.RoomStateReady)
	wantEQ(t, "成对：解除后", "ban_until", after.BanUntil, int64(0))
	// 两次写各自推了一格版本（合计 +2），中间没被谁偷偷重置。
	wantEQ(t, "成对：版本推进", "state_version", after.StateVersion, int32(3))

	wantSeq(t, "成对往返全程", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", banRoomID),
		"live_room_idempotency.Claim:pair-ban",
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		fmt.Sprintf("live_session.FindActiveByRoom:%d", banRoomID),
		"db.TransactCtx",
		"live_room_ban.InsertTx",
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateReady, model.RoomStateBanned, int32(1)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateReady, model.RoomStateBanned, banReason),
		"live_room_idempotency.SaveResult:pair-ban",
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		fmt.Sprintf("live_room_ban.FindActiveByRoom:%d@%d", banRoomID, banNow),
		"live_room_idempotency.Claim:pair-lift",
		"db.TransactCtx",
		fmt.Sprintf("live_room_ban.LiftTx:%d", newID),
		fmt.Sprintf("live_room.TransitionTx:%d:%d->%d/v%d",
			banRoomID, model.RoomStateBanned, model.RoomStateReady, int32(2)),
		fmt.Sprintf("live_room_state_log.InsertTx:t%d:%d->%d:%s",
			model.LogTypeRoomState, model.RoomStateBanned, model.RoomStateReady, banLiftReason),
		fmt.Sprintf("live_room.FindOne:%d", banRoomID),
		"live_room_idempotency.SaveResult:pair-lift",
	)
	wantTxCount(t, "成对往返", st.conn, 2)
	wantEQ(t, "成对往返", "审计行数", len(st.logsOf(banRoomID)), 2)
}
