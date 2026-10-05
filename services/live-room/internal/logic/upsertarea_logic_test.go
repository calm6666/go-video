package logic

// upsertarea_logic_test.go 覆盖写侧方法 UpsertArea（upsertarealogic.go）。
//
// 锁的结论（全部由 upsertarealogic.go + fakes_test.go 的 live_area 语义反推）：
//  1. 名称唯一性**没有**「先查再写」预检：uniq_area_name 是唯一真值，冲突由 Insert/Update
//     直接翻译成 ErrAreaNameConflict（:30-31 的注释与实现一致）。
//  2. 层级校验（checkParent）在抢幂等键**之前**（:54-58），而占用校验（checkAreaReusable）
//     在抢键**之后**（:106-110）：同一个方法里两种顺序并存，被拒时键是否被消费必须分头断言。
//  3. created 的判据是 `area_id == 0`，与「这行是否真的新插」无关：Update 命中 0 行
//     （行在 FindOne 之后被删）时回 ErrAreaNotFound，不会伪造 created=true。
//  4. 停用分区 = 「不能再被新房间选择」，不是删数据：有启用中的子分区、或有**未关闭**
//     房间挂在上面都拒绝。占用口径由 areaOccupancyStates() 给出（5 个状态，FINISHED 除外）。
//  5. 停用只在「原本启用、这次要停用」时触发占用校验：已停用分区再存一次停用是直通写。
//  6. Update 走整行覆盖：parent_area_id 只要 >=0 就照写入参（model/live_area.go 的
//     `if a.ParentAreaID >= 0`），所以改二级分区时不带 parent_area_id 会把它**悄悄降级**。
//     parent=0 又让 checkParent 整条跳过，没有任何一道校验会拦。见哨兵用例。
//  7. 分区名长度上限在 model 侧是硬编码 AreaNameMaxRunes=32，logic 侧读
//     config.LiveRoom.AreaNameMaxLength：两处可各自漂移，本文件用 TestUpsertAreaConfigWidthDrift
//     把「config 放宽 -> 键已烧 -> model 才拒」这条漂移后果钉住。

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
	uaNow      int64 = 1_700_000_500
	uaOperator int64 = 555
	uaParent   int64 = 4101
	uaChild    int64 = 4102
	uaArea     int64 = 4103
	uaRoom     int64 = 3301
	uaRoomOwn  int64 = 9301
)

// uaReq 组一条「新建一级启用分区」的请求，area_id=0 走 Insert 分支。
func uaReq(reqID string) *rpc.UpsertAreaReq {
	return &rpc.UpsertAreaReq{
		AreaName: "手工", ParentAreaId: 0, Sort: 7, State: model.AreaStateEnabled,
		OperatorMid: uaOperator, RequestId: reqID,
	}
}

func newUpsertAreaLogic(t *testing.T, st *store) *UpsertAreaLogic {
	t.Helper()
	return NewUpsertAreaLogic(context.Background(), st.svcCtx())
}

// newUpsertAreaLogicWith 用来改 AreaNameMaxLength 这类会跨层漂移的配置。
func newUpsertAreaLogicWith(t *testing.T, st *store, edit func(*config.LiveRoomConf)) *UpsertAreaLogic {
	t.Helper()
	return NewUpsertAreaLogic(context.Background(), st.svcCtxWith(testLiveRoomConfDo(edit)))
}

// uaUpdate 组一条改已有分区的请求（默认改回启用、不带 parent）。
func uaUpdate(reqID string, areaID int64, name string, state int32) *rpc.UpsertAreaReq {
	return &rpc.UpsertAreaReq{
		AreaId: areaID, AreaName: name, Sort: 9, State: state,
		OperatorMid: uaOperator, RequestId: reqID,
	}
}

