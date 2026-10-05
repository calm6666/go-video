package logic

import (
	"context"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"go-video/services/live-media/model"
	"go-video/services/live-media/rpc"
)

// 回放读侧三个方法：GetReplayTask / ListReplayTasks / ListReplayAssetRefs。
//
// 三个方法共享同一句契约：**只读**。所以每个用例都以 wantNoWrites + wantEvents(nil) 收尾，
// 而不是只在「成功路径」那一个用例里顺手看一眼 —— 读接口一旦能写，
// 就等于给只读账号开了写路径，还会把 Worker 正在用的 version 撞废（同 record_read_test.go 的判据）。
//
// 三条各自独有的口径，各自钉死：
//   - GetReplayTask：查无此行必须是 ErrReplayTaskNotFound，绝不能回零值 Info。
//     回放侧的后果比录制更硬：Worker 会把 version=0 当 expected_version，
//     此后每次 ReportReplayProgress 都永远撞版本冲突（见 getreplaytasklogic.go 头注释）；
//   - 缓存只对终态开：taskDetailTTL(cfg, terminal) 是这条策略的唯一实现处，
//     而非终态行返回 0 秒（本包 Cache 恒为 nil，所以行为侧只能验到「每次必回源」，
//     策略本身用纯函数钉住 —— 两者一起才构成完整证据）；
//   - ListReplayTasks / ListReplayAssetRefs：归一后的 pn/ps/过滤值只能靠 db.lastReplayList /
//     db.lastReplayRefList 钉住，返回值里看不出「model 到底收到了什么」。
//     同时钉住「rpc 未暴露的 RecordId/AnchorMid 恒传 0」与「OnlyUnsynced 恒 false」——
//     这两个开关一旦被 logic 凭空打开，就是把内部补刷通道当成了公开过滤条件。
//
// 投影边界不得美化（两个列表方法的头注释都在说这件事）：
// LiveReplayTaskInfo 里**没有** review_state，稿件事实状态只在 live_replay_asset_ref
// 做 video 侧只读投影。两处暴露同义字段必然出现不一致，所以这条用反射钉成结构事实，
// 而不是写在注释里等人读。

// ---------------------------------------------------------------- 请求构造与小工具

func getReplayReq(replayID int64) *rpc.ReplayTaskReq {
	return &rpc.ReplayTaskReq{ReplayId: replayID}
}

func listReplaysReq(pn, ps int32) *rpc.ListReplayTasksReq {
	return &rpc.ListReplayTasksReq{Page: &rpc.PageParam{Pn: pn, Ps: ps}}
}

func listRefsReq(pn, ps int32) *rpc.ListReplayAssetRefsReq {
	return &rpc.ListReplayAssetRefsReq{Page: &rpc.PageParam{Pn: pn, Ps: ps}}
}

func findReplayRow(reply *rpc.ListReplayTasksReply, replayID int64) (*rpc.LiveReplayTaskInfo, bool) {
	for _, row := range reply.GetTasks() {
		if row.GetReplayId() == replayID {
			return row, true
		}
	}
	return nil, false
}

func findRefRow(reply *rpc.ListReplayAssetRefsReply, refID int64) (*rpc.ReplayAssetRefInfo, bool) {
	for _, row := range reply.GetRefs() {
		if row.GetId() == refID {
			return row, true
		}
	}
	return nil, false
}

// replayOrder 把回复里的 replay_id 序列拼成字符串：排序口径（replay_id DESC）是
// 「一次断言」而不是「逐行 if」，页码/游标错位时一眼看得出错在哪。
func replayOrder(rows []*rpc.LiveReplayTaskInfo) string {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetReplayId())
	}
	return joinInt64(ids)
}

func refOrder(rows []*rpc.ReplayAssetRefInfo) string {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetId())
	}
	return joinInt64(ids)
}

// ---------------------------------------------------------------- GetReplayTask

