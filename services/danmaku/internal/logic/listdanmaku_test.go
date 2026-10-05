package logic

import (
	"context"
	"fmt"
	"testing"

	"go-video/services/danmaku/model"
	"go-video/services/danmaku/rpc"
)

// 本文件覆盖 ListDanmaku：时间轴窗口解析 → 段缓存读穿 → 本人待审回显 →
// 用户级屏蔽过滤 → 段计数。可见性口径（state=NORMAL 且 pool=NORMAL 才下发）与
// 「段计数失败只降级、不阻断拉取」都是被测契约。

const (
	listOid    = int64(1001)
	listViewer = int64(7001)
)

func listDanmaku(t *testing.T, e *env, in *rpc.ListDanmakuReq) (*rpc.ListDanmakuReply, error) {
	t.Helper()
	return NewListDanmakuLogic(context.Background(), e.svcCtx).ListDanmaku(in)
}

// --- 期望序列片段 ---

func segGetOp(oid int64, seg int32) string {
	return "cache.GetSeg:" + fmt.Sprintf(keySegList, oid, seg)
}
func segDelOp(oid int64, seg int32) string {
	return "cache.DelSeg:" + fmt.Sprintf(keySegList, oid, seg)
}
func segSetOp(oid int64, seg, n int32) string {
	return "cache.SetSeg:" + fmt.Sprintf(keySegList+"=%d", oid, seg, n)
}
func cntGetOp(oid int64, seg int32) string {
	return "cache.GetCnt:" + fmt.Sprintf(keySegCount, oid, seg)
}
func cntSetOp(oid int64, seg, v int32) string {
	return "cache.SetCnt:" + fmt.Sprintf(keySegCount+"=%d", oid, seg, v)
}
func cntIncrOp(oid int64, seg, delta int32) string {
	return "cache.IncrCnt:" + fmt.Sprintf(keySegCount+"=%+d", oid, seg, delta)
}
func listVisibleOp(oid int64, limit int32, segs ...int32) string {
	return fmt.Sprintf("danmaku.ListVisible:%d/%s/%d", oid, segLabel(segs), limit)
}
func listMineOp(oid, mid int64, segs ...int32) string {
	return fmt.Sprintf("danmaku.ListMine:%d/%d/%s", oid, mid, segLabel(segs))
}
func segListByOp(oid int64, segs ...int32) string {
	return fmt.Sprintf("segment.ListBySegs:%d/%s", oid, segLabel(segs))
}
func ubGetOp(mid int64) string { return "cache.GetUB:" + fmt.Sprintf(keyUserBlock, mid) }
func ubSetOp(mid int64, n int32) string {
	return "cache.SetUB:" + fmt.Sprintf(keyUserBlock+"=%d", mid, n)
}

// readThroughOps 拼出「窗口内每段都 miss、库里无可见弹幕、段表也无行」的完整读穿轨迹：
// 逐段读缓存 → 一次合并回源 → 逐段回填空数组 → 逐段读计数 → 一次回源段表 → 逐段回填空计数。
// 把它作为期望值，等于同时钉死了「miss 段合并成一次查询」和「空段也回填」两条口径。
func readThroughOps(oid int64, segs ...int32) []string {
	limit := int32(defaultConf().MaxListLimit)
	ops := make([]string, 0, 4*len(segs)+2)
	for _, s := range segs {
		ops = append(ops, segGetOp(oid, s))
	}
	ops = append(ops, listVisibleOp(oid, limit, segs...))
	for _, s := range segs {
		ops = append(ops, segSetOp(oid, s, 0))
	}
	for _, s := range segs {
		ops = append(ops, cntGetOp(oid, s))
	}
	ops = append(ops, segListByOp(oid, segs...))
	for _, s := range segs {
		ops = append(ops, cntSetOp(oid, s, 0))
	}
	return ops
}

// warmOne 造一条属于某段的普通池弹幕（投影用例的最小布景）。
func warmOne(oid int64, dmid int64, mid int64, progressMs int64, seg int32, content string) *model.Danmaku {
	return &model.Danmaku{Dmid: dmid, Oid: oid, Mid: mid, ProgressMs: progressMs,
		SegNo: seg, State: model.StateNormal, Pool: model.PoolNormal, Content: content}
}

