package logic

// read_logic_test.go 覆盖 catalog 的 7 个读方法：GetWork / GetEpisode /
// ListWorks / ListSeasons / ListEpisodes / ListTags / ListZones。
//
// 读侧最容易悄悄坏掉的四件事，逐条钉住：
//  1. 守卫先于触库（非法 ID/超限 ps 不能让请求打到 DB 或 Redis）。
//  2. 归一化后的过滤条件与分页参数**只能靠替身收到的入参**校验：返回值里看不出来。
//  3. 缓存语义：miss 才查库并回填、hit 不再查库、写失败不失效（失效在写侧测）。
//  4. 「查无此行」与「读失败」是两种结论：前者映射业务 NotFound，后者必须原样上抛。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/catalog/model"
	"go-video/services/catalog/rpc"
)

// === GetWork ===

func TestGetWorkGuardAndNotFound(t *testing.T) {
	st := newStore()
	l := NewGetWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.GetWork(&rpc.WorkReq{})
	wantErrIs(t, "season_id=0", err, model.ErrInvalidSeasonID)
	wantNoCall(t, "season_id=0", st, 0)

	_, err = l.GetWork(&rpc.WorkReq{SeasonId: -1})
	wantErrIs(t, "season_id<0", err, model.ErrInvalidSeasonID)
	wantNoCall(t, "season_id<0", st, 0)

	_, err = l.GetWork(&rpc.WorkReq{SeasonId: 404})
	wantErrIs(t, "作品不存在", err, model.ErrWorkNotFound)
	// 查无此行只读库，不回填缓存（回填只在拿到行之后发生）。
	wantOps(t, "作品不存在", st.log.opsFrom(0), []string{
		fmt.Sprintf("cache.Get:"+cacheKeyWork, 404),
		"work.FindOne:404",
	})
}

func TestGetWorkProjectsRow(t *testing.T) {
	st := newStore()
	l := NewGetWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	row := seedWork(st, model.StateOnline, model.WorkTypeDoc, "纪录片")
	calls := st.log.snapshot()

	reply, err := l.GetWork(&rpc.WorkReq{SeasonId: row.SeasonID})
	if err != nil {
		t.Fatalf("GetWork() = %v", err)
	}
	wantOps(t, "读作品", st.log.opsFrom(calls), []string{
		fmt.Sprintf("cache.Get:cat:w:%d", row.SeasonID),
		fmt.Sprintf("work.FindOne:%d", row.SeasonID),
		fmt.Sprintf("cache.Set:cat:w:%d", row.SeasonID),
	})
	wantEQ(t, "读作品", "season_id", reply.GetSeasonId(), row.SeasonID)
	wantEQ(t, "读作品", "title", reply.GetTitle(), row.Title)
	wantEQ(t, "读作品", "cover", reply.GetCover(), row.Cover)
	wantEQ(t, "读作品", "typeid", reply.GetTypeid(), row.TypeID)
	wantEQ(t, "读作品", "intro", reply.GetIntro(), row.Intro)
	wantEQ(t, "读作品", "state", reply.GetState(), row.State)

	// 第二次读必须命中缓存：分区/详情读放大是 catalog 的主要成本（README 缓存策略）。
	calls = st.log.snapshot()
	again, err := l.GetWork(&rpc.WorkReq{SeasonId: row.SeasonID})
	if err != nil {
		t.Fatalf("第二次 GetWork() = %v", err)
	}
	wantOps(t, "读作品命中缓存", st.log.opsFrom(calls), []string{fmt.Sprintf("cache.Get:cat:w:%d", row.SeasonID)})
	wantEQ(t, "读作品命中缓存", "state 来自缓存回填", again.GetState(), row.State)
	wantEQ(t, "读作品命中缓存", "title 与库一致", again.GetTitle(), row.Title)
}

