// 本文件覆盖读侧「流」的两条入口：ListStreams（开播巡检 / 断流扫描）与
// GetStreamState（按 stream_id 或房间活跃流查当前状态）。
//
// 读侧用例只盯三件事，其余交给 model 层的 migration_parity_test.go：
//  1. 越权收口发生在**下推给 SQL 的过滤条件**里，不是「查全表再在内存里筛」——
//     所以返回集合与 total 必须同时收敛（只收敛集合不收敛 total 会泄露规模，
//     只收敛 total 不收敛集合等于没收口）；
//  2. 分页是「夹取 + 回显」而不是「拒绝」：归一后的 pn/ps 必须回显，
//     否则调用方按自己的入参翻页会跳页或漏行；
//  3. 读路径零副作用：九张表行数与事务开合次数都不许动。
//
// 排序断言的口径：真 SQL 是 ORDER BY last_heartbeat_at ASC（没有次级键），
// ctime 相同 / 心跳相同的两行在 MySQL 里谁先出现是未定义的，
// 因此本文件所有排序用例都铺**互不相同**的心跳值，不拿替身的兜底次序立契约。

package logic

import (
	"fmt"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// listStreams 走真 logic；默认带一个合法 operator（读侧的非 admin 收敛路径要有主体）。
func listStreams(e *testEnv, mut ...func(*rpc.ListStreamsReq)) (*rpc.ListStreamsReply, error) {
	in := &rpc.ListStreamsReq{OperatorMid: 1001}
	for _, m := range mut {
		m(in)
	}
	return NewListStreamsLogic(bg(), e.svc).ListStreams(in)
}

func getStreamState(e *testEnv, mut ...func(*rpc.GetStreamStateReq)) (*rpc.GetStreamStateReply, error) {
	in := &rpc.GetStreamStateReq{}
	for _, m := range mut {
		m(in)
	}
	return NewGetStreamStateLogic(bg(), e.svc).GetStreamState(in)
}

// idsOfStreams 把响应投影成 stream_id 序列，便于整列比对（比逐字段 more/less 更能暴露错位）。
func idsOfStreams(rows []*rpc.StreamInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetStreamId())
	}
	return out
}

func requireStreamIDs(t *testing.T, rows []*rpc.StreamInfo, want []string, label string) {
	t.Helper()
	got := idsOfStreams(rows)
	if len(got) != len(want) {
		t.Fatalf("%s：条数不符\n  got =%v\n want=%v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：第 %d 条不符\n  got =%v\n want=%v", label, i, got, want)
		}
	}
}

// seedPublishing 铺一条 PUBLISHING 的流：心跳值同时充当排序键，必须彼此不同。
func (e *testEnv) seedPublishing(t *testing.T, id string, room, anchor, heartbeat int64) *model.Stream {
	t.Helper()
	return e.seedStream(t, &model.Stream{
		StreamID: id, RoomID: room, AnchorMid: anchor, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: heartbeat,
	})
}

// --- 不变量 5 的读侧半边：越权收口下推到过滤条件 ---

func TestListStreams_NonAdminScopeCollapsesOntoOperatorItself(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-OWN-1", 11, 1001, 1001)
	e.seedPublishing(t, "S-OWN-2", 12, 1001, 1002)
	e.seedPublishing(t, "S-OTH-1", 21, 2002, 1003)
	e.seedPublishing(t, "S-OTH-2", 22, 2002, 1004)

	reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Ps = 10 })
	wantOK(t, reply, err, "非 admin 列表")
	// 集合与 total 同时收敛：只有两条自己的流，别人的房间连规模都不许透出。
	requireStreamIDs(t, reply.GetStreams(), []string{"S-OWN-1", "S-OWN-2"}, "非 admin 只看得到 operator 自己的流")
	if reply.GetTotal() != 2 {
		t.Fatalf("非 admin 的 total 未收敛：%d（应为 2，别人的 2 条不该计入）", reply.GetTotal())
	}
}

