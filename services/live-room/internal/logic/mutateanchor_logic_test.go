package logic

// mutateanchor_logic_test.go 覆盖写侧方法 MutateAnchor（mutateanchorlogic.go）。
//
// 锁的结论（全部由 mutateanchorlogic.go 的实现反推，不是理想设计）：
//  1. **归属校验只有「生效房主」一条路**：`:67-73` 读 FindOwner 并要求 operator_mid 等于房主，
//     契约里没有 admin 位（对比 CloseRoom 的 `admin=true` 会整条跳过 FindOwner），
//     所以运营侧陌生操作者在这里同样被拒。这道校验在**抢幂等键之前**（:71 vs :81）。
//  2. 角色归一（anchorRoleForAction）在两次读之后、抢键之前（:74）：
//     BIND 必须给具体角色且拒绝 OWNER，UNBIND 允许 0（全部非房主）但拒绝 OWNER。
//  3. **本方法没有任何事务**：绑定/解绑与幂等键的两条语句是各自独立的写，
//     `db.TransactCtx` 次数恒为 0；也不写 live_room_state_log（成员变更在审计矩阵里
//     没有 state_type，见 model.LogType*，只能靠 anchor 行的 state/mtime 复原）。
//  4. claim 早于 guardLivingAnchor 与 checkBindQuota（:81 vs :101/:109）：
//     「正在开播不可解绑」「配额已满」这类业务拒绝会把 request_id 消费掉且永不回填结果
//     ——与 README 已知缺口第 14 条同族，本文件用哨兵用例钉住（TODO(缺陷)）。
//  5. `bound_count` 回的是**按角色统计**的生效房间数（:120 传的是归一后的 role），
//     与配额里的 `CountActiveRoomsByMid(mid, Unspecified)`（不分角色、DISTINCT room）不是同一个数。
//  6. UNBIND 的 RowsAffected 被丢弃（:104 `_`）：解绑一个根本没绑过的 mid 也是成功，
//     应答 state=0、bound_count 照算，不区分「解了几行」。
//  7. 房间只有 FINISHED 被拒（:63），BANNED / DISABLED 照样能改成员——与 CloseRoom 的
//     「非终态都能关」同口径，用例用判别对照钉住「只有 FINISHED 算终态」。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-room/internal/config"
	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

const (
	maNow      int64 = 1_700_000_300
	maRoom     int64 = 3301
	maOwner    int64 = 9301
	maTarget   int64 = 9302
	maStranger int64 = 9998
	maSess     int64 = 5301
	// 另外两个房间只用来撑起「按 mid 统计的房间数」这道路径，不需要有真实归属。
	maRoomB int64 = 4401
	maRoomC int64 = 4402
	maMidB  int64 = 9401
	maMidC  int64 = 9402
)

// maReq 组一条「房主把 target 绑成联合主播」的除幂等键外都合法的请求。
func maReq(reqID string, action rpc.AnchorAction, role rpc.AnchorRole) *rpc.MutateAnchorReq {
	return &rpc.MutateAnchorReq{
		RoomId: maRoom, OperatorMid: maOwner, TargetMid: maTarget,
		Action: action, Role: role, RequestId: reqID, TraceId: "trace-anchor",
	}
}

func newMutateAnchorLogic(t *testing.T, st *store) *MutateAnchorLogic {
	t.Helper()
	return NewMutateAnchorLogic(context.Background(), st.svcCtx())
}

// newMutateAnchorLogicWith 改配额配置：三道上限（每房间联合主播 / 房管、每 mid 房间数）
// 都在 config 里，默认 testLiveRoomConf() 全为 0（不限制），必须显式打开才可测配额分支。
func newMutateAnchorLogicWith(t *testing.T, st *store, edit func(*config.LiveRoomConf)) *MutateAnchorLogic {
	t.Helper()
	return NewMutateAnchorLogic(context.Background(), st.svcCtxWith(testLiveRoomConfDo(edit)))
}

// maQuotas 打开三道配额上限。
func maQuotas(cohost, manager, roomsPerMid int32) func(*config.LiveRoomConf) {
	return func(c *config.LiveRoomConf) {
		c.MaxCohostPerRoom = cohost
		c.MaxManagerPerRoom = manager
		c.MaxOwnerBindingsPerMid = roomsPerMid
	}
}

// seedAnchorScene 布「某状态的房间 + 生效房主」。
// 房主行始终布上：归属校验要么用它，要么用它证明「陌生操作者被拒」。
//
// 房间投影的改动必须走 edits 在**播种之前**改：seedRoom 存的是它自己 copy 出来的行，
// roomAt 读回的又是副本，所以拿到返回值再改字段不会影响库存（closeroom 用例用
// `st.rooms.rows[0] = r` 原地覆盖，这里用 edits 达到同样效果）。
func seedAnchorScene(t *testing.T, st *store, state int32, edits ...func(*model.LiveRoom)) {
	t.Helper()
	r := baseRoom(maRoom, maOwner, state)
	for _, edit := range edits {
		edit(r)
	}
	st.seedRoom(r)
	st.seedAnchor(baseAnchor(901, maRoom, maOwner, model.AnchorRoleOwner))
}

// maLivingWith 把挂机位写进房间行，配合 RoomStateLiving 布出「正在开播」的投影。
func maLivingWith(sessID int64) func(*model.LiveRoom) {
	return func(r *model.LiveRoom) {
		r.ActiveSessionID = sessID
		r.ActiveStreamID = fmt.Sprintf("stream-%d", sessID)
	}
}

// seedTwoMoreRooms 给 target 布两个「已经在别的房间占位」的场景：
// 4401 里它是房管、4402 里它是联合主播，于是 r0 与 r2 两种口径统计出来不同。
func seedTwoMoreRooms(st *store) {
	st.seedRoom(baseRoom(maRoomB, maMidB, model.RoomStateReady))
	st.seedRoom(baseRoom(maRoomC, maMidC, model.RoomStateReady))
	st.seedAnchor(baseAnchor(902, maRoomB, maTarget, model.AnchorRoleManager))
	st.seedAnchor(baseAnchor(903, maRoomC, maTarget, model.AnchorRoleCohost))
}