// 逐列回显：读接口最重要的能力是「库里是什么就答什么」，包括脏的派生列。
// segment_count/gap_count 是提交时按 StatsInRange 落的快照，不是切片表的现值，
// 用例因此故意让两者不一致（库内 0 行切片 vs 快照 5 片）。
func TestGetReplayTaskReturnsEveryCommittedColumn(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedReplay(db, model.ReplayStateCompleted, func(r *model.LiveReplayTask) {
		r.RecordId, r.FromSeq, r.ToSeq = 5150, 2, 6
		r.SegmentCount, r.GapCount, r.DurationMs = 5, 2, 60000
		r.StartAt, r.EndAt = 1000000, 1000060
		r.AllowGaps, r.AnchorMid = 1, testAnchor
		r.OutputBucket, r.OutputKey = replayProductBucket, replayProductKey
		r.AssetId, r.Aid, r.Bvid = replayAssetID, replayAid, replayBvid
		r.Title, r.Description = "昨晚直播回放", "来自录制 5150"
		r.Reason, r.Errno, r.ErrMsg = model.ReasonManual, 0, "settled"
		r.Version, r.RequestId, r.TraceId = 7, "req-replay-get", "trace-replay-get"
	})
	before := snapshotWrites(db)

	info, err := NewGetReplayTaskLogic(context.Background(), svcCtx).GetReplayTask(getReplayReq(task.ReplayId))
	info = wantOK(t, info, err, "查询回放任务")
	wantReplayEchoesRow(t, "查询回放任务", info, mustReplay(t, db, task.ReplayId))
	if info.GetGapCount() != 2 || info.GetSegmentCount() != 5 {
		t.Fatalf("读接口不得二次推断快照计数：segment_count=%d gap_count=%d",
			info.GetSegmentCount(), info.GetGapCount())
	}
	if n := int64(countSegments(db, task.RecordId)); n != 0 {
		t.Fatalf("前置数据不成立：切片表应当是空的，实际 %d 行", n)
	}

	// 读接口不许顺手重算派生列：那是写路径（ReportReplayProgress/SubmitReplayTask）的职责，
	// 在读侧做等于给只读账号开写路径。
	wantCalls(t, db, "Segments.StatsInRange", 0, 0, "读接口不得重算切片统计")
	wantCalls(t, db, "ReplayTasks.UpdateState", 0, 0, "读接口不得推进状态")
	wantNoWrites(t, db, before, "查询回放任务")
	wantEvents(t, db, nil, "查询回放任务")
	wantNoLeak(t, db, "查询回放任务")
}

// 本服务里「回放能否对外」只有一个事实源：live_replay_asset_ref.review_state 投影。
// 任务行一旦也暴露同义字段，两处必然漂移，所以这条结构性事实用反射钉住。
// 后半段同时自校验探针本身：引用行信息里确实有 review_state，否则「没有」只是探针写错了。
func TestReplayTaskInfoCarriesNoReviewStateButRefInfoDoes(t *testing.T) {
	found := map[string]bool{}
	for _, typ := range []reflect.Type{reflect.TypeOf(rpc.LiveReplayTaskInfo{}), reflect.TypeOf(rpc.ReplayAssetRefInfo{})} {
		for i := 0; i < typ.NumField(); i++ {
			if strings.Contains(strings.ToLower(typ.Field(i).Name), "review") {
				found[typ.Name()] = true
			}
		}
	}
	if found["LiveReplayTaskInfo"] {
		t.Fatal("LiveReplayTaskInfo 出现了 review* 字段：任务行不得复制稿件投影状态（唯一事实源在引用行）")
	}
	if !found["ReplayAssetRefInfo"] {
		t.Fatal("探针失效：ReplayAssetRefInfo 里没有 review* 字段，上一句断言等于永真")
	}
}

// taskDetailTTL 是「只缓存终态」这条策略的唯一实现处。
// 非终态行缓存到旧值，Worker 就拿旧 version 当 expected_version（永远撞版本冲突），
// 或按旧状态判断还能不能绑定 —— 所以 0 秒必须是 0 秒，配置调大也不改变。
func TestGetReplayTaskCachesOnlyTerminalRows(t *testing.T) {
	cfg := testConf()
	for _, tc := range []struct {
		state    int32
		terminal bool
	}{
		{model.ReplayStatePending, false},
		{model.ReplayStateUploading, false},
		{model.ReplayStateRegistered, false},
		{model.ReplayStateReviewSubmitted, false}, // 随时可能被 video 投影推进
		{model.ReplayStateCompleted, true},
		{model.ReplayStateFailed, true},
		{model.ReplayStateCancelled, true},
	} {
		label := "replay state=" + strconv.Itoa(int(tc.state))
		wantField(t, label, "IsReplayTerminal", model.IsReplayTerminal(tc.state), tc.terminal)
		want := 0
		if tc.terminal {
			want = cfg.TaskCacheTTLSeconds
		}
		wantField(t, label, "缓存秒数", taskDetailTTL(cfg, tc.terminal), want)
	}
	// 配置为 0（关闭缓存）时终态也不写缓存：TTL 必须原样是 0，而不是「无期限制」。
	off := testConf()
	off.TaskCacheTTLSeconds = 0
	wantField(t, "关闭缓存", "终态秒数", taskDetailTTL(off, true), 0)
}

