package logic

// crud_logic_test.go 覆盖合同/窗口的创建与读侧 7 个方法
// （CreateContract、GetContract、ListContracts、CreateWindow、GetWindow、ListWindows、ListExpiring）。
//
// 这里钉住的三件事：
//  1. 守卫一律发生在触库之前：参数非法时不能先查一次库再拒绝（写放大 + 掩盖真实原因）。
//  2. 窗口创建必须先过「关联合同生效中」，且合同校验失败时**一行都不落**：
//     孤儿窗口会让 CheckPlayable 在没有任何合同支撑的情况下判可播。
//  3. 写成功才动缓存：CreateWindow 插入失败时不得失效 CheckPlayable 缓存，
//     也不能推进状态；插入成功时必须失效旧结论，否则新窗口要等 TTL 才生效。
//
// 分页口径（替身与真 SQL 的差别，别误读）：替身复刻了 WHERE 过滤、ORDER BY 方向与 total，
// 但不做 LIMIT/OFFSET 切片；因此用例对 pn/ps 的断言是**透传值**，真正的分页由 model 的 SQL 负责，
// 那部分无单测（见 README 已知缺口）。

import (
	"context"
	"errors"
	"testing"

	"go-video/services/rights/model"
	"go-video/services/rights/rpc"
)

// === CreateContract ===

func TestCreateContractGuardsTouchNothing(t *testing.T) {
	for _, c := range []struct {
		label   string
		in      *rpc.CreateContractReq
		wantErr error
	}{
		{"owner_id=0", &rpc.CreateContractReq{OwnerId: 0, Title: "T", StartDate: 1, EndDate: 2}, model.ErrInvalidOwnerID},
		{"owner_id<0", &rpc.CreateContractReq{OwnerId: -1, Title: "T", StartDate: 1, EndDate: 2}, model.ErrInvalidOwnerID},
		{"title 为空", &rpc.CreateContractReq{OwnerId: 7001, Title: "", StartDate: 1, EndDate: 2}, model.ErrInvalidTitle},
		{"end_date=start_date", &rpc.CreateContractReq{OwnerId: 7001, Title: "T", StartDate: 100, EndDate: 100}, model.ErrInvalidDateRange},
		{"end_date<start_date", &rpc.CreateContractReq{OwnerId: 7001, Title: "T", StartDate: 100, EndDate: 99}, model.ErrInvalidDateRange},
	} {
		st := newStore()
		l := NewCreateContractLogic(context.Background(), newTestSvc(st))

		_, err := l.CreateContract(c.in)
		wantErrIs(t, c.label, err, c.wantErr)
		wantNoCall(t, c.label, st, 0)
	}
}

func TestCreateContractStoresActiveContractAndRepliesAssignedID(t *testing.T) {
	st := newStore()
	// 自增起点抬高： reply 里若是 0 或未回写的入参值，断言必定失败而不是恰好撞上 1。
	st.contract.next = 900
	l := NewCreateContractLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CreateContract(&rpc.CreateContractReq{
		OwnerId: 7001, Title: "某番剧中国大陆及港台授权", SignDate: 1_700_000_000,
		StartDate: 1_710_000_000, EndDate: 1_900_000_000,
		Regions: []string{"CN", "", "TW", "HK"},
	})
	wantNoErr(t, "新建合同", err)
	wantOps(t, "新建合同", st.log.opsFrom(before), []string{"contract.Insert"})

	c := reply.GetContract()
	wantEQ(t, "新建合同", "回写主键", c.GetContractId(), int64(901))
	wantEQ(t, "新建合同", "owner_id", c.GetOwnerId(), int64(7001))
	wantEQ(t, "新建合同", "title", c.GetTitle(), "某番剧中国大陆及港台授权")
	wantEQ(t, "新建合同", "sign_date", c.GetSignDate(), int64(1_700_000_000))
	wantEQ(t, "新建合同", "start_date", c.GetStartDate(), int64(1_710_000_000))
	wantEQ(t, "新建合同", "end_date", c.GetEndDate(), int64(1_900_000_000))
	// 新建合同一律 active：无需运营再补一次状态写。
	wantEQ(t, "新建合同", "state", c.GetState(), rpc.ContractState_CONTRACT_STATE_ACTIVE)
	wantEQ(t, "新建合同", "ctime 已落", c.GetCtime() > 0, true)
	wantEQ(t, "新建合同", "mtime 已落", c.GetMtime() > 0, true)
	// regions 里的空串在入库前被规范化掉，不会存成 "CN,,TW,HK"。
	wantEQ(t, "新建合同", "库存 regions CSV", st.contract.rows[901].RegionsCSV, "CN,TW,HK")
	wantEQ(t, "新建合同", "投影回列表", len(c.GetRegions()), 3)
	wantEQ(t, "新建合同", "投影第 2 项", c.GetRegions()[1], "TW")
}

