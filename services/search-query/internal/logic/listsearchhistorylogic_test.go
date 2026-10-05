package logic

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go-video/services/search-query/internal/repository"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

// listReq 给一条「所有守卫都能过」的基准历史列表请求（各用例只改自己要的那一项）。
func listReq() *rpc.ListSearchHistoryReq {
	return &rpc.ListSearchHistoryReq{Mid: 88, Limit: 20, Platform: "android", TraceId: "trace-list-1"}
}

func doListHistory(t *testing.T, st *store, in *rpc.ListSearchHistoryReq) (*rpc.ListSearchHistoryReply, error) {
	t.Helper()
	return NewListSearchHistoryLogic(context.Background(), st.svcCtx()).ListSearchHistory(in)
}

// TestListSearchHistoryRejectsInvalidRequestsBeforeAnyDependency 守卫表：
// 隐私列表接口在打缓存/打库/打引擎之前就必须拒掉非法请求——
// 尤其 cursor 解析发生在 model 之前，坏游标不允许变成一次对历史表的实际查询。
func TestListSearchHistoryRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	ok := listReq()

	cases := []struct {
		name  string
		req   *rpc.ListSearchHistoryReq
		want  error
		cause string
	}{
		{"空请求", nil, model.ErrInvalidMid, "in==nil 与非法 mid 同码"},
		{"游客 mid=0", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Mid = 0 }), model.ErrInvalidMid,
			"历史是用户级隐私数据，游客没有历史"},
		{"负 mid", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Mid = -88 }), model.ErrInvalidMid,
			"负 mid 不允许当作查询条件（本接口也没有「查他人历史」的 admin 分支）"},
		{"未知端", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Platform = "windows-phone" }), model.ErrInvalidPlatform,
			"端白名单（AGENTS.md §6）"},
		{"负 limit", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Limit = -1 }), model.ErrInvalidPage,
			"limit 与 ps 不同：非法值不回落默认"},
		{"limit 超单页上限", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Limit = 31 }), model.ErrInvalidPage,
			"批量上限超出即拒绝，而不是静默截断（与 Search 的 ps 截断刻意相反）"},
		{"limit 远超上限", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Limit = 100_000 }), model.ErrInvalidPage,
			"一次拉空整张隐私表必须被拒"},
		{"游标不是合法编码", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Cursor = "not-a-cursor!!" }),
			model.ErrInvalidCursor, "base64url/JSON 解不开"},
		{"游标版本不兼容", bad(ok, func(r *rpc.ListSearchHistoryReq) { r.Cursor = rawCursor(t, 9) }),
			repository.ErrUnsupportedCursorVersion, "滚动发布期不允许按别的结构解释游标"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Search.HistoryLimit = 30
			st := newStore(t, cfg)
			st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(300), nowMinus(60))

			reply, err := doListHistory(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestListSearchHistoryProjectsEveryFieldInMtimeOrder 正常路径逐字段投影 + 读取口径：
//   - 排序：mtime DESC, id DESC（最近搜过的词在前）；
//   - 只下发 state=HistoryStateNormal 的行，运营标记/待清理行不外泄；
//   - 只下发本人的行；
//   - 请求端不参与过滤：跨端历史合并成一份（换机后仍能看到旧端记录）。
func TestListSearchHistoryProjectsEveryFieldInMtimeOrder(t *testing.T) {
	st := newStore(t, testConfig())
	oldest, tombstoned, flagged, midOldest, foreign, newest :=
		nowMinus(9000), nowMinus(3000), nowMinus(2500), nowMinus(2000), nowMinus(1500), nowMinus(100)
	st.history.seedRow(88, "最早的一行", model.HistoryStateNormal, "android", oldest, oldest)
	st.history.seedRow(88, "深夜电台", model.HistoryStateTombstone, "android", tombstoned, tombstoned)
	st.history.seedRow(88, "被标记的行", model.HistoryStateFlagged, "ios", flagged, flagged)
	st.history.seedRow(88, "番剧 推荐", model.HistoryStateNormal, "harmony", midOldest, midOldest)
	st.history.seedRow(99, "别人的词", model.HistoryStateNormal, "desktop", foreign, foreign)
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "ios", newest, newest)

	req := bad(listReq(), func(r *rpc.ListSearchHistoryReq) { r.Limit = 10 })
	reply, err := doListHistory(t, st, req)
	wantNoErr(t, "历史列表", err)

	wantEQ(t, "历史列表", "len(Items)（他人行与非正常状态行被过滤）", len(reply.Items), 3)
	wantEQ(t, "历史列表", "HasMore（3 行 < limit 10）", reply.HasMore, false)
	// 无更多数据时必须清空游标：仓库仍算出了末页游标，客户端拿到的必须是空串。
	wantEQ(t, "历史列表", "NextCursor（末页不给游标）", reply.NextCursor, "")
	wantSliceEQ(t, "历史列表", "关键词顺序（mtime 倒序）", historyWordsOf(reply.Items),
		[]string{"开源软件", "番剧 推荐", "最早的一行"})

	it0 := reply.Items[0]
	wantEQ(t, "首条", "Keyword", it0.Keyword, "开源软件")
	wantEQ(t, "首条", "Ctime", it0.Ctime, newest)
	wantEQ(t, "首条", "Mtime", it0.Mtime, newest)
	wantEQ(t, "首条", "State", it0.State, int32(model.HistoryStateNormal))
	wantEQ(t, "首条", "Platform（原样下发，不按请求端改写）", it0.Platform, "ios")
	it1 := reply.Items[1]
	wantEQ(t, "次条", "Keyword", it1.Keyword, "番剧 推荐")
	wantEQ(t, "次条", "Ctime", it1.Ctime, midOldest)
	wantEQ(t, "次条", "Mtime", it1.Mtime, midOldest)
	wantEQ(t, "次条", "State", it1.State, int32(model.HistoryStateNormal))
	wantEQ(t, "次条", "Platform", it1.Platform, "harmony")
	it2 := reply.Items[2]
	wantEQ(t, "末条", "Keyword", it2.Keyword, "最早的一行")
	wantEQ(t, "末条", "Ctime", it2.Ctime, oldest)
	wantEQ(t, "末条", "Mtime", it2.Mtime, oldest)
	wantEQ(t, "末条", "Platform", it2.Platform, "android")
	if strings.Contains(jsonDump(t, reply), "别人的词") || strings.Contains(jsonDump(t, reply), "深夜电台") ||
		strings.Contains(jsonDump(t, reply), "被标记的行") {
		t.Errorf("历史列表：他人/非正常状态的词不得出现在响应里：%s", jsonDump(t, reply))
	}

	// 顺序断言：首页只发一次 keyset 查询，limit+1 用于判定 hasMore；不带游标即首页。
	wantOps(t, "历史列表调用序列", st.log.ops, []string{"history.ListByKeyset:88/0/0/11"})
	// 隐私数据不经过任何缓存与引擎：整条链路一次 Redis/ES/屏蔽词判定都不碰。
	wantNoOpsWith(t, "历史列表", st.log, 0, "cache.", "es.", "block.")
}

