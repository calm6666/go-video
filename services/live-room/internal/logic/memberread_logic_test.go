package logic

// memberread_logic_test.go 覆盖成员与运营审计维度的三个读方法：
// ListAnchors / ListAreas / ListRoomBans。
//
// 这三个方法的「读侧口径」各不相同，用例分别锁定：
//   - ListAnchors 只读 live_room_anchor，不校验房间是否存在（房间删了绑定还在，
//     审计要看得到），且 AnchorListQuery.Offset 到不了 SQL（缺陷，见 README）。
//   - ListAreas 的「不过滤」是 -1 而不是 0：0 对 parent_area_id（一级分区）和
//     state（停用）都是真实取值，所以本方法必须把 0 原样送到 model；
//     分区页大小上限与房间列表不同（200 vs 100），两条通道不得互相放水。
//   - ListRoomBans 是审计列表：必须带 operator_mid 归因，且**不看生效窗口**
//     （end_at 已过但 state 仍是 ACTIVE 的记录照样列出，回收由 cron 负责）。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/live-room/model"
	"go-video/services/live-room/rpc"
)

// --- ListAnchors ---

// TestListAnchorsOrdersByRoleThenID 锁住「顺序就是 model 的 ORDER BY，不是 seed 顺序」：
// seed 顺序刻意与期望顺序不同，期望顺序是 role ASC, id ASC（model/live_room_anchor.go:194）。
func TestListAnchorsOrdersByRoleThenID(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(13, 101, 9003, model.AnchorRoleManager))
	st.seedAnchor(baseAnchor(12, 101, 9002, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(14, 101, 9004, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})

	wantNoErr(t, "ListAnchors", err)
	wantInt64sEQ(t, "绑定顺序", "anchors", anchorIDs(reply.GetAnchors()), []int64{11, 12, 14, 13})
	wantEQ(t, "ListAnchors", "total", reply.GetTotal(), int32(4))
	// List 与 Count 必须收到同一个条件对象，否则「total 与页内容同口径」不成立。
	wantSeq(t, "ListAnchors", st.log, 0, "live_room_anchor.List:101", "live_room_anchor.Count:101")
	if len(st.anchors.listQueries) != 1 || len(st.anchors.countQueries) != 1 {
		t.Fatalf("条件捕获数 list=%d count=%d, want 1/1", len(st.anchors.listQueries), len(st.anchors.countQueries))
	}
	wantDeepEQ(t, "ListAnchors 条件", "query", st.anchors.listQueries[0], model.AnchorListQuery{
		RoomID: 101, Limit: 20,
	})
	wantDeepEQ(t, "ListAnchors 同口径", "count 条件 == list 条件", st.anchors.countQueries[0], st.anchors.listQueries[0])
	wantNoDirectSQL(t, "ListAnchors", st.conn)
}

// TestListAnchorsProjectsEveryExposedField 逐字段核对投影，重点是 role/state 的
// 原值透传（本服务不把「已解绑」藏起来，可见性由调用方决定）。
func TestListAnchorsProjectsEveryExposedField(t *testing.T) {
	st := newStore()
	row := st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})

	wantNoErr(t, "ListAnchors(投影)", err)
	if len(reply.GetAnchors()) != 1 {
		t.Fatalf("anchors 条数 = %d, want 1", len(reply.GetAnchors()))
	}
	info := reply.GetAnchors()[0]
	wantEQ(t, "投影", "id", info.GetId(), row.ID)
	wantEQ(t, "投影", "room_id", info.GetRoomId(), row.RoomID)
	wantEQ(t, "投影", "mid", info.GetMid(), row.Mid)
	wantEQ(t, "投影", "role", info.GetRole(), rpc.AnchorRole_ANCHOR_ROLE_OWNER)
	wantEQ(t, "投影", "state", info.GetState(), model.BindStateEnabled)
	wantEQ(t, "投影", "ctime", info.GetCtime(), row.Ctime)
	wantEQ(t, "投影", "mtime", info.GetMtime(), row.Mtime)
}

func TestListAnchorsRejectsMissingRoomIDBeforeAnyQuery(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListAnchorsReq
	}{
		{"nil 请求", nil},
		{"room_id 为 0", &rpc.ListAnchorsReq{}},
		{"room_id 为负", &rpc.ListAnchorsReq{RoomId: -1}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
		logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListAnchors(tc.in)

		wantErrIs(t, "ListAnchors("+tc.name+")", err, model.ErrInvalidRoomID)
		wantEQ(t, "ListAnchors("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListAnchors("+tc.name+")", st.log, 0)
	}
}

// TestListAnchorsRoleFilterRejectsUndefinedValue 锁「未知取值不等于不过滤」：
// 若这里放行，运营传错枚举就会看到全量成员。
func TestListAnchorsRoleFilterRejectsUndefinedValue(t *testing.T) {
	cases := []struct {
		name string
		role rpc.AnchorRole
	}{
		{"role=9 未定义", rpc.AnchorRole(9)},
		{"role=-1 负数", rpc.AnchorRole(-1)},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
		logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Role: tc.role})

		wantErrIs(t, "ListAnchors("+tc.name+")", err, model.ErrAnchorRoleInvalid)
		wantErrContains(t, "ListAnchors("+tc.name+")", err, "role=")
		wantEQ(t, "ListAnchors("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListAnchors("+tc.name+")", st.log, 0)
	}

	// UNSPECIFIED 才是不过滤：条件里 Role 必须是 0 且能读到全部角色。
	st := newStore()
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.seedAnchor(baseAnchor(12, 101, 9002, model.AnchorRoleCohost))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())
	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Role: rpc.AnchorRole_ANCHOR_ROLE_UNSPECIFIED})
	wantNoErr(t, "ListAnchors(UNSPECIFIED)", err)
	wantEQ(t, "ListAnchors(UNSPECIFIED)", "total", reply.GetTotal(), int32(2))
	wantEQ(t, "ListAnchors(UNSPECIFIED)", "条件 Role", st.anchors.listQueries[0].Role, int32(model.AnchorRoleUnspecified))
}

