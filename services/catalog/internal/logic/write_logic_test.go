package logic

// write_logic_test.go 覆盖 catalog 的 5 个写方法：CreateWork / CreateSeason /
// CreateEpisode / PublishEpisode / OfflineEpisode。
//
// 每个方法固定三类断言，缺一条就不算覆盖：
//  1. 入参守卫：拒绝时**一次依赖都不许碰**（callLog 快照 + fake 下游计数钉死），
//     防止「先落库再校验」把脏数据写进 PGC 目录。
//  2. 判定链：状态机与 rights/asset 前置校验的次序与结论（AGENTS.md §8）。
//  3. 副作用：真正写了几次库、写了什么状态、缓存失效有没有发生、返回值与入库行是否一致。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/catalog/internal/config"
	"go-video/services/catalog/internal/repository"
	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"
)

// --- 下游 fake 便捷构造 ---
//
// 返回类型刻意写成接口：直接把 (*fakeAsset)(nil) 传给接口参数会得到一个
// 「非 nil 的接口装着 nil 指针」，guard 的 `g.asset == nil` 判定就失效，
// 用例会以 panic 而非预期错误失败。

func assetOK(state repository.AssetState) repository.AssetClient {
	return &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, Duration: 1200, State: state}}
}

func assetDown(err error) repository.AssetClient {
	return &fakeAsset{err: err}
}

func rightsOK() repository.RightsClient {
	return &fakeRights{result: &repository.RightsCheckResult{Playable: true, WindowID: 9, EndTime: 2000}}
}

func rightsDown(err error) repository.RightsClient {
	return &fakeRights{err: err}
}

func rightsDenied() repository.RightsClient {
	return &fakeRights{result: &repository.RightsCheckResult{Playable: false}}
}

// === CreateWork ===

func TestCreateWorkRejectsBeforeTouchingStore(t *testing.T) {
	st := newStore()
	l := NewCreateWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	before := st.log.snapshot()

	if _, err := l.CreateWork(&rpc.CreateWorkReq{Typeid: 1, Operator: "op"}); !errors.Is(err, model.ErrInvalidTitle) {
		t.Fatalf("空标题 = %v, want ErrInvalidTitle", err)
	}
	wantNoCall(t, "空标题", st, before)

	if _, err := l.CreateWork(&rpc.CreateWorkReq{Title: "片名", Operator: "op"}); !errors.Is(err, model.ErrInvalidType) {
		t.Fatalf("typeid=0 = %v, want ErrInvalidType", err)
	}
	wantNoCall(t, "typeid=0", st, before)

	if _, err := l.CreateWork(&rpc.CreateWorkReq{Title: "片名", Typeid: -3, Operator: "op"}); !errors.Is(err, model.ErrInvalidType) {
		t.Fatalf("typeid<0 = %v, want ErrInvalidType", err)
	}
	wantNoCall(t, "typeid<0", st, before)
}

func TestCreateWorkStoresDraftAndEchoesRow(t *testing.T) {
	st := newStore()
	l := NewCreateWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	reply, err := l.CreateWork(&rpc.CreateWorkReq{
		Title: "长津湖", Typeid: model.WorkTypeMovie, Cover: "c.png", Intro: "i", Operator: "op-1",
	})
	if err != nil {
		t.Fatalf("CreateWork() = %v", err)
	}
	if reply.GetSeasonId() <= 0 {
		t.Fatalf("season_id = %d, want 新分配主键", reply.GetSeasonId())
	}
	row := st.work.rows[reply.GetSeasonId()]
	if row == nil {
		t.Fatalf("库里没有新作品行，rows=%v", st.work.rows)
	}
	// 返回值必须与入库行一致（缺陷 #83 同类：响应字段与落库值分叉会让运营看到假状态）。
	wantEQ(t, "建作品", "入库 state 必须是草稿", row.State, int32(model.StateDraft))
	wantEQ(t, "建作品", "reply.state", reply.GetState(), row.State)
	wantEQ(t, "建作品", "入库 title", row.Title, "长津湖")
	wantEQ(t, "建作品", "reply.title", reply.GetTitle(), row.Title)
	wantEQ(t, "建作品", "入库 typeid", row.TypeID, int32(model.WorkTypeMovie))
	wantEQ(t, "建作品", "reply.typeid", reply.GetTypeid(), row.TypeID)
	wantEQ(t, "建作品", "入库 cover", row.Cover, "c.png")
	wantEQ(t, "建作品", "reply.cover", reply.GetCover(), row.Cover)
	wantEQ(t, "建作品", "入库 intro", row.Intro, "i")
	wantEQ(t, "建作品", "reply.intro", reply.GetIntro(), row.Intro)
	wantEQ(t, "建作品", "只写一行", len(st.work.rows), 1)
	wantOps(t, "建作品", st.log.ops, []string{"work.Insert"})
}