func TestListStreams_NonAdminAskingOtherRoomsGetsEmptyNotEveryones(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-OWN", 11, 1001, 1001)
	e.seedPublishing(t, "S-OTH", 21, 2002, 1002)

	// 越权探测：把 room_ids 填成别人的房间。收口若发生在「返回后过滤」，
	// 这一格会变成「条件被忽略 → 回全量」，正是最难发现的一类泄露。
	reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.RoomIds = []int64{21} })
	wantOK(t, reply, err, "非 admin 查别人的房间")
	if n := len(reply.GetStreams()); n != 0 {
		t.Fatalf("非 admin 查别人的房间拿到了 %d 条流（anchor_mid 收敛失效）", n)
	}
	if reply.GetTotal() != 0 {
		t.Fatalf("非 admin 查别人的房间 total=%d（应为 0）", reply.GetTotal())
	}
}

func TestListStreams_AdminSeesEveryAnchor(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-OWN", 11, 1001, 1001)
	e.seedPublishing(t, "S-OTH", 21, 2002, 1002)
	// admin 也不该顺手把终态流带出来：状态维度与权限维度是正交的两层收敛。
	e.seedStream(t, &model.Stream{StreamID: "S-STOP", RoomID: 31, AnchorMid: 3003,
		Protocol: 1, State: model.StreamStateStopped, Seq: 3, LastHeartbeatAt: 1003})

	reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Admin = true })
	wantOK(t, reply, err, "运营侧列表")
	requireStreamIDs(t, reply.GetStreams(), []string{"S-OWN", "S-OTH"}, "admin 按心跳升序看全量非终态流")
	if reply.GetTotal() != 2 {
		t.Fatalf("admin total=%d（应为 2；终态流由状态维度排除，不是由权限维度）", reply.GetTotal())
	}
}

// --- 排序契约：最久没心跳的在前，巡检第一页就是要处理的 ---

func TestListStreams_StaleHeartbeatSortsFirst(t *testing.T) {
	e := newTestEnv(t)
	// 故意按「新→旧」的插入顺序铺数据，若实现退化成「按插入序返回」就会被抓到。
	e.seedPublishing(t, "S-FRESH", 41, 1001, 3000)
	e.seedPublishing(t, "S-STALE", 42, 1001, 1000)
	e.seedPublishing(t, "S-MIDDLE", 43, 1001, 2000)

	reply, err := listStreams(e)
	wantOK(t, reply, err, "断流扫描排序")
	requireStreamIDs(t, reply.GetStreams(), []string{"S-STALE", "S-MIDDLE", "S-FRESH"}, "last_heartbeat_at 升序")
	if got := reply.GetStreams()[0].GetLastHeartbeatAt(); got != 1000 {
		t.Fatalf("首条心跳不是最小值：%d", got)
	}
}

// --- 状态维度：UNSPECIFIED 是「只看非终态」而不是「不过滤」 ---

func TestListStreams_UnspecifiedStateExcludesTerminalRows(t *testing.T) {
	e := newTestEnv(t)
	for i, st := range []int32{model.StreamStateIdle, model.StreamStatePublishing,
		model.StreamStateInterrupted, model.StreamStateStopped} {
		e.seedStream(t, &model.Stream{StreamID: fmt.Sprintf("S-ST-%d", i), RoomID: 51,
			AnchorMid: 1001, Protocol: 1, State: st, Seq: int64(i), LastHeartbeatAt: int64(1000 + i)})
	}

	reply, err := listStreams(e)
	wantOK(t, reply, err, "默认状态过滤")
	requireStreamIDs(t, reply.GetStreams(), []string{"S-ST-0", "S-ST-1", "S-ST-2"}, "未指定状态只看非终态")
	if reply.GetTotal() != 3 {
		t.Fatalf("total=%d（STOPPED 那一行不该计入）", reply.GetTotal())
	}
}