// TestMutateAnchorBindCohostFullTrace 是本方法的骨架用例：一次 BIND 打了哪 7 条 SQL、
// 顺序是什么、落库长什么样。配额全开，所以两道上限的读都在序列里。
func TestMutateAnchorBindCohostFullTrace(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	seedTwoMoreRooms(st)

	reply, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 20)).
		MutateAnchor(maReq("req-ma-bind", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantNoErr(t, "绑定联合主播", err)
	defer st.checkRaces(t)

	wantSeq(t, "绑定首次执行", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-bind",
		// checkBindQuota：房间维度（Count 带 role + Find 判「是否已生效」）
		fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
		fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
		// checkBindQuota：主播维度（不分角色的 DISTINCT 房间数 + 房间清单）
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
		fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
		"live_room_anchor.Bind:m9302",
		// 应答里的 bound_count 走**按角色**的口径
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
		"live_room_idempotency.SaveResult:req-ma-bind",
	)
	// 第 3 条：全程零事务、零审计行。
	wantTxCount(t, "绑定联合主播", st.conn, 0)
	wantMethodCount(t, "绑定联合主播", st.log, "db.TransactCtx", 0)
	wantMethodCount(t, "绑定联合主播", st.log, "live_room_state_log.Insert", 0)
	wantMethodCount(t, "绑定联合主播", st.log, "live_room_state_log.InsertTx", 0)
	// BIND 不允许碰房主位，所以 TransferOwner 一次都不该被调用（model 侧它也无人调用）。
	wantMethodCount(t, "绑定联合主播", st.log, "live_room_anchor.TransferOwner", 0)
	// 解绑分支的读不得出现。
	wantMethodCount(t, "绑定联合主播", st.log, "live_room_anchor.Unbind", 0)
	wantMethodCount(t, "绑定联合主播", st.log, "live_session.FindOne", 0)

	wantEQ(t, "绑定应答", "room_id", reply.GetRoomId(), maRoom)
	wantEQ(t, "绑定应答", "target_mid", reply.GetTargetMid(), maTarget)
	wantEQ(t, "绑定应答", "state", reply.GetState(), model.BindStateEnabled)
	// 4402（联合主播）+ 3301（这次）= 2；若实现误用不分角色的 r0，这里会是 3。
	wantEQ(t, "绑定应答", "bound_count 按角色统计", reply.GetBoundCount(), int32(2))
	wantEQ(t, "绑定应答", "replayed", reply.GetReplayed(), false)

	row := st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost)
	wantEQ(t, "新绑定行", "state", row.State, model.BindStateEnabled)
	wantEQ(t, "新绑定行", "role", row.Role, model.AnchorRoleCohost)
	wantEQ(t, "新绑定行", "ctime=mtime=now", row.Mtime, maNow)
	wantEQ(t, "新绑定行", "ctime", row.Ctime, maNow)
	// 非房主行不得占用 uniq_active_owner。
	if row.OwnerRoomID.Valid {
		t.Errorf("新绑定行：owner_room_id = %#v, want NULL（只有生效房主占位）", row.OwnerRoomID)
	}
	// 生成的主键 = 表内 max+1（已有 901/902/903）。
	wantEQ(t, "新绑定行", "id 自增", row.ID, int64(904))
	// 房主行不受影响：占位与状态版本都不该被 BIND 碰。
	owner := st.anchorRow(t, maRoom, maOwner, model.AnchorRoleOwner)
	wantEQ(t, "房主行", "state 仍生效", owner.State, model.BindStateEnabled)
	if !owner.OwnerRoomID.Valid || owner.OwnerRoomID.Int64 != maRoom {
		t.Errorf("房主行：owner_room_id = %#v, want 占位 %d", owner.OwnerRoomID, maRoom)
	}
	wantDeepEQ(t, "绑定落库面", "counts", st.counts(),
		storeCounts{rooms: 3, anchors: 4, idem: 1})
}

// TestMutateAnchorQuotaReadsAreScopedAsDocumented 把配额的两道读的**入参口径**钉住：
// 房间维度 Count 带 role 且只数生效行，主播维度 CountActiveRoomsByMid 不分角色。
// 与骨架用例成对：那里锁顺序，这里锁「按什么条件数」。
func TestMutateAnchorQuotaReadsAreScopedAsDocumented(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	seedTwoMoreRooms(st)
	// 同房间里另一个 mid 的联合主播 + 一个已停用的房管行：都不该被计入 role=2 的生效数。
	st.seedAnchor(baseAnchor(904, maRoom, maStranger, model.AnchorRoleCohost))
	disabled := baseAnchor(905, maRoom, maMidB, model.AnchorRoleManager)
	disabled.State = model.BindStateDisabled
	st.seedAnchor(disabled)

	// 房管上限压到 1，好让 OnlyEnabled 这个条件有判别力：
	// 库里 role=3 的行只有 905 一条且已停用，Count(onlyEnabled=true) 回 0 -> 放行；
	// 若实现漏掉 OnlyEnabled，cnt=1 >= 1 且 cur==nil，这条请求就会被拒。
	l := newMutateAnchorLogicWith(t, st, maQuotas(8, 1, 20))
	reply, err := l.MutateAnchor(
		maReq("req-ma-quota", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_MANAGER))
	wantNoErr(t, "绑定房管", err)

	wantSeq(t, "绑定房管", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-quota",
		fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
		fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
		fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
		"live_room_anchor.Bind:m9302",
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleManager),
		"live_room_idempotency.SaveResult:req-ma-quota",
	)
	// 房间维度那道 Count 的入参：本房间 + role=3 + 只看生效行（上面 cnt=0 已放行）。
	wantDeepEQ(t, "房管配额读", "AnchorListQuery", st.anchors.countQueries,
		[]model.AnchorListQuery{{RoomID: maRoom, Role: model.AnchorRoleManager, OnlyEnabled: true}})
	// 主播维度那道读不分角色：op 键里的 `/r0` 就是它的入参（见 wantSeq 第 6 条），
	// 与应答 bound_count 用的 `/r3` 是两次不同调用（wantSeq 第 9 条）。
	// target 以房管身份生效的房间 = {4401, 3301} = 2；若 bound_count 误用 r0 口径会是 3。
	wantEQ(t, "绑定房管应答", "bound_count 按 role=3 统计", reply.GetBoundCount(), int32(2))
	wantEQ(t, "绑定房管应答", "state", reply.GetState(), model.BindStateEnabled)
}

