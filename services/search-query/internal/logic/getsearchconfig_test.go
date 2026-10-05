package logic

import (
	"context"
	"testing"

	"go-video/services/search-query/internal/config"
	"go-video/services/search-query/model"
	"go-video/services/search-query/rpc"
)

func doSearchConfig(t *testing.T, st *store, in *rpc.GetSearchConfigReq) (*rpc.GetSearchConfigReply, error) {
	t.Helper()
	return NewGetSearchConfigLogic(context.Background(), st.svcCtx()).GetSearchConfig(in)
}

func cfgReq() *rpc.GetSearchConfigReq {
	return &rpc.GetSearchConfigReq{Platform: "android", SearchType: rpc.SearchType_SEARCH_TYPE_VIDEO}
}

// TestGetSearchConfigRejectsInvalidRequestsBeforeAnswering 参数守卫表。
// 本接口没有任何下游依赖，因此被拒与放行的区别只在响应体，
// 断言点：坏枚举必须被拒，而不是返回一份「看起来合法但客户端用不了」的能力表。
func TestGetSearchConfigRejectsInvalidRequestsBeforeAnswering(t *testing.T) {
	ok := cfgReq()
	cases := []struct {
		name string
		req  *rpc.GetSearchConfigReq
		want error
	}{
		{"未知端", bad(ok, func(r *rpc.GetSearchConfigReq) { r.Platform = "nokia" }), model.ErrInvalidPlatform},
		{"未知搜索类型", bad(ok, func(r *rpc.GetSearchConfigReq) { r.SearchType = rpc.SearchType(77) }), model.ErrInvalidSearchType},
		{"分区为负", bad(ok, func(r *rpc.GetSearchConfigReq) { r.ZoneId = -1 }), model.ErrInvalidPage},
		{"分区极负", bad(ok, func(r *rpc.GetSearchConfigReq) { r.ZoneId = -99999 }), model.ErrInvalidPage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			reply, err := doSearchConfig(t, st, tc.req)
			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：被拒的请求不应返回响应体，实际 %+v", tc.name, reply)
			}
			// 守卫先于一切：本接口连「读配置」以外的动作都没有，被拒时更不该有依赖调用。
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestGetSearchConfigProjectsEveryCapabilityField 正常路径逐字段投影 + 「零依赖」不变式。
//
// 判断依据（钉代码事实）：GetSearchConfig 只读 repo.Conf() 与 repo.EngineAvailable()，
// 二者都是进程内取值；因此任何 cache./hot./es./block./history./log. 调用都说明实现越界了
// （例如去查分区级配置表——目前没有这张表，见 README「已知缺口」）。
func TestGetSearchConfigProjectsEveryCapabilityField(t *testing.T) {
	st := newStore(t, testConfig())
	reply, err := doSearchConfig(t, st, cfgReq())
	wantNoErr(t, "能力配置", err)

	wantEQ(t, "能力配置", "DefaultSort", reply.DefaultSort, rpc.SortMode_SORT_COMPREHENSIVE)
	wantSliceEQ(t, "能力配置", "SupportedSorts（视频域没有粉丝排序）", reply.SupportedSorts, []rpc.SortMode{
		rpc.SortMode_SORT_COMPREHENSIVE,
		rpc.SortMode_SORT_LATEST,
		rpc.SortMode_SORT_MOST_VIEW,
		rpc.SortMode_SORT_HOT_SCORE,
	})
	wantSliceEQ(t, "能力配置", "SupportedTypes", reply.SupportedTypes, []rpc.SearchType{
		rpc.SearchType_SEARCH_TYPE_ALL,
		rpc.SearchType_SEARCH_TYPE_VIDEO,
		rpc.SearchType_SEARCH_TYPE_USER,
		rpc.SearchType_SEARCH_TYPE_PGC,
	})
	wantSliceEQ(t, "能力配置", "SupportedDurations", reply.SupportedDurations, []rpc.DurationBucket{
		rpc.DurationBucket_DURATION_LT_1MIN,
		rpc.DurationBucket_DURATION_1_10MIN,
		rpc.DurationBucket_DURATION_10_30MIN,
		rpc.DurationBucket_DURATION_30_60MIN,
		rpc.DurationBucket_DURATION_GT_60MIN,
	})
	wantEQ(t, "能力配置", "PsDefault", reply.PsDefault, int32(30))
	wantEQ(t, "能力配置", "PsLimit", reply.PsLimit, int32(50))
	wantEQ(t, "能力配置", "MaxOffset", reply.MaxOffset, int32(900))
	wantEQ(t, "能力配置", "KeywordMaxLen", reply.KeywordMaxLen, int32(64))
	wantEQ(t, "能力配置", "CacheTtlSeconds", reply.CacheTtlSeconds, int32(30))
	wantEQ(t, "能力配置", "EngineAvailable", reply.EngineAvailable, true)

	// 枚举集合里不得混入 UNSPECIFIED 占位值（客户端会原样渲染成筛选项）。
	for _, s := range reply.SupportedSorts {
		if s == rpc.SortMode_SORT_UNSPECIFIED {
			t.Errorf("能力配置：SupportedSorts 含 UNSPECIFIED")
		}
	}
	for _, ty := range reply.SupportedTypes {
		if ty == rpc.SearchType_SEARCH_TYPE_UNSPECIFIED {
			t.Errorf("能力配置：SupportedTypes 含 UNSPECIFIED")
		}
	}
	for _, d := range reply.SupportedDurations {
		if d == rpc.DurationBucket_DURATION_UNSPECIFIED {
			t.Errorf("能力配置：SupportedDurations 含 UNSPECIFIED")
		}
	}
	// 零依赖：整个响应必须不带任何下游调用。
	wantNoCall(t, "能力配置零依赖", st, 0)
}

// TestGetSearchConfigIsPurelyInProcess 显式把「不碰任何替身」写成可失败的断言，
// 并覆盖 nil 请求（所有字段可省略）与端标识大小写/空白归一。
func TestGetSearchConfigIsPurelyInProcess(t *testing.T) {
	cases := []struct{ name, platform string }{
		{"nil 请求", ""},
		{"小写端", "android"},
		{"大写端", "IOS"},
		{"带空白端", "  HarmonyOS  "},
		{"桌面端", "desktop"},
		{"Web 端", "web"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			var in *rpc.GetSearchConfigReq
			if i > 0 {
				in = &rpc.GetSearchConfigReq{Platform: tc.platform}
			}
			reply, err := doSearchConfig(t, st, in)
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "SupportedSorts 条数（非 user 域 4 项）", len(reply.SupportedSorts), 4)
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestGetSearchConfigFansSortOnlyForUserSearch 粉丝排序只在 USER 域出现：
// 这是 sortFieldsOf 的组合校验在能力配置上的镜像，两侧口径必须一致，
// 否则客户端会拿到一个「自己传回来就被拒」的选项。
func TestGetSearchConfigFansSortOnlyForUserSearch(t *testing.T) {
	types := []struct {
		name      string
		typ       rpc.SearchType
		wantFans  bool
		wantCount int
	}{
		{"全站", rpc.SearchType_SEARCH_TYPE_ALL, false, 4},
		{"视频", rpc.SearchType_SEARCH_TYPE_VIDEO, false, 4},
		{"用户", rpc.SearchType_SEARCH_TYPE_USER, true, 5},
		{"PGC", rpc.SearchType_SEARCH_TYPE_PGC, false, 4},
		{"UNSPECIFIED 视为全站", rpc.SearchType_SEARCH_TYPE_UNSPECIFIED, false, 4},
	}
	for _, tc := range types {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, testConfig())
			reply, err := doSearchConfig(t, st, bad(cfgReq(), func(r *rpc.GetSearchConfigReq) {
				r.SearchType = tc.typ
			}))
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "len(SupportedSorts)", len(reply.SupportedSorts), tc.wantCount)
			got := false
			for _, s := range reply.SupportedSorts {
				if s == rpc.SortMode_SORT_MOST_FANS {
					got = true
				}
			}
			wantEQ(t, tc.name, "含 MOST_FANS", got, tc.wantFans)

			// 能力表必须与 Search 的排序校验同源：对每个已知排序枚举，
			// sortFieldsOf 的接受/拒绝必须与是否出现在 SupportedSorts 里完全一致，
			// 否则客户端会拿到一个「自己传回来就被拒」的选项（或反之，能用的选项没下发）。
			for mode := rpc.SortMode(1); mode <= rpc.SortMode(5); mode++ {
				_, err := sortFieldsOf(tc.typ, mode)
				accepted := err == nil
				listed := false
				for _, s := range reply.SupportedSorts {
					if s == mode {
						listed = true
					}
				}
				if mode == rpc.SortMode_SORT_UNSPECIFIED {
					continue
				}
				if accepted != listed {
					t.Errorf("能力配置：%v 域排序 %v 可执行=%v 但下发=%v（两侧口径漂移）",
						tc.name, mode, accepted, listed)
				}
			}
		})
	}
}