func TestListStreams_ExplicitStateFilterReturnsThatStateOnly(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-IDLE", RoomID: 52, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateIdle, LastHeartbeatAt: 1000})
	e.seedStream(t, &model.Stream{StreamID: "S-PUB", RoomID: 52, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: 1001})
	e.seedStream(t, &model.Stream{StreamID: "S-STOP", RoomID: 52, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateStopped, Seq: 3, LastHeartbeatAt: 1002})
	// 别的房间的同态行：证明状态过滤与房间过滤是 AND 而不是覆盖。
	e.seedStream(t, &model.Stream{StreamID: "S-STOP-ELSEWHERE", RoomID: 53, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateStopped, Seq: 3, LastHeartbeatAt: 1003})

	for _, tc := range []struct {
		name  string
		state rpc.StreamState
		want  []string
	}{
		{"只查 STOPPED", rpc.StreamState_STREAM_STATE_STOPPED, []string{"S-STOP"}},
		{"只查 PUBLISHING", rpc.StreamState_STREAM_STATE_PUBLISHING, []string{"S-PUB"}},
		{"只查 IDLE", rpc.StreamState_STREAM_STATE_IDLE, []string{"S-IDLE"}},
	} {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) {
			r.State = tc.state
			r.RoomIds = []int64{52}
		})
		wantOK(t, reply, err, tc.name)
		requireStreamIDs(t, reply.GetStreams(), tc.want, tc.name)
		if reply.GetTotal() != int32(len(tc.want)) {
			t.Fatalf("%s：total=%d want=%d", tc.name, reply.GetTotal(), len(tc.want))
		}
	}
}

// --- 分页：夹取 + 回显 + 下推 ---

func TestListStreams_PnPsAreClampedAndEchoedBack(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-1", 61, 1001, 1001)
	e.seedPublishing(t, "S-2", 62, 1001, 1002)

	for _, tc := range []struct {
		name    string
		pn, ps  int32
		wantPn  int32
		wantPs  int32
		wantIDs []string
	}{
		{"省略分页参数取默认页大小", 0, 0, 1, defaultListPageSize, []string{"S-1", "S-2"}},
		{"负页大小取默认", 1, -5, 1, defaultListPageSize, []string{"S-1", "S-2"}},
		{"页码 0 归一到第 1 页", 0, 10, 1, 10, []string{"S-1", "S-2"}},
		{"负页码归一到第 1 页", -7, 10, 1, 10, []string{"S-1", "S-2"}},
		{"页大小超上限夹到 MaxListPageSize", 1, 51, 1, 50, []string{"S-1", "S-2"}},
		{"巨大页大小同样夹到上限", 1, 99999, 1, 50, []string{"S-1", "S-2"}},
	} {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Pn, r.Ps = tc.pn, tc.ps })
		wantOK(t, reply, err, tc.name)
		if reply.GetPn() != tc.wantPn || reply.GetPs() != tc.wantPs {
			t.Fatalf("%s：回显不符 pn=%d/%d ps=%d/%d", tc.name, reply.GetPn(), tc.wantPn, reply.GetPs(), tc.wantPs)
		}
		requireStreamIDs(t, reply.GetStreams(), tc.wantIDs, tc.name+" 的命中集合")
	}
}

func TestListStreams_PageOffsetActuallySlicesTheOrderedSet(t *testing.T) {
	e := newTestEnv(t)
	for i := 1; i <= 5; i++ {
		e.seedPublishing(t, fmt.Sprintf("S-P%d", i), int64(70+i), 1001, int64(1000+i))
	}

	// offset=(pn-1)*ps：第 2 页必须跳过前 2 条，而不是「回显了 pn 但仍从第 1 页取」。
	second, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Pn, r.Ps = 2, 2 })
	wantOK(t, second, err, "第 2 页")
	requireStreamIDs(t, second.GetStreams(), []string{"S-P3", "S-P4"}, "第 2 页（ps=2）")
	if second.GetTotal() != 5 {
		t.Fatalf("total=%d（total 是全量命中数，不是本页条数）", second.GetTotal())
	}

	third, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Pn, r.Ps = 3, 2 })
	wantOK(t, third, err, "第 3 页")
	requireStreamIDs(t, third.GetStreams(), []string{"S-P5"}, "第 3 页（末页不满一页）")
	if third.GetTotal() != 5 {
		t.Fatalf("末页 total=%d want=5", third.GetTotal())
	}
}

func TestListStreams_PageBeyondLastIsNotEmptyTotal(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-1", 81, 1001, 1001)
	e.seedPublishing(t, "S-2", 82, 1001, 1002)

	// 越过末页：空页 + 原 total。把 total 归零会让调用方以为「数据被删了」。
	reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Pn, r.Ps = 9, 5 })
	wantOK(t, reply, err, "越过末页")
	if n := len(reply.GetStreams()); n != 0 {
		t.Fatalf("越过末页还返回 %d 条", n)
	}
	if reply.GetTotal() != 2 {
		t.Fatalf("越过末页把 total 也改了：%d want=2", reply.GetTotal())
	}
}