// TestListDanmakuRejectsInvalidRequests 窗口参数守卫必须发生在任何缓存/DB 读之前。
func TestListDanmakuRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name    string
		in      *rpc.ListDanmakuReq
		wantErr error
		wantMsg string // 空表示不校验文案
	}{
		{"oid 为 0", &rpc.ListDanmakuReq{}, model.ErrInvalidOid, ""},
		{"oid 为负", &rpc.ListDanmakuReq{Oid: -1}, model.ErrInvalidOid, ""},
		{"起始分段大于结束分段", &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 5, EndSeg: 4}, model.ErrInvalidSegRange, ""},
		{"起始分段为负", &rpc.ListDanmakuReq{Oid: listOid, StartSeg: -1, EndSeg: 3}, model.ErrInvalidSegRange, ""},
		{"分段窗口超上限", &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 0, EndSeg: 60}, model.ErrSegRangeTooLarge, "61 > 60"},
		{"时间轴窗口超上限", &rpc.ListDanmakuReq{Oid: listOid, StartProgressMs: 1, EndProgressMs: 60 * 6000},
			model.ErrSegRangeTooLarge, "61 > 60"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			before := e.st.log.snapshot()

			reply, err := listDanmaku(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.wantErr)
			if tc.wantMsg != "" {
				wantErrContains(t, tc.name, err, tc.wantMsg)
			}
			if reply != nil {
				t.Errorf("%s：非法窗口仍返回 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
		})
	}
}

// TestListDanmakuMaxSegWindowFallbackWhenConfigMissing 配置未给窗口上限时回落到 60，
// 60 段放行、61 段拒绝（边界含等号）。
func TestListDanmakuMaxSegWindowFallbackWhenConfigMissing(t *testing.T) {
	cfg := defaultConf()
	cfg.MaxSegWindow = 0
	cases := []struct {
		name    string
		endSeg  int32
		wantErr error
	}{
		{"恰好 60 段放行", 59, nil},
		{"61 段拒绝", 60, model.ErrSegRangeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnvConf(t, cfg)
			reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 0, EndSeg: tc.endSeg})
			if tc.wantErr != nil {
				wantErrIs(t, tc.name, err, tc.wantErr)
				wantErrContains(t, tc.name, err, "61 > 60")
				return
			}
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "next_seg", reply.NextSeg, int32(60))
		})
	}
}

// TestListDanmakuResolvesWindowFromTimeline 分段窗口的两种表达：progress_ms 优先于 seg_no，
// end < start 按 start 处理，未指定窗口只取第 0 段。这里都通过「回源 SQL 拿到的分段列表」断言。
func TestListDanmakuResolvesWindowFromTimeline(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ListDanmakuReq
		// wantSegs 是服务端解析出的分段闭区间，直接体现在回源 SQL 的 seg_no IN (...) 里
		wantSegs []int32
		wantNext int32
	}{
		{
			name: "时间轴 12.0s~17.99s 落在第 2 段",
			in: &rpc.ListDanmakuReq{
				Oid: listOid, StartProgressMs: 12_000, EndProgressMs: 17_999,
				StartSeg: 99, EndSeg: 99, // 故意同时给 seg，验证 progress_ms 优先
			},
			wantSegs: []int32{2}, wantNext: 3,
		},
		{
			name:     "跨段的时间轴窗口展开成连续分段",
			in:       &rpc.ListDanmakuReq{Oid: listOid, StartProgressMs: 5_000, EndProgressMs: 18_000},
			wantSegs: []int32{0, 1, 2, 3},
			wantNext: 4,
		},
		{
			name:     "end 小于 start 按 start 处理",
			in:       &rpc.ListDanmakuReq{Oid: listOid, StartProgressMs: 17_000, EndProgressMs: 12_000},
			wantSegs: []int32{2},
			wantNext: 3,
		},
		{
			name:     "seg_no 窗口直接使用",
			in:       &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 7, EndSeg: 8},
			wantSegs: []int32{7, 8},
			wantNext: 9,
		},
		{
			name:     "未指定窗口只取第 0 段",
			in:       &rpc.ListDanmakuReq{Oid: listOid},
			wantSegs: []int32{0},
			wantNext: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			reply, err := listDanmaku(t, e, tc.in)
			wantNoErr(t, tc.name, err)
			wantOps(t, tc.name, e.ops(), readThroughOps(listOid, tc.wantSegs...))
			// next_seg 是客户端下一次请求的起点，必须落在解析出的窗口右端 +1
			wantEQ(t, tc.name, "next_seg", reply.NextSeg, tc.wantNext)
			wantEQ(t, tc.name, "下发条数", len(reply.Danmaku), 0)
			wantEQ(t, tc.name, "段计数条目数与窗口对齐", len(reply.SegmentCounts), len(tc.wantSegs))
		})
	}
}