func TestCreateWorkPropagatesStoreError(t *testing.T) {
	st := newStore()
	boom := errors.New("duplicate entry")
	st.work.failWith("Insert", boom)
	l := NewCreateWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.CreateWork(&rpc.CreateWorkReq{Title: "片名", Typeid: 1, Operator: "op"})
	wantErrIs(t, "建作品写库失败", err, boom)
	wantEQ(t, "建作品写库失败", "不留半行", len(st.work.rows), 0)
}

// === CreateSeason ===

func TestCreateSeasonGuardsAndParentSemantics(t *testing.T) {
	st := newStore()
	l := NewCreateSeasonLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	before := st.log.snapshot()

	if _, err := l.CreateSeason(&rpc.CreateSeasonReq{SeasonNo: 1, Operator: "op"}); !errors.Is(err, model.ErrInvalidWorkID) {
		t.Fatalf("season_id=0 = %v, want ErrInvalidWorkID", err)
	}
	if _, err := l.CreateSeason(&rpc.CreateSeasonReq{SeasonId: -1, SeasonNo: 1}); !errors.Is(err, model.ErrInvalidWorkID) {
		t.Fatalf("season_id<0 = %v, want ErrInvalidWorkID", err)
	}
	if _, err := l.CreateSeason(&rpc.CreateSeasonReq{SeasonId: 10}); !errors.Is(err, model.ErrInvalidSeasonNo) {
		t.Fatalf("season_no=0 = %v, want ErrInvalidSeasonNo", err)
	}
	wantNoCall(t, "建季守卫", st, before)

	// 契约要点：入参 season_id 是**父作品**主季 ID，回复里的 season_id 是新建季自身 ID。
	// 替身把父作品的主键推到 777：两张表各自自增时 ID 完全可能相同，错开才能让
	// 下面「回复回显了入参 ID」这条断言真的有机会失败。
	st.work.next = 776
	work := seedWork(st, model.StateDraft, model.WorkTypeAnima, "番剧")
	calls := st.log.snapshot()
	reply, err := l.CreateSeason(&rpc.CreateSeasonReq{
		SeasonId: work.SeasonID, SeasonNo: 2, Title: "第二季", Cover: "s.png", Operator: "op",
	})
	if err != nil {
		t.Fatalf("CreateSeason() = %v", err)
	}
	row := st.season.rows[reply.GetSeasonId()]
	if row == nil {
		t.Fatalf("季未入库，reply=%+v", reply)
	}
	if row.SeasonID == work.SeasonID {
		t.Errorf("新季主键与父作品相同（%d），说明回复回显了入参 ID 而非新分配 ID", row.SeasonID)
	}
	wantEQ(t, "建季", "work_id 指向父作品", row.WorkID, work.SeasonID)
	wantEQ(t, "建季", "season_no", row.SeasonNo, int32(2))
	wantEQ(t, "建季", "reply.season_no", reply.GetSeasonNo(), row.SeasonNo)
	wantEQ(t, "建季", "state 草稿", row.State, int32(model.StateDraft))
	wantEQ(t, "建季", "reply.state", reply.GetState(), row.State)
	wantEQ(t, "建季", "reply.title", reply.GetTitle(), row.Title)
	wantEQ(t, "建季", "reply.cover", reply.GetCover(), row.Cover)
	wantOps(t, "建季", st.log.opsFrom(calls), []string{"season.Insert"})
}