// TestListSearchHistoryKeysetPaginationHasNoOverlapOrGaps 分页口径：每页恰好 limit 行、
// 游标锚点等于本页最后一行的 (mtime, id)，下一页严格从它之后开始——既不能重复也不能漏行；
// 末页必须清空游标，避免客户端把「末页游标」再翻一次。
func TestListSearchHistoryKeysetPaginationHasNoOverlapOrGaps(t *testing.T) {
	st := newStore(t, testConfig())
	fifth, fourth, foreign, third, second, newest :=
		nowMinus(5000), nowMinus(4000), nowMinus(3500), nowMinus(3000), nowMinus(2000), nowMinus(1000)
	st.history.seedRow(88, "第五旧", model.HistoryStateNormal, "android", fifth, fifth)
	st.history.seedRow(88, "第四旧", model.HistoryStateNormal, "android", fourth, fourth)
	st.history.seedRow(99, "别人的词", model.HistoryStateNormal, "ios", foreign, foreign)
	st.history.seedRow(88, "被清理", model.HistoryStateTombstone, "ios", third, third)
	st.history.seedRow(88, "第三旧", model.HistoryStateNormal, "harmony", third, third)
	st.history.seedRow(88, "第二旧", model.HistoryStateNormal, "web", second, second)
	st.history.seedRow(88, "最新", model.HistoryStateNormal, "desktop", newest, newest)

	// 期望分页：每页 2 行，第三页 1 行收尾（干扰行始终不出现）。
	pages := [][]string{{"最新", "第二旧"}, {"第三旧", "第四旧"}, {"第五旧"}}
	req := bad(listReq(), func(r *rpc.ListSearchHistoryReq) { r.Limit = 2 })
	var got []string
	// 每次查库的锚点条件必须由上一页最后一行给出：首页 0/0，之后就是上一页末行的 (mtime, id)。
	var anchorMtime, anchorID int64
	for i, want := range pages {
		before := st.log.snapshot()
		reply, err := doListHistory(t, st, req)
		label := "分页第 " + itoa(int64(i+1)) + " 页"
		wantNoErr(t, label, err)
		wantSliceEQ(t, label, "本页关键词", historyWordsOf(reply.Items), want)
		// 多取一条（limit+1=3）用于判定 hasMore，锚点等于上一页末行 => 不重不漏。
		wantOpsFrom(t, label+"：调用序列", st.log, before, []string{
			"history.ListByKeyset:88/" + itoa(anchorMtime) + "/" + itoa(anchorID) + "/3",
		})
		got = append(got, historyWordsOf(reply.Items)...)

		isLast := i == len(pages)-1
		wantEQ(t, label, "HasMore", reply.HasMore, !isLast)
		if isLast {
			// 仓库对末页仍会算出游标，logic 必须清掉它。
			wantEQ(t, label, "NextCursor（末页必须清空）", reply.NextCursor, "")
			break
		}
		last := reply.Items[len(reply.Items)-1]
		mtime, id, derr := repository.DecodeKeysetCursor(reply.NextCursor)
		wantNoErr(t, label+"：游标可解码", derr)
		wantEQ(t, label, "游标 mtime 段=本页末行 mtime", mtime, last.Mtime)
		wantEQ(t, label, "游标 id 段=本页末行 id", id, rowID(t, st, 88, last.Keyword))
		anchorMtime, anchorID = mtime, id
		req.Cursor = reply.NextCursor
	}
	wantStringsEQ(t, "翻页聚合结果", "关键词序列（顺序、条数、不重不漏）", got,
		[]string{"最新", "第二旧", "第三旧", "第四旧", "第五旧"})
	wantCount(t, "整轮翻页查库次数", st.log, "history.ListByKeyset", 3)
	wantEQ(t, "整轮翻页不写任何数据", "ops 总数", len(st.log.ops), 3)
}