// TestListDanmakuReadThroughBackfillsEverySegmentIncludingEmpty 是段缓存读穿口径：
// miss 的分段合并成一次回源，逐段回填，**空段也回填**（防击穿），第二次请求完全不打 DB。
func TestListDanmakuReadThroughBackfillsEverySegmentIncludingEmpty(t *testing.T) {
	e := newEnv(t)
	// 第 5 段两条可见 + 一条待审；第 6 段只有折叠；第 7 段空。
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Aid: 500, Mid: 2002, ProgressMs: 30_100,
		Mode: 1, Fontsize: 25, Color: 0xFFFFFF, Content: "第二段里较晚的一条", State: model.StateNormal, Pool: model.PoolNormal, SegNo: 5})
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Aid: 500, Mid: 2003, ProgressMs: 30_050,
		Mode: 2, Fontsize: 30, Color: 0x00FF00, Content: "第二段里较早的一条", State: model.StateNormal, Pool: model.PoolNormal, SegNo: 5})
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Aid: 500, Mid: 2004, ProgressMs: 30_200,
		Content: "待审不该下发", State: model.StatePending, Pool: model.PoolReview, SegNo: 5})
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Aid: 500, Mid: 2005, ProgressMs: 36_000,
		Content: "折叠不该下发", State: model.StateFolded, Pool: model.PoolBlock, SegNo: 6})
	seedSegment(t, e.st, listOid, 5, 2)

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 5, EndSeg: 7, Limit: 500})
	wantNoErr(t, "首次读穿", err)

	wantOps(t, "首次读穿", e.ops(), []string{
		segGetOp(listOid, 5), segGetOp(listOid, 6), segGetOp(listOid, 7),
		listVisibleOp(listOid, 500, 5, 6, 7),
		segSetOp(listOid, 5, 2), segSetOp(listOid, 6, 0), segSetOp(listOid, 7, 0),
		cntGetOp(listOid, 5), cntGetOp(listOid, 6), cntGetOp(listOid, 7),
		segListByOp(listOid, 5, 6, 7),
		cntSetOp(listOid, 5, 2), cntSetOp(listOid, 6, 0), cntSetOp(listOid, 7, 0),
	})
	// 可见性口径：只有 state=NORMAL 且 pool=NORMAL 进入下发包，且按 progress_ms 升序
	wantStringsEQ(t, "首次读穿", "正文顺序", []string{reply.Danmaku[0].Content, reply.Danmaku[1].Content},
		[]string{"第二段里较早的一条", "第二段里较晚的一条"})
	wantEQ(t, "首次读穿", "条数", len(reply.Danmaku), 2)
	wantEQ(t, "首次读穿", "cache_hits", reply.CacheHits, int32(0))
	// 空段回填后成为「有效命中」，下次不再回源
	rows7, hit7 := e.st.cache.segmentRows(listOid, 7)
	wantEQ(t, "首次读穿", "空段已回填", hit7, true)
	wantEQ(t, "首次读穿", "空段行数", len(rows7), 0)

	e.st.log.ops = nil
	reply, err = listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 5, EndSeg: 7, Limit: 500})
	wantNoErr(t, "第二次全命中", err)
	wantOps(t, "第二次全命中", e.ops(), []string{
		segGetOp(listOid, 5), segGetOp(listOid, 6), segGetOp(listOid, 7),
		cntGetOp(listOid, 5), cntGetOp(listOid, 6), cntGetOp(listOid, 7),
	})
	wantEQ(t, "第二次全命中", "cache_hits", reply.CacheHits, int32(3))
	wantEQ(t, "第二次全命中", "条数不变", len(reply.Danmaku), 2)
}