func TestCreateSeasonPropagatesStoreError(t *testing.T) {
	st := newStore()
	boom := errors.New("deadlock")
	st.season.failWith("Insert", boom)
	l := NewCreateSeasonLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.CreateSeason(&rpc.CreateSeasonReq{SeasonId: 10, SeasonNo: 1, Operator: "op"})
	wantErrIs(t, "建季写库失败", err, boom)
	wantEQ(t, "建季写库失败", "不留半行", len(st.season.rows), 0)
}

// === CreateEpisode ===

func TestCreateEpisodeGuardsRunBeforeAssetCheck(t *testing.T) {
	st := newStore()
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateScanned}}
	l := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, asset))

	cases := []struct {
		label string
		in    *rpc.CreateEpisodeReq
		want  error
	}{
		{"season_id=0", &rpc.CreateEpisodeReq{EpNo: 1, AssetId: 77}, model.ErrInvalidSeasonID},
		{"ep_no=0", &rpc.CreateEpisodeReq{SeasonId: 10, AssetId: 77}, model.ErrInvalidEpNo},
		{"asset_id=0", &rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1}, model.ErrInvalidAssetID},
		{"season_id<0", &rpc.CreateEpisodeReq{SeasonId: -1, EpNo: 1, AssetId: 77}, model.ErrInvalidSeasonID},
	}
	for _, c := range cases {
		before := st.log.snapshot()
		_, err := l.CreateEpisode(c.in)
		wantErrIs(t, c.label, err, c.want)
		wantNoCall(t, c.label+"：不得触库", st, before)
		// 入参不合法时连媒资都不该去查（省一次下游 RPC，也避免拿 0 去查）。
		wantEQ(t, c.label, "asset 调用次数", asset.calls, 0)
	}
}

func TestCreateEpisodeAssetStateGate(t *testing.T) {
	cases := []struct {
		label    string
		state    repository.AssetState
		wantErr  error
		wantPass bool
	}{
		{"已扫描可建集", repository.AssetStateScanned, nil, true},
		{"已转码可建集", repository.AssetStateTranscoded, nil, true},
		{"仅上传完成不可建集", repository.AssetStateUploaded, model.ErrAssetNotReady, false},
		{"媒资处理失败不可建集", repository.AssetStateFailed, model.ErrAssetNotReady, false},
	}
	for _, c := range cases {
		st := newStore()
		l := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, assetOK(c.state)))
		calls := st.log.snapshot()

		reply, err := l.CreateEpisode(&rpc.CreateEpisodeReq{
			SeasonId: 10, EpNo: 1, Title: "第1集", AssetId: 77, Duration: 1200, Operator: "op",
		})
		if c.wantPass {
			if err != nil {
				t.Fatalf("%s：CreateEpisode() = %v, want 放行", c.label, err)
			}
			row := st.ep.rows[reply.GetEpid()]
			wantEQ(t, c.label, "入库 state 草稿", row.State, int32(model.EpStateDraft))
			wantEQ(t, c.label, "reply.state", reply.GetState(), row.State)
			wantEQ(t, c.label, "入库 asset_id", row.AssetID, int64(77))
			wantEQ(t, c.label, "reply.asset_id", reply.GetAssetId(), row.AssetID)
			wantEQ(t, c.label, "入库 season_id", row.SeasonID, int64(10))
			wantEQ(t, c.label, "reply.season_id", reply.GetSeasonId(), row.SeasonID)
			wantEQ(t, c.label, "入库 ep_no", row.EpNo, int32(1))
			wantEQ(t, c.label, "reply.ep_no", reply.GetEpNo(), row.EpNo)
			wantEQ(t, c.label, "入库 duration", row.Duration, int64(1200))
			wantEQ(t, c.label, "reply.duration", reply.GetDuration(), row.Duration)
			wantEQ(t, c.label, "reply.title", reply.GetTitle(), row.Title)
			wantOps(t, c.label, st.log.opsFrom(calls), []string{"ep.Insert"})
			continue
		}
		wantErrIs(t, c.label, err, c.wantErr)
		wantNoCall(t, c.label, st, calls)
	}
}