func TestListAnchorsRoleFilterNarrowsListAndTotalTogether(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.seedAnchor(baseAnchor(12, 101, 9002, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(13, 101, 9003, model.AnchorRoleCohost))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Role: rpc.AnchorRole_ANCHOR_ROLE_COHOST})

	wantNoErr(t, "ListAnchors(role=COHOST)", err)
	wantInt64sEQ(t, "ListAnchors(role=COHOST)", "anchors", anchorIDs(reply.GetAnchors()), []int64{12, 13})
	wantEQ(t, "页内容与总数同口径", "total", reply.GetTotal(), int32(2))
	wantEQ(t, "ListAnchors(role=COHOST)", "条件 Role", st.anchors.listQueries[0].Role, int32(model.AnchorRoleCohost))
}

// TestListAnchorsOnlyEnabledHidesUnboundRow 锁「解绑是置位不是删除」的读侧后果：
// 不开 only_enabled 时已解绑行仍在，开了才滤掉，且 total 必须跟着变。
func TestListAnchorsOnlyEnabledHidesUnboundRow(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	enabledCohost := st.seedAnchor(baseAnchor(12, 101, 9002, model.AnchorRoleCohost))
	unbound := baseAnchor(13, 101, 9003, model.AnchorRoleManager)
	unbound.State = model.BindStateDisabled
	st.seedAnchor(unbound)
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	full, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})
	wantNoErr(t, "ListAnchors(全部)", err)
	wantInt64sEQ(t, "含已解绑", "anchors", anchorIDs(full.GetAnchors()), []int64{11, 12, 13})
	wantEQ(t, "含已解绑", "total", full.GetTotal(), int32(3))

	enabled, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, OnlyEnabled: true})
	wantNoErr(t, "ListAnchors(仅生效)", err)
	wantInt64sEQ(t, "仅生效", "anchors", anchorIDs(enabled.GetAnchors()), []int64{11, enabledCohost.ID})
	wantEQ(t, "仅生效的 total 必须跟着缩", "total", enabled.GetTotal(), int32(2))
	wantEQ(t, "仅生效", "条件 OnlyEnabled", st.anchors.listQueries[1].OnlyEnabled, true)
	wantDeepEQ(t, "仅生效同口径", "count 条件", st.anchors.countQueries[1], st.anchors.listQueries[1])
}

func TestListAnchorsEmptyRoomIsZeroTotalNotNil(t *testing.T) {
	st := newStore()
	st.seedRoom(baseRoom(101, 9001, model.RoomStateReady))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})

	wantNoErr(t, "ListAnchors(无绑定)", err)
	if reply == nil {
		t.Fatal("空结果必须回 reply，不能回 nil")
	}
	wantEQ(t, "无绑定", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "无绑定", "anchors 条数", len(reply.GetAnchors()), 0)
	// 空列表不等于「房间不存在」：本方法不读 live_room，也不报 ErrRoomNotFound。
	wantSeq(t, "无绑定", st.log, 0, "live_room_anchor.List:101", "live_room_anchor.Count:101")
	wantMethodCount(t, "成员列表不得读房间表", st.log, "live_room.FindOne", 0)
}

// TestListAnchorsRoomExistenceNotChecked 房间不存在也照常返回空列表：
// 「房间是否还在」不是本方法的语义（终端要展示历史成员时房间可能已关闭）。
func TestListAnchorsRoomExistenceNotChecked(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 999})

	wantNoErr(t, "ListAnchors(陌生 room_id)", err)
	wantEQ(t, "陌生 room_id", "total", reply.GetTotal(), int32(0))
	wantSeq(t, "陌生 room_id", st.log, 0, "live_room_anchor.List:999", "live_room_anchor.Count:999")
	wantMethodCount(t, "不得回查房间表", st.log, "live_room.FindOne", 0)
}

func TestListAnchorsValidatesPageSizeAndPageBeforeQuery(t *testing.T) {
	st := newStore()
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	// 房间列表通道的上限是 100：150 必须拒绝（分区通道的 200 不得泄漏到这里）。
	_, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, PageSize: 150})
	wantErrIs(t, "ListAnchors(页大小越界)", err, model.ErrPageSizeTooLarge)
	wantErrContains(t, "ListAnchors(页大小越界)", err, "max=100")

	_, err = logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Page: -1})
	wantErrIs(t, "ListAnchors(负页码)", err, model.ErrInvalidPage)

	// 校验顺序：room_id → role → page_size → page。四个都错时先报最外层的必填键。
	_, err = logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 0, Role: rpc.AnchorRole(9), Page: -1, PageSize: 999})
	wantErrIs(t, "ListAnchors(校验顺序)", err, model.ErrInvalidRoomID)
	_, err = logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Role: rpc.AnchorRole(9), Page: -1, PageSize: 999})
	wantErrIs(t, "ListAnchors(校验顺序)", err, model.ErrAnchorRoleInvalid)
	_, err = logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Page: -1, PageSize: 999})
	wantErrIs(t, "ListAnchors(校验顺序)", err, model.ErrPageSizeTooLarge)
	wantNoCallAfter(t, "ListAnchors(入参守卫)", st.log, 0)
}