func TestListStreams_PageSizeCapIsPushedDownToSql(t *testing.T) {
	e := newTestEnv(t)
	const rows = 55
	for i := 1; i <= rows; i++ {
		e.seedPublishing(t, fmt.Sprintf("S-CAP%02d", i), int64(100+i), 1001, int64(1000+i))
	}

	// ps=100 被夹到 MaxListPageSize(50)：夹取必须落到 LIMIT 上，
	// 只改回显不改 LIMIT 就是「一次全表捞取」，这正是要挡的形态。
	reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.Ps = 100 })
	wantOK(t, reply, err, "页大小上限下推")
	if reply.GetPs() != 50 {
		t.Fatalf("回显 ps=%d want=50", reply.GetPs())
	}
	if n := len(reply.GetStreams()); n != 50 {
		t.Fatalf("实际返回 %d 条，页大小上限没有落到 SQL LIMIT", n)
	}
	if reply.GetTotal() != rows {
		t.Fatalf("total=%d want=%d", reply.GetTotal(), rows)
	}
	want := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		want = append(want, fmt.Sprintf("S-CAP%02d", i))
	}
	requireStreamIDs(t, reply.GetStreams(), want, "第 1 页是最可疑的 50 条")
}

// --- 其余过滤维度 ---

func TestListStreams_RoomNodeAndHeartbeatWindowFilters(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-A", RoomID: 91, AnchorMid: 1001, Protocol: 1,
		NodeID: "node-x", State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: 1000})
	e.seedStream(t, &model.Stream{StreamID: "S-B", RoomID: 92, AnchorMid: 1001, Protocol: 2,
		NodeID: "node-y", State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: 2000})
	e.seedStream(t, &model.Stream{StreamID: "S-C", RoomID: 93, AnchorMid: 1001, Protocol: 1,
		NodeID: "node-x", State: model.StreamStateInterrupted, Seq: 2, LastHeartbeatAt: 3000})

	t.Run("room_ids 批量巡检", func(t *testing.T) {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.RoomIds = []int64{91, 93} })
		wantOK(t, reply, err, "房间批量")
		requireStreamIDs(t, reply.GetStreams(), []string{"S-A", "S-C"}, "room_ids IN")
	})
	t.Run("node_id 节点视角", func(t *testing.T) {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.NodeId = "node-x" })
		wantOK(t, reply, err, "节点过滤")
		requireStreamIDs(t, reply.GetStreams(), []string{"S-A", "S-C"}, "node_id =")
	})
	t.Run("heartbeat_before 边界含等号", func(t *testing.T) {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) { r.HeartbeatBefore = 2000 })
		wantOK(t, reply, err, "心跳窗口")
		// 真 SQL 是 last_heartbeat_at <= ?：等于边界的 S-B 必须在结果里。
		requireStreamIDs(t, reply.GetStreams(), []string{"S-A", "S-B"}, "last_heartbeat_at <=")
	})
	t.Run("protocol 协议过滤", func(t *testing.T) {
		reply, err := listStreams(e, func(r *rpc.ListStreamsReq) {
			r.Protocol = rpc.IngestProtocol_PROTOCOL_SRT
		})
		wantOK(t, reply, err, "协议过滤")
		requireStreamIDs(t, reply.GetStreams(), []string{"S-B"}, "protocol =")
		if reply.GetStreams()[0].GetProtocol() != rpc.IngestProtocol_PROTOCOL_SRT {
			t.Fatalf("协议回显成了 %v（库里是 2=SRT）", reply.GetStreams()[0].GetProtocol())
		}
	})
}

// --- 入参校验：拒绝且零副作用 ---