func TestGetWorkReadErrorIsNotSwallowedAsNotFound(t *testing.T) {
	st := newStore()
	down := errors.New("too many connections")
	st.work.failWith("FindOne", down)
	l := NewGetWorkLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.GetWork(&rpc.WorkReq{SeasonId: 7})
	wantErrIs(t, "读库失败", err, down)
	if errors.Is(err, model.ErrWorkNotFound) {
		t.Errorf("读库失败被伪装成「作品不存在」：%v", err)
	}

	// 缓存故障同样原样上抛：不静默降级成「查库」，否则 Redis 挂了会被 DB 流量打穿。
	st2 := newStore()
	boom := errors.New("redis down")
	st2.cache.fail = boom
	l2 := NewGetWorkLogic(context.Background(), newTestSvc(cfgCN, st2, nil, nil))
	_, err = l2.GetWork(&rpc.WorkReq{SeasonId: 7})
	wantErrIs(t, "缓存故障", err, boom)
	wantEQ(t, "缓存故障", "不再查库", st2.log.countPrefix("work.FindOne"), 0)
}

// === GetEpisode ===

func TestGetEpisodeGuardNotFoundAndProjection(t *testing.T) {
	st := newStore()
	l := NewGetEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	calls := st.log.snapshot()
	_, err := l.GetEpisode(&rpc.EpisodeReq{})
	wantErrIs(t, "epid=0", err, model.ErrInvalidEpid)
	wantNoCall(t, "epid=0", st, calls)

	_, err = l.GetEpisode(&rpc.EpisodeReq{Epid: 404})
	wantErrIs(t, "集不存在", err, model.ErrEpisodeNotFound)

	season := seedWork(st, model.StateOnline, model.WorkTypeAnima, "番")
	row := seedEpisode(st, season.SeasonID, 5, model.EpStateOnline, 77)
	calls = st.log.snapshot()
	reply, err := l.GetEpisode(&rpc.EpisodeReq{Epid: row.Epid})
	if err != nil {
		t.Fatalf("GetEpisode() = %v", err)
	}
	wantOps(t, "读集", st.log.opsFrom(calls), []string{
		fmt.Sprintf("cache.Get:cat:e:%d", row.Epid),
		fmt.Sprintf("ep.FindOne:%d", row.Epid),
		fmt.Sprintf("cache.Set:cat:e:%d", row.Epid),
	})
	wantEQ(t, "读集", "epid", reply.GetEpid(), row.Epid)
	wantEQ(t, "读集", "season_id", reply.GetSeasonId(), row.SeasonID)
	wantEQ(t, "读集", "ep_no", reply.GetEpNo(), row.EpNo)
	wantEQ(t, "读集", "title", reply.GetTitle(), row.Title)
	wantEQ(t, "读集", "asset_id", reply.GetAssetId(), row.AssetID)
	wantEQ(t, "读集", "duration", reply.GetDuration(), row.Duration)
	wantEQ(t, "读集", "state", reply.GetState(), row.State)
}

func TestGetEpisodeHitCacheSkipsStore(t *testing.T) {
	st := newStore()
	l := NewGetEpisodeLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	season := seedWork(st, model.StateOnline, model.WorkTypeAnima, "番")
	row := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
	if _, err := l.GetEpisode(&rpc.EpisodeReq{Epid: row.Epid}); err != nil {
		t.Fatalf("首次 GetEpisode() = %v", err)
	}
	calls := st.log.snapshot()

	reply, err := l.GetEpisode(&rpc.EpisodeReq{Epid: row.Epid})
	if err != nil {
		t.Fatalf("二次 GetEpisode() = %v", err)
	}
	wantOps(t, "读集命中缓存", st.log.opsFrom(calls), []string{fmt.Sprintf("cache.Get:cat:e:%d", row.Epid)})
	wantEQ(t, "读集命中缓存", "ep_no", reply.GetEpNo(), row.EpNo)
	wantEQ(t, "读集命中缓存", "缓存命中次数", st.cache.hits(fmt.Sprintf("cat:e:%d", row.Epid)), 2)
}