// TestListAnchorsOffsetIsComputedButDroppedByModelSQL 锁缺陷：
// logic 正确折算出 offset，但 model/live_room_anchor.go:194 的 SQL 只有 `LIMIT ?`，
// 没有 OFFSET 子句，所以第 2 页内容与第 1 页完全相同，而 total 仍显示还有下一页。
// 这里按当前真实行为断言并保留 total 的证据；修复应落在 model 侧（补 OFFSET）。
func TestListAnchorsOffsetIsComputedButDroppedByModelSQL(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(21, 101, 9001, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(22, 101, 9002, model.AnchorRoleCohost))
	st.seedAnchor(baseAnchor(23, 101, 9003, model.AnchorRoleCohost))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	first, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Page: 1, PageSize: 2})
	wantNoErr(t, "第一页", err)
	second, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, Page: 2, PageSize: 2})
	wantNoErr(t, "第二页", err)

	wantEQ(t, "logic 侧 offset 算对了", "第二页 offset", st.anchors.listQueries[1].Offset, int32(2))
	wantEQ(t, "第一页 offset", "offset", st.anchors.listQueries[0].Offset, int32(0))

	wantInt64sEQ(t, "第一页内容", "anchors", anchorIDs(first.GetAnchors()), []int64{21, 22})
	wantInt64sEQ(t, "缺陷：第二页与第一页同内容", "anchors", anchorIDs(second.GetAnchors()), []int64{21, 22})
	wantEQ(t, "总数说明还有下一页", "total", second.GetTotal(), int32(3))
}

func TestListAnchorsPropagatesStoreErrorsWithoutCounting(t *testing.T) {
	st := newStore()
	st.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st.anchors.failWith("List", errors.New("db down"))
	logic := NewListAnchorsLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})
	wantErrContains(t, "ListAnchors(List 故障)", err, "db down")
	wantEQ(t, "ListAnchors(List 故障)", "reply", reply, nil)
	// 取页失败就不该再数总数：两次 SQL 的口径此时必然不一致。
	wantMethodCount(t, "List 失败后不得再 Count", st.log, "live_room_anchor.Count", 0)

	st2 := newStore()
	st2.seedAnchor(baseAnchor(11, 101, 9001, model.AnchorRoleOwner))
	st2.anchors.failWith("Count", errors.New("count boom"))
	logic2 := NewListAnchorsLogic(context.Background(), st2.svcCtx())
	reply2, err := logic2.ListAnchors(&rpc.ListAnchorsReq{RoomId: 101})
	wantErrContains(t, "ListAnchors(Count 故障)", err, "count boom")
	wantEQ(t, "ListAnchors(Count 故障)", "reply", reply2, nil)
	wantSeq(t, "Count 故障", st2.log, 0, "live_room_anchor.List:101", "live_room_anchor.Count:101")
}

// --- ListAreas ---

// areaSorted 在 baseArea 之上显式给 sort：baseArea 的 sort 派生自 area_id，
// 想让「排序键」与「主键」给出不同答案必须手工分开设置，否则顺序断言只是在测主键。
func areaSorted(areaID, parent int64, state, sort int32) *model.LiveArea {
	a := baseArea(areaID, parent, state)
	a.Sort = sort
	return a
}

// TestListAreasOrdersByParentSortID 锁 ORDER BY parent_area_id ASC, sort ASC, area_id ASC。
// 三列的优先级用「sort 与 area_id 方向相反」+「sort 相同才轮到 area_id」两组数据分开证明。
func TestListAreasOrdersByParentSortID(t *testing.T) {
	st := newStore()
	st.seedArea(areaSorted(7001, 0, model.AreaStateEnabled, 2))
	st.seedArea(areaSorted(7002, 0, model.AreaStateDisabled, 1))
	st.seedArea(areaSorted(7003, 7001, model.AreaStateEnabled, 5))
	st.seedArea(areaSorted(7004, 7001, model.AreaStateEnabled, 5))
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: areaCacheNoFilter, State: areaCacheNoFilter})

	wantNoErr(t, "ListAreas(不过滤)", err)
	// parent 0 组按 sort 升序 → 7002(sort1), 7001(sort2)；parent 7001 组 sort 相同 → area_id 升序。
	wantInt64sEQ(t, "分区顺序", "areas", areaIDs(reply.GetAreas()), []int64{7002, 7001, 7003, 7004})
	wantEQ(t, "ListAreas(不过滤)", "total", reply.GetTotal(), int32(4))
	wantSeq(t, "ListAreas", st.log, 0, "live_area.List", "live_area.Count")
	wantDeepEQ(t, "ListAreas 条件", "query", st.areas.listQueries[0], model.AreaListQuery{
		ParentAreaID: -1, State: -1, Limit: 20,
	})
	wantDeepEQ(t, "ListAreas 同口径", "count 条件", st.areas.countQueries[0], st.areas.listQueries[0])
	wantNoDirectSQL(t, "ListAreas", st.conn)
}