// TestListDanmakuProjectsEveryRPCField 逐字段投影：DanmakuInfo 的每一列都得与库里的行一致，
// 幂等键与 trace_id 这类内部列不允许下发（不在 DanmakuInfo 里）。
func TestListDanmakuProjectsEveryRPCField(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 4, &model.Danmaku{
		Dmid: 900, Oid: listOid, Aid: 500, Mid: 2002, ProgressMs: 24_500,
		Mode: int32(rpc.DanmakuMode_MODE_COLOR), Fontsize: 40, Color: 0x123456,
		Content: "逐字段投影", State: model.StateNormal, Pool: model.PoolNormal, SegNo: 4,
		IdempotencyKey: "internal-key-must-not-leak", TraceId: "trace-x",
		Ctime: 1_700_000_111, Mtime: 1_700_000_222,
	})
	e.st.cache.warmSegmentCount(listOid, 4, 7)

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 4, EndSeg: 4})
	wantNoErr(t, "投影", err)

	info := reply.Danmaku[0]
	wantEQ(t, "投影", "dmid", info.Dmid, int64(900))
	wantEQ(t, "投影", "oid", info.Oid, listOid)
	wantEQ(t, "投影", "aid", info.Aid, int64(500))
	wantEQ(t, "投影", "mid", info.Mid, int64(2002))
	wantEQ(t, "投影", "progress_ms", info.ProgressMs, int64(24_500))
	wantEQ(t, "投影", "mode", info.Mode, int32(rpc.DanmakuMode_MODE_COLOR))
	wantEQ(t, "投影", "fontsize", info.Fontsize, int32(40))
	wantEQ(t, "投影", "color", info.Color, int32(0x123456))
	wantEQ(t, "投影", "content", info.Content, "逐字段投影")
	wantEQ(t, "投影", "state", info.State, model.StateNormal)
	wantEQ(t, "投影", "pool", info.Pool, model.PoolNormal)
	wantEQ(t, "投影", "seg_no", info.SegNo, int32(4))
	wantEQ(t, "投影", "ctime", info.Ctime, int64(1_700_000_111))
	wantEQ(t, "投影", "mtime", info.Mtime, int64(1_700_000_222))

	wantEQ(t, "投影", "segment_seconds", reply.SegmentSeconds, int32(6))
	wantEQ(t, "投影", "next_seg", reply.NextSeg, int32(5))
	wantEQ(t, "投影", "cache_hits", reply.CacheHits, int32(1))
	wantEQ(t, "投影", "计数条数", len(reply.SegmentCounts), 1)
	wantEQ(t, "投影", "计数分段号", reply.SegmentCounts[0].SegNo, int32(4))
	wantEQ(t, "投影", "计数值", reply.SegmentCounts[0].Count, int32(7))
}

// TestListDanmakuLimitIsClampedToMaxListLimit 客户端传 0 或超大 limit 时都按服务端上限执行，
// 且截断发生在合并排序之后（跨段一起算条数）。
func TestListDanmakuLimitIsClampedToMaxListLimit(t *testing.T) {
	cases := []struct {
		name     string
		limit    int32
		wantRows int
		wantLast int64 // 末条 dmid：截断保留的是 progress_ms 最小的前 n 条
	}{
		{"limit 为 0 用服务端上限", 0, 3, 903},
		{"limit 超上限被截到上限", 999_999, 3, 903},
		{"limit 在上限内按请求", 2, 2, 902},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmSegment(listOid, 0,
				warmOne(listOid, 901, 1, 100, 0, "第一条"),
				warmOne(listOid, 902, 1, 200, 0, "第二条"),
				warmOne(listOid, 903, 1, 300, 0, "第三条"),
			)
			e.st.cache.warmSegmentCount(listOid, 0, 3)

			reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 0, EndSeg: 0, Limit: tc.limit})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "返回条数", len(reply.Danmaku), tc.wantRows)
			wantEQ(t, tc.name, "末条 dmid", reply.Danmaku[len(reply.Danmaku)-1].Dmid, tc.wantLast)
			// 三条都在缓存里，回源一次都不该发生
			wantCount(t, tc.name, e.st.log, "danmaku.ListVisible", 0)
		})
	}
}

// TestListDanmakuLimitIsAppliedToDbQueryWhenSegmentsMiss 请求里的 limit 要原样下推成
// 回源 SQL 的 LIMIT，否则一个段几万条的存量会先把内存打爆再截断。
func TestListDanmakuLimitIsAppliedToDbQueryWhenSegmentsMiss(t *testing.T) {
	e := newEnv(t)
	_, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 4, Limit: 7})
	wantNoErr(t, "回源 limit", err)
	wantOps(t, "回源 limit", e.ops(), []string{
		segGetOp(listOid, 3), segGetOp(listOid, 4),
		listVisibleOp(listOid, 7, 3, 4),
		segSetOp(listOid, 3, 0), segSetOp(listOid, 4, 0),
		cntGetOp(listOid, 3), cntGetOp(listOid, 4),
		segListByOp(listOid, 3, 4),
		cntSetOp(listOid, 3, 0), cntSetOp(listOid, 4, 0),
	})
}