// Cache 恒为 nil（等价线上没配 Redis）：每次读都必须真的回源主表，
// 不允许「读到上一次副本」这种在内存测试里看不出来、上线才炸的路径。
func TestGetReplayTaskAlwaysReadsMainTableWithoutCache(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStateCompleted, nil)
	l := NewGetReplayTaskLogic(context.Background(), svcCtx)

	for i := 0; i < 3; i++ {
		info, err := l.GetReplayTask(getReplayReq(row.ReplayId))
		info = wantOK(t, info, err, "重复读终态行")
		wantField(t, "重复读终态行", "state", int32(info.GetState()), model.ReplayStateCompleted)
	}
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 3, "无缓存时每次都要回源主表")
}

func TestGetReplayTaskRejectsNonPositiveIDWithoutQuerying(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedReplay(db, model.ReplayStatePending, nil)
	before := snapshotWrites(db)

	for _, id := range []int64{0, -1} {
		info, err := NewGetReplayTaskLogic(context.Background(), svcCtx).GetReplayTask(getReplayReq(id))
		wantFail(t, info, err, model.ErrReplayTaskNotFound, "非正数 replay_id")
	}
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 0, "非正数 replay_id 不得查库")
	wantNoWrites(t, db, before, "非正数 replay_id")
	wantEvents(t, db, nil, "非正数 replay_id")
}

// 查无此行必须是错误而不是零值 Info：Worker 会拿 version=0 当 expected_version，
// 之后每一次 ReportReplayProgress 都会撞版本冲突，任务永远停在原地。
func TestGetReplayTaskMissingRowIsNotFoundNotZeroInfo(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedReplay(db, model.ReplayStateMerging, nil)

	info, err := NewGetReplayTaskLogic(context.Background(), svcCtx).GetReplayTask(getReplayReq(4242))
	wantFail(t, info, err, model.ErrReplayTaskNotFound, "不存在的回放任务")
	wantCalls(t, db, "ReplayTasks.FindOne", 0, 1, "不存在的回放任务应真的查过一次")
	wantNoLeak(t, db, "不存在的回放任务")
}

func TestGetReplayTaskFailsClosedOnReadError(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	task := seedReplay(db, model.ReplayStateUploading, nil)
	db.failOn("ReplayTasks.FindOne", errModelDown)

	info, err := NewGetReplayTaskLogic(context.Background(), svcCtx).GetReplayTask(getReplayReq(task.ReplayId))
	wantModelDown(t, err)
	if !isNilPtr(info) {
		t.Errorf("读故障路径不得带回响应体：%+v", info)
	}
	wantEvents(t, db, nil, "读故障")
}

// ---------------------------------------------------------------- ListReplayTasks