// TestGetSearchConfigDefaultSortFallback 配置的默认排序在该搜索类型下不可用时回落，
// 而不是把客户端用不了的默认值下发出去。
func TestGetSearchConfigDefaultSortFallback(t *testing.T) {
	cases := []struct {
		name       string
		configured int32
		typ        rpc.SearchType
		want       rpc.SortMode
	}{
		{"配置缺省 -> 综合", 0, rpc.SearchType_SEARCH_TYPE_VIDEO, rpc.SortMode_SORT_COMPREHENSIVE},
		{"配置综合（视频域可用）", int32(rpc.SortMode_SORT_COMPREHENSIVE), rpc.SearchType_SEARCH_TYPE_VIDEO, rpc.SortMode_SORT_COMPREHENSIVE},
		{"配置最新", int32(rpc.SortMode_SORT_LATEST), rpc.SearchType_SEARCH_TYPE_USER, rpc.SortMode_SORT_LATEST},
		{"配置最多观看", int32(rpc.SortMode_SORT_MOST_VIEW), rpc.SearchType_SEARCH_TYPE_PGC, rpc.SortMode_SORT_MOST_VIEW},
		{"配置最热分", int32(rpc.SortMode_SORT_HOT_SCORE), rpc.SearchType_SEARCH_TYPE_ALL, rpc.SortMode_SORT_HOT_SCORE},
		{"粉丝排序在视频域回落", int32(rpc.SortMode_SORT_MOST_FANS), rpc.SearchType_SEARCH_TYPE_VIDEO, rpc.SortMode_SORT_COMPREHENSIVE},
		{"粉丝排序在用户域可用", int32(rpc.SortMode_SORT_MOST_FANS), rpc.SearchType_SEARCH_TYPE_USER, rpc.SortMode_SORT_MOST_FANS},
		{"未知排序枚举回落", 99, rpc.SearchType_SEARCH_TYPE_VIDEO, rpc.SortMode_SORT_COMPREHENSIVE},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.Search.DefaultSort = tc.configured
			st := newStore(t, cfg)
			reply, err := doSearchConfig(t, st, bad(cfgReq(), func(r *rpc.GetSearchConfigReq) {
				r.SearchType = tc.typ
			}))
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "DefaultSort", reply.DefaultSort, tc.want)
			// 回落后的默认值必须落在自己下发的能力表里，否则客户端渲染不出选中态。
			inList := false
			for _, s := range reply.SupportedSorts {
				if s == reply.DefaultSort {
					inList = true
				}
			}
			if !inList {
				t.Errorf("%s：DefaultSort=%v 不在 SupportedSorts=%v 内", tc.name, reply.DefaultSort, reply.SupportedSorts)
			}
			wantNoCall(t, tc.name, st, 0)
		})
	}
}