// TestListAreasProjectsEveryExposedField 逐字段核对分区投影。
func TestListAreasProjectsEveryExposedField(t *testing.T) {
	st := newStore()
	row := st.seedArea(&model.LiveArea{
		AreaID: 7001, AreaName: "户外", ParentAreaID: 0, Sort: 7,
		State: model.AreaStateEnabled, OperatorMid: 555, Ctime: 111, Mtime: 222,
	})
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAreas(&rpc.ListAreasReq{State: areaCacheNoFilter})

	wantNoErr(t, "ListAreas(投影)", err)
	if len(reply.GetAreas()) != 1 {
		t.Fatalf("areas 条数 = %d, want 1", len(reply.GetAreas()))
	}
	info := reply.GetAreas()[0]
	wantEQ(t, "投影", "area_id", info.GetAreaId(), row.AreaID)
	wantEQ(t, "投影", "area_name", info.GetAreaName(), "户外")
	wantEQ(t, "投影", "parent_area_id", info.GetParentAreaId(), row.ParentAreaID)
	wantEQ(t, "投影", "sort", info.GetSort(), row.Sort)
	wantEQ(t, "投影", "state", info.GetState(), model.AreaStateEnabled)
	wantEQ(t, "投影", "operator_mid", info.GetOperatorMid(), row.OperatorMid)
	wantEQ(t, "投影", "ctime", info.GetCtime(), int64(111))
	wantEQ(t, "投影", "mtime", info.GetMtime(), int64(222))
}

// TestListAreasNilRequestReturnsEmptyReply 锁本方法与 ListAnchors 的口径差异：
// nil 请求不报错，回空 reply 且不触库。这条分支存在是因为 rpc 允许 nil，
// 「报 ErrInvalidAreaID」在这里没有意义（没有任何过滤条件被写错）。
func TestListAreasNilRequestReturnsEmptyReply(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAreas(nil)

	wantNoErr(t, "ListAreas(nil)", err)
	if reply == nil {
		t.Fatal("ListAreas(nil) 必须回一个空 reply 而不是 nil")
	}
	wantEQ(t, "ListAreas(nil)", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "ListAreas(nil)", "areas 条数", len(reply.GetAreas()), 0)
	wantNoCallAfter(t, "ListAreas(nil)", st.log, 0)
}

// TestListAreasZeroIsARealFilterNotNoFilter 锁住最容易被「顺手归一」写错的一条：
// parent_area_id=0 表示「只取一级分区」，state=0 表示「只取停用分区」，
// 两者都必须原样送到 model；只有 -1 才是不过滤。
func TestListAreasZeroIsARealFilterNotNoFilter(t *testing.T) {
	cases := []struct {
		name       string
		parent     int64
		state      int32
		wantIDs    []int64
		wantParent int64
		wantState  int32
	}{
		// baseArea 的 sort = area_id % 100，所以同级就是 7001(1) → 7002(2) → 7003(3)。
		{"不过滤", areaCacheNoFilter, areaCacheNoFilter, []int64{7001, 7002, 7003}, -1, -1},
		{"只取一级分区", 0, areaCacheNoFilter, []int64{7001, 7002}, 0, -1},
		{"只取停用分区", areaCacheNoFilter, model.AreaStateDisabled, []int64{7002}, -1, 0},
		{"只取二级启用分区", 7001, model.AreaStateEnabled, []int64{7003}, 7001, 1},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))  // 一级·启用
		st.seedArea(baseArea(7002, 0, model.AreaStateDisabled)) // 一级·停用
		st.seedArea(baseArea(7003, 7001, model.AreaStateEnabled))
		logic := NewListAreasLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: tc.parent, State: tc.state})

		wantNoErr(t, "ListAreas("+tc.name+")", err)
		wantInt64sEQ(t, "ListAreas("+tc.name+")", "areas", areaIDs(reply.GetAreas()), tc.wantIDs)
		wantEQ(t, "ListAreas("+tc.name+")", "total", reply.GetTotal(), int32(len(tc.wantIDs)))
		wantDeepEQ(t, "ListAreas("+tc.name+") 条件原样送达", "query", st.areas.listQueries[0], model.AreaListQuery{
			ParentAreaID: tc.wantParent, State: tc.wantState, Limit: 20,
		})
	}
}

func TestListAreasRejectsOutOfRangeFiltersInFixedOrder(t *testing.T) {
	st := newStore()
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	// 校验顺序：parent_area_id → state → page_size → page。
	_, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -2, State: 9, Page: -1, PageSize: 999})
	wantErrIs(t, "ListAreas(校验顺序)", err, model.ErrInvalidAreaID)

	_, err = logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: 0, State: 2, Page: -1, PageSize: 999})
	wantErrIs(t, "ListAreas(校验顺序)", err, model.ErrAreaStateInvalid)

	_, err = logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: 0, State: -1, Page: -1, PageSize: 999})
	wantErrIs(t, "ListAreas(校验顺序)", err, model.ErrPageSizeTooLarge)

	_, err = logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: 0, State: -1, Page: -1, PageSize: 10})
	wantErrIs(t, "ListAreas(校验顺序)", err, model.ErrInvalidPage)
	wantNoCallAfter(t, "ListAreas(入参守卫)", st.log, 0)

	// 边界：-1 放行、-2 拒绝；state 1 放行、2 拒绝。
	_, err = logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: model.AreaStateEnabled})
	wantNoErr(t, "ListAreas(边界放行)", err)
}