func TestListStreams_InputValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-1", 101, 1001, 1001)
	before := e.effects()

	cases := []struct {
		name   string
		mut    func(*rpc.ListStreamsReq)
		target error
	}{
		{"缺 operator", func(r *rpc.ListStreamsReq) { r.OperatorMid = 0 }, model.ErrOperatorRequired},
		{"负 operator", func(r *rpc.ListStreamsReq) { r.OperatorMid = -1 }, model.ErrOperatorRequired},
		{"room_ids 含 0", func(r *rpc.ListStreamsReq) { r.RoomIds = []int64{0} }, model.ErrInvalidRoomId},
		{"room_ids 含负数", func(r *rpc.ListStreamsReq) { r.RoomIds = []int64{101, -3} }, model.ErrInvalidRoomId},
		{"room_ids 超上限", func(r *rpc.ListStreamsReq) {
			ids := make([]int64, maxRoomIDsPerQuery+1)
			for i := range ids {
				ids[i] = int64(200 + i)
			}
			r.RoomIds = ids
		}, model.ErrInvalidRoomId},
		{"heartbeat_before 为负", func(r *rpc.ListStreamsReq) { r.HeartbeatBefore = -1 }, model.ErrInvalidStreamId},
		{"非法状态枚举", func(r *rpc.ListStreamsReq) { r.State = rpc.StreamState(9) }, model.ErrInvalidStreamState},
		{"非法协议枚举", func(r *rpc.ListStreamsReq) { r.Protocol = rpc.IngestProtocol(9) }, model.ErrInvalidProtocol},
	}
	for _, tc := range cases {
		mut := tc.mut
		t.Run(tc.name, func(t *testing.T) {
			_, err := listStreams(e, mut)
			wantFail(t, err, tc.target, tc.name)
			e.requireSameEffects(t, before, tc.name+" 必须零写入")
			e.requireNoTransaction(t, before, tc.name+" 不该开事务")
		})
	}

	// 校验顺序也要钉住：状态维度先于协议维度报错，两个都坏时只回第一个，
	// 调用方才不会「修了一个还剩一个」。
	_, err := listStreams(e, func(r *rpc.ListStreamsReq) {
		r.State = rpc.StreamState(9)
		r.Protocol = rpc.IngestProtocol(9)
	})
	wantFail(t, err, model.ErrInvalidStreamState, "非法状态先于非法协议被拒")

	// 房间数超限要在逐个校验取值之前拒：51 个非法 id 也只报「超上限」这一条原因。
	_, err = listStreams(e, func(r *rpc.ListStreamsReq) {
		r.RoomIds = make([]int64, maxRoomIDsPerQuery+1)
	})
	wantFail(t, err, model.ErrInvalidRoomId, "全 0 的超长 room_ids")
	e.requireSameEffects(t, before, "全部拒绝路径合计零写入")
}

func TestListStreams_ReadPathWritesNothingAndEchoesServerTime(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-1", 111, 1001, 1001)
	before := e.effects()
	// 写方法调用计数取增量而不是绝对值：种子自己走真 Insert，把种子的调用算到被测代码
	// 头上，就等于拿测试自己写的行当被测代码的功绩。
	// 探针里只允许出现「替身有留痕的写方法」——fakeStream 未实现 TouchHeartbeat/ApplyHealth
	// （走嵌入的 nil 接口，一旦被调就直接 panic，那是比计数更硬的响铃），
	// 把它们列进来只会得到一个永远不动的装饰性数字。
	writeCalls := func() [4]int {
		return [4]int{e.db.call("Stream.ApplyTransition"), e.db.call("Stream.MarkStopped"),
			e.db.call("Stream.SetNode"), e.db.call("Stream.OpenInterruption")}
	}
	beforeCalls := writeCalls()

	reply, err := listStreams(e)
	wantOK(t, reply, err, "正常列表")
	e.requireSameEffects(t, before, "读侧列表不得改任何表")
	e.requireNoTransaction(t, before, "读侧列表不该开事务")
	if got := writeCalls(); got != beforeCalls {
		t.Fatalf("读路径调用了状态机写方法：前 %v 后 %v", beforeCalls, got)
	}
	if reply.GetServerTime() <= 0 {
		t.Fatalf("server_time 没有回显：%d", reply.GetServerTime())
	}
	// total 与集合都非空才有意义：确认种子真的进了投影。
	if reply.GetTotal() != 1 || reply.GetStreams()[0].GetRoomId() != 111 {
		t.Fatalf("投影不符：total=%d room=%d", reply.GetTotal(), reply.GetStreams()[0].GetRoomId())
	}
}