// TestListDanmakuDeliversOnlyNormalPoolRowsAcrossAllFiveStates 五种状态 × 三种池的组合里
// 只有 NORMAL/NORMAL 能被下发（AGENTS.md §8 的读侧体现）。
// 必须走回源路径：段缓存里的下发包本身就该是已过滤的，缓存命中时不会二次过滤。
func TestListDanmakuDeliversOnlyNormalPoolRowsAcrossAllFiveStates(t *testing.T) {
	e := newEnv(t)
	rows := []*model.Danmaku{
		{Content: "正常池", State: model.StateNormal, Pool: model.PoolNormal},
		{Content: "正常态但待审池", State: model.StateNormal, Pool: model.PoolReview},
		{Content: "正常态但屏蔽池", State: model.StateNormal, Pool: model.PoolBlock},
		{Content: "待审态但普通池", State: model.StatePending, Pool: model.PoolNormal},
		{Content: "折叠态", State: model.StateFolded, Pool: model.PoolNormal},
		{Content: "驳回态", State: model.StateRejected, Pool: model.PoolNormal},
		{Content: "删除态", State: model.StateDeleted, Pool: model.PoolNormal},
	}
	for i, r := range rows {
		r.Oid = listOid
		r.Mid = int64(3000 + i)
		r.ProgressMs = int64(18_000 + i*100)
		r.SegNo = 3
		seedDanmaku(t, e.st, r)
	}
	e.st.cache.warmSegmentCount(listOid, 3, 1)

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3})
	wantNoErr(t, "状态过滤", err)
	wantStringsEQ(t, "状态过滤", "下发正文", contentsOf(reply.Danmaku), []string{"正常池"})
	wantInt64sEQ(t, "状态过滤", rpcDmids(reply.Danmaku), []int64{101}) // 库里第一行，即唯一可见的那条

	// 回填的下发包里也只有 1 行：过滤发生在 SQL，不是在应用层丢弃
	wantOps(t, "状态过滤", e.ops(), []string{
		segGetOp(listOid, 3), listVisibleOp(listOid, int32(e.conf.MaxListLimit), 3),
		segSetOp(listOid, 3, 1), cntGetOp(listOid, 3),
	})
	cached, _ := e.st.cache.segmentRows(listOid, 3)
	wantEQ(t, "状态过滤", "回填行数", len(cached), 1)
}

// TestListDanmakuSelfPendingEchoesOwnHiddenRows 本人待审/折叠单独回显，
// 不进段缓存（段缓存是跨用户共享的下发包），且合流后仍按 progress_ms 排序。
func TestListDanmakuSelfPendingEchoesOwnHiddenRows(t *testing.T) {
	e := newEnv(t)
	// 公共下发包里只有他人的一条可见弹幕
	e.st.cache.warmSegment(listOid, 3, &model.Danmaku{Dmid: 900, Oid: listOid, Mid: 2002,
		ProgressMs: 21_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "他人的可见弹幕"})
	e.st.cache.warmSegmentCount(listOid, 3, 1)
	// 库里本人有较后的待审 + 较早的折叠；他人还有一条待审（不该被回显）
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Mid: listViewer, ProgressMs: 21_500, SegNo: 3,
		Content: "我的待审", State: model.StatePending, Pool: model.PoolReview})
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Mid: listViewer, ProgressMs: 18_000, SegNo: 3,
		Content: "我的折叠", State: model.StateFolded, Pool: model.PoolBlock})
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Mid: 2002, ProgressMs: 20_000, SegNo: 3,
		Content: "他人的待审", State: model.StatePending, Pool: model.PoolReview})

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{
		Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer, WithSelfPending: true,
	})
	wantNoErr(t, "本人回显", err)
	wantStringsEQ(t, "本人回显", "按 progress 合流后的正文", contentsOf(reply.Danmaku),
		[]string{"我的折叠", "他人的可见弹幕", "我的待审"})
	wantInt32sEQ(t, "本人回显 合流后的状态列", statesOf(reply.Danmaku),
		[]int32{model.StateFolded, model.StateNormal, model.StatePending})

	// 顺序：段缓存 → 本人回显 → 用户屏蔽视图（miss → 回源 → 回填）→ 段计数
	wantOps(t, "本人回显", e.ops(), []string{
		segGetOp(listOid, 3), listMineOp(listOid, listViewer, 3),
		ubGetOp(listViewer), fmt.Sprintf("userblock.ListEnabled:%d", listViewer), ubSetOp(listViewer, 0),
		cntGetOp(listOid, 3),
	})
	// 回显行不能污染共享下发包
	cached, _ := e.st.cache.segmentRows(listOid, 3)
	wantEQ(t, "本人回显", "段缓存仍只有 1 行", len(cached), 1)
}

