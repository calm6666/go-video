package logic

import (
	"context"
	"errors"
	"testing"

	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

func doHotKeywords(t *testing.T, st *store, in *rpc.HotKeywordsReq) (*rpc.HotKeywordsReply, error) {
	t.Helper()
	return NewHotKeywordsLogic(context.Background(), st.svcCtx()).HotKeywords(in)
}

// hotReq 一条所有守卫都能过的热词请求（limit 刻意取 5，与其它默认值区分开）。
func hotReq() *rpc.HotKeywordsReq {
	return &rpc.HotKeywordsReq{Scope: model.ScopeGlobal, Limit: 5, Platform: "android"}
}

// TestHotKeywordsRejectsInvalidRequestsBeforeAnyDependency 参数守卫表：
// 坏输入必须在读热词缓存/查快照表之前被拒（callLog 必须为空）。
func TestHotKeywordsRejectsInvalidRequestsBeforeAnyDependency(t *testing.T) {
	cfg := testConfig() // HotKeywordLimit=20
	ok := hotReq()
	cases := []struct {
		name string
		req  *rpc.HotKeywordsReq
		want error
	}{
		{"未知端", bad(ok, func(r *rpc.HotKeywordsReq) { r.Platform = "nokia" }), model.ErrInvalidPlatform},
		{"未知 scope", bad(ok, func(r *rpc.HotKeywordsReq) { r.Scope = "trending" }), model.ErrInvalidScope},
		{"zone:0", bad(ok, func(r *rpc.HotKeywordsReq) { r.Scope = "zone:0" }), model.ErrInvalidScope},
		{"zone 负数", bad(ok, func(r *rpc.HotKeywordsReq) { r.Scope = "zone:-16" }), model.ErrInvalidScope},
		{"zone 非数字", bad(ok, func(r *rpc.HotKeywordsReq) { r.Scope = "zone:abc" }), model.ErrInvalidScope},
		{"zone 溢出 int32", bad(ok, func(r *rpc.HotKeywordsReq) { r.Scope = "zone:9999999999" }), model.ErrInvalidScope},
		{"limit 为负", bad(ok, func(r *rpc.HotKeywordsReq) { r.Limit = -1 }), model.ErrInvalidPage},
		{"limit 超上限", bad(ok, func(r *rpc.HotKeywordsReq) { r.Limit = cfg.Search.HotKeywordLimit + 1 }), model.ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, cfg)
			st.hot.seed(model.ScopeGlobal, "开源软件", 90, 100)
			reply, err := doHotKeywords(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, st, 0)
		})
	}

	// nil 请求不 panic：与 Search/Suggest 不同，本接口的所有字段都可省略，
	// 缺省即「global + 配置上限条数」（proto 里 scope 注释「空视为 global」）。
	st := newStore(t, cfg)
	st.hot.seed(model.ScopeGlobal, "开源软件", 90, 100)
	reply, err := doHotKeywords(t, st, nil)
	wantNoErr(t, "nil 请求按默认值放行", err)
	wantEQ(t, "nil 请求", "Keywords 条数", len(reply.Keywords), 1)
	wantOps(t, "nil 请求调用序列", st.log.ops, []string{
		"cache.GetHot:global",
		"hot.ListByScope:global/40", // limit=20 -> +20 屏蔽余量
		"cache.SetHot:global/30",
		"cache.GetBlockSet",
		"block.ListActive:0/2000",
		"cache.SetBlockSet:0/60",
	})
}