// TestUpsertAreaInsertTakesGeneratedIDAndSavesResult 骨架用例：新建走 Insert，
// 应答的 area_id 是生成的主键而不是 0，且结果快照回填进幂等表。
func TestUpsertAreaInsertTakesGeneratedIDAndSavesResult(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()

	reply, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-new"))
	wantNoErr(t, "新建分区", err)
	wantSeq(t, "新建分区", st.log, 0,
		"live_room_idempotency.Claim:req-ua-new",
		"live_area.Insert",
		"live_room_idempotency.SaveResult:req-ua-new",
	)
	wantEQ(t, "新建分区应答", "area_id", reply.GetAreaId(), int64(1))
	wantEQ(t, "新建分区应答", "created", reply.GetCreated(), true)
	// 没有事务：分区表是运营侧低频小表，Insert/Update 都是单句。
	wantTxCount(t, "新建分区", st.conn, 0)
	wantMethodCount(t, "新建分区", st.log, "live_area.FindOne", 0)

	row := st.areas.rows[0]
	wantEQ(t, "新建分区落库", "area_name", row.AreaName, "手工")
	wantEQ(t, "新建分区落库", "parent_area_id", row.ParentAreaID, int64(0))
	wantEQ(t, "新建分区落库", "sort", row.Sort, int32(7))
	wantEQ(t, "新建分区落库", "state", row.State, model.AreaStateEnabled)
	wantEQ(t, "新建分区落库", "operator_mid", row.OperatorMid, uaOperator)
	wantEQ(t, "新建分区落库", "ctime=mtime=now", row.Mtime, uaNow)
	wantEQ(t, "新建分区落库", "ctime", row.Ctime, uaNow)

	rec := st.idemAt("req-ua-new")
	if rec == nil {
		t.Fatal("幂等键未登记")
	}
	// 分区运营没有房间归属：claimDedup 传的 room_id/session_id/trace_id 全是 0/空。
	wantEQ(t, "新建分区幂等行", "room_id", rec.RoomID, int64(0))
	wantEQ(t, "新建分区幂等行", "session_id", rec.SessionID, int64(0))
	wantEQ(t, "新建分区幂等行", "kind", rec.Kind, model.IdempotencyKindRequest)
	wantEQ(t, "新建分区幂等行", "rpc", rec.Rpc, rpcUpsertArea)
	wantDeepEQ(t, "新建分区落库面", "counts", st.counts(), storeCounts{areas: 1, idem: 1})
}

// TestUpsertAreaNameConflictComesFromUniqueIndex 钉住第 1 条：冲突只由 uniq_area_name 判，
// 所以**没有任何一次读**——Insert 之前不去查同名，轨迹里只有 Claim + Insert。
func TestUpsertAreaNameConflictComesFromUniqueIndex(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
	st.areas.rows[0].AreaName = "手工"

	in := uaReq("req-ua-dupname")
	in.AreaName = " 手工 " // 两侧空白被 logic 归一掉，撞的仍是同一个名字

	reply, err := newUpsertAreaLogic(t, st).UpsertArea(in)
	wantErrIs(t, "同名分区", err, model.ErrAreaNameConflict)
	if reply != nil {
		t.Fatalf("应答 = %+v, want nil", reply)
	}
	wantSeq(t, "同名分区", st.log, 0,
		"live_room_idempotency.Claim:req-ua-dupname",
		"live_area.Insert",
	)
	wantMethodCount(t, "同名分区", st.log, "live_area.FindByName", 0)
	wantEQ(t, "同名分区残留", "分区行数", st.counts().areas, 1)
	wantEQ(t, "同名分区残留", "mtime 未被改", st.areas.rows[0].Mtime, 6000+uaArea)
	// 键被消费了但没结果：重放同名请求只会报「结果缺失」，不会重新尝试。
	wantKeyBurnedNoResult(t, "同名分区", "req-ua-dupname", st)
}