func TestCreateContractWithoutRegionsStoresEmptyCSV(t *testing.T) {
	st := newStore()
	l := NewCreateContractLogic(context.Background(), newTestSvc(st))

	reply, err := l.CreateContract(&rpc.CreateContractReq{OwnerId: 7001, Title: "全球授权", StartDate: 1, EndDate: 2})
	wantNoErr(t, "无地区合同", err)
	// 空 regions 是合法形状（当前实现不校验）；下游按 CSV 反查时得到空列表而不是 [""]。
	wantEQ(t, "无地区合同", "库存 regions CSV", st.contract.rows[reply.GetContract().GetContractId()].RegionsCSV, "")
	wantEQ(t, "无地区合同", "投影 regions 行数", len(reply.GetContract().GetRegions()), 0)
}

func TestCreateContractInsertErrorPropagates(t *testing.T) {
	st := newStore()
	boom := errors.New("duplicate column")
	st.contract.failWith("Insert", boom)
	l := NewCreateContractLogic(context.Background(), newTestSvc(st))

	_, err := l.CreateContract(&rpc.CreateContractReq{OwnerId: 7001, Title: "T", StartDate: 1, EndDate: 2})
	wantErrIs(t, "插入失败", err, boom)
	wantEQ(t, "插入失败", "不落半行", len(st.contract.rows), 0)
}

// === GetContract ===

func TestGetContractGuardAndNotFound(t *testing.T) {
	st := newStore()
	l := NewGetContractLogic(context.Background(), newTestSvc(st))

	_, err := l.GetContract(&rpc.ContractReq{ContractId: 0})
	wantErrIs(t, "contract_id=0", err, model.ErrInvalidContractID)
	wantNoCall(t, "contract_id=0", st, 0)

	before := st.log.snapshot()
	_, err = l.GetContract(&rpc.ContractReq{ContractId: 404})
	wantErrIs(t, "查无此合同", err, model.ErrContractNotFound)
	wantOps(t, "查无此合同", st.log.opsFrom(before), []string{"contract.FindOne:404"})
}

func TestGetContractProjectsStoredRow(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive, "CN", "TW")
	l := NewGetContractLogic(context.Background(), newTestSvc(st))

	reply, err := l.GetContract(&rpc.ContractReq{ContractId: 1})
	wantNoErr(t, "读合同", err)
	wantEQ(t, "读合同", "contract_id", reply.GetContract().GetContractId(), int64(1))
	wantEQ(t, "读合同", "owner_id", reply.GetContract().GetOwnerId(), int64(7001))
	wantEQ(t, "读合同", "state", reply.GetContract().GetState(), rpc.ContractState_CONTRACT_STATE_ACTIVE)
	// regions 由 CSV 还原成列表：投影丢一段就等于运营看不到授权范围。
	wantStringsEQ(t, "读合同", "regions 由 CSV 还原", reply.GetContract().GetRegions(), []string{"CN", "TW"})
}

func TestGetContractReadErrorIsNotNotFound(t *testing.T) {
	st := newStore()
	down := errors.New("too many connections")
	st.contract.failWith("FindOne", down)
	l := NewGetContractLogic(context.Background(), newTestSvc(st))

	_, err := l.GetContract(&rpc.ContractReq{ContractId: 1})
	// 下游故障必须是故障：判成「合同不存在」会让运营以为数据被删了。
	wantErrIs(t, "合同读故障", err, down)
}

// === ListContracts ===