// TestMutateAnchorUnbindRoleZeroClearsEveryNonOwnerRow 覆盖 UNBIND 的 role=0 语义：
// 「解绑该 mid 在本房间的全部非房主角色」，一次 UPDATE 命中两行，房主行不受影响。
func TestMutateAnchorUnbindRoleZeroClearsEveryNonOwnerRow(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(903, maRoom, maTarget, model.AnchorRoleManager))

	reply, err := newMutateAnchorLogic(t, st).
		MutateAnchor(maReq("req-ma-unbind-all", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED))
	wantNoErr(t, "解绑全部非房主角色", err)
	wantSeq(t, "解绑全部角色", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-unbind-all",
		fmt.Sprintf("live_room_anchor.Unbind:%d/%d/r%d", maRoom, maTarget, model.AnchorRoleUnspecified),
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
		"live_room_idempotency.SaveResult:req-ma-unbind-all",
	)
	// UNBIND 不做配额预检（两道上限只在 BIND 分支），所以一条 Count 都不许出现。
	wantMethodCount(t, "解绑全部角色", st.log, "live_room_anchor.Count", 0)
	wantMethodCount(t, "解绑全部角色", st.log, "live_room_anchor.ListRoomsByMid", 0)
	wantMethodCount(t, "解绑全部角色", st.log, "live_room_anchor.Bind", 0)
	// 房间非 LIVING：guardLivingAnchor 直接返回，不读场次。
	wantMethodCount(t, "解绑全部角色", st.log, "live_session.FindOne", 0)

	wantEQ(t, "解绑应答", "state", reply.GetState(), model.BindStateDisabled)
	wantEQ(t, "解绑应答", "bound_count 归零", reply.GetBoundCount(), int32(0))

	for _, role := range []int32{model.AnchorRoleCohost, model.AnchorRoleManager} {
		row := st.anchorRow(t, maRoom, maTarget, role)
		wantEQ(t, fmt.Sprintf("被解绑行 role=%d", role), "state", row.State, model.BindStateDisabled)
		wantEQ(t, fmt.Sprintf("被解绑行 role=%d", role), "mtime", row.Mtime, maNow)
		if row.OwnerRoomID.Valid {
			t.Errorf("被解绑行 role=%d：owner_room_id = %#v, want NULL（解绑要释放占位）", role, row.OwnerRoomID)
		}
		// 软删除：行还在，只是停用（审计证据保留，重绑走同一行）。
	}
	// 房主行必须原样：Unbind 的 UPDATE 无条件排除 role=1。
	owner := st.anchorRow(t, maRoom, maOwner, model.AnchorRoleOwner)
	wantEQ(t, "房主行未被解绑", "state", owner.State, model.BindStateEnabled)
	wantEQ(t, "房主行未被解绑", "mtime 未被改", owner.Mtime, int64(4000+901))
	wantDeepEQ(t, "解绑落库面", "counts", st.counts(), storeCounts{rooms: 1, anchors: 3, idem: 1})
}

// TestMutateAnchorUnbindWithoutAnyBindingIsSilentSuccess 钉住第 6 条：
// Unbind 的 RowsAffected 被丢弃，所以「解绑一个从未绑过的 mid」是成功而非 not-found，
// 库里一行都没多、也没少。判别前提：同一场景把 room/ mid 换成已存在的房主行也解不掉（见下）。
func TestMutateAnchorUnbindWithoutAnyBindingIsSilentSuccess(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)

	reply, err := newMutateAnchorLogic(t, st).
		MutateAnchor(maReq("req-ma-unbind-noop", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantNoErr(t, "解绑不存在的绑定", err)
	wantSeq(t, "解绑不存在的绑定", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-unbind-noop",
		fmt.Sprintf("live_room_anchor.Unbind:%d/%d/r%d", maRoom, maTarget, model.AnchorRoleCohost),
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
		"live_room_idempotency.SaveResult:req-ma-unbind-noop",
	)
	wantEQ(t, "解绑不存在的绑定应答", "state", reply.GetState(), model.BindStateDisabled)
	wantEQ(t, "解绑不存在的绑定应答", "bound_count", reply.GetBoundCount(), int32(0))
	wantEQ(t, "解绑不存在的绑定", "应答不区分命中行数", reply.GetReplayed(), false)
	// 残留形态：anchor 表仍是「房间 + 房主」两行，没有为 target 插出任何行。
	wantDeepEQ(t, "解绑不存在的绑定", "counts", st.counts(), storeCounts{rooms: 1, anchors: 1, idem: 1})
	wantEQ(t, "解绑不存在的绑定", "target 行数", len(st.anchors.rows)-1, 0)
}

// TestMutateAnchorOwnerGuardRunsBeforeClaim 第 1 条：三种「不是生效房主」的形态都在抢键前被拒，
// 键完好（可换 operator_mid 后复用同一 request_id 重试）。
func TestMutateAnchorOwnerGuardRunsBeforeClaim(t *testing.T) {
	t.Run("操作者是陌生 mid", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		in := maReq("req-ma-notowner", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST)
		in.OperatorMid = maStranger

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(in)
		wantErrIs(t, "非房主绑定", err, model.ErrAnchorForbidden)
		wantErrContains(t, "非房主绑定", err, "不是该房间生效房主")
		wantSeq(t, "非房主绑定", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		)
		wantKeyUnburned(t, "非房主绑定", "req-ma-notowner", st)
		wantEQ(t, "非房主绑定", "anchor 行数", st.counts().anchors, 1)
	})

	t.Run("房主行已停用", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		st.seedRoom(baseRoom(maRoom, maOwner, model.RoomStateReady))
		a := baseAnchor(901, maRoom, maOwner, model.AnchorRoleOwner)
		// 停用的房主行必须释放 uniq_active_owner 占位（model/live_room_anchor.go 的
		// Unbind/TransferOwner 都是这么写的），否则 seedAnchor 会拦下这个非法形态。
		a.State = model.BindStateDisabled
		a.OwnerRoomID = sql.NullInt64{}
		st.seedAnchor(a)

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-noowner", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		// FindOwner 的条件含 state=1：有行但没生效 == 没有房主，operator 是本人也照拒。
		wantErrIs(t, "无生效房主", err, model.ErrAnchorForbidden)
		wantSeq(t, "无生效房主", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		)
		wantKeyUnburned(t, "无生效房主", "req-ma-noowner", st)
	})

	t.Run("房间里一行绑定都没有", func(t *testing.T) {
		// 判别对照：上面那例是「有行但停用」，这里是「压根没有行」——两种形态都得拒，
		// 且都停在 FindOwner 返回 nil 上，不区分「无行」与「行不生效」。
		fixClock(t, maNow)
		st := newStore()
		st.seedRoom(baseRoom(maRoom, maOwner, model.RoomStateReady))

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-norow", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "无房主行", err, model.ErrAnchorForbidden)
		wantSeq(t, "无房主行", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		)
		wantKeyUnburned(t, "无房主行", "req-ma-norow", st)
	})

	t.Run("联合主播不能解绑房管（role=2 也不是管理员位）", func(t *testing.T) {
		// 判别对照：UpdateRoomInfo 允许生效联合主播改资料，本方法只认房主。
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))
		in := maReq("req-ma-cohost-operator", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_MANAGER)
		in.OperatorMid = maTarget
		in.TargetMid = maMidB

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(in)
		wantErrIs(t, "联合主播操作", err, model.ErrAnchorForbidden)
		wantKeyUnburned(t, "联合主播操作", "req-ma-cohost-operator", st)
		wantSeq(t, "联合主播操作", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		)
	})
}