// TestAreaPageSizeCapIsIndependentFromRoomListCap 锁两条通道的上限互不串用：
// 150 在分区通道合法（配置 MaxAreaPageSize=200），在成员/房间通道必须被拒。
func TestAreaPageSizeCapIsIndependentFromRoomListCap(t *testing.T) {
	st := newStore()
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1, Page: 1, PageSize: 150})
	wantNoErr(t, "ListAreas(page_size=150)", err)
	wantEQ(t, "分区通道接受 150", "limit", st.areas.listQueries[0].Limit, int32(150))
	wantEQ(t, "ListAreas(150)", "total", reply.GetTotal(), int32(0))

	_, err = logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1, PageSize: 201})
	wantErrIs(t, "ListAreas(page_size=201)", err, model.ErrPageSizeTooLarge)
	wantErrContains(t, "ListAreas(page_size=201)", err, "max=200")

	_, err = NewListAnchorsLogic(context.Background(), st.svcCtx()).
		ListAnchors(&rpc.ListAnchorsReq{RoomId: 101, PageSize: 150})
	wantErrIs(t, "成员通道不接受 150", err, model.ErrPageSizeTooLarge)
}

// TestListAreasPagingHonoursOffset 分区列表的 SQL 是 `LIMIT ? OFFSET ?`，
// 所以分页真的前进——与 live_room.List / live_room_anchor.List 的缺陷形成对照
// （那两条没有 OFFSET 子句）。
func TestListAreasPagingHonoursOffset(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))
	st.seedArea(baseArea(7002, 0, model.AreaStateEnabled))
	st.seedArea(baseArea(7003, 0, model.AreaStateEnabled))
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	first, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1, Page: 1, PageSize: 2})
	wantNoErr(t, "第一页", err)
	second, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1, Page: 2, PageSize: 2})
	wantNoErr(t, "第二页", err)

	wantInt64sEQ(t, "第一页", "areas", areaIDs(first.GetAreas()), []int64{7001, 7002})
	wantInt64sEQ(t, "第二页", "areas", areaIDs(second.GetAreas()), []int64{7003})
	wantEQ(t, "第二页 offset", "offset", st.areas.listQueries[1].Offset, int32(2))
	wantEQ(t, "第二页 total", "total", second.GetTotal(), int32(3))
}

// TestListAreasWithoutCacheAlwaysGoesToMySQL 锁 AGENTS.md §5 的读侧降级口径：
// 配了 TTL 但 Cache 未接入时，每次都必须回源 MySQL，且不得因为 nil client 而
// 返回空列表或报错（缓存是加速手段，不是数据源）。
// 缓存命中/回填两支无法纯内存覆盖（*redis.Redis 是具体类型，没有接口缝）。
func TestListAreasWithoutCacheAlwaysGoesToMySQL(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))
	conf := testLiveRoomConf()
	conf.AreaListCacheTTLSeconds = 300
	logic := NewListAreasLogic(context.Background(), st.svcCtxWith(conf))

	if logic.svcCtx.Cache != nil {
		t.Fatal("本用例前提是 Cache 未接入")
	}
	for i := 0; i < 3; i++ {
		reply, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1})
		wantNoErr(t, "ListAreas(无缓存)", err)
		wantEQ(t, "无缓存仍要读到数据", "total", reply.GetTotal(), int32(1))
	}
	wantCount(t, "三次请求都要回源", st.log, "live_area.List", 3)
	wantCount(t, "三次请求都要数总数", st.log, "live_area.Count", 3)
}

func TestListAreasEmptyResultAndStoreError(t *testing.T) {
	st := newStore()
	st.seedArea(baseArea(7001, 0, model.AreaStateEnabled))
	logic := NewListAreasLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListAreas(&rpc.ListAreasReq{ParentAreaId: 999999, State: -1})
	wantNoErr(t, "ListAreas(空结果)", err)
	wantEQ(t, "空结果", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "空结果", "areas 条数", len(reply.GetAreas()), 0)
	wantSeq(t, "空结果仍要数总数", st.log, 0, "live_area.List", "live_area.Count")

	st2 := newStore()
	st2.areas.failWith("List", errors.New("db down"))
	logic2 := NewListAreasLogic(context.Background(), st2.svcCtx())
	reply2, err := logic2.ListAreas(&rpc.ListAreasReq{ParentAreaId: -1, State: -1})
	wantErrContains(t, "ListAreas(List 故障)", err, "db down")
	wantEQ(t, "ListAreas(List 故障)", "reply", reply2, nil)
	wantMethodCount(t, "List 失败后不得再 Count", st2.log, "live_area.Count", 0)
}

// --- ListRoomBans ---

// banWithMid 覆盖 baseBan 由 room_id 派生的 mid，让「按主播过滤」有独立区分度。
func banWithMid(b *model.LiveRoomBan, mid int64) *model.LiveRoomBan {
	b.Mid = mid
	return b
}