// === ListWorks ===

func TestListWorksPsLimitRejectedBeforeStore(t *testing.T) {
	st := newStore()
	l := NewListWorksLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	before := st.log.snapshot()

	_, err := l.ListWorks(&rpc.ListReq{Typeid: 1, State: -1, Pn: 1, Ps: 51})
	wantErrIs(t, "ps=51", err, model.ErrInvalidPs)
	wantNoCall(t, "ps=51", st, before)
}

func TestListWorksPassesFiltersThroughUnchanged(t *testing.T) {
	st := newStore()
	seedWork(st, model.StateOnline, model.WorkTypeMovie, "上架片")
	seedWork(st, model.StateDraft, model.WorkTypeDrama, "草稿剧")
	l := NewListWorksLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	cases := []struct {
		label    string
		in       *rpc.ListReq
		wantArgs string
	}{
		{"按类型+状态过滤", &rpc.ListReq{Typeid: model.WorkTypeMovie, State: model.StateOnline, Pn: 2, Ps: 10}, "1/1/2/10"},
		{"只看类型不筛状态", &rpc.ListReq{Typeid: model.WorkTypeDrama, State: -1, Pn: 1, Ps: 50}, "2/-1/1/50"},
		{"只筛状态不筛类型", &rpc.ListReq{Typeid: 0, State: model.StateDraft, Pn: 1, Ps: 20}, "0/0/1/20"},
		// 契约现状（不是缺陷判断，只是钉住行为）：state 不传即 0，被当作「只看草稿」，
		// 与 state=-1 的「不过滤」不同。proto 注释要按这条口径写，否则调用方会拿到空列表。
		{"state 默认 0 就是筛草稿", &rpc.ListReq{Typeid: 0, Pn: 1, Ps: 20}, "0/0/1/20"},
	}
	for _, c := range cases {
		before := st.log.snapshot()
		if _, err := l.ListWorks(c.in); err != nil {
			t.Fatalf("%s：ListWorks() = %v", c.label, err)
		}
		wantOps(t, c.label, st.log.opsFrom(before), []string{"work.List:" + c.wantArgs})
		wantEQ(t, c.label, "model 收到的参数", st.work.calls[len(st.work.calls)-1], c.wantArgs)
	}
}

func TestListWorksProjectsNewestFirstWithTotal(t *testing.T) {
	st := newStore()
	a := seedWork(st, model.StateOnline, model.WorkTypeMovie, "A")
	b := seedWork(st, model.StateOnline, model.WorkTypeMovie, "B")
	c := seedWork(st, model.StateOnline, model.WorkTypeMovie, "C")
	l := NewListWorksLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	reply, err := l.ListWorks(&rpc.ListReq{State: -1, Pn: 1, Ps: 20})
	if err != nil {
		t.Fatalf("ListWorks() = %v", err)
	}
	wantEQ(t, "作品列表", "total", reply.GetTotal(), int32(3))
	if got := len(reply.GetWorks()); got != 3 {
		t.Fatalf("作品行数 = %d, want 3", got)
	}
	// 与 model 的 ORDER BY season_id DESC 同向：新作品在前。
	wantEQ(t, "作品列表", "第1行", reply.GetWorks()[0].GetSeasonId(), c.SeasonID)
	wantEQ(t, "作品列表", "第2行", reply.GetWorks()[1].GetSeasonId(), b.SeasonID)
	wantEQ(t, "作品列表", "第3行", reply.GetWorks()[2].GetSeasonId(), a.SeasonID)
	wantEQ(t, "作品列表", "投影 title", reply.GetWorks()[0].GetTitle(), "C")
	wantEQ(t, "作品列表", "投影 typeid", reply.GetWorks()[0].GetTypeid(), int32(model.WorkTypeMovie))
}

func TestListWorksReadErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("query timeout")
	st.work.failWith("List", down)
	l := NewListWorksLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.ListWorks(&rpc.ListReq{State: -1, Pn: 1, Ps: 20})
	wantErrIs(t, "作品列表读库失败", err, down)
}

// === ListSeasons ===

func TestListSeasonsGuardOrderAndProjection(t *testing.T) {
	st := newStore()
	l := NewListSeasonsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	before := st.log.snapshot()

	if _, err := l.ListSeasons(&rpc.SeasonReq{}); !errors.Is(err, model.ErrInvalidWorkID) {
		t.Fatalf("season_id=0 = %v, want ErrInvalidWorkID", err)
	}
	wantNoCall(t, "season_id=0", st, before)

	work := seedWork(st, model.StateOnline, model.WorkTypeDrama, "剧")
	// 故意乱序写入，验证返回按 season_no 升序（model 的 ORDER BY season_no ASC）。
	s3 := seedSeason(st, work.SeasonID, 3, model.StateDraft)
	s1 := seedSeason(st, work.SeasonID, 1, model.StateOnline)
	other := seedSeason(st, 999, 1, model.StateOnline) // 别的作品的季，不应出现
	calls := st.log.snapshot()                         // 播种的 Insert 不算进本用例的序列断言

	reply, err := l.ListSeasons(&rpc.SeasonReq{SeasonId: work.SeasonID})
	if err != nil {
		t.Fatalf("ListSeasons() = %v", err)
	}
	wantEQ(t, "季列表", "只返回本作品的季", len(reply.GetSeasons()), 2)
	wantEQ(t, "季列表", "第1季", reply.GetSeasons()[0].GetSeasonId(), s1.SeasonID)
	wantEQ(t, "季列表", "第3季", reply.GetSeasons()[1].GetSeasonId(), s3.SeasonID)
	if s1.SeasonID == other.SeasonID {
		t.Fatal("测试数据撞了主键，过滤断言无效")
	}
	wantEQ(t, "季列表", "season_no 投影", reply.GetSeasons()[1].GetSeasonNo(), s3.SeasonNo)
	wantEQ(t, "季列表", "state 投影", reply.GetSeasons()[1].GetState(), s3.State)
	wantEQ(t, "季列表", "title 投影", reply.GetSeasons()[1].GetTitle(), s3.Title)
	wantEQ(t, "季列表", "cover 投影", reply.GetSeasons()[1].GetCover(), s3.Cover)
	wantOps(t, "季列表", st.log.opsFrom(calls), []string{fmt.Sprintf("season.ListByWork:%d", work.SeasonID)})
}

func TestListSeasonsReadErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("deadlock")
	st.season.failWith("ListByWork", down)
	l := NewListSeasonsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.ListSeasons(&rpc.SeasonReq{SeasonId: 10})
	wantErrIs(t, "季列表读库失败", err, down)
}

// === ListEpisodes ===

func TestListEpisodesGuardOrderAndProjection(t *testing.T) {
	st := newStore()
	l := NewListEpisodesLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
	before := st.log.snapshot()

	if _, err := l.ListEpisodes(&rpc.SeasonReq{}); !errors.Is(err, model.ErrInvalidSeasonID) {
		t.Fatalf("season_id=0 = %v, want ErrInvalidSeasonID", err)
	}
	wantNoCall(t, "season_id=0", st, before)

	work := seedWork(st, model.StateOnline, model.WorkTypeAnima, "番")
	season := seedSeason(st, work.SeasonID, 1, model.StateOnline)
	e2 := seedEpisode(st, season.SeasonID, 2, model.EpStateOnline, 78)
	e1 := seedEpisode(st, season.SeasonID, 1, model.EpStateDraft, 77)
	seedEpisode(st, 555, 9, model.EpStateOnline, 79) // 别的季
	calls := st.log.snapshot()                       // 播种的 Insert 不算进本用例的序列断言

	reply, err := l.ListEpisodes(&rpc.SeasonReq{SeasonId: season.SeasonID})
	if err != nil {
		t.Fatalf("ListEpisodes() = %v", err)
	}
	wantEQ(t, "集列表", "行数", len(reply.GetEpisodes()), 2)
	wantEQ(t, "集列表", "按 ep_no 升序第1", reply.GetEpisodes()[0].GetEpid(), e1.Epid)
	wantEQ(t, "集列表", "按 ep_no 升序第2", reply.GetEpisodes()[1].GetEpid(), e2.Epid)
	wantEQ(t, "集列表", "asset_id 投影", reply.GetEpisodes()[0].GetAssetId(), e1.AssetID)
	wantEQ(t, "集列表", "duration 投影", reply.GetEpisodes()[0].GetDuration(), e1.Duration)
	wantEQ(t, "集列表", "state 投影", reply.GetEpisodes()[1].GetState(), e2.State)
	wantOps(t, "集列表", st.log.opsFrom(calls), []string{fmt.Sprintf("ep.ListBySeason:%d", season.SeasonID)})
}