func TestListContractsPsGuardTouchesNothing(t *testing.T) {
	for _, ps := range []int32{0, -1, 51} {
		st := newStore()
		l := NewListContractsLogic(context.Background(), newTestSvc(st))

		_, err := l.ListContracts(&rpc.ListReq{OwnerId: 7001, Ps: ps})
		wantErrIs(t, "ps 非法", err, model.ErrPsTooLarge)
		wantNoCall(t, "ps 非法", st, 0)
	}
}

func TestListContractsFilterAndPagingPassThrough(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive)
	seedContract(st, model.ContractStateTerminated)
	// 另一版权方的合同：不得出现在本次结果里。
	st.contract.rows[99] = &model.RightsContract{ContractID: 99, OwnerID: 8002, Title: "别家合同", State: model.ContractStateActive}
	l := NewListContractsLogic(context.Background(), newTestSvc(st))
	// 布景的 Insert 也在同一条轨迹上：断言前必须重新取起点，否则序列永远多几行。
	before := st.log.snapshot()

	reply, err := l.ListContracts(&rpc.ListReq{OwnerId: 7001, State: rpc.ContractState_CONTRACT_STATE_ACTIVE, Pn: 2, Ps: 10})
	wantNoErr(t, "按版权方查合同", err)
	wantOps(t, "按版权方查合同", st.log.opsFrom(before), []string{"contract.List:7001/1/2/10"})
	wantEQ(t, "按版权方查合同", "total 只算命中行", reply.GetTotal(), int32(1))
	wantEQ(t, "按版权方查合同", "行数", len(reply.GetContracts()), 1)
	wantEQ(t, "按版权方查合同", "行内容", reply.GetContracts()[0].GetContractId(), int64(1))

	// state=UNSPECIFIED 是「不筛选」而不是「查 state=0 的行」。
	all, err := l.ListContracts(&rpc.ListReq{Ps: 50})
	wantNoErr(t, "不筛选", err)
	wantEQ(t, "不筛选", "total", all.GetTotal(), int32(3))
	wantEQ(t, "不筛选", "按 contract_id 降序第 1", all.GetContracts()[0].GetContractId(), int64(99))
	wantEQ(t, "不筛选", "按 contract_id 降序末位", all.GetContracts()[2].GetContractId(), int64(1))
}

func TestListContractsStateFilterExcludesTerminated(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive)
	seedContract(st, model.ContractStateTerminated)
	l := NewListContractsLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListContracts(&rpc.ListReq{State: rpc.ContractState_CONTRACT_STATE_TERMINATED, Ps: 10})
	wantNoErr(t, "只看终止合同", err)
	wantEQ(t, "只看终止合同", "行数", len(reply.GetContracts()), 1)
	wantEQ(t, "只看终止合同", "state", reply.GetContracts()[0].GetState(), rpc.ContractState_CONTRACT_STATE_TERMINATED)
}

func TestListContractsErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("count query failed")
	st.contract.failWith("List", down)
	l := NewListContractsLogic(context.Background(), newTestSvc(st))

	_, err := l.ListContracts(&rpc.ListReq{Ps: 10})
	wantErrIs(t, "合同列表读故障", err, down)
}

// === CreateWindow ===

func TestCreateWindowGuardsTouchNothing(t *testing.T) {
	for _, c := range []struct {
		label   string
		in      *rpc.CreateWindowReq
		wantErr error
	}{
		{"contract_id=0", &rpc.CreateWindowReq{ContractId: 0, ContentId: 5, Region: "CN", StartTime: 1, EndTime: 2}, model.ErrInvalidContractID},
		{"content_id=0", &rpc.CreateWindowReq{ContractId: 1, ContentId: 0, Region: "CN", StartTime: 1, EndTime: 2}, model.ErrInvalidContentID},
		{"region 为空", &rpc.CreateWindowReq{ContractId: 1, ContentId: 5, Region: "", StartTime: 1, EndTime: 2}, model.ErrInvalidRegion},
		{"end_time=start_time", &rpc.CreateWindowReq{ContractId: 1, ContentId: 5, Region: "CN", StartTime: 10, EndTime: 10}, model.ErrInvalidTimeRange},
		{"end_time<start_time", &rpc.CreateWindowReq{ContractId: 1, ContentId: 5, Region: "CN", StartTime: 10, EndTime: 9}, model.ErrInvalidTimeRange},
	} {
		st := newStore()
		// 合同存在也不该被读到：参数守卫在合同校验之前。
		seedContract(st, model.ContractStateActive)
		l := NewCreateWindowLogic(context.Background(), newTestSvc(st))
		before := st.log.snapshot()

		_, err := l.CreateWindow(c.in)
		wantErrIs(t, c.label, err, c.wantErr)
		wantNoCall(t, c.label, st, before)
	}
}