func TestCreateEpisodeAssetCheckerUnavailableByDefault(t *testing.T) {
	st := newStore()
	l := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.CreateEpisode(&rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1, AssetId: 77, Operator: "op"})
	wantErrIs(t, "asset 客户端未配置", err, model.ErrAssetCheckerUnavailable)
	wantNoCall(t, "asset 客户端未配置", st, 0)

	// 下游报错按「不可用」处理，既不当成「媒资不存在」，也不当成通过。
	st2 := newStore()
	down := errors.New("deadline exceeded")
	l2 := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgCN, st2, nil, assetDown(down)))
	_, err = l2.CreateEpisode(&rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1, AssetId: 77, Operator: "op"})
	wantErrIs(t, "asset 下游故障", err, model.ErrAssetCheckerUnavailable)
	wantErrIs(t, "asset 下游故障", err, down) // 原始原因必须可追溯
	wantNoCall(t, "asset 下游故障", st2, 0)

	// 下游明确回答不存在 → ErrAssetNotFound（与「不可用」区分，运营提示不同）。
	st3 := newStore()
	l3 := NewCreateEpisodeLogic(context.Background(),
		newTestSvc(cfgCN, st3, nil, assetDown(model.ErrAssetNotFound)))
	_, err = l3.CreateEpisode(&rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1, AssetId: 77, Operator: "op"})
	wantErrIs(t, "媒资不存在", err, model.ErrAssetNotFound)
	wantNoCall(t, "媒资不存在", st3, 0)

	// 只有显式 DisableAssetCheck 才跳过校验（灰度/回滚开关），且此时真的不调下游。
	st4 := newStore()
	skipped := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateFailed}}
	l4 := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgNoAssetChk, st4, nil, skipped))
	if _, err := l4.CreateEpisode(&rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1, AssetId: 77, Operator: "op"}); err != nil {
		t.Fatalf("DisableAssetCheck 时应放行，得到 %v", err)
	}
	wantEQ(t, "DisableAssetCheck", "asset 未被调用", skipped.calls, 0)
	wantEQ(t, "DisableAssetCheck", "集已入库", len(st4.ep.rows), 1)
}

func TestCreateEpisodePropagatesStoreErrorAfterGuard(t *testing.T) {
	st := newStore()
	boom := errors.New("lock wait timeout")
	st.ep.failWith("Insert", boom)
	l := NewCreateEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, assetOK(repository.AssetStateScanned)))

	_, err := l.CreateEpisode(&rpc.CreateEpisodeReq{SeasonId: 10, EpNo: 1, AssetId: 77, Operator: "op"})
	wantErrIs(t, "建集写库失败", err, boom)
	wantEQ(t, "建集写库失败", "媒资校验先跑过再触库", st.log.count("ep.Insert"), 1)
	wantEQ(t, "建集写库失败", "不留半行", len(st.ep.rows), 0)
}

// === PublishEpisode ===