// TestMutateAnchorTerminalRoomRejectedBeforeOwnerLookup 第 7 条：只有 FINISHED 被拒，
// BANNED / DISABLED 作为判别对照照样能改成员。
func TestMutateAnchorTerminalRoomRejectedBeforeOwnerLookup(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateFinished)

	_, err := newMutateAnchorLogic(t, st).MutateAnchor(
		maReq("req-ma-finished", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantErrIs(t, "终态房间改成员", err, model.ErrRoomFinished)
	// FindOwner 都还没跑：终态判定在归属校验之前。
	wantSeq(t, "终态房间改成员", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", maRoom))
	wantKeyUnburned(t, "终态房间改成员", "req-ma-finished", st)

	for _, state := range []int32{model.RoomStateBanned, model.RoomStateDisabled} {
		t.Run(fmt.Sprintf("state=%d 仍可改成员", state), func(t *testing.T) {
			fixClock(t, maNow)
			st := newStore()
			seedAnchorScene(t, st, state)

			_, err := newMutateAnchorLogic(t, st).MutateAnchor(
				maReq("req-ma-nonterminal", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
			wantNoErr(t, "非终态房间改成员", err)
			wantEQ(t, "非终态房间改成员", "state 未被 BIND 改动",
				st.roomAt(t, maRoom).State, state)
		})
	}
}

// TestMutateAnchorLivingAnchorCannotBeUnbound 覆盖 guardLivingAnchor 的三条分支，
// 并钉住第 4 条的缺陷后果：拒绝发生在抢键之后，键被消费且不回填结果。
func TestMutateAnchorLivingAnchorCannotBeUnbound(t *testing.T) {
	// 分支一：目标 mid 就是开播人，场次非终态 -> 拒。
	t.Run("开播中的主播不可解绑", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateLiving, maLivingWith(maSess))
		st.seedSession(baseSession(maSess, maRoom, maTarget, model.SessionStateLiving))
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-living", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "在播主播解绑", err, model.ErrAnchorForbidden)
		wantErrContains(t, "在播主播解绑", err, "正在场次")
		wantSeq(t, "在播主播解绑", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-living",
			fmt.Sprintf("live_session.FindOne:%d", maSess),
		)
		wantMethodCount(t, "在播主播解绑", st.log, "live_room_anchor.Unbind", 0)
		// 业务被拒却已经把键消费掉：同一 request_id 重试恒为 ErrIdempotencyResultMissing。
		wantKeyBurnedNoResult(t, "在播主播解绑", "req-ma-living", st)
		wantEQ(t, "在播主播解绑残留", "绑定行仍生效",
			st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost).State, model.BindStateEnabled)
	})

	// 分支二（判别对照）：在播的是**别人**，target 只是同房间另一个联合主播 -> 放行。
	t.Run("在播场次属于别的 mid", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateLiving, maLivingWith(maSess))
		st.seedSession(baseSession(maSess, maRoom, maOwner, model.SessionStateLiving))
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-living-other", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "解绑非开播主播", err)
		wantSeq(t, "解绑非开播主播", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-living-other",
			fmt.Sprintf("live_session.FindOne:%d", maSess),
			fmt.Sprintf("live_room_anchor.Unbind:%d/%d/r%d", maRoom, maTarget, model.AnchorRoleCohost),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
			"live_room_idempotency.SaveResult:req-ma-living-other",
		)
		wantEQ(t, "解绑非开播主播", "绑定行已停用",
			st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost).State, model.BindStateDisabled)
	})

	// 分支三（判别对照）：房间挂着老场次号但场次**已是终态** -> guard 的第三个条件放行。
	// 这是「房间投影与场次表不一致」的异常形态（EndLive/ReportStreamState 会清挂机位），
	// 布它只为证明 guard 读的是场次的 state 而不是房间的 state。
	t.Run("挂机位指向已终态场次", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateLiving, maLivingWith(maSess))
		st.seedSession(baseSession(maSess, maRoom, maTarget, model.SessionStateEnded))
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-living-ended", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "老场次已终态", err)
		wantMethodCount(t, "老场次已终态", st.log, "live_room_anchor.Unbind", 1)
	})

	// 分支四（判别对照）：ActiveSessionID<=0 时 guard 连场次都不读。
	t.Run("在播但没登记挂机位", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateLiving) // baseRoom 的 active_session_id 是 0
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-nosession", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "无挂机位", err)
		wantMethodCount(t, "无挂机位", st.log, "live_session.FindOne", 0)
	})
}