// TestUpsertAreaParentMustBeEnabledFirstLevel 覆盖 checkParent 的四条拒绝与一条放行。
// 全部断言「抢键之前就被拒」：这些请求不该消费幂等键。
func TestUpsertAreaParentMustBeEnabledFirstLevel(t *testing.T) {
	t.Run("上级是一级启用分区则放行", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))

		in := uaReq("req-ua-child")
		in.ParentAreaId = uaParent
		reply, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantNoErr(t, "合法上级", err)
		wantEQ(t, "合法上级应答", "created", reply.GetCreated(), true)
		wantSeq(t, "合法上级", st.log, 0,
			fmt.Sprintf("live_area.LevelOf:%d", uaParent),
			fmt.Sprintf("live_area.FindOne:%d", uaParent),
			"live_room_idempotency.Claim:req-ua-child",
			"live_area.Insert",
			"live_room_idempotency.SaveResult:req-ua-child",
		)
		wantEQ(t, "合法上级落库", "parent_area_id", st.areas.rows[1].ParentAreaID, uaParent)
	})

	t.Run("上级不存在", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		in := uaReq("req-ua-noparent")
		in.ParentAreaId = uaParent

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		// LevelOf 对「查不到」返回 ErrAreaNotFound（model/live_area.go:235-237），
		// 而 checkParent 只在 (nil,nil) 时才翻译成 ErrAreaParentInvalid（:140-142）——
		// 那条分支在真实 model 下取不到输入，见 README 已知缺口。
		wantErrIs(t, "上级不存在", err, model.ErrAreaNotFound)
		wantSeq(t, "上级不存在", st.log, 0,
			fmt.Sprintf("live_area.LevelOf:%d", uaParent),
			fmt.Sprintf("live_area.FindOne:%d", uaParent),
		)
		wantEQ(t, "上级不存在", "幂等表行数", st.counts().idem, 0)
	})

	t.Run("上级已停用", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaParent, 0, model.AreaStateDisabled))
		in := uaReq("req-ua-disabledparent")
		in.ParentAreaId = uaParent

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantErrIs(t, "上级停用", err, model.ErrAreaParentInvalid)
		wantNoCallAfter(t, "上级停用", st.log, 2)
		wantKeyUnburned(t, "上级停用", "req-ua-disabledparent", st)
	})

	t.Run("三级嵌套被拒", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
		st.seedArea(baseArea(uaChild, uaParent, model.AreaStateEnabled))
		in := uaReq("req-ua-threelevel")
		in.ParentAreaId = uaChild // 父在第 2 层 -> 再挂就是三级

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantErrIs(t, "三级嵌套", err, model.ErrAreaParentInvalid)
		wantSeq(t, "三级嵌套", st.log, 0,
			fmt.Sprintf("live_area.LevelOf:%d", uaChild),
			fmt.Sprintf("live_area.FindOne:%d", uaChild),
			fmt.Sprintf("live_area.FindOne:%d", uaParent),
		)
		wantKeyUnburned(t, "三级嵌套", "req-ua-threelevel", st)
	})

	t.Run("父指向自己", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
		in := uaReq("req-ua-selfparent")
		in.AreaId = uaParent
		in.ParentAreaId = uaParent

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantErrIs(t, "自指", err, model.ErrAreaParentInvalid)
		// self==parent 的短路连 LevelOf 都不该发出去。
		wantNoCallAfter(t, "自指", st.log, 0)
		wantKeyUnburned(t, "自指", "req-ua-selfparent", st)
	})
}

// TestUpsertAreaUpdateOverwritesWholeRow 覆盖改分区主路径：整行覆盖 + 结果回填。
func TestUpsertAreaUpdateOverwritesWholeRow(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))

	reply, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-up", uaArea, "绘画", model.AreaStateEnabled))
	wantNoErr(t, "改分区", err)
	wantSeq(t, "改分区", st.log, 0,
		"live_room_idempotency.Claim:req-ua-up",
		fmt.Sprintf("live_area.FindOne:%d", uaArea),
		fmt.Sprintf("live_area.Update:%d", uaArea),
		"live_room_idempotency.SaveResult:req-ua-up",
	)
	wantEQ(t, "改分区应答", "area_id", reply.GetAreaId(), uaArea)
	wantEQ(t, "改分区应答", "created=false 是改而非建", reply.GetCreated(), false)

	row := st.areas.rows[0]
	wantEQ(t, "改分区落库", "area_name", row.AreaName, "绘画")
	wantEQ(t, "改分区落库", "sort 被覆盖", row.Sort, int32(9))
	wantEQ(t, "改分区落库", "operator 被覆盖", row.OperatorMid, uaOperator)
	wantEQ(t, "改分区落库", "mtime 推进", row.Mtime, uaNow)
	wantEQ(t, "改分区落库", "ctime 不动", row.Ctime, 6000+uaArea)
	// 停用校验没被触发：state 一路都是启用。
	wantMethodCount(t, "改分区", st.log, "live_area.CountChildren", 0)
	wantMethodCount(t, "改分区", st.log, "live_room.CountByArea", 0)
	wantDeepEQ(t, "改分区落库面", "counts", st.counts(), storeCounts{areas: 1, idem: 1})
}