// 过滤口径 + 「rpc 没暴露的条件一律不凭空打开」。
// ReplayTaskFilter 里的 RecordId/AnchorMid 是真实现支持的列过滤，但 ListReplayTasksReq
// 里没有这两个字段（契约缺口，见 services/live-media/README）：
// logic 若拿别的字段去凑（比如把 live_session_id 填进 record_id），
// 就会答出一个看起来对、实际上过滤错列的结果集。
func TestListReplayTasksPassesNormalizedFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedReplay(db, model.ReplayStateMerging, nil)
	b := seedReplay(db, model.ReplayStateCompleted, func(r *model.LiveReplayTask) {
		r.LiveSession = testSession2
	})
	c := seedReplay(db, model.ReplayStateCompleted, func(r *model.LiveReplayTask) { r.RoomId = testRoomOther })
	before := snapshotWrites(db)

	svc := NewListReplayTasksLogic(context.Background(), svcCtx)

	in := listReplaysReq(1, 10)
	in.RoomId = testRoomID
	reply, err := svc.ListReplayTasks(in)
	reply = wantOK(t, reply, err, "按房间过滤")
	wantField(t, "按房间过滤", "排序为 replay_id DESC", replayOrder(reply.GetTasks()), joinInt64([]int64{b.ReplayId, a.ReplayId}))
	wantField(t, "按房间过滤", "total 是过滤后的总数", reply.GetPage().GetTotal(), int32(2))
	ff := db.lastReplayList
	if ff == nil {
		t.Fatal("model 未被调用，过滤条件无从校验")
	}
	wantField(t, "按房间过滤", "room_id", ff.RoomId, testRoomID)
	wantField(t, "按房间过滤", "未给的场次不过滤", ff.SessionId, int64(0))
	wantField(t, "按房间过滤", "未给的状态不过滤", ff.State, int32(0))
	wantField(t, "按房间过滤", "rpc 未暴露 record_id，不得凭空填", ff.RecordId, int64(0))
	wantField(t, "按房间过滤", "rpc 未暴露 anchor_mid，不得凭空填", ff.AnchorMid, int64(0))
	wantField(t, "按房间过滤", "pn", ff.Pn, int32(1))
	wantField(t, "按房间过滤", "ps", ff.Ps, int32(10))
	wantField(t, "按房间过滤", "max_ps", ff.MaxPageSize, int32(testConf().MaxListPageSize))

	in2 := listReplaysReq(1, 10)
	in2.LiveSessionId = testSession2
	in2.State = rpc.ReplayState_REPLAY_STATE_COMPLETED
	reply2, err := svc.ListReplayTasks(in2)
	reply2 = wantOK(t, reply2, err, "按场次+状态过滤")
	wantField(t, "按场次+状态过滤", "行数", int32(len(reply2.GetTasks())), int32(1))
	wantField(t, "按场次+状态过滤", "命中行", reply2.GetTasks()[0].GetReplayId(), b.ReplayId)
	wantField(t, "按场次+状态过滤", "session_id", db.lastReplayList.SessionId, testSession2)
	wantField(t, "按场次+状态过滤", "state", db.lastReplayList.State, model.ReplayStateCompleted)

	// 不带任何条件仍是合法的全表读（运营排障用），跨房间的行必须出现。
	reply3, err := svc.ListReplayTasks(listReplaysReq(1, 10))
	reply3 = wantOK(t, reply3, err, "不带过滤条件")
	if _, ok := findReplayRow(reply3, c.ReplayId); !ok {
		t.Errorf("跨房间的行必须出现（本方法不是房间隔离读口）：%+v", reply3)
	}

	wantNoWrites(t, db, before, "回放列表只读")
	wantEvents(t, db, nil, "回放列表只读")
}

// 过滤值越界必须报错且不查库：退化成「恒空结果集」会把调用方的枚举版本错误
// 伪装成「这个房间没有回放」。
func TestListReplayTasksRejectsOutOfRangeStateFilter(t *testing.T) {
	for _, st := range []rpc.ReplayState{rpc.ReplayState(9), rpc.ReplayState(-1)} {
		label := "state=" + strconv.Itoa(int(st))
		t.Run(label, func(t *testing.T) {
			db := newStore()
			svcCtx := newTestSvc(db)
			seedReplay(db, model.ReplayStatePending, nil)
			before := snapshotWrites(db)
			listCalls := db.count("ReplayTasks.List")

			in := listReplaysReq(1, 20)
			in.State = st
			reply, err := NewListReplayTasksLogic(context.Background(), svcCtx).ListReplayTasks(in)
			wantFail(t, reply, err, model.ErrInvalidTransition, label)
			wantCalls(t, db, "ReplayTasks.List", listCalls, 0, "越界枚举不得查库")
			wantNoWrites(t, db, before, label)
		})
	}

	// UNSPECIFIED(0) 是「不过滤」，且必须原样以 0 传下去（model 只在 >0 时附加条件）。
	db := newStore()
	svcCtx := newTestSvc(db)
	row := seedReplay(db, model.ReplayStatePending, nil)
	in := listReplaysReq(1, 20)
	in.State = rpc.ReplayState_REPLAY_STATE_UNSPECIFIED
	reply, err := NewListReplayTasksLogic(context.Background(), svcCtx).ListReplayTasks(in)
	reply = wantOK(t, reply, err, "UNSPECIFIED 表示不过滤")
	wantField(t, "UNSPECIFIED 表示不过滤", "行数", int32(len(reply.GetTasks())), int32(1))
	wantField(t, "UNSPECIFIED 表示不过滤", "命中行", reply.GetTasks()[0].GetReplayId(), row.ReplayId)
	wantField(t, "UNSPECIFIED 表示不过滤", "传给 model 的 state", db.lastReplayList.State, int32(0))
}