func TestListStreams_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewListStreamsLogic(bg(), svcCtx).ListStreams(&rpc.ListStreamsReq{OperatorMid: 1001})
	wantFail(t, err, errNoRepository, "未装配 Repository 的列表")
}

// --- GetStreamState：两种选择子 + found 语义 ---

func TestGetStreamState_ByStreamIdProjectsTheWholeRow(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{
		StreamID: "S-FULL", KeyID: 4242, StreamName: "name.full", RoomID: 71, SessionID: 72,
		AnchorMid: 1001, Protocol: 2, NodeID: "node-b", State: model.StreamStateInterrupted,
		Seq: 9, PublishStartedAt: 1600, StateChangedAt: 1700, LastHeartbeatAt: 1710,
		InterruptedTotalSecs: 30, InterruptionCount: 3, StopReason: model.StopReasonNodeTimeout,
		HealthState: model.HealthStateCritical, HealthReportedAt: 1699,
		VideoBitrateBps: 1_200_000, AudioBitrateBps: 128_000, FpsX100: 3000, PacketLossPpm: 500,
		TraceID: "trace-1", Ctime: 1500,
	})

	reply, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.StreamId = "S-FULL" })
	wantOK(t, reply, err, "按 stream_id 查状态")
	mustTrue(t, reply.GetFound(), "存在的流必须 found=true")
	s := reply.GetStream()
	if s == nil {
		t.Fatalf("found=true 却没带 stream")
	}
	// 时间/计数列逐列比对：读侧最怕「少投影一列」——编译期发现不了，运行期只是回 0。
	for _, tc := range []struct {
		name string
		got  int64
		want int64
	}{
		{"key_id", s.GetKeyId(), 4242},
		{"room_id", s.GetRoomId(), 71},
		{"session_id", s.GetSessionId(), 72},
		{"anchor_mid", s.GetAnchorMid(), 1001},
		{"seq", s.GetSeq(), 9},
		{"publish_started_at", s.GetPublishStartedAt(), 1600},
		{"state_changed_at", s.GetStateChangedAt(), 1700},
		{"last_heartbeat_at", s.GetLastHeartbeatAt(), 1710},
		{"interrupted_total_seconds", s.GetInterruptedTotalSeconds(), 30},
		{"health_reported_at", s.GetHealthReportedAt(), 1699},
		{"video_bitrate_bps", s.GetVideoBitrateBps(), 1_200_000},
		{"audio_bitrate_bps", s.GetAudioBitrateBps(), 128_000},
		{"ctime", s.GetCtime(), 1500},
		{"mtime", s.GetMtime(), 1500},
	} {
		if tc.got != tc.want {
			t.Fatalf("投影字段 %s 不符：got=%d want=%d", tc.name, tc.got, tc.want)
		}
	}
	if s.GetStreamId() != "S-FULL" || s.GetStreamName() != "name.full" || s.GetNodeId() != "node-b" {
		t.Fatalf("标识类投影不符：%s/%s/%s", s.GetStreamId(), s.GetStreamName(), s.GetNodeId())
	}
	if s.GetState() != rpc.StreamState_STREAM_STATE_INTERRUPTED {
		t.Fatalf("state 投影成 %v", s.GetState())
	}
	if s.GetProtocol() != rpc.IngestProtocol_PROTOCOL_SRT {
		t.Fatalf("protocol 投影成 %v", s.GetProtocol())
	}
	if s.GetStopReason() != rpc.StopReason_STOP_REASON_NODE_TIMEOUT {
		t.Fatalf("stop_reason 投影成 %v", s.GetStopReason())
	}
	// health_state 走的是原始 int32 而不是枚举映射：口径要与 live_stream.health_state 一致。
	if s.GetHealthState() != model.HealthStateCritical || s.GetInterruptionCount() != 3 ||
		s.GetFps() != 3000 || s.GetPacketLossPpm() != 500 {
		t.Fatalf("健康/断流计数投影不符：health=%d count=%d fps=%d loss=%d",
			s.GetHealthState(), s.GetInterruptionCount(), s.GetFps(), s.GetPacketLossPpm())
	}
}