// TestUpsertAreaDisableGuardsAreOrderedAfterClaim 钉住第 2、4、5 条：
// 停用前的占用校验发生在抢键之后，所以被拒时键已被消费；两条占用口径各判一次。
func TestUpsertAreaDisableGuardsAreOrderedAfterClaim(t *testing.T) {
	t.Run("有子分区不得停用", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
		st.seedArea(baseArea(uaChild, uaParent, model.AreaStateEnabled))

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-haschild", uaParent, "户外", model.AreaStateDisabled))
		wantErrIs(t, "有子分区", err, model.ErrAreaInUse)
		wantSeq(t, "有子分区", st.log, 0,
			"live_room_idempotency.Claim:req-ua-haschild",
			fmt.Sprintf("live_area.FindOne:%d", uaParent),
			fmt.Sprintf("live_area.CountChildren:%d", uaParent),
		)
		wantMethodCount(t, "有子分区", st.log, "live_area.Update", 0)
		wantEQ(t, "有子分区残留", "父分区仍启用", st.areas.rows[0].State, model.AreaStateEnabled)
		wantKeyBurnedNoResult(t, "有子分区", "req-ua-haschild", st)
	})

	t.Run("有未关闭房间不得停用", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
		st.seedRoom(baseRoom(uaRoom, uaRoomOwn, model.RoomStateFinished)) // 已关房：不占口径
		st.seedRoom(func() *model.LiveRoom {
			r := baseRoom(uaRoom+1, uaRoomOwn, model.RoomStateLiving)
			r.AreaID = uaArea
			return r
		}())
		st.seedRoom(func() *model.LiveRoom {
			r := baseRoom(uaRoom+2, uaRoomOwn, model.RoomStateBanned)
			r.AreaID = uaArea
			return r
		}())

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-hasroom", uaArea, "跳舞", model.AreaStateDisabled))
		wantErrIs(t, "有在用房间", err, model.ErrAreaInUse)
		wantSeq(t, "有在用房间", st.log, 0,
			"live_room_idempotency.Claim:req-ua-hasroom",
			fmt.Sprintf("live_area.FindOne:%d", uaArea),
			fmt.Sprintf("live_area.CountChildren:%d", uaArea),
			fmt.Sprintf("live_room.CountByArea:%d", uaArea),
		)
		// 占用口径必须正好是 5 个未关闭状态（含 BANNED/DISABLED，排除 FINISHED）。
		if len(st.rooms.countAreaCalls) != 1 {
			t.Fatalf("CountByArea 调用次数 = %d, want 1", len(st.rooms.countAreaCalls))
		}
		wantDeepEQ(t, "占用口径", "states", st.rooms.countAreaCalls[0].states,
			[]int32{model.RoomStatePending, model.RoomStateReady, model.RoomStateLiving,
				model.RoomStateBanned, model.RoomStateDisabled})
		wantEQ(t, "有在用房间残留", "分区仍启用", st.areas.rows[0].State, model.AreaStateEnabled)
	})

	t.Run("停用再停用不触发占用校验", func(t *testing.T) {
		// 第 5 条：cur.State 已经是停用 -> 整条 checkAreaReusable 跳过。
		// 这条与上面两条成对，判别出「只在 启用->停用 这一条边上校验」。
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaArea, 0, model.AreaStateDisabled))
		st.seedRoom(func() *model.LiveRoom {
			r := baseRoom(uaRoom, uaRoomOwn, model.RoomStateLiving)
			r.AreaID = uaArea
			return r
		}())

		reply, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-keepdisabled", uaArea, "手工", model.AreaStateDisabled))
		wantNoErr(t, "停用改停用", err)
		wantEQ(t, "停用改停用", "created", reply.GetCreated(), false)
		wantMethodCount(t, "停用改停用", st.log, "live_area.CountChildren", 0)
		wantMethodCount(t, "停用改停用", st.log, "live_room.CountByArea", 0)
		wantEQ(t, "停用改停用", "分区仍停用", st.areas.rows[0].State, model.AreaStateDisabled)
	})

	t.Run("重新启用不带占用校验", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaArea, 0, model.AreaStateDisabled))
		st.seedRoom(func() *model.LiveRoom {
			r := baseRoom(uaRoom, uaRoomOwn, model.RoomStateLiving)
			r.AreaID = uaArea
			return r
		}())

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-enable", uaArea, "手工", model.AreaStateEnabled))
		wantNoErr(t, "重新启用", err)
		wantMethodCount(t, "重新启用", st.log, "live_area.CountChildren", 0)
		wantEQ(t, "重新启用落库", "state", st.areas.rows[0].State, model.AreaStateEnabled)
	})
}