func TestPublishEpisodeGuardsAndNotFound(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, asset))

	_, err := l.PublishEpisode(&rpc.EpisodeReq{})
	wantErrIs(t, "epid=0", err, model.ErrInvalidEpid)
	wantNoCall(t, "epid=0", st, 0)
	wantEQ(t, "epid=0", "rights 未调用", rights.calls, 0)

	_, err = l.PublishEpisode(&rpc.EpisodeReq{Epid: 404, Region: "CN", OperatorMid: 7})
	wantErrIs(t, "集不存在", err, model.ErrEpisodeNotFound)
	wantEQ(t, "集不存在", "不写状态", st.log.countPrefix("ep.UpdateState"), 0)
	wantEQ(t, "集不存在", "不做下游校验", rights.calls, 0)
	wantEQ(t, "集不存在", "asset 未调用", asset.calls, 0)

	// 读库失败必须原样上抛，不能被吞成「不存在」——否则下游抖动会伪装成数据缺失。
	st2 := newStore()
	down := errors.New("bad connection")
	st2.ep.failWith("FindOne", down)
	l2 := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st2, nil, nil))
	_, err = l2.PublishEpisode(&rpc.EpisodeReq{Epid: 5})
	wantErrIs(t, "读集失败", err, down)
}

func TestPublishEpisodeIdempotentOnOnline(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, asset))
	season := seedWork(st, model.StateOnline, model.WorkTypeDrama, "剧")
	ep := seedEpisode(st, season.SeasonID, 1, model.EpStateOnline, 77)
	calls := st.log.snapshot()

	reply, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "CN", OperatorMid: 7})
	if err != nil {
		t.Fatalf("已上架重复调用 = %v, want 幂等成功", err)
	}
	wantEQ(t, "重复上架", "返回状态仍为上架", reply.GetState(), int32(model.EpStateOnline))
	// 幂等边既不再校验也不再写：否则窗口已过时「重复点上架」会把该下架的内容推回去。
	wantOps(t, "重复上架", st.log.opsFrom(calls), opsEpisodeRead(ep.Epid))
	wantEQ(t, "重复上架", "rights 未调用", rights.calls, 0)
	wantEQ(t, "重复上架", "asset 未调用", asset.calls, 0)
}

func TestPublishEpisodeIllegalSourceState(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, assetOK(repository.AssetStateTranscoded)))
	season := seedWork(st, model.StateDraft, model.WorkTypeDrama, "剧")
	ep := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
	// 脏数据态（转换表里没有的 from）：必须先被状态机挡住，不能进入校验与写入。
	st.ep.rows[ep.Epid].State = 9
	calls := st.log.snapshot()

	_, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "CN", OperatorMid: 7})
	wantErrIs(t, "未知来源态", err, model.ErrInvalidStateTrans)
	wantEQ(t, "未知来源态", "不写状态", st.log.countPrefix("ep.UpdateState"), 0)
	wantEQ(t, "未知来源态", "不做下游校验（状态机在校验之前）", rights.calls, 0)
	wantEQ(t, "未知来源态", "库里状态没被改动", st.ep.state(ep.Epid), int32(9))
	wantOps(t, "未知来源态", st.log.opsFrom(calls), opsEpisodeRead(ep.Epid))
}