func TestGetStreamState_ByRoomIdPicksNewestNonTerminalInRoom(t *testing.T) {
	e := newTestEnv(t)
	// 房间 7：STOPPED 是最新（ctime 3000），必须被 state IN（非终态） 排除掉，
	// 于是回 PUBLISHING(ctime 2000)——「活跃」优先于「最近」。
	e.seedStream(t, &model.Stream{StreamID: "S-STOPPED-NEW", RoomID: 7, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateStopped, Seq: 3, LastHeartbeatAt: 3000, Ctime: 3000})
	e.seedStream(t, &model.Stream{StreamID: "S-PUBLISHING", RoomID: 7, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: 2000, Ctime: 2000})
	e.seedStream(t, &model.Stream{StreamID: "S-IDLE-OLD", RoomID: 7, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateIdle, LastHeartbeatAt: 1000, Ctime: 1000})
	// 别的房间更新的流：证明 room_id 是真的过滤条件而不是排序提示。
	e.seedStream(t, &model.Stream{StreamID: "S-OTHER-ROOM", RoomID: 8, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateInterrupted, Seq: 2, LastHeartbeatAt: 4000, Ctime: 5000})

	reply, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.RoomId = 7 })
	wantOK(t, reply, err, "按房间查活跃流")
	mustTrue(t, reply.GetFound(), "房间里有非终态流")
	if got := reply.GetStream().GetStreamId(); got != "S-PUBLISHING" {
		t.Fatalf("按房间取到 %s（应为房间内最新的非终态流 S-PUBLISHING）", got)
	}
	if e.db.call("Stream.FindActiveByRoom") != 1 {
		t.Fatalf("房间查询走错了方法：FindActiveByRoom 调了 %d 次", e.db.call("Stream.FindActiveByRoom"))
	}
}

func TestGetStreamState_StoppedStreamFoundByIdButNotByRoom(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-DONE", RoomID: 9, AnchorMid: 1001,
		Protocol: 1, State: model.StreamStateStopped, Seq: 4, StopReason: model.StopReasonAnchorStop,
		LastHeartbeatAt: 1000, Ctime: 1000})

	// 房间维度：终态即「这间房现在没活流」，回 found=false。
	byRoom, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.RoomId = 9 })
	wantOK(t, byRoom, err, "按房间查已停的流")
	mustFalse(t, byRoom.GetFound(), "终态流不该算房间的活跃流")
	if byRoom.GetStream() != nil {
		t.Fatalf("found=false 还带着 stream 投影，调用方会当成活流")
	}

	// stream_id 维度：历史会话仍可查（对账/回放定位要用），不得按终态过滤。
	byID, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.StreamId = "S-DONE" })
	wantOK(t, byID, err, "按 stream_id 查已停的流")
	mustTrue(t, byID.GetFound(), "按 ID 查终态流要有结果")
	if byID.GetStream().GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
		t.Fatalf("终态流的 state=%v", byID.GetStream().GetState())
	}
}

func TestGetStreamState_MissingStreamIsNotFoundNotAnError(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-ELSE", 21, 1001, 1001)
	before := e.effects()

	// 开播前置检查要把「没有活流」和「查失败」分开处理，所以「查不到」必须是 found=false。
	byID, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.StreamId = "S-NOPE" })
	wantOK(t, byID, err, "按不存在的 stream_id 查")
	mustFalse(t, byID.GetFound(), "不存在的流应回 found=false 而不是错误")

	byRoom, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.RoomId = 999999 })
	wantOK(t, byRoom, err, "按没有流的房间查")
	mustFalse(t, byRoom.GetFound(), "空房间应回 found=false 而不是错误")

	e.requireSameEffects(t, before, "未命中查询必须零写入")
	e.requireNoTransaction(t, before, "未命中查询不该开事务")
}