// TestUpsertAreaUpdateMissIsNotFound 行在 FindOne 之后被删：Update 命中 0 行必须报
// ErrAreaNotFound，绝不回 created=true（第 3 条）。
func TestUpsertAreaUpdateMissIsNotFound(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	defer st.checkRaces(t)
	st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
	st.raceBefore("live_area.Update", func() { st.areas.rows = nil })

	_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-miss", uaArea, "绘画", model.AreaStateEnabled))
	wantErrIs(t, "整行覆盖未命中", err, model.ErrAreaNotFound)
	wantSeq(t, "整行覆盖未命中", st.log, 0,
		"live_room_idempotency.Claim:req-ua-miss",
		fmt.Sprintf("live_area.FindOne:%d", uaArea),
		fmt.Sprintf("live_area.Update:%d", uaArea),
	)
	wantMethodCount(t, "整行覆盖未命中", st.log, "live_room_idempotency.SaveResult", 0)
	wantKeyBurnedNoResult(t, "整行覆盖未命中", "req-ua-miss", st)
}

// TestUpsertAreaMissingRowRejectedAfterClaim Update 分支的 FindOne 在抢键之后：
// 改一个不存在的分区会把键烧掉。与 checkParent 的前置形成对照。
func TestUpsertAreaMissingRowRejectedAfterClaim(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()

	_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-norow", uaArea, "绘画", model.AreaStateEnabled))
	wantErrIs(t, "分区不存在", err, model.ErrAreaNotFound)
	wantSeq(t, "分区不存在", st.log, 0,
		"live_room_idempotency.Claim:req-ua-norow",
		fmt.Sprintf("live_area.FindOne:%d", uaArea),
	)
	wantKeyBurnedNoResult(t, "分区不存在", "req-ua-norow", st)
	wantMethodCount(t, "分区不存在", st.log, "live_area.Update", 0)
}

// TestUpsertAreaReplayTriad 幂等三态：有快照原样回放、无快照报结果缺失、
// 键被别的 RPC 用过报错。重放时 created 沿用首次执行的事实（契约里没有 replayed 位）。
func TestUpsertAreaReplayTriad(t *testing.T) {
	t.Run("有结果快照", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedIdem("req-ua-replay", rpcUpsertArea, `{"area_id":777,"created":true}`)

		reply, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-replay"))
		wantNoErr(t, "重放", err)
		wantSeq(t, "重放", st.log, 0,
			"live_room_idempotency.Claim:req-ua-replay",
			"live_room_idempotency.Find:req-ua-replay",
		)
		wantEQ(t, "重放应答", "area_id 沿用快照", reply.GetAreaId(), int64(777))
		wantEQ(t, "重放应答", "created 沿用快照", reply.GetCreated(), true)
		wantMethodCount(t, "重放", st.log, "live_area.Insert", 0)
		wantEQ(t, "重放", "分区表未被写", st.counts().areas, 0)
	})

	t.Run("无结果快照", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedIdem("req-ua-noreplay", rpcUpsertArea, "")

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-noreplay"))
		wantErrIs(t, "结果缺失", err, model.ErrIdempotencyResultMissing)
		wantSeq(t, "结果缺失", st.log, 0,
			"live_room_idempotency.Claim:req-ua-noreplay",
			"live_room_idempotency.Find:req-ua-noreplay",
		)
	})

	t.Run("键被别的 RPC 用过", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedIdem("req-ua-cross", rpcCloseRoom, `{"state":4}`)

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-cross"))
		wantErrIs(t, "串方法", err, model.ErrRequestIDReused)
		wantErrContains(t, "串方法", err, rpcCloseRoom)
	})

	t.Run("回查失败原样抛出", func(t *testing.T) {
		// Claim 说不是首次之后，Find 这一步依赖失败：只能报错，不能当首次重跑。
		fixClock(t, uaNow)
		st := newStore()
		findFail := errors.New("read timeout")
		st.seedIdem("req-ua-finderr", rpcUpsertArea, `{"area_id":777}`)
		st.idem.failWith("Find", findFail)

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-finderr"))
		wantErrIs(t, "回查失败", err, findFail)
		wantSeq(t, "回查失败", st.log, 0,
			"live_room_idempotency.Claim:req-ua-finderr",
			"live_room_idempotency.Find:req-ua-finderr",
		)
		wantMethodCount(t, "回查失败", st.log, "live_area.Insert", 0)
	})
}