func TestListReplayTasksPagingAndClamping(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < 3; i++ {
		seedReplay(db, model.ReplayStateUploading, nil)
	}
	svc := NewListReplayTasksLogic(context.Background(), svcCtx)

	first, err := svc.ListReplayTasks(listReplaysReq(1, 2))
	first = wantOK(t, first, err, "第一页")
	wantField(t, "第一页", "行数", int32(len(first.GetTasks())), int32(2))
	wantField(t, "第一页", "total 是全量数而非本页数", first.GetPage().GetTotal(), int32(3))
	wantField(t, "第一页", "pn 原样透传", db.lastReplayList.Pn, int32(1))

	second, err := svc.ListReplayTasks(listReplaysReq(2, 2))
	second = wantOK(t, second, err, "第二页")
	wantField(t, "第二页", "行数", int32(len(second.GetTasks())), int32(1))
	wantField(t, "第二页", "pn 原样透传", db.lastReplayList.Pn, int32(2))
	if _, ok := findReplayRow(second, first.GetTasks()[0].GetReplayId()); ok {
		t.Errorf("第二页与第一页首行重叠：OFFSET 没生效")
	}

	if _, err := svc.ListReplayTasks(listReplaysReq(1, 999)); err != nil {
		t.Fatalf("ps 越界应夹取而不是报错：%v", err)
	}
	wantField(t, "ps 越界", "夹取后的 ps", db.lastReplayList.Ps, int32(20))

	if _, err := svc.ListReplayTasks(&rpc.ListReplayTasksReq{}); err != nil {
		t.Fatalf("缺 page 应归一为 1/20：%v", err)
	}
	wantField(t, "缺 page", "pn", db.lastReplayList.Pn, int32(1))
	wantField(t, "缺 page", "ps", db.lastReplayList.Ps, int32(20))

	if _, err := svc.ListReplayTasks(listReplaysReq(-3, -5)); err != nil {
		t.Fatalf("pn/ps 非正数应归一而不是报错：%v", err)
	}
	wantField(t, "pn/ps 归一", "pn", db.lastReplayList.Pn, int32(1))
	wantField(t, "pn/ps 归一", "ps", db.lastReplayList.Ps, int32(20))

	// OFFSET 的代价是「线性扫描后丢弃的行数」，页码乘页大小就是扫描面：超窗口拒绝且不查库。
	listCalls := db.count("ReplayTasks.List")
	before := snapshotWrites(db)
	deep, err := svc.ListReplayTasks(listReplaysReq(int32(maxListOffset/20+2), 20))
	wantFail(t, deep, err, model.ErrInvalidPage, "深翻页")
	if !strings.Contains(err.Error(), "OFFSET") {
		t.Errorf("错误必须说明是 OFFSET 代价失控：%v", err)
	}
	wantCalls(t, db, "ReplayTasks.List", listCalls, 0, "深翻页不得扫库")
	wantNoWrites(t, db, before, "深翻页")

	// 窗口边缘（offset == maxListOffset）仍允许，且判定用的是夹取后的 ps。
	if _, err := svc.ListReplayTasks(listReplaysReq(int32(maxListOffset/20+1), 999)); err != nil {
		t.Fatalf("恰好落在窗口边缘不应拒绝：%v", err)
	}
}