// TestMutateAnchorRoomQuotaRejection 房间维度上限：满员时拒；
// 判别对照是「target 本来就以同一角色生效着」——重复绑定按幂等处理，不占新额度。
func TestMutateAnchorRoomQuotaRejection(t *testing.T) {
	t.Run("联合主播已满", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedAnchor(baseAnchor(902, maRoom, maStranger, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogicWith(t, st, maQuotas(1, 30, 20)).MutateAnchor(
			maReq("req-ma-roomfull", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "房间联合主播满员", err, model.ErrAnchorLimitExceeded)
		wantErrContains(t, "房间联合主播满员", err, "已有 1 个生效绑定，上限 1")
		wantSeq(t, "房间联合主播满员", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-roomfull",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
		)
		// 配额被拒发生在 Bind 之前：一行都没写，但键已经被消费掉。
		wantMethodCount(t, "房间联合主播满员", st.log, "live_room_anchor.Bind", 0)
		wantKeyBurnedNoResult(t, "房间联合主播满员", "req-ma-roomfull", st)
		wantDeepEQ(t, "房间联合主播满员残留", "counts", st.counts(),
			storeCounts{rooms: 1, anchors: 2, idem: 1})
	})

	t.Run("同一角色重复绑定不占新额度", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleCohost))

		reply, err := newMutateAnchorLogicWith(t, st, maQuotas(1, 30, 20)).MutateAnchor(
			maReq("req-ma-again", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "重复绑定", err)
		wantSeq(t, "重复绑定", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-again",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
			fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
			"live_room_anchor.Bind:m9302",
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
			"live_room_idempotency.SaveResult:req-ma-again",
		)
		wantEQ(t, "重复绑定应答", "state", reply.GetState(), model.BindStateEnabled)
		wantEQ(t, "重复绑定应答", "bound_count", reply.GetBoundCount(), int32(1))
		// ON DUPLICATE 的更新分支不插新行：还是那一条，mtime 被推到 now。
		wantEQ(t, "重复绑定", "anchor 行数不变", st.counts().anchors, 2)
		wantEQ(t, "重复绑定", "mtime", st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost).Mtime, maNow)
	})

	t.Run("已停用的行重新占用额度", func(t *testing.T) {
		// 判别对照：cur != nil 但 cur.State != 生效 -> 仍按新额度算，满员就拒。
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedAnchor(baseAnchor(902, maRoom, maStranger, model.AnchorRoleCohost))
		old := baseAnchor(903, maRoom, maTarget, model.AnchorRoleCohost)
		old.State = model.BindStateDisabled
		st.seedAnchor(old)

		_, err := newMutateAnchorLogicWith(t, st, maQuotas(1, 30, 20)).MutateAnchor(
			maReq("req-ma-reactivate", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "停用行重新绑定", err, model.ErrAnchorLimitExceeded)
		wantEQ(t, "停用行重新绑定", "停用行未被唤醒",
			st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost).State, model.BindStateDisabled)
	})
}

// TestMutateAnchorMidRoomQuotaUsesDistinctRoomCount 主播维度上限：
// 同一 mid 在同一房间占两个角色时，配额读 DISTINCT 后只算 1 间，
// 而 ListRoomsByMid 不带 DISTINCT 会回两个相同 room_id——两者口径不同，
// alreadyIn 判定用的是后者。
func TestMutateAnchorMidRoomQuotaUsesDistinctRoomCount(t *testing.T) {
	t.Run("两个角色同一房间只算一间", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedRoom(baseRoom(maRoomB, maMidB, model.RoomStateReady))
		st.seedAnchor(baseAnchor(902, maRoomB, maTarget, model.AnchorRoleCohost))
		st.seedAnchor(baseAnchor(903, maRoomB, maTarget, model.AnchorRoleManager))

		_, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 1)).MutateAnchor(
			maReq("req-ma-midroom", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		// 判别点就在这里：DISTINCT 口径回 1（只有 4401 一间），
		// 若实现改用 ListRoomsByMid 的长度（同一房间占两角色 → 2），错误消息里的数会是 2。
		wantErrIs(t, "mid 房间数按 DISTINCT", err, model.ErrAnchorLimitExceeded)
		wantErrContains(t, "mid 房间数按 DISTINCT", err, "已绑定 1 个房间，上限 1")
		wantSeq(t, "mid 房间数按 DISTINCT", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-midroom",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
			fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
		)
		// alreadyIn 判定读的是 ListRoomsByMid（不带 DISTINCT），本例它回 [4401 4401]，
		// 里面没有 3301 -> alreadyIn=false -> 才走到拒绝；「本房间已绑定则放行」那条子用例
		// 是它的判别对照。
		wantMethodCount(t, "mid 房间数按 DISTINCT", st.log, "live_room_anchor.Bind", 0)
		wantKeyBurnedNoResult(t, "mid 房间数按 DISTINCT", "req-ma-midroom", st)
	})

	t.Run("达到上限但本房间已绑定则放行", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		// target 已经以房管身份在本房间（另一角色），本房间算 alreadyIn。
		st.seedAnchor(baseAnchor(902, maRoom, maTarget, model.AnchorRoleManager))
		st.seedRoom(baseRoom(maRoomB, maMidB, model.RoomStateReady))
		st.seedAnchor(baseAnchor(903, maRoomB, maTarget, model.AnchorRoleCohost))

		reply, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 2)).MutateAnchor(
			maReq("req-ma-alreadyin", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "alreadyIn 放行", err)
		wantSeq(t, "alreadyIn 放行", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-alreadyin",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
			fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
			"live_room_anchor.Bind:m9302",
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
			"live_room_idempotency.SaveResult:req-ma-alreadyin",
		)
		// DISTINCT 房间数 = {3301, 4401} = 2 = 上限，但 alreadyIn=true 所以放行。
		wantEQ(t, "alreadyIn 放行", "bound_count（role=2 口径）", reply.GetBoundCount(), int32(2))
	})

	t.Run("超过上限且本房间未绑定则拒", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedRoom(baseRoom(maRoomB, maMidB, model.RoomStateReady))
		st.seedRoom(baseRoom(maRoomC, maMidC, model.RoomStateReady))
		st.seedAnchor(baseAnchor(902, maRoomB, maTarget, model.AnchorRoleCohost))
		st.seedAnchor(baseAnchor(903, maRoomC, maTarget, model.AnchorRoleCohost))

		_, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 2)).MutateAnchor(
			maReq("req-ma-midfull", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "mid 房间数超限", err, model.ErrAnchorLimitExceeded)
		wantErrContains(t, "mid 房间数超限", err, "已绑定 2 个房间，上限 2")
		wantSeq(t, "mid 房间数超限", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-midfull",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
			fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
		)
		wantKeyBurnedNoResult(t, "mid 房间数超限", "req-ma-midfull", st)
		wantEQ(t, "mid 房间数超限", "anchor 行数", st.counts().anchors, 3)
	})

	t.Run("配额上限配 0 时两道读都不发", func(t *testing.T) {
		// 默认 testLiveRoomConf() 的三个上限是 0（不限制），序列应塌成 4 条。
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-nolimit", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "不限制配额", err)
		wantSeq(t, "不限制配额", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-nolimit",
			"live_room_anchor.Bind:m9302",
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
			"live_room_idempotency.SaveResult:req-ma-nolimit",
		)
	})
}