// TestHotKeywordsProjectsEveryFieldFromSnapshot 正常路径（缓存未命中 -> 读快照表）逐字段投影。
func TestHotKeywordsProjectsEveryFieldFromSnapshot(t *testing.T) {
	st := newStore(t, testConfig())
	// 故意按非分数顺序布行：下发顺序必须等于 SQL 的 score DESC, id ASC。
	st.hot.seed(model.ScopeGlobal, "低分词", 10, 100)
	st.hot.seed(model.ScopeGlobal, "高分词", 90, 200)
	st.hot.seed(model.ScopeGlobal, "中分词", 50, 300)
	st.hot.seed("zone:16", "分区词", 999, 400) // 另一个 scope：不得泄漏进 global

	reply, err := doHotKeywords(t, st, hotReq())
	wantNoErr(t, "热词读快照", err)
	wantEQ(t, "热词读快照", "len(Keywords)", len(reply.Keywords), 3)
	wantEQ(t, "热词读快照", "FromCache（本次是 DB 回源）", reply.FromCache, false)
	wantEQ(t, "热词读快照", "SnapshotAt（本批快照时间）", reply.SnapshotAt, int64(300))
	wantEQ(t, "热词读快照", "Ttl", reply.Ttl, int32(30))
	wantStringsEQ(t, "热词读快照", "顺序=分数倒序", hotWordsOf(reply.Keywords),
		[]string{"高分词", "中分词", "低分词"})
	wantSliceEQ(t, "热词读快照", "Score 逐条投影", hotScoresOf(reply.Keywords),
		[]float64{90, 50, 10})
	wantSliceEQ(t, "热词读快照", "SnapshotAt 逐条投影", hotSnapshotOf(reply.Keywords),
		[]int64{200, 300, 100})
	wantStringsEQ(t, "热词读快照", "Scope 逐条投影", hotScopeOf(reply.Keywords),
		[]string{model.ScopeGlobal, model.ScopeGlobal, model.ScopeGlobal})
	for _, it := range reply.Keywords {
		if it.Keyword == "分区词" {
			t.Errorf("热词读快照：zone:16 的快照泄漏进 global 结果")
		}
	}

	// limit=5 -> 多取 20 条屏蔽余量（blockFilterExtra），缓存回写的 TTL 用 CacheTTLSeconds。
	wantOps(t, "热词调用序列", st.log.ops, []string{
		"cache.GetHot:global",
		"hot.ListByScope:global/25",
		"cache.SetHot:global/30",
		"cache.GetBlockSet",
		"block.ListActive:0/2000",
		"cache.SetBlockSet:0/60",
	})
}

// TestHotKeywordsScopeNormalizationReachesBothLayers " ZONE:16 " 归一化成 "zone:16" 后
// 必须同时用于缓存 key 与 DB 查询条件，否则同一 scope 会在两层各存一份、互相看不到。
func TestHotKeywordsScopeNormalizationReachesBothLayers(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"大写带空白", " ZONE:16 ", "zone:16"},
		{"小写", "zone:16", "zone:16"},
		{"空串视为 global", "", model.ScopeGlobal},
		{"GLOBAL 大写", "GLOBAL", model.ScopeGlobal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			st.hot.seed(tc.want, "分区热词", 70, 500)
			req := hotReq()
			req.Scope = tc.in
			reply, err := doHotKeywords(t, st, req)
			wantNoErr(t, "scope 归一", err)
			wantEQ(t, "scope 归一", "len(Keywords)", len(reply.Keywords), 1)
			wantEQ(t, "scope 归一", "下发 Scope", reply.Keywords[0].Scope, tc.want)
			wantOps(t, "scope 归一调用序列", st.log.ops, []string{
				"cache.GetHot:" + tc.want,
				"hot.ListByScope:" + tc.want + "/25",
				"cache.SetHot:" + tc.want + "/30",
				"cache.GetBlockSet",
				"block.ListActive:0/2000",
				"cache.SetBlockSet:0/60",
			})
		})
	}
}