// 逐行投影 + 空结果不是错误 + 依赖故障 fail closed。
func TestListReplayTasksProjectsRowsAndFailsClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	a := seedReplay(db, model.ReplayStateCompleted, func(r *model.LiveReplayTask) {
		r.SegmentCount, r.GapCount, r.DurationMs = 99, 7, 600000
		r.Reason, r.ErrMsg, r.Version = model.ReasonDataGap, "merge hole", 6
	})
	b := seedReplay(db, model.ReplayStateFailed, func(r *model.LiveReplayTask) {
		r.RoomId, r.LiveSession, r.Title = testRoomOther, testSession2, "另一场"
	})
	svc := NewListReplayTasksLogic(context.Background(), svcCtx)
	before := snapshotWrites(db)

	reply, err := svc.ListReplayTasks(listReplaysReq(1, 10))
	reply = wantOK(t, reply, err, "列表逐行投影")
	wantField(t, "列表逐行投影", "行数", int32(len(reply.GetTasks())), int32(2))
	for _, info := range reply.GetTasks() {
		switch info.GetReplayId() {
		case a.ReplayId:
			wantReplayEchoesRow(t, "列表逐行投影 A", info, mustReplay(t, db, a.ReplayId))
		case b.ReplayId:
			wantReplayEchoesRow(t, "列表逐行投影 B", info, mustReplay(t, db, b.ReplayId))
		default:
			t.Fatalf("列表出现了未知行：%+v", info)
		}
	}
	// 脏快照照实回传：读侧不做二次推断（gap_count 与切片表无关）。
	first, ok := findReplayRow(reply, a.ReplayId)
	if !ok {
		t.Fatal("A 行未出现在列表里")
	}
	wantField(t, "脏快照照实回传", "gap_count", first.GetGapCount(), int64(7))

	// 空结果集是正常答复（不是错误），且 total 为 0。
	in := listReplaysReq(1, 10)
	in.RoomId = 999999
	empty, err := svc.ListReplayTasks(in)
	empty = wantOK(t, empty, err, "无匹配行")
	wantField(t, "无匹配行", "行数", int32(len(empty.GetTasks())), int32(0))
	wantField(t, "无匹配行", "total", empty.GetPage().GetTotal(), int32(0))

	// 依赖故障必须原样抛出，不能被读成「没有数据」。
	db.failOn("ReplayTasks.List", errModelDown)
	failed, err := svc.ListReplayTasks(listReplaysReq(1, 10))
	wantModelDown(t, err)
	if !isNilPtr(failed) {
		t.Errorf("故障路径不得带回响应体：%+v", failed)
	}
	wantNoWrites(t, db, before, "列表只读")
	wantEvents(t, db, nil, "列表只读")
}

// ---------------------------------------------------------------- ListReplayAssetRefs