// TestMutateAnchorBusinessRejectionBurnsRequestID 钉住第 4 条的现状（README 已知缺口）。
//
// TODO(缺陷) mutateanchorlogic.go:81 的 claimDedup 早于 :101 guardLivingAnchor 与
// :109 checkBindQuota，而两条拒绝路径（:102/:110 直接 return err）都不回填结果。
// 于是「配额已满」「在播不可解绑」这类**确定性**业务拒绝会把 request_id 消费掉，
// 调用方换参数重试同一个键只能拿到 ErrIdempotencyResultMissing —— 既看不到原错误，
// 也无法在同一键上完成操作。下面的第二条序列就是这个事实：它钉的是当前行为，
// 不是应有行为；修好后应改为断言「重试仍返回同一个 ErrAnchorLimitExceeded」。
func TestMutateAnchorBusinessRejectionBurnsRequestID(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	st.seedAnchor(baseAnchor(902, maRoom, maStranger, model.AnchorRoleCohost))
	l := newMutateAnchorLogicWith(t, st, maQuotas(1, 30, 20))

	_, err := l.MutateAnchor(maReq("req-ma-burned",
		rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantErrIs(t, "满员拒绝", err, model.ErrAnchorLimitExceeded)
	first := st.log.snapshot()

	// 换 target 也不行：同一个 request_id 已经在 Claim 里登记了。
	retry := maReq("req-ma-burned", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST)
	retry.TargetMid = maMidB
	_, err = l.MutateAnchor(retry)
	wantErrIs(t, "满员后重试同一键", err, model.ErrIdempotencyResultMissing)
	wantSeq(t, "满员后重试同一键", st.log, first,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-burned",
		"live_room_idempotency.Find:req-ma-burned",
	)
	// 重试既没解开死锁，也没留下副作用：绑定行数与首次失败后完全一致。
	wantDeepEQ(t, "满员后重试同一键", "counts", st.counts(), storeCounts{rooms: 1, anchors: 2, idem: 1})
}

// TestMutateAnchorReplayIdempotencyTriad 幂等三态：回读原结果 / 键被别的方法用过 /
// 键被烧掉但没有结果。三条共用「FindOne + FindOwner + Claim + Find」前缀。
func TestMutateAnchorReplayIdempotencyTriad(t *testing.T) {
	t.Run("回读首次结果", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedIdem("req-ma-replay", rpcMutateAnchor,
			`{"room_id":3301,"target_mid":9302,"state":1,"bound_count":7}`)

		reply, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-replay", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantNoErr(t, "重放", err)
		wantSeq(t, "重放", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-replay",
			"live_room_idempotency.Find:req-ma-replay",
		)
		wantEQ(t, "重放", "replayed", reply.GetReplayed(), true)
		wantEQ(t, "重放", "state", reply.GetState(), model.BindStateEnabled)
		// 快照里是 7，库里其实一条都没绑：回读的是**当时的应答**而不是当前投影。
		wantEQ(t, "重放", "bound_count 来自快照", reply.GetBoundCount(), int32(7))
		wantEQ(t, "重放", "anchor 行数", st.counts().anchors, 1)
		wantMethodCount(t, "重放", st.log, "live_room_anchor.Bind", 0)
		wantMethodCount(t, "重放", st.log, "live_room_idempotency.SaveResult", 0)
	})

	t.Run("键被别的方法用过", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedIdem("shared-anchor", rpcStartLive, `{"session_id":1}`)

		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("shared-anchor", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "串方法的键", err, model.ErrRequestIDReused)
		wantErrContains(t, "串方法的键", err, "used by StartLive")
		wantSeq(t, "串方法的键", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:shared-anchor",
			"live_room_idempotency.Find:shared-anchor",
		)
	})

	t.Run("烧过的键没有结果", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.seedIdem("burned-anchor", rpcMutateAnchor, "")

		reply, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("burned-anchor", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "烧过的键", err, model.ErrIdempotencyResultMissing)
		if reply != nil {
			t.Fatalf("应答 = %+v, want nil", reply)
		}
		wantMethodCount(t, "烧过的键", st.log, "live_room_anchor.Unbind", 0)
	})
}