// TestListDanmakuSelfPendingNeedsViewer with_self_pending 但没有 viewer 身份时不回显：
// 匿名请求无从判定「本人」。
func TestListDanmakuSelfPendingNeedsViewer(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 3, &model.Danmaku{Dmid: 900, Oid: listOid, Mid: 2002,
		ProgressMs: 21_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal})
	e.st.cache.warmSegmentCount(listOid, 3, 1)
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Mid: 0, ProgressMs: 21_500, SegNo: 3,
		Content: "不该被回显", State: model.StatePending, Pool: model.PoolReview})

	_, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, WithSelfPending: true})
	wantNoErr(t, "匿名回显", err)
	wantCount(t, "匿名回显", e.st.log, "danmaku.ListMine", 0)
	wantCount(t, "匿名回显", e.st.log, "cache.GetUB", 0)
}

// TestListDanmakuAppliesViewerBlocks 用户级屏蔽在读取侧过滤：
// 被屏蔽用户的弹幕消失、命中关键词的弹幕消失、本人弹幕永远保留、已解除的屏蔽不参与过滤、
// 脏数据（dmid=0）丢弃，且**不改动主表状态也不改写共享下发包**。
func TestListDanmakuAppliesViewerBlocks(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 3,
		&model.Danmaku{Dmid: 901, Oid: listOid, Mid: 2002, ProgressMs: 18_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "被屏蔽用户的第一条"},
		&model.Danmaku{Dmid: 902, Oid: listOid, Mid: 2003, ProgressMs: 18_100, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "这句有剧透注意"},
		&model.Danmaku{Dmid: 903, Oid: listOid, Mid: 2004, ProgressMs: 18_200, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "正常弹幕"},
		&model.Danmaku{Dmid: 904, Oid: listOid, Mid: listViewer, ProgressMs: 18_300, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "我自己也有剧透"},
		&model.Danmaku{Dmid: 905, Oid: listOid, Mid: 2005, ProgressMs: 18_400, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "屏蔽已解除所以可见"},
		&model.Danmaku{Dmid: 0, Oid: listOid, Mid: 2006, ProgressMs: 18_500, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "无 dmid 的脏行"},
	)
	e.st.cache.warmSegmentCount(listOid, 3, 6)
	seedUserBlock(t, e.st, &model.UserBlock{Mid: listViewer, Type: model.UserBlockMid, BlockedMid: 2002, State: model.UserBlockOn})
	seedUserBlock(t, e.st, &model.UserBlock{Mid: listViewer, Type: model.UserBlockKeyword, Keyword: "剧透", State: model.UserBlockOn})
	seedUserBlock(t, e.st, &model.UserBlock{Mid: listViewer, Type: model.UserBlockMid, BlockedMid: 2005, State: model.UserBlockOff}) // 已解除，不参与过滤

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer})
	wantNoErr(t, "读取侧屏蔽", err)
	wantStringsEQ(t, "读取侧屏蔽", "过滤后的正文", contentsOf(reply.Danmaku), []string{
		"正常弹幕", "我自己也有剧透", "屏蔽已解除所以可见",
	})
	wantInt64sEQ(t, "读取侧屏蔽 过滤后的 dmid", rpcDmids(reply.Danmaku), []int64{903, 904, 905})
	wantInt32sEQ(t, "读取侧屏蔽 过滤后的状态列", statesOf(reply.Danmaku),
		[]int32{model.StateNormal, model.StateNormal, model.StateNormal})

	// 屏蔽视图 miss → 回源 → 回填，顺序排在段计数之前
	wantOps(t, "读取侧屏蔽", e.ops(), []string{
		segGetOp(listOid, 3),
		ubGetOp(listViewer),
		fmt.Sprintf("userblock.ListEnabled:%d", listViewer),
		ubSetOp(listViewer, 2),
		cntGetOp(listOid, 3),
	})
	wantEQ(t, "读取侧屏蔽", "主表未被写入", e.st.danmaku.countRows(), 0) // 过滤不落库
	// 共享下发包保持原样：解除屏蔽后立刻重新可见，不需要等缓存过期
	cached, _ := e.st.cache.segmentRows(listOid, 3)
	wantEQ(t, "读取侧屏蔽", "段缓存行数不变", len(cached), 6)
}