// TestUpsertAreaGuardTableRejectsWithZeroDependencyCalls 入参守卫表：
// 名称长度用**默认配置**（32）时，超必须在触库前被拒；与下面的漂移用例成对。
func TestUpsertAreaGuardTableRejectsWithZeroDependencyCalls(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(in *rpc.UpsertAreaReq)
		want   error
		frag   string
	}{
		{"缺操作者", func(in *rpc.UpsertAreaReq) { in.OperatorMid = 0 }, model.ErrOperatorRequired, ""},
		{"缺幂等键", func(in *rpc.UpsertAreaReq) { in.RequestId = "  " }, model.ErrRequestIDRequired, ""},
		{"超长幂等键", func(in *rpc.UpsertAreaReq) { in.RequestId = strings.Repeat("r", 65) }, model.ErrDedupIDTooLong, "65"},
		{"空分区名", func(in *rpc.UpsertAreaReq) { in.AreaName = "   " }, model.ErrAreaNameInvalid, ""},
		{"默认上限之外的分区名", func(in *rpc.UpsertAreaReq) { in.AreaName = strings.Repeat("名", 33) }, model.ErrAreaNameInvalid, "33 > 32"},
		{"area_id 为负", func(in *rpc.UpsertAreaReq) { in.AreaId = -1 }, model.ErrInvalidAreaID, ""},
		{"parent_area_id 为负", func(in *rpc.UpsertAreaReq) { in.ParentAreaId = -2 }, model.ErrInvalidAreaID, ""},
		{"state 未知取值", func(in *rpc.UpsertAreaReq) { in.State = 3 }, model.ErrAreaStateInvalid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixClock(t, uaNow)
			st := newStore()
			in := uaReq("req-ua-guard")
			tc.mutate(in)

			_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
			wantErrIs(t, tc.name, err, tc.want)
			if tc.frag != "" {
				wantErrContains(t, tc.name, err, tc.frag)
			}
			wantNoCallAfter(t, tc.name, st.log, 0)
			wantEQ(t, tc.name, "幂等表行数", st.counts().idem, 0)
		})
	}
}

// TestUpsertAreaConfigWidthDrift 钉住第 7 条：分区名上限在 logic 侧读 config、
// 在 model 侧硬编码 32（model/live_area.go:25）。把 config.AreaNameMaxLength 放宽到 64 之后，
// 一个 40 字符的名字会被 logic 放行、被 model 拒绝，而**这时幂等键已经烧掉了**。
// 判别对：同样的名字在默认配置（32）下于触库前就被拒、键完好。
// 这一条不是「测幻想」：它把两处常量的耦合变成可失败的断言。
func TestUpsertAreaConfigWidthDrift(t *testing.T) {
	name40 := strings.Repeat("名", 40)

	t.Run("默认配置在触库前就拒", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		in := uaReq("req-ua-w32")
		in.AreaName = name40

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantErrIs(t, "默认宽度", err, model.ErrAreaNameInvalid)
		wantNoCallAfter(t, "默认宽度", st.log, 0)
		wantKeyUnburned(t, "默认宽度", "req-ua-w32", st)
	})

	t.Run("config 放宽后键先烧掉再被 model 拒", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		in := uaReq("req-ua-w64")
		in.AreaName = name40
		l := newUpsertAreaLogicWith(t, st, func(c *config.LiveRoomConf) { c.AreaNameMaxLength = 64 })

		_, err := l.UpsertArea(in)
		wantErrIs(t, "漂移后的 model 拒绝", err, model.ErrAreaNameInvalid)
		wantSeq(t, "漂移后的 model 拒绝", st.log, 0,
			"live_room_idempotency.Claim:req-ua-w64",
			"live_area.Insert",
		)
		wantEQ(t, "漂移后果", "分区表未被写", st.counts().areas, 0)
		// 这才是漂移的实际代价：同一请求重投只会拿到「结果缺失」。
		wantKeyBurnedNoResult(t, "漂移后果", "req-ua-w64", st)
	})
}