// TestHotKeywordsCacheHitSkipsSnapshotTable 命中 Redis 时不再碰 DB，
// 但出口屏蔽过滤仍然执行（缓存是旧快照，屏蔽不能因为命中缓存而跳过）。
func TestHotKeywordsCacheHitSkipsSnapshotTable(t *testing.T) {
	st := newStore(t, testConfig())
	// 第 3 行的 snapshot_at 最大但会被 limit 截掉：快照时间是「本批」的属性，
	// repository 用全量行算它（snapshotAtOf），不是用截断后的行 —— 钉住这个口径。
	st.cache.warmHot(model.ScopeGlobal,
		&model.SearchHotKeyword{Id: 1, Scope: model.ScopeGlobal, Keyword: "甲", Score: 90, SnapshotAt: 100},
		&model.SearchHotKeyword{Id: 2, Scope: model.ScopeGlobal, Keyword: "乙", Score: 80, SnapshotAt: 200},
		&model.SearchHotKeyword{Id: 3, Scope: model.ScopeGlobal, Keyword: "丙", Score: 70, SnapshotAt: 999},
	)
	st.cache.warmBlockSet("乙")

	req := hotReq()
	req.Limit = 2
	reply, err := doHotKeywords(t, st, req)
	wantNoErr(t, "热词缓存命中", err)
	wantEQ(t, "热词缓存命中", "FromCache", reply.FromCache, true)
	wantStringsEQ(t, "热词缓存命中", "命中缓存也要出口屏蔽", hotWordsOf(reply.Keywords), []string{"甲"})
	wantEQ(t, "热词缓存命中", "len(Keywords)", len(reply.Keywords), 1)
	wantEQ(t, "热词缓存命中", "SnapshotAt 取本批最大值（含被截断的行）", reply.SnapshotAt, int64(999))
	wantEQ(t, "热词缓存命中", "逐条 SnapshotAt", reply.Keywords[0].SnapshotAt, int64(100))
	wantOps(t, "命中缓存的调用序列", st.log.ops, []string{
		"cache.GetHot:global",
		"cache.GetBlockSet",
	})
	wantNoOpsWith(t, "命中缓存不得回源 DB", st.log, 0, "hot.")
}

// TestHotKeywordsBlockedWordAppliedAfterTruncation 屏蔽过滤发生在「截断到 limit」之后，
// 因此代码注释里「多取一些再屏蔽，保证屏蔽后仍能凑满 limit」只在被屏蔽词落在 limit
// 之外时成立。两个方向都钉住，避免将来有人改了顺序却无人发现。
func TestHotKeywordsBlockedWordAppliedAfterTruncation(t *testing.T) {
	t.Run("被屏蔽词在 limit 内：结果少于 limit", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.hot.seed(model.ScopeGlobal, "甲", 90, 100)
		st.hot.seed(model.ScopeGlobal, "违规词", 80, 100)
		st.hot.seed(model.ScopeGlobal, "乙", 70, 100)
		st.cache.warmBlockSet("违规词")

		req := hotReq()
		req.Limit = 2
		reply, err := doHotKeywords(t, st, req)
		wantNoErr(t, "屏蔽词在窗口内", err)
		wantStringsEQ(t, "屏蔽词在窗口内", "候选词", hotWordsOf(reply.Keywords), []string{"甲"})
		wantEQ(t, "屏蔽词在窗口内", "len(Keywords)（未用余量补齐）", len(reply.Keywords), 1)
	})

	t.Run("被屏蔽词在 limit 外：余量生效", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.hot.seed(model.ScopeGlobal, "甲", 90, 100)
		st.hot.seed(model.ScopeGlobal, "乙", 80, 100)
		st.hot.seed(model.ScopeGlobal, "违规词", 70, 100)
		st.cache.warmBlockSet("违规词")

		req := hotReq()
		req.Limit = 2
		reply, err := doHotKeywords(t, st, req)
		wantNoErr(t, "屏蔽词在窗口外", err)
		wantStringsEQ(t, "屏蔽词在窗口外", "候选词", hotWordsOf(reply.Keywords), []string{"甲", "乙"})
	})

	// 屏蔽词表来自 DB 时：只有 active 词进集合，inactive 词不屏蔽。
	st := newStore(t, testConfig())
	st.hot.seed(model.ScopeGlobal, "甲", 90, 100)
	st.hot.seed(model.ScopeGlobal, "已放开的词", 80, 100)
	st.block.seedWord("已放开的词", model.BlockWordStateInactive)
	st.block.seedWord("只在库里的词", model.BlockWordStateActive)
	req := hotReq()
	req.Limit = 5
	reply, err := doHotKeywords(t, st, req)
	wantNoErr(t, "屏蔽词表回源", err)
	wantStringsEQ(t, "屏蔽词表回源", "inactive 词照常下发", hotWordsOf(reply.Keywords),
		[]string{"甲", "已放开的词"})
	wantOpsFrom(t, "屏蔽词表回源调用序列", st.log, 3, []string{
		"cache.GetBlockSet",
		"block.ListActive:0/2000",
		"cache.SetBlockSet:1/60", // 只回写 active 词
	})
}