func TestGetStreamState_StreamIdSelectorNeverFallsBackToRoom(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-ROOM-ACTIVE", 31, 1001, 1001)
	e.seedStream(t, &model.Stream{StreamID: "S-OTHER-STOPPED", RoomID: 32, AnchorMid: 2002,
		Protocol: 1, State: model.StreamStateStopped, Seq: 4, LastHeartbeatAt: 900})

	// 两个选择子都给：以 stream_id 为准，绝不回落到房间活跃流。
	// 若回落，这里会「查到」S-ROOM-ACTIVE——一次静默的串房间读。
	reply, err := getStreamState(e, func(r *rpc.GetStreamStateReq) {
		r.StreamId = "S-OTHER-STOPPED"
		r.RoomId = 31
	})
	wantOK(t, reply, err, "两个选择子同时给")
	mustTrue(t, reply.GetFound(), "按 stream_id 命中")
	if got := reply.GetStream().GetStreamId(); got != "S-OTHER-STOPPED" {
		t.Fatalf("回落到房间活跃流了：%s", got)
	}
	if reply.GetStream().GetRoomId() != 32 {
		t.Fatalf("房间串了：%d", reply.GetStream().GetRoomId())
	}
	if n := e.db.call("Stream.FindActiveByRoom"); n != 0 {
		t.Fatalf("带 stream_id 的请求还是查了房间活跃流 %d 次", n)
	}
}

func TestGetStreamState_SelectorValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishing(t, "S-1", 41, 1001, 1001)
	before := e.effects()

	long := make([]byte, maxStreamRefBytes+1)
	for i := range long {
		long[i] = 'S'
	}
	cases := []struct {
		name   string
		mut    func(*rpc.GetStreamStateReq)
		target error
	}{
		{"两个选择子都空", func(r *rpc.GetStreamStateReq) {}, model.ErrInvalidStreamId},
		{"room_id 为 0 且无 stream_id", func(r *rpc.GetStreamStateReq) { r.RoomId = 0 }, model.ErrInvalidStreamId},
		// TODO(缺陷 #3)：负 room_id 属于「非键参数不合法」，本应与 ListStreamKeys 一样回
		// ErrInvalidRoomId；现状把房间维度错误报成了流 ID 错误（同一类入参在三个读入口
		// 给出三种哨兵）。这里按现状钉住，不改生产代码。见 README「已知缺口」。
		{"room_id 为负", func(r *rpc.GetStreamStateReq) { r.RoomId = -5 }, model.ErrInvalidStreamId},
		{"纯空白 stream_id", func(r *rpc.GetStreamStateReq) { r.StreamId = "   " }, model.ErrInvalidStreamId},
		{"空白 stream_id 也不回落查房间", func(r *rpc.GetStreamStateReq) {
			r.StreamId = "   "
			r.RoomId = 41
		}, model.ErrInvalidStreamId},
		{"含路径分隔符的 stream_id", func(r *rpc.GetStreamStateReq) { r.StreamId = "S/1" }, model.ErrInvalidStreamId},
		{"超长 stream_id", func(r *rpc.GetStreamStateReq) { r.StreamId = string(long) }, model.ErrInvalidStreamId},
	}
	for _, tc := range cases {
		mut := tc.mut
		t.Run(tc.name, func(t *testing.T) {
			_, err := getStreamState(e, mut)
			wantFail(t, err, tc.target, tc.name)
			e.requireSameEffects(t, before, tc.name+" 必须零写入")
			e.requireNoTransaction(t, before, tc.name+" 不该开事务")
		})
	}
	// 空白 stream_id 被拒而不是回落：房间活跃流一次都不该被读到。
	if n := e.db.call("Stream.FindActiveByRoom"); n != 0 {
		t.Fatalf("非法 stream_id 触发了房间查询 %d 次", n)
	}
}

func TestGetStreamState_ReadPathWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-1", RoomID: 51, AnchorMid: 1001, Protocol: 1,
		State: model.StreamStatePublishing, Seq: 1, LastHeartbeatAt: 1001, HealthState: model.HealthStateHealthy,
		HealthReportedAt: 1000})
	before := e.effects()

	reply, err := getStreamState(e, func(r *rpc.GetStreamStateReq) { r.RoomId = 51 })
	wantOK(t, reply, err, "按房间查状态")
	e.requireSameEffects(t, before, "读侧状态查询不得改任何表")
	e.requireNoTransaction(t, before, "读侧状态查询不该开事务")
	if reply.GetStream().GetHealthState() != model.HealthStateHealthy {
		t.Fatalf("health_state 被读路径改写了：%d", reply.GetStream().GetHealthState())
	}
}

func TestGetStreamState_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewGetStreamStateLogic(bg(), svcCtx).GetStreamState(&rpc.GetStreamStateReq{StreamId: "S-1"})
	wantFail(t, err, errNoRepository, "未装配 Repository 的状态查询")
}