// TestUpsertAreaRenameIntoOtherAreasNameIsConflict 改名校到别人头上：
// 由 Update 里的 uniq_area_name 预检等价物拒绝（同名且不是自己）。
func TestUpsertAreaRenameIntoOtherAreasNameIsConflict(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
	st.seedArea(baseArea(uaChild, uaParent, model.AreaStateEnabled))
	st.areas.rows[1].AreaName = "国画"

	_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-rename", uaParent, "国画", model.AreaStateEnabled))
	wantErrIs(t, "改名撞车", err, model.ErrAreaNameConflict)
	wantSeq(t, "改名撞车", st.log, 0,
		"live_room_idempotency.Claim:req-ua-rename",
		fmt.Sprintf("live_area.FindOne:%d", uaParent),
		fmt.Sprintf("live_area.Update:%d", uaParent),
	)
	wantEQ(t, "改名撞车残留", "子分区名未变", st.areas.rows[1].AreaName, "国画")
	wantEQ(t, "改名撞车残留", "父分区名未变", st.areas.rows[0].AreaName, fmt.Sprintf("分区-%d", uaParent))
	wantKeyBurnedNoResult(t, "改名撞车", "req-ua-rename", st)
}

// TestUpsertAreaSameNameOwnRowIsAllowed 与上一条成对：同名的那一行就是自己时不算冲突
// （Update 的判据是 exist.AreaID != a.AreaID）。删掉这个条件两条用例立刻分出胜负。
func TestUpsertAreaSameNameOwnRowIsAllowed(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
	st.areas.rows[0].AreaName = "陶艺"

	reply, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-samename", uaArea, "陶艺", model.AreaStateEnabled))
	wantNoErr(t, "同名即自身", err)
	wantEQ(t, "同名即自身", "created", reply.GetCreated(), false)
	wantMethodCount(t, "同名即自身", st.log, "live_area.Update", 1)
	wantEQ(t, "同名即自身", "mtime 推进", st.areas.rows[0].Mtime, uaNow)
}