// TestHotKeywordsFailureSemantics 缓存/DB/屏蔽词表三侧的失败口径。
//
// 判断依据（钉代码事实）：
//   - repository.HotKeywords 里 GetHot 报错只 logx 后当作 miss（继续读 DB），
//     因为 Redis 是加速层，读不到不等于没有数据；
//   - ListByScope 错误直接 `return nil, err` 上抛（快照表是唯一数据源，挂了不能编造榜单）；
//   - SetHot / 屏蔽词表读取失败都只 logx，不影响已成功的结果；
//   - logic 层对 repo.HotKeywords 的错误一律 `return nil, err`。
func TestHotKeywordsFailureSemantics(t *testing.T) {
	redisBoom := errors.New("hot cache redis down")
	dbBoom := errors.New("hot snapshot db down")
	blockBoom := errors.New("block word db down")

	t.Run("缓存读失败降级为 miss：仍从快照表取到数据", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.cache.failWith("GetHot", redisBoom)
		st.hot.seed(model.ScopeGlobal, "甲", 90, 100)
		reply, err := doHotKeywords(t, st, hotReq())
		wantNoErr(t, "缓存读失败", err)
		wantEQ(t, "缓存读失败", "FromCache（降级读 DB，不是命中缓存）", reply.FromCache, false)
		wantStringsEQ(t, "缓存读失败", "候选词", hotWordsOf(reply.Keywords), []string{"甲"})
		wantOps(t, "缓存读失败的调用序列", st.log.ops, []string{
			"cache.GetHot:global",
			"hot.ListByScope:global/25",
			"cache.SetHot:global/30",
			"cache.GetBlockSet",
			"block.ListActive:0/2000",
			"cache.SetBlockSet:0/60",
		})
	})

	t.Run("快照表失败：硬错误，不返回空榜单冒充成功", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.hot.failWith("ListByScope", dbBoom)
		reply, err := doHotKeywords(t, st, hotReq())
		wantErrIs(t, "快照表失败", err, dbBoom)
		if reply != nil {
			t.Errorf("快照表失败：不应返回响应体，实际 %+v", reply)
		}
		wantNoOpsWith(t, "快照表失败", st.log, 0, "cache.SetHot")
		wantCount(t, "快照表失败不得写缓存", st.log, "cache.SetHot", 0)
	})

	t.Run("缓存回写失败：结果照常返回", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.cache.failWith("SetHot", redisBoom)
		st.hot.seed(model.ScopeGlobal, "甲", 90, 100)
		reply, err := doHotKeywords(t, st, hotReq())
		wantNoErr(t, "回写失败", err)
		wantStringsEQ(t, "回写失败", "候选词", hotWordsOf(reply.Keywords), []string{"甲"})
	})

	t.Run("屏蔽词表不可用：不过滤但明确不放宽主链路", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.cache.failWith("GetBlockSet", redisBoom)
		st.block.failWith("ListActive", blockBoom)
		st.hot.seed(model.ScopeGlobal, "违规词", 90, 100)
		reply, err := doHotKeywords(t, st, hotReq())
		wantNoErr(t, "屏蔽词表挂了", err)
		wantStringsEQ(t, "屏蔽词表挂了", "无词表时不做猜测式屏蔽", hotWordsOf(reply.Keywords), []string{"违规词"})
		wantOps(t, "屏蔽词表挂了的调用序列", st.log.ops, []string{
			"cache.GetHot:global",
			"hot.ListByScope:global/25",
			"cache.SetHot:global/30",
			"cache.GetBlockSet",
			"block.ListActive:0/2000",
		})
		// 词表读取失败时不写回缓存（否则会把「查不到」缓存成「没有屏蔽词」）。
		wantCount(t, "词表失败不得写回屏蔽集缓存", st.log, "cache.SetBlockSet", 0)
	})

	t.Run("空快照：成功但必须可识别", func(t *testing.T) {
		st := newStore(t, testConfig())
		reply, err := doHotKeywords(t, st, hotReq())
		wantNoErr(t, "空快照", err)
		wantEQ(t, "空快照", "len(Keywords)", len(reply.Keywords), 0)
		wantEQ(t, "空快照", "SnapshotAt=0 让客户端展示「暂无热词」", reply.SnapshotAt, int64(0))
		wantEQ(t, "空快照", "Ttl 缩短到 5s", reply.Ttl, int32(5))
		// 没有行就不必读屏蔽词表。
		wantOps(t, "空快照调用序列", st.log.ops, []string{
			"cache.GetHot:global",
			"hot.ListByScope:global/25",
			"cache.SetHot:global/30",
		})
		wantNoOpsWith(t, "空快照", st.log, 0, "cache.GetBlockSet", "block.")
	})
}