func TestListRoomBansProjectsAuditFields(t *testing.T) {
	st := newStore()
	row := st.seedBan(&model.LiveRoomBan{
		BanID: 301, RoomID: 101, Mid: 1001, BanType: model.BanTypeTemporary,
		Reason: "涉政内容", StartAt: 1000, EndAt: 2000, State: model.BanStateActive,
		OperatorMid: 555, TraceID: "trace-301", Ctime: 900,
	})
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, OperatorMid: 555})

	wantNoErr(t, "ListRoomBans", err)
	if len(reply.GetBans()) != 1 {
		t.Fatalf("bans 条数 = %d, want 1", len(reply.GetBans()))
	}
	info := reply.GetBans()[0]
	wantEQ(t, "投影", "ban_id", info.GetBanId(), row.BanID)
	wantEQ(t, "投影", "room_id", info.GetRoomId(), row.RoomID)
	wantEQ(t, "投影", "mid", info.GetMid(), row.Mid)
	wantEQ(t, "投影", "ban_type", info.GetBanType(), rpc.BanType_BAN_TYPE_TEMPORARY)
	wantEQ(t, "投影", "state", info.GetState(), model.BanStateActive)
	wantEQ(t, "投影", "start_at", info.GetStartAt(), row.StartAt)
	wantEQ(t, "投影", "end_at", info.GetEndAt(), row.EndAt)
	wantEQ(t, "投影", "operator_mid", info.GetOperatorMid(), row.OperatorMid)
	// reason / lift_reason 是本服务对运营面的读出口：不在这里脱敏，
	// 因为「猜谁是运营」是越权风险最高的地方；脱敏属 gateway/admin。
	wantEQ(t, "投影", "reason", info.GetReason(), "涉政内容")
	wantEQ(t, "投影", "lift_reason", info.GetLiftReason(), "")
	wantEQ(t, "投影", "lift_operator_mid", info.GetLiftOperatorMid(), int64(0))
	wantEQ(t, "投影", "lifted_at", info.GetLiftedAt(), int64(0))
	wantEQ(t, "投影", "ctime", info.GetCtime(), row.Ctime)
	wantNoDirectSQL(t, "ListRoomBans", st.conn)
}

// TestListRoomBansRequiresOperatorAttribution 无归因主体的审计读取不允许发生：
// nil 与 operator_mid<=0 都必须报 ErrOperatorRequired，且一次依赖调用都不发生。
func TestListRoomBansRequiresOperatorAttribution(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListRoomBansReq
	}{
		{"nil 请求", nil},
		{"operator_mid 缺省", &rpc.ListRoomBansReq{RoomId: 101}},
		{"operator_mid 为负", &rpc.ListRoomBansReq{RoomId: 101, OperatorMid: -5}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedBan(baseBan(301, 101, model.BanStateActive))
		logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListRoomBans(tc.in)

		wantErrIs(t, "ListRoomBans("+tc.name+")", err, model.ErrOperatorRequired)
		wantEQ(t, "ListRoomBans("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListRoomBans("+tc.name+")", st.log, 0)
	}
}

// TestListRoomBansRejectsNegativeRoomIDOrMid 锁当前真实行为：room_id 与 mid
// 共用一条 `||` 守卫，所以**负 mid 报的是 ErrInvalidRoomID**。
// 这是缺陷（错误消息会指认错位的字段），但行为本身（拒绝负数）是对的，
// 所以按现状断言并把缺陷登记在 README「已知缺口」。
func TestListRoomBansRejectsNegativeRoomIDOrMid(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListRoomBansReq
	}{
		{"room_id 为负", &rpc.ListRoomBansReq{RoomId: -1, OperatorMid: 555}},
		{"mid 为负", &rpc.ListRoomBansReq{Mid: -1, OperatorMid: 555}},
	}
	for _, tc := range cases {
		st := newStore()
		st.seedBan(baseBan(301, 101, model.BanStateActive))
		logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

		reply, err := logic.ListRoomBans(tc.in)

		wantErrIs(t, "缺陷待修：负数一律报 ErrInvalidRoomID｜"+tc.name, err, model.ErrInvalidRoomID)
		wantEQ(t, "ListRoomBans("+tc.name+")", "reply", reply, nil)
		wantNoCallAfter(t, "ListRoomBans("+tc.name+")", st.log, 0)
	}
}

func TestListRoomBansStateFilterRejectsUndefinedValue(t *testing.T) {
	st := newStore()
	st.seedBan(baseBan(301, 101, model.BanStateActive))
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	// 0 才是不过滤；未定义取值拒绝，不能退化成「不过滤」。
	_, err := logic.ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555, State: 4})
	wantErrIs(t, "ListRoomBans(state=4)", err, model.ErrInvalidBanTransition)
	wantErrContains(t, "ListRoomBans(state=4)", err, "state=4")

	_, err = logic.ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555, State: -1})
	wantErrIs(t, "ListRoomBans(state=-1)", err, model.ErrInvalidBanTransition)

	wantNoCallAfter(t, "ListRoomBans(状态守卫)", st.log, 0)

	// 0 → 不过滤：条件里 State 必须是 0，且三种状态都能看到。
	st2 := newStore()
	st2.seedBan(baseBan(301, 101, model.BanStateActive))
	lifted := baseBan(302, 101, model.BanStateLifted)
	st2.seedBan(lifted)
	expired := baseBan(303, 101, model.BanStateExpired)
	st2.seedBan(expired)
	all, err := NewListRoomBansLogic(context.Background(), st2.svcCtx()).
		ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555})
	wantNoErr(t, "ListRoomBans(不过滤)", err)
	wantInt64sEQ(t, "三种状态都在", "bans", banIDs(all.GetBans()), []int64{303, 302, 301})
	wantEQ(t, "不过滤", "条件 State", st2.bans.listQueries[0].State, int32(0))
}