// TestListDanmakuGuestSkipsUserBlockLookup 游客（viewer_mid<=0）不做过滤，
// 也不该为「查不到人」而读一次屏蔽缓存。
func TestListDanmakuGuestSkipsUserBlockLookup(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 3, &model.Danmaku{Dmid: 901, Oid: listOid, Mid: 2002,
		ProgressMs: 18_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal})
	e.st.cache.warmSegmentCount(listOid, 3, 1)
	seedUserBlock(t, e.st, &model.UserBlock{Mid: listViewer, Type: model.UserBlockMid, BlockedMid: 2002, State: model.UserBlockOn})

	for _, mid := range []int64{0, -1} {
		e.st.log.ops = nil
		reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: mid})
		wantNoErr(t, "游客不过滤", err)
		wantEQ(t, "游客不过滤", "条数", len(reply.Danmaku), 1)
		wantCount(t, "游客不过滤", e.st.log, "cache.GetUB", 0)
		wantCount(t, "游客不过滤", e.st.log, "userblock.", 0)
	}
}

// TestListDanmakuSkipsBlockLookupWhenNoRows 窗口内没有弹幕时不读屏蔽视图，
// 但仍然会取段计数（客户端要靠它画进度条密度）。
func TestListDanmakuSkipsBlockLookupWhenNoRows(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 3) // 空段命中
	e.st.cache.warmSegmentCount(listOid, 3, 0)

	_, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer})
	wantNoErr(t, "空窗口", err)
	wantCount(t, "空窗口", e.st.log, "cache.GetUB", 0)
	wantCount(t, "空窗口", e.st.log, "cache.GetCnt", 1)
}

// TestListDanmakuEmptyBlockViewIsCachedAsHit 没有任何屏蔽项时也要回填空数组，
// 否则每次拉弹幕都会回源 danmaku_user_block。
func TestListDanmakuEmptyBlockViewIsCachedAsHit(t *testing.T) {
	e := newEnv(t)
	e.st.cache.warmSegment(listOid, 3, &model.Danmaku{Dmid: 901, Oid: listOid, Mid: 2002,
		ProgressMs: 18_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal})
	e.st.cache.warmSegmentCount(listOid, 3, 1)

	// 第一次 miss → 回源（库里 0 行）→ 回填空数组；第二次必须把空数组当命中
	_, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer})
	wantNoErr(t, "空屏蔽视图 首次", err)
	wantOps(t, "空屏蔽视图 首次", e.ops(), []string{
		segGetOp(listOid, 3), ubGetOp(listViewer),
		fmt.Sprintf("userblock.ListEnabled:%d", listViewer), ubSetOp(listViewer, 0),
		cntGetOp(listOid, 3),
	})
	e.st.log.ops = nil

	_, err = listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer})
	wantNoErr(t, "空屏蔽视图 二次", err)
	wantOps(t, "空屏蔽视图 二次", e.ops(), []string{
		segGetOp(listOid, 3), ubGetOp(listViewer), cntGetOp(listOid, 3),
	})
}

// TestListDanmakuPropagatesDownstreamFailures 每个依赖各注入一次故障：
// 错误必须原样上抛，不能返回半截下发包（宁可整页报错，也不给客户端一份缺段的列表）。
func TestListDanmakuPropagatesDownstreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		mut     func(e *env)
		arm     func(e *env)
		wantErr error // 本用例实际注入的那个哨兵，逐个写死而不是从注入表里回猜
		wantOps []string
	}{
		{
			name:    "段缓存读失败",
			arm:     func(e *env) { e.st.cache.failWith("GetSegment", errCache) },
			wantErr: errCache,
			wantOps: []string{segGetOp(listOid, 3)},
		},
		{
			name:    "分段回源查询失败",
			arm:     func(e *env) { e.st.danmaku.failWith("ListVisibleBySegs", errDB) },
			wantErr: errDB,
			wantOps: []string{segGetOp(listOid, 3), listVisibleOp(listOid, int32(defaultConf().MaxListLimit), 3)},
		},
		{
			name: "本人回显查询失败",
			mut: func(e *env) {
				e.st.cache.warmSegment(listOid, 3, warmOne(listOid, 901, listViewer, 1, 3, "窗口内的可见弹幕"))
			},
			arm:     func(e *env) { e.st.danmaku.failWith("ListMineBySegs", errDB) },
			wantErr: errDB,
			wantOps: []string{segGetOp(listOid, 3), listMineOp(listOid, listViewer, 3)},
		},
		{
			name: "用户屏蔽视图回源失败",
			mut: func(e *env) {
				e.st.cache.warmSegment(listOid, 3, warmOne(listOid, 901, 2002, 1, 3, "窗口内的可见弹幕"))
			},
			arm:     func(e *env) { e.st.userBlock.failWith("ListEnabled", errDB) },
			wantErr: errDB,
			wantOps: []string{segGetOp(listOid, 3), listMineOp(listOid, listViewer, 3),
				ubGetOp(listViewer), fmt.Sprintf("userblock.ListEnabled:%d", listViewer)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.mut != nil {
				tc.mut(e)
			}
			tc.arm(e)

			reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{
				Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer, WithSelfPending: true,
			})

			wantErrIs(t, tc.name, err, tc.wantErr)
			if reply != nil {
				t.Errorf("%s：故障仍返回 %+v", tc.name, reply)
			}
			wantOps(t, tc.name, e.ops(), tc.wantOps)
		})
	}
}