func TestListEpisodesReadErrorPropagates(t *testing.T) {
	st := newStore()
	down := errors.New("broken pipe")
	st.ep.failWith("ListBySeason", down)
	l := NewListEpisodesLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.ListEpisodes(&rpc.SeasonReq{SeasonId: 10})
	wantErrIs(t, "集列表读库失败", err, down)
}

// === ListTags ===

func TestListTagsBranches(t *testing.T) {
	seeded := func(st *store) {
		seedTag(st, 11, "动画")
		seedTag(st, 22, "热血动画")
	}

	t.Run("两者皆空直接空回复不查库", func(t *testing.T) {
		st := newStore()
		seeded(st)
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
		before := st.log.snapshot()

		reply, err := l.ListTags(&rpc.ListTagsReq{})
		if err != nil {
			t.Fatalf("ListTags() = %v", err)
		}
		wantNoCall(t, "空查询", st, before)
		wantEQ(t, "空查询", "行数", len(reply.GetTags()), 0)
	})

	t.Run("name 优先于 tagids", func(t *testing.T) {
		st := newStore()
		seeded(st)
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
		before := st.log.snapshot()

		reply, err := l.ListTags(&rpc.ListTagsReq{Name: "动画", Tagids: []int64{11}, Pn: 1, Ps: 10})
		if err != nil {
			t.Fatalf("ListTags() = %v", err)
		}
		wantOps(t, "name 优先", st.log.opsFrom(before), []string{"tag.SearchByName:动画/1/10"})
		wantEQ(t, "name 优先", "命中行数", len(reply.GetTags()), 2)
		wantEQ(t, "name 优先", "按 tagid 升序第1", reply.GetTags()[0].GetTagid(), int64(11))
		wantEQ(t, "name 优先", "投影 name", reply.GetTags()[1].GetName(), "热血动画")
	})

	t.Run("无 name 时按 tagids 查", func(t *testing.T) {
		st := newStore()
		seeded(st)
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
		before := st.log.snapshot()

		reply, err := l.ListTags(&rpc.ListTagsReq{Tagids: []int64{22, 999}})
		if err != nil {
			t.Fatalf("ListTags() = %v", err)
		}
		wantOps(t, "按 tagids", st.log.opsFrom(before), []string{"tag.FindByIDs:2"})
		// 不存在的 tagid 被跳过而不是补零值行：运营看到的每一行都得是真实标签。
		wantEQ(t, "按 tagids", "行数", len(reply.GetTags()), 1)
		wantEQ(t, "按 tagids", "tagid", reply.GetTags()[0].GetTagid(), int64(22))
	})

	t.Run("tagids 超 100 在查库前拒绝", func(t *testing.T) {
		st := newStore()
		seeded(st)
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
		before := st.log.snapshot()

		ids := make([]int64, 101)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		_, err := l.ListTags(&rpc.ListTagsReq{Tagids: ids})
		wantErrIs(t, "tagids 超限", err, model.ErrTooManyTagIDs)
		wantNoCall(t, "tagids 超限", st, before)
	})

	t.Run("ps 超 50 在查库前拒绝", func(t *testing.T) {
		st := newStore()
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))
		before := st.log.snapshot()

		_, err := l.ListTags(&rpc.ListTagsReq{Name: "动画", Ps: 51})
		wantErrIs(t, "ps 超限", err, model.ErrInvalidPs)
		wantNoCall(t, "ps 超限", st, before)
	})
}