// TestMutateAnchorInputGuardTable 入参守卫表：每一项都必须在**触库之前**被拒。
func TestMutateAnchorInputGuardTable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.MutateAnchorReq)
		want   error
		frag   string
	}{
		{"缺房间号", func(in *rpc.MutateAnchorReq) { in.RoomId = 0 }, model.ErrInvalidRoomID, ""},
		{"房间号为负", func(in *rpc.MutateAnchorReq) { in.RoomId = -7 }, model.ErrInvalidRoomID, ""},
		{"缺操作者", func(in *rpc.MutateAnchorReq) { in.OperatorMid = 0 }, model.ErrOperatorRequired, ""},
		{"目标 mid 为 0", func(in *rpc.MutateAnchorReq) { in.TargetMid = 0 }, model.ErrInvalidMid, ""},
		{"目标 mid 为负", func(in *rpc.MutateAnchorReq) { in.TargetMid = -1 }, model.ErrInvalidMid, ""},
		{"幂等键空", func(in *rpc.MutateAnchorReq) { in.RequestId = "" }, model.ErrRequestIDRequired, ""},
		{"幂等键纯空白", func(in *rpc.MutateAnchorReq) { in.RequestId = "   " }, model.ErrRequestIDRequired, ""},
		{"幂等键超长", func(in *rpc.MutateAnchorReq) {
			in.RequestId = strings.Repeat("r", maxDedupIDBytes+1)
		}, model.ErrDedupIDTooLong, ""},
		{"动作未指定", func(in *rpc.MutateAnchorReq) { in.Action = rpc.AnchorAction_ANCHOR_ACTION_UNSPECIFIED },
			model.ErrAnchorActionInvalid, "action=0"},
		{"动作未定义", func(in *rpc.MutateAnchorReq) { in.Action = rpc.AnchorAction(9) },
			model.ErrAnchorActionInvalid, "action=9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, maNow)
			st := newStore()
			seedAnchorScene(t, st, model.RoomStateReady)
			in := maReq("req-ma-guard", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST)
			tc.mutate(in)

			_, err := newMutateAnchorLogic(t, st).MutateAnchor(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantTxCount(t, tc.name, st.conn, 0)
			wantKeyUnburned(t, tc.name, "req-ma-guard", st)
		})
	}

	t.Run("nil 请求", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		before := st.log.snapshot()
		_, err := newMutateAnchorLogic(t, st).MutateAnchor(nil)
		wantErrIs(t, "nil 请求", err, model.ErrInvalidRoomID)
		wantNoCallAfter(t, "nil 请求", st.log, before)
	})
}

// TestMutateAnchorRoleGuardRunsAfterReadsButBeforeClaim 第 2 条：角色归一在两次读之后、
// 抢键之前。这一条与上一条成对，判别出「入参守卫 vs 依赖读」的边界到底在哪一行。
func TestMutateAnchorRoleGuardRunsAfterReadsButBeforeClaim(t *testing.T) {
	cases := []struct {
		name   string
		action rpc.AnchorAction
		role   rpc.AnchorRole
		want   error
		frag   string
	}{
		{"BIND 不给角色", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED,
			model.ErrAnchorRoleInvalid, "role=0"},
		{"BIND 未知角色", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole(9),
			model.ErrAnchorRoleInvalid, "role=9"},
		{"BIND 房主位", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_OWNER,
			model.ErrAnchorRoleInvalid, "TransferOwner"},
		{"UNBIND 房主位", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_OWNER,
			model.ErrCannotUnbindOwner, ""},
		{"UNBIND 未知角色", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole(9),
			model.ErrAnchorRoleInvalid, "role=9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, maNow)
			st := newStore()
			seedAnchorScene(t, st, model.RoomStateReady)

			_, err := newMutateAnchorLogic(t, st).MutateAnchor(maReq("req-ma-role", tc.action, tc.role))
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			wantSeq(t, tc.name, st.log, 0,
				fmt.Sprintf("live_room.FindOne:%d", maRoom),
				fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			)
			// 被拒的角色不得留下任何写，也不得消费键。
			wantKeyUnburned(t, tc.name, "req-ma-role", st)
			wantEQ(t, tc.name, "anchor 行数", st.counts().anchors, 1)
		})
	}

	t.Run("UNBIND 允许角色 0", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-role0", rpc.AnchorAction_ANCHOR_ACTION_UNBIND, rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED))
		wantNoErr(t, "UNBIND role=0", err)
	})
}