// TestListRoomBansAuditListIgnoresEffectiveWindow 是本方法最关键的一条口径：
// 审计列表的 WHERE 里没有任何时间谓词（model/live_room_ban.go:banWhere 只有
// room_id / mid / state 三个条件），所以 end_at 已过的 ACTIVE 记录**仍然会列出**。
// 与之相对，判定「现在还在不在禁播中」的 FindActiveByRoom 才看 end_at > now。
// 也就是说：过期记录从「生效」变成 state=EXPIRED 依赖 cron 回收，
// 在 cron 跑之前，本方法读到的 ACTIVE 是「记录状态」而不是「此刻生效」。
func TestListRoomBansAuditListIgnoresEffectiveWindow(t *testing.T) {
	st := newStore()
	// 临时禁播，end_at 早已过去，但 state 仍是 ACTIVE（未被 SweepExpired 回收）。
	stale := st.seedBan(&model.LiveRoomBan{
		BanID: 301, RoomID: 101, Mid: 1001, BanType: model.BanTypeTemporary,
		Reason: "临时违规", StartAt: 1, EndAt: 2, State: model.BanStateActive,
		OperatorMid: 555, Ctime: 3,
	})
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, State: model.BanStateActive, OperatorMid: 555})

	wantNoErr(t, "ListRoomBans(过期未回收)", err)
	wantEQ(t, "当前真实行为：窗口已过仍按 ACTIVE 列出", "total", reply.GetTotal(), int32(1))
	wantEQ(t, "列表不裁窗口", "end_at", reply.GetBans()[0].GetEndAt(), stale.EndAt)
	// 时间窗口要由调用方自己按 start_at/end_at 判，或者交给 FindActiveByRoom。
	wantDeepEQ(t, "条件里没有 now/时间字段", "query", st.bans.listQueries[0],
		model.BanListQuery{RoomID: 101, State: model.BanStateActive, Limit: 20})
}

// TestListRoomBansLiftedRowKeepsLiftFields 已解除记录要能审计回「谁解除的」：
// lift_* 三列必须原样下发，否则解除动作无法追责。
func TestListRoomBansLiftedRowKeepsLiftFields(t *testing.T) {
	st := newStore()
	row := st.seedBan(&model.LiveRoomBan{
		BanID: 302, RoomID: 101, Mid: 1001, BanType: model.BanTypePermanent,
		Reason: "永久违规", StartAt: 1000, EndAt: 0, State: model.BanStateLifted,
		OperatorMid: 555, LiftOperatorMid: 777, LiftReason: "申诉通过", LiftedAt: 1600,
		Ctime: 900,
	})
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555, State: model.BanStateLifted})

	wantNoErr(t, "ListRoomBans(已解除)", err)
	wantEQ(t, "已解除", "total", reply.GetTotal(), int32(1))
	info := reply.GetBans()[0]
	wantEQ(t, "已解除", "state", info.GetState(), model.BanStateLifted)
	wantEQ(t, "已解除归因", "lift_operator_mid", info.GetLiftOperatorMid(), row.LiftOperatorMid)
	wantEQ(t, "已解除归因", "lift_reason", info.GetLiftReason(), "申诉通过")
	wantEQ(t, "已解除归因", "lifted_at", info.GetLiftedAt(), int64(1600))
}

func TestListRoomBansFiltersTotalAndPageWithSameQuery(t *testing.T) {
	// baseBan 的 mid 派生自 room_id，同一房间的几条记录 mid 会相同，
	// 那样「按 mid 过滤」和「按 room 过滤」的区分度是 0，所以这里显式给 mid。
	st := newStore()
	st.seedBan(banWithMid(baseBan(301, 101, model.BanStateActive), 1001))
	st.seedBan(banWithMid(baseBan(302, 101, model.BanStateLifted), 1002))
	st.seedBan(banWithMid(baseBan(303, 102, model.BanStateActive), 1001))
	st.seedBan(banWithMid(baseBan(304, 102, model.BanStateActive), 1003))
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	// 按房间过滤
	byRoom, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 102, OperatorMid: 555})
	wantNoErr(t, "ListRoomBans(room_id=102)", err)
	wantInt64sEQ(t, "按房间", "bans", banIDs(byRoom.GetBans()), []int64{304, 303})
	wantEQ(t, "按房间的 total", "total", byRoom.GetTotal(), int32(2))

	// 按主播过滤：同一个 mid 在两个房间都被禁过，跨房间聚合正是审计列表要做的事
	byMid, err := logic.ListRoomBans(&rpc.ListRoomBansReq{Mid: 1001, OperatorMid: 555})
	wantNoErr(t, "ListRoomBans(mid)", err)
	wantInt64sEQ(t, "按主播", "bans", banIDs(byMid.GetBans()), []int64{303, 301})
	wantEQ(t, "按主播的 total", "total", byMid.GetTotal(), int32(2))

	// 两个维度都给：交集
	both, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, Mid: 1001, OperatorMid: 555})
	wantNoErr(t, "ListRoomBans(交集)", err)
	wantInt64sEQ(t, "交集", "bans", banIDs(both.GetBans()), []int64{301})
	wantEQ(t, "交集", "total", both.GetTotal(), int32(1))
	wantSeq(t, "每次读都是 List→Count 成对", st.log, 0,
		"live_room_ban.List", "live_room_ban.Count",
		"live_room_ban.List", "live_room_ban.Count",
		"live_room_ban.List", "live_room_ban.Count")
	for i, q := range st.bans.listQueries {
		wantDeepEQ(t, "List/Count 同口径", fmt.Sprintf("第 %d 次 count 条件", i), st.bans.countQueries[i], q)
	}
}