func TestListTagsReadErrorPropagates(t *testing.T) {
	for _, c := range []struct {
		label string
		name  string
		ids   []int64
	}{
		{"按名字查询失败", "动画", nil},
		{"按 ID 查询失败", "", []int64{11}},
	} {
		st := newStore()
		down := errors.New("read timeout")
		st.tag.failWith("SearchByName", down)
		st.tag.failWith("FindByIDs", down)
		l := NewListTagsLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

		_, err := l.ListTags(&rpc.ListTagsReq{Name: c.name, Tagids: c.ids, Pn: 1, Ps: 10})
		wantErrIs(t, c.label, err, down)
	}
}

// === ListZones ===

func TestListZonesCacheThroughAndProjection(t *testing.T) {
	st := newStore()
	st.zone.rows = []*model.Zone{
		{ZoneID: 1, Name: "番剧", Parent: 0},
		{ZoneID: 17, Name: "动画片", Parent: 1},
	}
	l := NewListZonesLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	reply, err := l.ListZones(&rpc.EmptyReq{})
	if err != nil {
		t.Fatalf("ListZones() = %v", err)
	}
	wantOps(t, "分区首次读", st.log.ops, []string{"cache.Get:cat:zones", "zone.ListAll", "cache.Set:cat:zones"})
	wantEQ(t, "分区", "行数", len(reply.GetZones()), 2)
	wantEQ(t, "分区", "zoneid", reply.GetZones()[0].GetZoneid(), int32(1))
	wantEQ(t, "分区", "name", reply.GetZones()[0].GetName(), "番剧")
	wantEQ(t, "分区", "parent 树形关系保留", reply.GetZones()[1].GetParent(), int32(1))
	wantEQ(t, "分区", "parent 顶级为 0", reply.GetZones()[0].GetParent(), int32(0))

	calls := st.log.snapshot()
	again, err := l.ListZones(&rpc.EmptyReq{})
	if err != nil {
		t.Fatalf("第二次 ListZones() = %v", err)
	}
	wantOps(t, "分区命中缓存", st.log.opsFrom(calls), []string{"cache.Get:cat:zones"})
	wantEQ(t, "分区命中缓存", "行数仍来自缓存", len(again.GetZones()), 2)
	wantEQ(t, "分区命中缓存", "内容一致", again.GetZones()[1].GetName(), "动画片")
}

func TestListZonesErrorsPropagate(t *testing.T) {
	st := newStore()
	down := errors.New("syntax error")
	st.zone.failWith("ListAll", down)
	l := NewListZonesLogic(context.Background(), newTestSvc(cfgCN, st, nil, nil))

	_, err := l.ListZones(&rpc.EmptyReq{})
	wantErrIs(t, "分区读库失败", err, down)
	wantEQ(t, "分区读库失败", "失败时不写缓存", st.log.countPrefix("cache.Set"), 0)

	st2 := newStore()
	boom := errors.New("redis down")
	st2.cache.fail = boom
	l2 := NewListZonesLogic(context.Background(), newTestSvc(cfgCN, st2, nil, nil))
	_, err = l2.ListZones(&rpc.EmptyReq{})
	wantErrIs(t, "分区缓存故障", err, boom)
	wantEQ(t, "分区缓存故障", "不降级查库", st2.log.countPrefix("zone.ListAll"), 0)
}