func TestPublishEpisodeHappyPathWritesAndInvalidatesCache(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true, WindowID: 9}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, asset))
	season := seedWork(st, model.StateDraft, model.WorkTypeAnima, "番")
	ep := seedEpisode(st, season.SeasonID, 3, model.EpStateDraft, 77)
	calls := st.log.snapshot()

	reply, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "TW", OperatorMid: 42})
	if err != nil {
		t.Fatalf("PublishEpisode() = %v", err)
	}
	wantEQ(t, "上架", "返回状态", reply.GetState(), int32(model.EpStateOnline))
	wantEQ(t, "上架", "库存状态", st.ep.state(ep.Epid), int32(model.EpStateOnline))
	wantEQ(t, "上架", "返回投影与库一致", reply.GetEpNo(), st.ep.rows[ep.Epid].EpNo)
	wantEQ(t, "上架", "返回 epid", reply.GetEpid(), ep.Epid)
	wantEQ(t, "上架", "返回 season_id", reply.GetSeasonId(), ep.SeasonID)
	wantEQ(t, "上架", "返回 asset_id", reply.GetAssetId(), ep.AssetID)
	wantEQ(t, "上架", "返回 duration", reply.GetDuration(), ep.Duration)
	wantOps(t, "上架", st.log.opsFrom(calls), append(opsEpisodeRead(ep.Epid),
		fmt.Sprintf("ep.UpdateState:%d->%d", ep.Epid, model.EpStateOnline),
		fmt.Sprintf("cache.Del:cat:e:%d", ep.Epid),
	))
	wantEQ(t, "上架", "媒资校验 1 次", asset.calls, 1)
	wantEQ(t, "上架", "版权校验 1 次", rights.calls, 1)
	wantEQ(t, "上架", "content_id 用 epid", rights.lastContentID, ep.Epid)
	wantEQ(t, "上架", "content_type 用 PGC", rights.lastType, repository.RightsContentTypePGC)
	wantEQ(t, "上架", "region 取请求值", rights.lastRegion, "TW")
}

func TestPublishEpisodeRightsDeniedBlocksWrite(t *testing.T) {
	cases := []struct {
		label   string
		rights  repository.RightsClient
		cfg     config.Config
		wantErr error
		region  string // 期望 rights 收到的 region（空表示不应调到 rights）
	}{
		{"窗口不授予", rightsDenied(), cfgCN, model.ErrRightsWindowClosed, "CN"},
		{"rights 下游故障", rightsDown(errors.New("unavailable")), cfgCN, model.ErrRightsCheckerUnavailable, "CN"},
		{"rights 客户端未配置", nil, cfgCN, model.ErrRightsCheckerUnavailable, ""},
		{"地区无法确定", rightsOK(), config.Config{}, model.ErrMissingRegion, ""},
	}
	for _, c := range cases {
		st := newStore()
		asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
		l := NewPublishEpisodeLogic(context.Background(), newTestSvc(c.cfg, st, c.rights, asset))
		season := seedWork(st, model.StateDraft, model.WorkTypeMovie, "片")
		ep := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
		calls := st.log.snapshot()

		_, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, OperatorMid: 7})
		wantErrIs(t, c.label, err, c.wantErr)
		wantEQ(t, c.label, "不写状态", st.log.countPrefix("ep.UpdateState"), 0)
		wantEQ(t, c.label, "状态保持草稿", st.ep.state(ep.Epid), int32(model.EpStateDraft))
		wantEQ(t, c.label, "不失效缓存", st.log.countPrefix("cache.Del"), 0)
		wantOps(t, c.label, st.log.opsFrom(calls), opsEpisodeRead(ep.Epid))
		wantEQ(t, c.label, "媒资校验已跑（先媒资后版权）", asset.calls, 1)
		if fr, ok := c.rights.(*fakeRights); ok && c.region != "" {
			wantEQ(t, c.label, "region 回落到 DefaultRegion", fr.lastRegion, c.region)
		}
	}
}

func TestPublishEpisodeAssetMustBeTranscoded(t *testing.T) {
	cases := []struct {
		label   string
		asset   repository.AssetClient
		wantErr error
	}{
		{"仅扫描完成不可上架", assetOK(repository.AssetStateScanned), model.ErrAssetNotReady},
		{"媒资失败不可上架", assetOK(repository.AssetStateFailed), model.ErrAssetNotReady},
		{"仅上传完成不可上架", assetOK(repository.AssetStateUploaded), model.ErrAssetNotReady},
		{"媒资不存在不可上架", assetDown(model.ErrAssetNotFound), model.ErrAssetNotFound},
	}
	for _, c := range cases {
		st := newStore()
		rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
		l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, c.asset))
		season := seedWork(st, model.StateDraft, model.WorkTypeMovie, "片")
		ep := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
		calls := st.log.snapshot()

		_, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "CN", OperatorMid: 7})
		wantErrIs(t, c.label, err, c.wantErr)
		wantEQ(t, c.label, "媒资不合格时不去查版权（次序）", rights.calls, 0)
		wantEQ(t, c.label, "不写状态", st.log.countPrefix("ep.UpdateState"), 0)
		wantEQ(t, c.label, "状态保持草稿", st.ep.state(ep.Epid), int32(model.EpStateDraft))
		wantOps(t, c.label, st.log.opsFrom(calls), opsEpisodeRead(ep.Epid))
	}
}