// TestListRoomBansPagingActuallyAdvances 禁播列表的 SQL 有 OFFSET，
// 所以分页会前进；同时锁页码回显（page=0 要回 1，不能回 0）。
func TestListRoomBansPagingActuallyAdvances(t *testing.T) {
	st := newStore()
	for _, id := range []int64{301, 302, 303} {
		st.seedBan(baseBan(id, 101, model.BanStateActive))
	}
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	first, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, OperatorMid: 555, Page: 0, PageSize: 2})
	wantNoErr(t, "第一页（page 缺省）", err)
	wantInt64sEQ(t, "第一页", "bans", banIDs(first.GetBans()), []int64{303, 302})
	wantEQ(t, "page 缺省要回显 1", "page", first.GetPage(), int32(1))
	wantEQ(t, "page_size 回显", "page_size", first.GetPageSize(), int32(2))

	second, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, OperatorMid: 555, Page: 2, PageSize: 2})
	wantNoErr(t, "第二页", err)
	wantInt64sEQ(t, "第二页", "bans", banIDs(second.GetBans()), []int64{301})
	wantEQ(t, "第二页 offset", "offset", st.bans.listQueries[1].Offset, int32(2))
	wantEQ(t, "第二页 total", "total", second.GetTotal(), int32(3))
	wantEQ(t, "第二页页码", "page", second.GetPage(), int32(2))

	// 超出范围的页：空页 + 真实 total（不是错误，也不是回绕）。
	third, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, OperatorMid: 555, Page: 3, PageSize: 2})
	wantNoErr(t, "第三页（越界）", err)
	wantEQ(t, "越界页", "bans 条数", len(third.GetBans()), 0)
	wantEQ(t, "越界页 total", "total", third.GetTotal(), int32(3))
}

func TestListRoomBansValidatesInFixedOrderBeforeQuery(t *testing.T) {
	st := newStore()
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	// 归因主体排第一：operator_mid 缺失时连「房间号写错了」都不报，
	// 因为这条请求本来就不该被受理。
	_, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: -1, State: 9, Page: -1, PageSize: 999})
	wantErrIs(t, "顺序 0（归因优先）", err, model.ErrOperatorRequired)

	_, err = logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: -1, State: 9, Page: -1, PageSize: 999, OperatorMid: 555})
	wantErrIs(t, "顺序 1", err, model.ErrInvalidRoomID)

	_, err = logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, State: 9, Page: -1, PageSize: 999, OperatorMid: 555})
	wantErrIs(t, "顺序 2", err, model.ErrInvalidBanTransition)

	_, err = logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, Page: -1, PageSize: 999, OperatorMid: 555})
	wantErrIs(t, "顺序 3（页大小上限 100）", err, model.ErrPageSizeTooLarge)
	wantErrContains(t, "ListRoomBans(页大小上限是 100)", err, "max=100")

	_, err = logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 101, Page: -1, PageSize: 50, OperatorMid: 555})
	wantErrIs(t, "顺序 4", err, model.ErrInvalidPage)

	wantNoCallAfter(t, "ListRoomBans(入参守卫)", st.log, 0)
}

func TestListRoomBansEmptyResultAndStoreError(t *testing.T) {
	st := newStore()
	st.seedBan(baseBan(301, 101, model.BanStateActive))
	logic := NewListRoomBansLogic(context.Background(), st.svcCtx())

	reply, err := logic.ListRoomBans(&rpc.ListRoomBansReq{RoomId: 999, OperatorMid: 555})
	wantNoErr(t, "ListRoomBans(空结果)", err)
	wantEQ(t, "空结果", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "空结果", "bans 条数", len(reply.GetBans()), 0)
	wantSeq(t, "空结果仍数总数", st.log, 0, "live_room_ban.List", "live_room_ban.Count")
	// 审计列表不 join live_room：房间不存在≠没有历史记录，这里也不能报 ErrRoomNotFound。
	wantMethodCount(t, "不得回查房间表", st.log, "live_room.FindOne", 0)

	st2 := newStore()
	st2.bans.failWith("Count", errors.New("count boom"))
	reply2, err := NewListRoomBansLogic(context.Background(), st2.svcCtx()).
		ListRoomBans(&rpc.ListRoomBansReq{OperatorMid: 555})
	wantErrContains(t, "ListRoomBans(Count 故障)", err, "count boom")
	wantEQ(t, "ListRoomBans(Count 故障)", "reply", reply2, nil)
}