// 引用行列表是本服务唯一能按「发布态」筛选回放的入口，
// 所以它的三条过滤开关必须逐条钉住：
//   - review_state=0（UNSPECIFIED）在这里只能是「不过滤」：0 同时也是列值意义上的
//     「未同步」（契约缺口，见方法头注释），本入口无法表达「只查未同步」；
//   - 精确查未同步要靠 ReplayRefFilter.OnlyUnsynced，那是投影补刷任务的内部开关，
//     本 RPC 恒传 false —— 一旦打开，「review_state=3」这类过滤就会被静默改写语义；
//   - anchor_mid 在 rpc 里有，必须原样透传（主播维度的回放清单是运营读口）。
func TestListReplayAssetRefsPassesNormalizedFiltersToModel(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	mine := seedReplayRef(db, func(r *model.LiveReplayAssetRef) { r.ReviewState = model.ReviewStatePublished })
	otherSession := seedReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.LiveSession, r.ReviewState = testSession2, model.ReviewStateReviewing
	})
	otherAnchor := seedReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.AnchorMid, r.ReviewState = testAnchor+1, model.ReviewStatePublished
	})
	before := snapshotWrites(db)

	svc := NewListReplayAssetRefsLogic(context.Background(), svcCtx)

	in := listRefsReq(1, 10)
	in.RoomId = testRoomID
	reply, err := svc.ListReplayAssetRefs(in)
	reply = wantOK(t, reply, err, "按房间过滤引用")
	wantField(t, "按房间过滤引用", "排序为 id DESC", refOrder(reply.GetRefs()),
		joinInt64([]int64{otherAnchor.Id, otherSession.Id, mine.Id}))
	ff := db.lastReplayRefList
	if ff == nil {
		t.Fatal("model 未被调用，过滤条件无从校验")
	}
	wantField(t, "按房间过滤引用", "room_id", ff.RoomId, testRoomID)
	wantField(t, "按房间过滤引用", "未给的场次不过滤", ff.SessionId, int64(0))
	wantField(t, "按房间过滤引用", "未给的主播不过滤", ff.AnchorMid, int64(0))
	wantField(t, "按房间过滤引用", "未给的投影状态不过滤", ff.ReviewState, int32(0))
	wantField(t, "按房间过滤引用", "内部补刷开关不得被公开入口打开", ff.OnlyUnsynced, false)
	wantField(t, "按房间过滤引用", "pn", ff.Pn, int32(1))
	wantField(t, "按房间过滤引用", "ps", ff.Ps, int32(10))
	wantField(t, "按房间过滤引用", "max_ps", ff.MaxPageSize, int32(testConf().MaxListPageSize))

	in2 := listRefsReq(1, 10)
	in2.AnchorMid = testAnchor
	in2.ReviewState = rpc.ReviewState_REVIEW_STATE_REVIEWING
	reply2, err := svc.ListReplayAssetRefs(in2)
	reply2 = wantOK(t, reply2, err, "按主播+投影态过滤")
	wantField(t, "按主播+投影态过滤", "行数", int32(len(reply2.GetRefs())), int32(1))
	wantField(t, "按主播+投影态过滤", "命中行", reply2.GetRefs()[0].GetId(), otherSession.Id)
	wantField(t, "按主播+投影态过滤", "anchor_mid", db.lastReplayRefList.AnchorMid, testAnchor)
	wantField(t, "按主播+投影态过滤", "review_state", db.lastReplayRefList.ReviewState, model.ReviewStateReviewing)
	wantField(t, "按主播+投影态过滤", "OnlyUnsynced", db.lastReplayRefList.OnlyUnsynced, false)

	// 已发布 + 未同步（0）两类行在「不过滤」时都必须出现：
	// 未同步行不能被隐藏，否则调用方会把「投影还没跟上」读成「没有这份回放」。
	unsynced := seedReplayRef(db, func(r *model.LiveReplayAssetRef) { r.ReviewState = model.ReviewStateUnsynced })
	reply3, err := svc.ListReplayAssetRefs(listRefsReq(1, 10))
	reply3 = wantOK(t, reply3, err, "不过滤")
	wantField(t, "不过滤", "行数（含未同步行）", int32(len(reply3.GetRefs())), int32(4))
	if _, ok := findRefRow(reply3, unsynced.Id); !ok {
		t.Errorf("review_state=0 的未同步行必须出现在不过滤的结果里：%+v", reply3)
	}

	wantNoWrites(t, db, before, "引用列表只读")
	wantEvents(t, db, nil, "引用列表只读")
}

func TestListReplayAssetRefsRejectsOutOfRangeReviewState(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	seedReplayRef(db, nil)
	before := snapshotWrites(db)
	listCalls := db.count("ReplayRefs.List")

	for _, st := range []rpc.ReviewState{rpc.ReviewState(6), rpc.ReviewState(-2)} {
		label := "review_state=" + strconv.Itoa(int(st))
		in := listRefsReq(1, 20)
		in.ReviewState = st
		reply, err := NewListReplayAssetRefsLogic(context.Background(), svcCtx).ListReplayAssetRefs(in)
		wantFail(t, reply, err, model.ErrInvalidReviewState, label)
	}
	wantCalls(t, db, "ReplayRefs.List", listCalls, 0, "越界投影值不得查库")
	wantNoWrites(t, db, before, "越界投影值")
	wantEvents(t, db, nil, "越界投影值")
}