func TestPublishEpisodeReonlineFromOfflineStillChecks(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, asset))
	season := seedWork(st, model.StateOnline, model.WorkTypeDrama, "剧")
	ep := seedEpisode(st, season.SeasonID, 2, model.EpStateOffline, 77)

	reply, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "CN", OperatorMid: 8})
	if err != nil {
		t.Fatalf("下架→重新上架 = %v", err)
	}
	wantEQ(t, "重新上架", "状态", reply.GetState(), int32(model.EpStateOnline))
	wantEQ(t, "重新上架", "必须重新校验媒资", asset.calls, 1)
	wantEQ(t, "重新上架", "必须重新校验窗口", rights.calls, 1)
}

func TestPublishEpisodeRightsCheckDisabledSkipsRightsOnly(t *testing.T) {
	st := newStore()
	rights := &fakeRights{err: errors.New("must not be called")}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgNoRights, st, rights, asset))
	season := seedWork(st, model.StateDraft, model.WorkTypeMovie, "片")
	ep := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)

	// 未传 region 也不报错：DisableRightsCheck 时 precheckPublish 连地区解析一并跳过。
	if _, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid}); err != nil {
		t.Fatalf("DisableRightsCheck 且未传 region 时应放行，得到 %v", err)
	}
	wantEQ(t, "DisableRightsCheck", "不调 rights", rights.calls, 0)
	wantEQ(t, "DisableRightsCheck", "媒资校验仍然生效", asset.calls, 1)
	wantEQ(t, "DisableRightsCheck", "写了一次状态",
		st.log.count(fmt.Sprintf("ep.UpdateState:%d->%d", ep.Epid, model.EpStateOnline)), 1)
	wantEQ(t, "DisableRightsCheck", "库存状态", st.ep.state(ep.Epid), int32(model.EpStateOnline))
}

func TestPublishEpisodePropagatesUpdateErrorWithoutCacheDel(t *testing.T) {
	st := newStore()
	l := NewPublishEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rightsOK(), assetOK(repository.AssetStateTranscoded)))
	season := seedWork(st, model.StateDraft, model.WorkTypeMovie, "片")
	ep := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
	boom := errors.New("row not found")
	st.ep.failWith("UpdateState", boom)
	calls := st.log.snapshot()

	_, err := l.PublishEpisode(&rpc.EpisodeReq{Epid: ep.Epid, Region: "CN", OperatorMid: 7})
	wantErrIs(t, "状态写入失败", err, boom)
	// 写失败就不该失效缓存：否则读侧拿到旧态的原因会被掩盖。
	wantEQ(t, "状态写入失败", "不失效缓存", st.log.countPrefix("cache.Del"), 0)
	wantEQ(t, "状态写入失败", "库存状态仍为草稿", st.ep.state(ep.Epid), int32(model.EpStateDraft))
	wantOps(t, "状态写入失败", st.log.opsFrom(calls), append(opsEpisodeRead(ep.Epid),
		fmt.Sprintf("ep.UpdateState:%d->%d", ep.Epid, model.EpStateOnline),
	))
}

// === OfflineEpisode ===