// TestListSearchHistoryTieBreaksOnIdWhenMtimeCollides 同一秒搜的多个词（mtime 相同）
// 必须靠 id 二级排序继续翻页：只按 mtime 翻页会把同秒的其余行整批漏掉。
func TestListSearchHistoryTieBreaksOnIdWhenMtimeCollides(t *testing.T) {
	st := newStore(t, testConfig())
	same, older := nowMinus(1500), nowMinus(9000)
	st.history.seedRow(88, "同秒A", model.HistoryStateNormal, "android", same, same)
	st.history.seedRow(88, "同秒B", model.HistoryStateNormal, "android", same, same)
	st.history.seedRow(88, "同秒C", model.HistoryStateNormal, "android", same, same)
	st.history.seedRow(88, "更早", model.HistoryStateNormal, "ios", older, older)

	req := bad(listReq(), func(r *rpc.ListSearchHistoryReq) { r.Limit = 2 })
	p1, err := doListHistory(t, st, req)
	wantNoErr(t, "同秒第一页", err)
	// id 倒序：最后布的两行（同秒C id=3、同秒B id=2）在前。
	wantSliceEQ(t, "同秒第一页", "顺序按 id DESC", historyWordsOf(p1.Items), []string{"同秒C", "同秒B"})
	wantEQ(t, "同秒第一页", "HasMore", p1.HasMore, true)

	req.Cursor = p1.NextCursor
	p2, err := doListHistory(t, st, req)
	wantNoErr(t, "同秒第二页", err)
	// 与游标同秒但 id 更小的行不能被跳过（只比较 mtime 就会漏掉同秒A）。
	wantSliceEQ(t, "同秒第二页", "同秒剩余行 + 更早行", historyWordsOf(p2.Items), []string{"同秒A", "更早"})
	wantEQ(t, "同秒第二页", "HasMore", p2.HasMore, false)
	wantEQ(t, "同秒第二页", "NextCursor", p2.NextCursor, "")
	wantOps(t, "同秒翻页调用序列", st.log.ops, []string{
		"history.ListByKeyset:88/0/0/3",
		"history.ListByKeyset:88/" + itoa(same) + "/" + itoa(rowID(t, st, 88, "同秒B")) + "/3",
	})
}