// TestListDanmakuSegmentCountFailureOnlyDegradesCounts 段计数是辅助信息：
// 缓存或段表读失败时降级为「计数全 0」，弹幕窗口本身必须照常下发（AGENTS.md §5 读侧可用性）。
func TestListDanmakuSegmentCountFailureOnlyDegradesCounts(t *testing.T) {
	cases := []struct {
		name string
		arm  func(e *env)
	}{
		{"计数缓存读失败", func(e *env) { e.st.cache.failWith("GetSegmentCounts", errCache) }},
		{"段表回源失败", func(e *env) { e.st.segment.failWith("ListBySegs", errDB) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.st.cache.warmSegment(listOid, 3, &model.Danmaku{Dmid: 901, Oid: listOid, Mid: 2002,
				ProgressMs: 18_000, SegNo: 3, State: model.StateNormal, Pool: model.PoolNormal, Content: "照常下发"})
			tc.arm(e)

			reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 4})
			wantNoErr(t, tc.name, err)
			wantEQ(t, tc.name, "弹幕照常下发", len(reply.Danmaku), 1)
			wantEQ(t, tc.name, "计数条目数仍与请求分段对齐", len(reply.SegmentCounts), 2)
			wantEQ(t, tc.name, "第一条计数降级为 0", reply.SegmentCounts[0].Count, int32(0))
			wantEQ(t, tc.name, "第二条计数降级为 0", reply.SegmentCounts[1].Count, int32(0))
			wantEQ(t, tc.name, "分段号顺序不变", reply.SegmentCounts[1].SegNo, int32(4))
		})
	}
}

// TestListDanmakuCacheWriteFailuresDoNotBreakDelivery 回填失败（段列表 / 屏蔽视图 / 段计数）
// 只记日志：这一轮数据仍要返回，且不能因为写失败就吞掉弹幕。
func TestListDanmakuCacheWriteFailuresDoNotBreakDelivery(t *testing.T) {
	e := newEnv(t)
	seedDanmaku(t, e.st, &model.Danmaku{Oid: listOid, Mid: 2002, ProgressMs: 18_000, SegNo: 3,
		Content: "回源可见弹幕", State: model.StateNormal, Pool: model.PoolNormal})
	e.st.cache.failWith("SetSegment", errCache)
	e.st.cache.failWith("SetUserBlocks", errCache)
	e.st.cache.failWith("SetSegmentCount", errCache)
	seedUserBlock(t, e.st, &model.UserBlock{Mid: listViewer, Type: model.UserBlockKeyword, Keyword: "不该命中的词", State: model.UserBlockOn})

	reply, err := listDanmaku(t, e, &rpc.ListDanmakuReq{Oid: listOid, StartSeg: 3, EndSeg: 3, ViewerMid: listViewer})
	wantNoErr(t, "回填失败", err)
	wantStringsEQ(t, "回填失败", "正文", contentsOf(reply.Danmaku), []string{"回源可见弹幕"})
	wantEQ(t, "回填失败", "段缓存未被污染", func() bool { _, ok := e.st.cache.segmentRows(listOid, 3); return ok }(), false)

	// 写失败仍记了尝试轨迹，且顺序不变
	wantOps(t, "回填失败", e.ops(), []string{
		segGetOp(listOid, 3), listVisibleOp(listOid, 3000, 3), segSetOp(listOid, 3, 1),
		ubGetOp(listViewer), fmt.Sprintf("userblock.ListEnabled:%d", listViewer), ubSetOp(listViewer, 1),
		cntGetOp(listOid, 3), segListByOp(listOid, 3), cntSetOp(listOid, 3, 0),
	})
}

// --- 小投影辅助 ---

func contentsOf(rows []*rpc.DanmakuInfo) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetContent())
	}
	return out
}

func statesOf(rows []*rpc.DanmakuInfo) []int32 {
	out := make([]int32, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetState())
	}
	return out
}