func TestCreateWindowRequiresActiveContractAndWritesNoRow(t *testing.T) {
	for _, c := range []struct {
		label string
		state int32
		want  error
	}{
		{"合同已终止", model.ContractStateTerminated, model.ErrContractNotActive},
		{"合同不存在", 0, model.ErrContractNotFound},
	} {
		st := newStore()
		if c.state != 0 {
			seedContract(st, c.state)
		}
		l := NewCreateWindowLogic(context.Background(), newTestSvc(st))

		_, err := l.CreateWindow(&rpc.CreateWindowReq{
			ContractId: 1, ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
			Region: "CN", StartTime: nowPlus(-10), EndTime: nowPlus(3600),
		})
		wantErrIs(t, c.label, err, c.want)
		// 合同校验失败绝不能留下窗口行：孤儿窗口会让 CheckPlayable 无依据判可播。
		wantEQ(t, c.label, "不写窗口表", len(st.window.rows), 0)
		wantEQ(t, c.label, "不碰缓存", st.log.countPrefix("cache."), 0)
	}
}

func TestCreateWindowInsertsActiveAndInvalidatesStaleCache(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive, "CN")
	st.window.next = 400
	// 布一条旧的「不可播」负缓存：新窗口若只写库不失效缓存，要等 TTL 才生效。
	st.cache.data[keyCheck(5, pgc, "CN")] = chkEntry{playable: false}
	l := NewCreateWindowLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.CreateWindow(&rpc.CreateWindowReq{
		ContractId: 1, ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
		Region: "CN", StartTime: nowPlus(-10), EndTime: nowPlus(3600),
	})
	wantNoErr(t, "新建窗口", err)
	wantOps(t, "新建窗口", st.log.opsFrom(before), []string{
		"contract.FindOne:1",
		"win.Insert",
		"cache.Del:rights:chk:5:1:CN",
	})

	w := reply.GetWindow()
	wantEQ(t, "新建窗口", "回写主键", w.GetWindowId(), int64(401))
	wantEQ(t, "新建窗口", "contract_id", w.GetContractId(), int64(1))
	wantEQ(t, "新建窗口", "content_id", w.GetContentId(), int64(5))
	wantEQ(t, "新建窗口", "content_type", w.GetContentType(), rpc.ContentType_CONTENT_TYPE_PGC)
	wantEQ(t, "新建窗口", "region", w.GetRegion(), "CN")
	wantEQ(t, "新建窗口", "state", w.GetState(), rpc.WindowState_WINDOW_STATE_ACTIVE)
	wantEQ(t, "新建窗口", "库存 state", st.window.rows[401].State, int32(model.WindowStateActive))
	_, cached := st.cache.entry(5, pgc, "CN")
	wantEQ(t, "新建窗口", "旧缓存已失效", cached, false)

	// 失效后第一次判定必须落库读到新窗口，而不是继续吃负缓存。
	_, err = NewCheckPlayableLogic(context.Background(), newTestSvc(st)).CheckPlayable(&rpc.CheckReq{
		ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC, Region: "CN",
	})
	wantNoErr(t, "新建窗口后判定", err)
}

func TestCreateWindowInsertFailureLeavesCacheUntouched(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive, "CN")
	boom := errors.New("deadlock found")
	st.window.failWith("Insert", boom)
	// 窗口没建成，缓存里既有结论仍然是对的，不该被顺手删掉。
	st.cache.data[keyCheck(5, pgc, "CN")] = chkEntry{playable: false}
	l := NewCreateWindowLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	_, err := l.CreateWindow(&rpc.CreateWindowReq{
		ContractId: 1, ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
		Region: "CN", StartTime: nowPlus(-10), EndTime: nowPlus(3600),
	})
	wantErrIs(t, "窗口插入失败", err, boom)
	wantOps(t, "窗口插入失败", st.log.opsFrom(before), []string{"contract.FindOne:1", "win.Insert"})
	wantEQ(t, "窗口插入失败", "不落行", len(st.window.rows), 0)
}