func TestListReplayAssetRefsPagingAndClamping(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	for i := 0; i < 3; i++ {
		seedReplayRef(db, nil)
	}
	svc := NewListReplayAssetRefsLogic(context.Background(), svcCtx)

	first, err := svc.ListReplayAssetRefs(listRefsReq(1, 2))
	first = wantOK(t, first, err, "第一页")
	wantField(t, "第一页", "行数", int32(len(first.GetRefs())), int32(2))
	wantField(t, "第一页", "total 是全量数而非本页数", first.GetPage().GetTotal(), int32(3))

	second, err := svc.ListReplayAssetRefs(listRefsReq(2, 2))
	second = wantOK(t, second, err, "第二页")
	wantField(t, "第二页", "pn 原样透传", db.lastReplayRefList.Pn, int32(2))
	wantField(t, "第二页", "行数", int32(len(second.GetRefs())), int32(1))
	if _, ok := findRefRow(second, first.GetRefs()[0].GetId()); ok {
		t.Errorf("第二页与第一页首行重叠：OFFSET 没生效")
	}

	if _, err := svc.ListReplayAssetRefs(listRefsReq(1, 999)); err != nil {
		t.Fatalf("ps 越界应夹取而不是报错：%v", err)
	}
	wantField(t, "ps 越界", "夹取后的 ps", db.lastReplayRefList.Ps, int32(20))

	if _, err := svc.ListReplayAssetRefs(&rpc.ListReplayAssetRefsReq{}); err != nil {
		t.Fatalf("缺 page 应归一为 1/20：%v", err)
	}
	wantField(t, "缺 page", "pn", db.lastReplayRefList.Pn, int32(1))
	wantField(t, "缺 page", "ps", db.lastReplayRefList.Ps, int32(20))

	listCalls := db.count("ReplayRefs.List")
	deep, err := svc.ListReplayAssetRefs(listRefsReq(int32(maxListOffset/20+2), 20))
	wantFail(t, deep, err, model.ErrInvalidPage, "深翻页")
	wantCalls(t, db, "ReplayRefs.List", listCalls, 0, "深翻页不得扫库")
}

// 已回收（retention_state=2）与带投影痕迹的行都要原样回传：
// 「产物已删但引用行留审计证据」是 AGENTS.md §8 的口径，读侧把它藏起来就等于销毁证据。
func TestListReplayAssetRefsProjectsProjectionAndFailsClosed(t *testing.T) {
	db := newStore()
	svcCtx := newTestSvc(db)
	reclaimed := seedReplayRef(db, func(r *model.LiveReplayAssetRef) {
		r.ReviewState, r.ReviewStateAt = model.ReviewStateDeleted, 1000002
		r.PublishedAt, r.LastEventId, r.Source = 1000001, "content.published.v1:evt-9", "video.rpc"
		r.RetentionState = model.RefRetentionStateReclaimed
		r.DurationMs, r.GapCount = 60000, 3
	})
	svc := NewListReplayAssetRefsLogic(context.Background(), svcCtx)

	reply, err := svc.ListReplayAssetRefs(listRefsReq(1, 10))
	reply = wantOK(t, reply, err, "已回收引用行照实回传")
	wantField(t, "已回收引用行照实回传", "行数", int32(len(reply.GetRefs())), int32(1))
	info := reply.GetRefs()[0]
	wantRefEchoesRow(t, "已回收引用行照实回传", info, mustRef(t, db, reclaimed.Id))
	wantField(t, "已回收引用行照实回传", "retention_state", info.GetRetentionState(), model.RefRetentionStateReclaimed)
	wantField(t, "已回收引用行照实回传", "review_state", int32(info.GetReviewState()), model.ReviewStateDeleted)
	// last_event_id / source 不进响应（信封外两列），但必须是库里那个值：
	// 它们是「谁把投影刷成这样的」唯一证据，响应里没有不等于库里可以没有。
	wantField(t, "信封外列", "库内 last_event_id", reclaimed.LastEventId, "content.published.v1:evt-9")
	wantField(t, "信封外列", "库内 source", reclaimed.Source, "video.rpc")

	// 空结果集是正常答复。
	in := listRefsReq(1, 10)
	in.RoomId = 999999
	empty, err := svc.ListReplayAssetRefs(in)
	empty = wantOK(t, empty, err, "无匹配引用行")
	wantField(t, "无匹配引用行", "行数", int32(len(empty.GetRefs())), int32(0))
	wantField(t, "无匹配引用行", "total", empty.GetPage().GetTotal(), int32(0))

	db.failOn("ReplayRefs.List", errModelDown)
	failed, err := svc.ListReplayAssetRefs(listRefsReq(1, 10))
	wantModelDown(t, err)
	if !isNilPtr(failed) {
		t.Errorf("故障路径不得带回响应体：%+v", failed)
	}
	wantEvents(t, db, nil, "引用列表故障")
}