// TestMutateAnchorMissingRoomRejectedWithoutClaim 房间不存在必须在抢键之前拒绝。
func TestMutateAnchorMissingRoomRejectedWithoutClaim(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()

	_, err := newMutateAnchorLogic(t, st).MutateAnchor(
		maReq("req-ma-missing", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantErrIs(t, "房间不存在", err, model.ErrRoomNotFound)
	wantSeq(t, "房间不存在", st.log, 0, fmt.Sprintf("live_room.FindOne:%d", maRoom))
	wantKeyUnburned(t, "房间不存在", "req-ma-missing", st)
	wantMethodCount(t, "房间不存在", st.log, "live_room_anchor.FindOwner", 0)
}

// TestMutateAnchorBindFailureLeavesBurnedKeyWithoutRow 第 3 条（无事务）的直接后果：
// Bind 自己失败时，键已消费、绑定行不存在，重放同一 request_id 永远拿不到结果。
// 这里用默认配置（三道配额上限为 0），所以序列里看不到配额读——Bind 是 Claim 之后的第一条写。
func TestMutateAnchorBindFailureLeavesBurnedKeyWithoutRow(t *testing.T) {
	bindErr := errors.New("dial tcp 127.0.0.1:3306: connect refused")

	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	st.anchors.failWith("Bind", bindErr)

	reply, err := newMutateAnchorLogic(t, st).MutateAnchor(
		maReq("req-ma-bindfail", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantErrContains(t, "Bind 失败", err, "live_room_anchor Bind")
	if reply != nil {
		t.Fatalf("应答 = %+v, want nil", reply)
	}
	wantSeq(t, "Bind 失败", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-bindfail",
		"live_room_anchor.Bind:m9302",
	)
	wantKeyBurnedNoResult(t, "Bind 失败", "req-ma-bindfail", st)
	wantMethodCount(t, "Bind 失败", st.log, "live_room_idempotency.SaveResult", 0)
	wantDeepEQ(t, "Bind 失败残留", "counts", st.counts(), storeCounts{rooms: 1, anchors: 1, idem: 1})
}

// TestMutateAnchorBoundCountFailureHappensAfterTheBindIsAlreadyCommitted 是本方法最能暴露
// 「副作用跨语句分裂」的一条：绑定已经落库，随后读 bound_count 失败，logic 直接抛错，
// 于是调用方拿到失败应答、库里却多了生效绑定（且没有事务可回滚）。
func TestMutateAnchorBoundCountFailureHappensAfterTheBindIsAlreadyCommitted(t *testing.T) {
	countErr := errors.New("anchor count failed")

	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	// 只让**最终那次** bound_count 读失败：把配额读整体关掉，避免同一方法被调两次。
	st.anchors.failWith("CountActiveRoomsByMid", countErr)

	_, err := newMutateAnchorLogic(t, st).MutateAnchor(
		maReq("req-ma-countfail", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
	wantErrIs(t, "bound_count 读失败", err, countErr)
	wantSeq(t, "bound_count 读失败", st.log, 0,
		fmt.Sprintf("live_room.FindOne:%d", maRoom),
		fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		"live_room_idempotency.Claim:req-ma-countfail",
		"live_room_anchor.Bind:m9302",
		fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleCohost),
	)
	// 残留：绑定生效 + 键烧掉没结果。
	wantEQ(t, "bound_count 读失败残留", "绑定行已生效",
		st.anchorRow(t, maRoom, maTarget, model.AnchorRoleCohost).State, model.BindStateEnabled)
	wantKeyBurnedNoResult(t, "bound_count 读失败", "req-ma-countfail", st)
	wantTxCount(t, "bound_count 读失败", st.conn, 0)
}

// TestMutateAnchorQuotaReadFailuresPropagateVerbatim 三条配额读的失败必须原样抛出，
// 并且各自停在序列的不同位置（判别出「哪一道读负责哪个上限」）。
func TestMutateAnchorQuotaReadFailuresPropagateVerbatim(t *testing.T) {
	dbFail := errors.New("dial tcp 127.0.0.1:3306: connect refused")

	t.Run("房间维度 Count 失败", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.anchors.failWith("Count", dbFail)
		_, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 20)).MutateAnchor(
			maReq("req-ma-q1", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "配额 Count 失败", err, dbFail)
		wantSeq(t, "配额 Count 失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-q1",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
		)
		wantKeyBurnedNoResult(t, "配额 Count 失败", "req-ma-q1", st)
	})

	t.Run("房间维度 Find 失败", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.anchors.failWith("Find", dbFail)
		_, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 20)).MutateAnchor(
			maReq("req-ma-q2", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "配额 Find 失败", err, dbFail)
		wantSeq(t, "配额 Find 失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-q2",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
		)
		wantKeyBurnedNoResult(t, "配额 Find 失败", "req-ma-q2", st)
		wantMethodCount(t, "配额 Find 失败", st.log, "live_room_anchor.Bind", 0)
	})

	t.Run("主播维度 ListRoomsByMid 失败", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.anchors.failWith("ListRoomsByMid", dbFail)
		_, err := newMutateAnchorLogicWith(t, st, maQuotas(8, 30, 20)).MutateAnchor(
			maReq("req-ma-q3", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "配额 ListRoomsByMid 失败", err, dbFail)
		wantSeq(t, "配额 ListRoomsByMid 失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-q3",
			fmt.Sprintf("live_room_anchor.Count:%d", maRoom),
			fmt.Sprintf("live_room_anchor.Find:%d/%d", maRoom, maTarget),
			fmt.Sprintf("live_room_anchor.CountActiveRoomsByMid:%d/r%d", maTarget, model.AnchorRoleUnspecified),
			fmt.Sprintf("live_room_anchor.ListRoomsByMid:%d", maTarget),
		)
		wantKeyBurnedNoResult(t, "配额 ListRoomsByMid 失败", "req-ma-q3", st)
		wantDeepEQ(t, "配额 ListRoomsByMid 失败残留", "counts", st.counts(),
			storeCounts{rooms: 1, anchors: 1, idem: 1})
	})

	t.Run("房主读失败在抢键之前", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.anchors.failWith("FindOwner", dbFail)
		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-q4", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "房主读失败", err, dbFail)
		wantSeq(t, "房主读失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
		)
		wantKeyUnburned(t, "房主读失败", "req-ma-q4", st)
	})

	t.Run("抢键失败", func(t *testing.T) {
		fixClock(t, maNow)
		st := newStore()
		seedAnchorScene(t, st, model.RoomStateReady)
		st.idem.failWith("Claim", dbFail)
		_, err := newMutateAnchorLogic(t, st).MutateAnchor(
			maReq("req-ma-q5", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST))
		wantErrIs(t, "抢键失败", err, dbFail)
		wantSeq(t, "抢键失败", st.log, 0,
			fmt.Sprintf("live_room.FindOne:%d", maRoom),
			fmt.Sprintf("live_room_anchor.FindOwner:%d", maRoom),
			"live_room_idempotency.Claim:req-ma-q5",
		)
		wantTxCount(t, "抢键失败", st.conn, 0)
	})
}

// TestMutateAnchorTraceIDTrimmedOnlyIntoIdempotencyRow trace_id 超长被裁到列宽，
// 并且**只落进幂等表**：本方法不写 live_room_state_log，所以没有第二处可见位置。
func TestMutateAnchorTraceIDTrimmedOnlyIntoIdempotencyRow(t *testing.T) {
	fixClock(t, maNow)
	st := newStore()
	seedAnchorScene(t, st, model.RoomStateReady)
	in := maReq("req-ma-trace", rpc.AnchorAction_ANCHOR_ACTION_BIND, rpc.AnchorRole_ANCHOR_ROLE_COHOST)
	in.TraceId = strings.Repeat("t", maxTraceIDBytes+50)

	_, err := newMutateAnchorLogic(t, st).MutateAnchor(in)
	wantNoErr(t, "超长 trace_id", err)
	rec := st.idemAt("req-ma-trace")
	if rec == nil {
		t.Fatal("幂等键未登记")
	}
	wantEQ(t, "超长 trace_id", "去重表裁过", len(rec.TraceID), maxTraceIDBytes)
	wantEQ(t, "超长 trace_id", "前缀保住", rec.TraceID[:8], "tttttttt")
	wantEQ(t, "超长 trace_id", "rpc 归因", rec.Rpc, rpcMutateAnchor)
	wantEQ(t, "超长 trace_id", "kind", rec.Kind, model.IdempotencyKindRequest)
	wantEQ(t, "超长 trace_id", "room_id", rec.RoomID, maRoom)
	// session_id 恒 0：本方法与场次无关。
	wantEQ(t, "超长 trace_id", "session_id", rec.SessionID, int64(0))
	wantEQ(t, "超长 trace_id", "审计行数", len(st.logsOf(maRoom)), 0)
}