func TestCreateWindowContractLookupFailurePropagates(t *testing.T) {
	st := newStore()
	down := errors.New("read timeout")
	st.contract.failWith("FindOne", down)
	l := NewCreateWindowLogic(context.Background(), newTestSvc(st))

	_, err := l.CreateWindow(&rpc.CreateWindowReq{
		ContractId: 1, ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
		Region: "CN", StartTime: 1, EndTime: 2,
	})
	wantErrIs(t, "合同校验故障", err, down)
	wantEQ(t, "合同校验故障", "不写窗口表", len(st.window.rows), 0)
}

// TestCreateWindowAcceptsUnspecifiedContentType 钉住一个**已记录在 README 的缺口**，
// 不是把它当成期望行为：content_type 与 CheckPlayable 的 content_type 都不校验枚举，
// 于是 UNSPECIFIED(0) 能建窗口、也能被 0 类型的判定命中，形成一条与 PGC/UGC 平行的暗道。
// 日后加守卫时这条用例会红——那时请连 README 缺口一起删掉，不要改成宽容断言。
func TestCreateWindowAcceptsUnspecifiedContentType(t *testing.T) {
	st := newStore()
	seedContract(st, model.ContractStateActive, "CN")
	l := NewCreateWindowLogic(context.Background(), newTestSvc(st))

	reply, err := l.CreateWindow(&rpc.CreateWindowReq{
		ContractId: 1, ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_UNSPECIFIED,
		Region: "CN", StartTime: nowPlus(-10), EndTime: nowPlus(3600),
	})
	wantNoErr(t, "未校验 content_type", err)
	wantEQ(t, "未校验 content_type", "库存 content_type", st.window.rows[reply.GetWindow().GetWindowId()].ContentType, int32(0))
	wantEQ(t, "未校验 content_type", "回复投影为 UNSPECIFIED", reply.GetWindow().GetContentType(), rpc.ContentType_CONTENT_TYPE_UNSPECIFIED)

	playable, err := NewCheckPlayableLogic(context.Background(), newTestSvc(st)).CheckPlayable(&rpc.CheckReq{
		ContentId: 5, ContentType: rpc.ContentType_CONTENT_TYPE_UNSPECIFIED, Region: "CN",
	})
	wantNoErr(t, "未校验 content_type", err)
	wantEQ(t, "未校验 content_type", "0 类型判定命中了 0 类型窗口", playable.GetPlayable(), true)
}

// === GetWindow ===

func TestGetWindowGuardNotFoundAndProjection(t *testing.T) {
	st := newStore()
	l := NewGetWindowLogic(context.Background(), newTestSvc(st))

	_, err := l.GetWindow(&rpc.WindowReq{WindowId: 0})
	wantErrIs(t, "window_id=0", err, model.ErrInvalidWindowID)
	wantNoCall(t, "window_id=0", st, 0)

	before := st.log.snapshot()
	_, err = l.GetWindow(&rpc.WindowReq{WindowId: 404})
	wantErrIs(t, "查无此窗口", err, model.ErrWindowNotFound)
	wantOps(t, "查无此窗口", st.log.opsFrom(before), []string{"win.FindOne:404"})

	w := seedWindow(st, 5, ugc, "CN", 100, 200, model.WindowStateRevoked)
	reply, err := l.GetWindow(&rpc.WindowReq{WindowId: w.WindowID})
	wantNoErr(t, "读窗口", err)
	wantEQ(t, "读窗口", "window_id", reply.GetWindow().GetWindowId(), w.WindowID)
	wantEQ(t, "读窗口", "content_type", reply.GetWindow().GetContentType(), rpc.ContentType_CONTENT_TYPE_UGC)
	wantEQ(t, "读窗口", "state 投影", reply.GetWindow().GetState(), rpc.WindowState_WINDOW_STATE_REVOKED)
	wantEQ(t, "读窗口", "start_time", reply.GetWindow().GetStartTime(), int64(100))
	wantEQ(t, "读窗口", "end_time", reply.GetWindow().GetEndTime(), int64(200))
}