// TestUpsertAreaOmittingParentSilentlyDemotesSecondLevel 哨兵用例（README 已知缺口）：
// 改一个二级分区时若不回填 parent_area_id，整行覆盖会把 parent 写成 0，
// 分区被**悄悄降级为一级**；而 parent=0 又让 checkParent 整条跳过，没有任何一道校验拦它。
// TODO(缺陷): 期望行为是 parent_area_id 缺省时沿用 cur.ParentAreaID（或显式要求回填并校验层级）。
// 这里钉的是当前实现的可观察后果，不是设计意图。
func TestUpsertAreaOmittingParentSilentlyDemotesSecondLevel(t *testing.T) {
	fixClock(t, uaNow)
	st := newStore()
	st.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
	st.seedArea(baseArea(uaChild, uaParent, model.AreaStateEnabled))

	in := uaUpdate("req-ua-demote", uaChild, "水彩", model.AreaStateEnabled) // 没带 parent_area_id
	reply, err := newUpsertAreaLogic(t, st).UpsertArea(in)
	wantNoErr(t, "漏传 parent", err)
	wantEQ(t, "漏传 parent", "created", reply.GetCreated(), false)
	wantSeq(t, "漏传 parent", st.log, 0,
		"live_room_idempotency.Claim:req-ua-demote",
		fmt.Sprintf("live_area.FindOne:%d", uaChild),
		// checkParent 没被调用（parent=0 直接跳过），所以轨迹里没有 LevelOf。
		fmt.Sprintf("live_area.Update:%d", uaChild),
		"live_room_idempotency.SaveResult:req-ua-demote",
	)
	wantMethodCount(t, "漏传 parent", st.log, "live_area.LevelOf", 0)
	child := st.areas.rows[1]
	wantEQ(t, "漏传 parent 后果", "child.parent_area_id 被写成 0", child.ParentAreaID, int64(0))
	wantEQ(t, "漏传 parent 后果", "child 已变一级", child.AreaName, "水彩")
	// 判别前提：同一请求只要把 parent_area_id 带上，层级就保得住。
	st2 := newStore()
	st2.seedArea(baseArea(uaParent, 0, model.AreaStateEnabled))
	st2.seedArea(baseArea(uaChild, uaParent, model.AreaStateEnabled))
	withParent := uaUpdate("req-ua-keep", uaChild, "水彩", model.AreaStateEnabled)
	withParent.ParentAreaId = uaParent
	_, err = newUpsertAreaLogic(t, st2).UpsertArea(withParent)
	wantNoErr(t, "带上 parent", err)
	wantSeq(t, "带上 parent", st2.log, 0,
		fmt.Sprintf("live_area.LevelOf:%d", uaParent),
		fmt.Sprintf("live_area.FindOne:%d", uaParent),
		"live_room_idempotency.Claim:req-ua-keep",
		fmt.Sprintf("live_area.FindOne:%d", uaChild),
		fmt.Sprintf("live_area.Update:%d", uaChild),
		"live_room_idempotency.SaveResult:req-ua-keep",
	)
	wantEQ(t, "带上 parent", "层级保住", st2.areas.rows[1].ParentAreaID, uaParent)
}

// TestUpsertAreaDependsOnReadFailurePropagates 依赖失败必须原样抛出（不吞、不降级）。
func TestUpsertAreaDependsOnReadFailurePropagates(t *testing.T) {
	dbFail := errors.New("dial tcp 127.0.0.1:3306: connect refused")

	t.Run("LevelOf 失败", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.areas.failWith("LevelOf", dbFail)
		in := uaReq("req-ua-lvl")
		in.ParentAreaId = uaParent

		_, err := newUpsertAreaLogic(t, st).UpsertArea(in)
		wantErrIs(t, "层级查询失败", err, dbFail)
		wantNoCallAfter(t, "层级查询失败", st.log, 1)
		wantKeyUnburned(t, "层级查询失败", "req-ua-lvl", st)
	})

	t.Run("FindOne 失败", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
		st.areas.failWith("FindOne", dbFail)

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-find", uaArea, "绘画", model.AreaStateEnabled))
		wantErrIs(t, "分区读失败", err, dbFail)
		wantSeq(t, "分区读失败", st.log, 0,
			"live_room_idempotency.Claim:req-ua-find",
			fmt.Sprintf("live_area.FindOne:%d", uaArea),
		)
		wantKeyBurnedNoResult(t, "分区读失败", "req-ua-find", st)
	})

	t.Run("Insert 失败", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.areas.failWith("Insert", dbFail)

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaReq("req-ua-ins"))
		wantErrIs(t, "分区插入失败", err, dbFail)
		wantSeq(t, "分区插入失败", st.log, 0,
			"live_room_idempotency.Claim:req-ua-ins",
			"live_area.Insert",
		)
	})

	t.Run("CountChildren 失败", func(t *testing.T) {
		fixClock(t, uaNow)
		st := newStore()
		st.seedArea(baseArea(uaArea, 0, model.AreaStateEnabled))
		st.areas.failWith("CountChildren", dbFail)

		_, err := newUpsertAreaLogic(t, st).UpsertArea(uaUpdate("req-ua-cc", uaArea, "绘画", model.AreaStateDisabled))
		wantErrIs(t, "子分区计数失败", err, dbFail)
		wantMethodCount(t, "子分区计数失败", st.log, "live_room.CountByArea", 0)
	})
}