// TestHotKeywordsTTLFollowsSnapshotAndConfig 下发给客户端的建议缓存秒数四分支。
func TestHotKeywordsTTLFollowsSnapshotAndConfig(t *testing.T) {
	cases := []struct {
		name     string
		cacheTTL int
		snapshot int64
		want     int32
	}{
		{"有快照 + 开启缓存", 30, 100, 30},
		{"无快照 + 开启缓存", 30, 0, 5},
		{"无快照 + 短缓存", 3, 0, 3},
		{"有快照 + 关闭缓存", 0, 100, 0},
		{"无快照 + 关闭缓存", 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Search.CacheTTLSeconds = tc.cacheTTL
			st := newStore(t, cfg)
			st.hot.seed(model.ScopeGlobal, "甲", 90, tc.snapshot)
			reply, err := doHotKeywords(t, st, hotReq())
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "Ttl", reply.Ttl, tc.want)
			// 回写缓存的 TTL 与下发给客户端的 ttl 是两件事：前者取配置值，
			// 而配置为 0 时真实 Cache 根本不发 Setex（替身复刻了这条短路）。
			if tc.cacheTTL > 0 {
				wantOpsAt(t, tc.name+" 回写缓存", st, 2, "cache.SetHot:global/"+itoa(int64(tc.cacheTTL)))
			} else {
				wantCount(t, tc.name+"（缓存关闭时不发 Setex）", st.log, "cache.SetHot", 0)
			}
		})
	}
}

// --- 本文件专用小工具 ---

func hotWordsOf(items []*rpc.HotKeyword) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Keyword)
	}
	return out
}

func hotScoresOf(items []*rpc.HotKeyword) []float64 {
	out := make([]float64, 0, len(items))
	for _, it := range items {
		out = append(out, it.Score)
	}
	return out
}

func hotSnapshotOf(items []*rpc.HotKeyword) []int64 {
	out := make([]int64, 0, len(items))
	for _, it := range items {
		out = append(out, it.SnapshotAt)
	}
	return out
}

func hotScopeOf(items []*rpc.HotKeyword) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Scope)
	}
	return out
}