func TestGetWindowReadErrorIsNotNotFound(t *testing.T) {
	st := newStore()
	down := errors.New("bad connection")
	st.window.failWith("FindOne", down)
	l := NewGetWindowLogic(context.Background(), newTestSvc(st))

	_, err := l.GetWindow(&rpc.WindowReq{WindowId: 1})
	wantErrIs(t, "窗口读故障", err, down)
}

// === ListWindows ===

func TestListWindowsPsGuardTouchesNothing(t *testing.T) {
	for _, ps := range []int32{0, -1, 51} {
		st := newStore()
		l := NewListWindowsLogic(context.Background(), newTestSvc(st))

		_, err := l.ListWindows(&rpc.ListWindowsReq{ContentId: 5, Ps: ps})
		wantErrIs(t, "ps 非法", err, model.ErrPsTooLarge)
		wantNoCall(t, "ps 非法", st, 0)
	}
}

func TestListWindowsFilterCombinationIsAnd(t *testing.T) {
	st := newStore()
	only := seedWindow(st, 5, pgc, "CN", 100, 200, model.WindowStateActive)
	seedWindow(st, 5, pgc, "TW", 100, 200, model.WindowStateExpired)
	seedWindow(st, 5, ugc, "CN", 100, 200, model.WindowStateActive)
	newest := seedWindow(st, 6, pgc, "CN", 100, 200, model.WindowStateActive)
	l := NewListWindowsLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.ListWindows(&rpc.ListWindowsReq{
		ContentId: 5, ContractId: 1, ContentType: rpc.ContentType_CONTENT_TYPE_PGC,
		State: rpc.WindowState_WINDOW_STATE_ACTIVE, Pn: 1, Ps: 20,
	})
	wantNoErr(t, "四条件与", err)
	wantOps(t, "四条件与", st.log.opsFrom(before), []string{"win.List:5/1/1/1/1/20"})
	// 四个条件同时命中才留行：只有第一条满足 content=5 & 合同=1 & PGC & active。
	wantEQ(t, "四条件与", "行数", len(reply.GetWindows()), 1)
	wantEQ(t, "四条件与", "window_id", reply.GetWindows()[0].GetWindowId(), only.WindowID)
	wantEQ(t, "四条件与", "total", reply.GetTotal(), int32(1))

	// 不传条件时不筛选，返回全部（含 expired）。
	all, err := l.ListWindows(&rpc.ListWindowsReq{Ps: 20})
	wantNoErr(t, "不筛选", err)
	wantEQ(t, "不筛选", "total", all.GetTotal(), int32(4))
	wantEQ(t, "不筛选", "按 window_id 降序第 1", all.GetWindows()[0].GetWindowId(), newest.WindowID)
}

func TestListWindowsStateUnspecifiedIncludesAllStates(t *testing.T) {
	st := newStore()
	seedWindow(st, 5, pgc, "CN", 100, 200, model.WindowStateActive)
	seedWindow(st, 5, pgc, "CN", 100, 200, model.WindowStateRevoked)
	l := NewListWindowsLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListWindows(&rpc.ListWindowsReq{ContentId: 5, Ps: 20})
	wantNoErr(t, "state 不筛选", err)
	wantEQ(t, "state 不筛选", "行数", len(reply.GetWindows()), 2)
}

func TestListWindowsErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("syntax error near LIMIT")
	st.window.failWith("List", down)
	l := NewListWindowsLogic(context.Background(), newTestSvc(st))

	_, err := l.ListWindows(&rpc.ListWindowsReq{ContentId: 5, Ps: 20})
	wantErrIs(t, "窗口列表读故障", err, down)
}

// === ListExpiring ===