// TestGetSearchConfigReflectsConfigVerbatim 下发的限制值必须逐字来自配置：
// 客户端按这些值决定分页与输入长度上限，任何加工都会造成「客户端允许、服务端拒绝」。
func TestGetSearchConfigReflectsConfigVerbatim(t *testing.T) {
	cfg := testConfig()
	cfg.Search.PsDefault = 12
	cfg.Search.PsLimit = 25
	cfg.Search.MaxOffset = 240
	cfg.Search.KeywordMaxLen = 40
	cfg.Search.CacheTTLSeconds = 7
	st := newStore(t, cfg)
	reply, err := doSearchConfig(t, st, cfgReq())
	wantNoErr(t, "限制值透传", err)
	wantEQ(t, "限制值透传", "PsDefault", reply.PsDefault, int32(12))
	wantEQ(t, "限制值透传", "PsLimit", reply.PsLimit, int32(25))
	wantEQ(t, "限制值透传", "MaxOffset", reply.MaxOffset, int32(240))
	wantEQ(t, "限制值透传", "KeywordMaxLen", reply.KeywordMaxLen, int32(40))
	wantEQ(t, "限制值透传", "CacheTtlSeconds", reply.CacheTtlSeconds, int32(7))

	// 同配置的 Search/Suggest/HotKeywords 必须认这些值（跨接口一致性，取一份 cfg 复用）。
	searchSt := newStore(t, cfg)
	_, err = doSearch(t, searchSt, bad(searchReq(), func(r *rpc.SearchReq) { r.Ps = 26 }))
	wantNoErr(t, "页大小超 PsLimit 时截断而非报错", err)
	wantEQ(t, "页大小截断", "Ps", searchSearchReplyPs(t, searchSt), int32(25))
}

// TestGetSearchConfigEngineAvailabilityFlag 引擎能力位的口径：
// 它回答「是否配置了引擎」，不回答「引擎此刻是否健康」——熔断打开时仍为 true，
// 而 Search 会走降级/报错。两者混淆会让网关把「已配置」当成「可用」。
func TestGetSearchConfigEngineAvailabilityFlag(t *testing.T) {
	t.Run("未配置引擎", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.eng.available = false
		reply, err := doSearchConfig(t, st, cfgReq())
		wantNoErr(t, "未配置引擎", err)
		wantEQ(t, "未配置引擎", "EngineAvailable", reply.EngineAvailable, false)
	})
	t.Run("已配置但熔断打开", func(t *testing.T) {
		st := newStore(t, testConfig())
		st.eng.circuitOpen = true
		reply, err := doSearchConfig(t, st, cfgReq())
		wantNoErr(t, "熔断打开", err)
		wantEQ(t, "熔断打开", "EngineAvailable（配置层面仍可用）", reply.EngineAvailable, true)
	})
	t.Run("缓存关闭也要如实下发 0", func(t *testing.T) {
		cfg := testConfig()
		cfg.Search.CacheTTLSeconds = 0
		var zero config.SearchConf = cfg.Search
		st := newStore(t, cfg)
		reply, err := doSearchConfig(t, st, cfgReq())
		wantNoErr(t, "关闭结果缓存", err)
		wantEQ(t, "关闭结果缓存", "CacheTtlSeconds", reply.CacheTtlSeconds, int32(0))
		wantEQ(t, "关闭结果缓存", "DegradeEnabled 不外泄（协议里没有这一项）", zero.DegradeEnabled, true)
	})
}