func TestOfflineEpisodePaths(t *testing.T) {
	st := newStore()
	rights := &fakeRights{result: &repository.RightsCheckResult{Playable: true}}
	asset := &fakeAsset{meta: &repository.AssetMeta{AssetID: 77, State: repository.AssetStateTranscoded}}
	l := NewOfflineEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, rights, asset))

	if _, err := l.OfflineEpisode(&rpc.EpisodeReq{}); !errors.Is(err, model.ErrInvalidEpid) {
		t.Fatalf("epid=0 = %v, want ErrInvalidEpid", err)
	}
	wantNoCall(t, "epid=0", st, 0)

	if _, err := l.OfflineEpisode(&rpc.EpisodeReq{Epid: 404}); !errors.Is(err, model.ErrEpisodeNotFound) {
		t.Fatalf("不存在 = %v, want ErrEpisodeNotFound", err)
	}

	season := seedWork(st, model.StateOnline, model.WorkTypeDrama, "剧")
	draft := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
	calls := st.log.snapshot()
	if _, err := l.OfflineEpisode(&rpc.EpisodeReq{Epid: draft.Epid}); !errors.Is(err, model.ErrInvalidStateTrans) {
		t.Fatalf("草稿直接下架 = %v, want ErrInvalidStateTrans", err)
	}
	wantOps(t, "草稿下架", st.log.opsFrom(calls), opsEpisodeRead(draft.Epid))
	wantEQ(t, "草稿下架", "不写状态", st.log.countPrefix("ep.UpdateState"), 0)

	online := seedEpisode(st, season.SeasonID, 2, model.EpStateOnline, 78)
	calls = st.log.snapshot()
	reply, err := l.OfflineEpisode(&rpc.EpisodeReq{Epid: online.Epid, Region: "CN", OperatorMid: 9})
	if err != nil {
		t.Fatalf("上架→下架 = %v", err)
	}
	wantEQ(t, "下架", "返回状态", reply.GetState(), int32(model.EpStateOffline))
	wantEQ(t, "下架", "库存状态", st.ep.state(online.Epid), int32(model.EpStateOffline))
	wantOps(t, "下架", st.log.opsFrom(calls), append(opsEpisodeRead(online.Epid),
		fmt.Sprintf("ep.UpdateState:%d->%d", online.Epid, model.EpStateOffline),
		fmt.Sprintf("cache.Del:cat:e:%d", online.Epid),
	))

	calls = st.log.snapshot()
	again, err := l.OfflineEpisode(&rpc.EpisodeReq{Epid: online.Epid, Region: "CN", OperatorMid: 9})
	if err != nil {
		t.Fatalf("重复下架 = %v, want 幂等成功", err)
	}
	wantEQ(t, "重复下架", "状态", again.GetState(), int32(model.EpStateOffline))
	wantOps(t, "重复下架", st.log.opsFrom(calls), opsEpisodeRead(online.Epid))

	// 下架不查媒资也不查版权：下架是收紧方向，不该被下游可用性挡住（版权撤权路径）。
	wantEQ(t, "下架链路", "rights 全程未调用", rights.calls, 0)
	wantEQ(t, "下架链路", "asset 全程未调用", asset.calls, 0)
}

func TestOfflineEpisodePropagatesUpdateError(t *testing.T) {
	st := newStore()
	l := NewOfflineEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	season := seedWork(st, model.StateOnline, model.WorkTypeDrama, "剧")
	ep := seedEpisode(st, season.SeasonID, 1, model.EpStateOnline, 77)
	boom := errors.New("connection reset")
	st.ep.failWith("UpdateState", boom)

	_, err := l.OfflineEpisode(&rpc.EpisodeReq{Epid: ep.Epid, OperatorMid: 9})
	wantErrIs(t, "下架写库失败", err, boom)
	wantEQ(t, "下架写库失败", "不失效缓存", st.log.countPrefix("cache.Del"), 0)
	wantEQ(t, "下架写库失败", "库存状态仍为上架", st.ep.state(ep.Epid), int32(model.EpStateOnline))
}