func TestListExpiringGuardOrderTouchesNothing(t *testing.T) {
	st := newStore()
	l := NewListExpiringLogic(context.Background(), newTestSvc(st))

	// within_seconds 先判：两个都非法时报 within，不报 ps。
	_, err := l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 0, Ps: 0})
	wantErrIs(t, "within=0", err, model.ErrExpiringWithinInvalid)
	wantNoCall(t, "within=0", st, 0)

	_, err = l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: -1, Ps: 10})
	wantErrIs(t, "within<0", err, model.ErrExpiringWithinInvalid)
	wantNoCall(t, "within<0", st, 0)

	_, err = l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 3600, Ps: 101})
	wantErrIs(t, "ps>100", err, model.ErrPsTooLarge)
	wantNoCall(t, "ps>100", st, 0)

	_, err = l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 3600, Ps: 0})
	wantErrIs(t, "ps=0", err, model.ErrPsTooLarge)
	wantNoCall(t, "ps=0", st, 0)
}

func TestListExpiringSelectsActiveWithinWindowInAscOrder(t *testing.T) {
	st := newStore()
	soon := seedWindow(st, 5, pgc, "CN", nowPlus(-7200), nowPlus(300), model.WindowStateActive)
	later := seedWindow(st, 6, pgc, "CN", nowPlus(-7200), nowPlus(600), model.WindowStateActive)
	far := seedWindow(st, 7, pgc, "CN", nowPlus(-7200), nowPlus(86_400), model.WindowStateActive)
	// 已到期但状态还没推进的 active 行：<= now+within 也命中，正是要被 cron 处理的对象。
	missed := seedWindow(st, 8, pgc, "CN", nowPlus(-7200), nowPlus(-60), model.WindowStateActive)
	seedWindow(st, 9, pgc, "CN", nowPlus(-7200), nowPlus(300), model.WindowStateExpired)
	seedWindow(st, 10, pgc, "CN", nowPlus(-7200), nowPlus(300), model.WindowStateRevoked)
	l := NewListExpiringLogic(context.Background(), newTestSvc(st))
	before := st.log.snapshot()

	reply, err := l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 3600, Pn: 1, Ps: 50})
	wantNoErr(t, "即将过期", err)
	wantOps(t, "即将过期", st.log.opsFrom(before), []string{"win.ListExpiring:3600/1/50"})

	got := idsOf(reply.GetWindows())
	// 只含 active 且 end_time<=now+within，按 end_time 升序：过期/撤权行不重复提醒，远端窗口不误报。
	wantEQ(t, "即将过期", "命中行数", len(got), 3)
	wantEQ(t, "即将过期", "升序第 1（最早到期）", got[0], missed.WindowID)
	wantEQ(t, "即将过期", "升序第 2", got[1], soon.WindowID)
	wantEQ(t, "即将过期", "升序第 3", got[2], later.WindowID)
	wantEQ(t, "即将过期", "total", reply.GetTotal(), int32(3))
	// 未到期的窗口不得混进提醒列表（否则 cron 会把远端窗口当临期处理）。
	wantEQ(t, "即将过期", "含远端窗口", contains(got, far.WindowID), false)
	// 纯读方法：不得顺手改状态或失效缓存（推进状态是 ExpireWindow 的职责）。
	wantEQ(t, "即将过期", "不改状态", st.log.countPrefix("win.UpdateState"), 0)
	wantEQ(t, "即将过期", "不碰缓存", st.log.countPrefix("cache."), 0)
}

func TestListExpiringEmptyResultIsNotError(t *testing.T) {
	st := newStore()
	seedWindow(st, 5, pgc, "CN", nowPlus(-7200), nowPlus(86_400), model.WindowStateActive)
	l := NewListExpiringLogic(context.Background(), newTestSvc(st))

	reply, err := l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 3600, Pn: 1, Ps: 50})
	wantNoErr(t, "无即将过期窗口", err)
	wantEQ(t, "无即将过期窗口", "total", reply.GetTotal(), int32(0))
	wantEQ(t, "无即将过期窗口", "行数", len(reply.GetWindows()), 0)
}

func TestListExpiringErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("count timed out")
	st.window.failWith("ListExpiring", down)
	l := NewListExpiringLogic(context.Background(), newTestSvc(st))

	_, err := l.ListExpiring(&rpc.ListExpiringReq{WithinSeconds: 3600, Ps: 50})
	wantErrIs(t, "过期扫描读故障", err, down)
}

// --- 本文件私有断言小工具 ---

func idsOf(ws []*rpc.Window) []int64 {
	out := make([]int64, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.GetWindowId())
	}
	return out
}

func contains(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