// TestListSearchHistoryDisabledOrEmptyReturnsIdentifiableEmptyPage 两种「空」的口径：
//   - 部署关闭历史能力：返回空页且**一次查询都不发**（不碰库、不碰缓存），也不报错；
//   - 能力开着但真没历史：查库一次，返回空页。
//     两者对客户端同形（无需区分），但服务端花销必须不同。
func TestListSearchHistoryDisabledOrEmptyReturnsIdentifiableEmptyPage(t *testing.T) {
	cfg := testConfig()
	cfg.Search.HistoryEnabled = false
	st := newStore(t, cfg)
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(300), nowMinus(60))

	reply, err := doListHistory(t, st, listReq())
	wantNoErr(t, "关闭历史能力", err)
	wantEQ(t, "关闭历史能力", "len(Items)", len(reply.Items), 0)
	wantEQ(t, "关闭历史能力", "HasMore", reply.HasMore, false)
	wantEQ(t, "关闭历史能力", "NextCursor", reply.NextCursor, "")
	wantNoCall(t, "关闭历史能力不得触库/触缓存/触引擎", st, 0)

	// 参数守卫仍先于开关：limit 越界在关闭状态下也照样报错（不能靠开关掩盖调用方错误）。
	_, err = doListHistory(t, st, bad(listReq(), func(r *rpc.ListSearchHistoryReq) { r.Limit = 99 }))
	wantErrIs(t, "关闭历史能力时参数仍受校验", err, model.ErrInvalidPage)
	wantNoCall(t, "关闭历史能力时参数仍受校验", st, 0)

	// 能力开着但真没历史：一次查库，空页。
	st2 := newStore(t, testConfig())
	reply2, err := doListHistory(t, st2, listReq())
	wantNoErr(t, "无历史", err)
	wantEQ(t, "无历史", "len(Items)", len(reply2.Items), 0)
	wantEQ(t, "无历史", "HasMore", reply2.HasMore, false)
	wantEQ(t, "无历史", "NextCursor", reply2.NextCursor, "")
	wantOps(t, "无历史调用序列", st2.log.ops, []string{"history.ListByKeyset:88/0/0/21"})
}

// TestListSearchHistoryPropagatesReadFailure 读库失败如实传出：
// 不返回空页冒充「没有历史」（那会让客户端把故障当作用户清空了历史并停止重试）。
func TestListSearchHistoryPropagatesReadFailure(t *testing.T) {
	st := newStore(t, testConfig())
	boom := errors.New("search_history ListByKeyset: invalid connection")
	st.history.failWith("ListByKeyset", boom)
	st.history.seedRow(88, "开源软件", model.HistoryStateNormal, "android", nowMinus(300), nowMinus(60))

	reply, err := doListHistory(t, st, listReq())
	wantErrIs(t, "读历史失败", err, boom)
	if reply != nil {
		t.Errorf("读历史失败：不应返回空页冒充成功，实际 %+v", reply)
	}
	wantOps(t, "读历史失败调用序列", st.log.ops, []string{"history.ListByKeyset:88/0/0/21"})
}

// --- 本文件专用小工具 ---

func historyWordsOf(items []*rpc.SearchHistoryItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Keyword)
	}
	return out
}

// rowID 取库里那一行的主键：响应体不下发 id（客户端不需要），
// 因此游标锚点的 id 只能回到库里取，避免用例自己编号。
func rowID(t *testing.T, st *store, mid int64, keyword string) int64 {
	t.Helper()
	row, ok := st.history.get(mid, keyword)
	if !ok {
		t.Fatalf("库里没有 mid=%d keyword=%s 的行，取不到主键", mid, keyword)
	}
	return row.Id
}
