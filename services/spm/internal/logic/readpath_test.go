package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/spm/model"
	"go-video/services/spm/rpc"
)

// 本文件钉住 README「窗口、水位与迟到」「数据保留策略」「其它实现约定」「契约缺口」对
// 读路径的全部承诺。写路径与幂等见 writemetricwindow_test.go，口径状态机见
// metricdefinition_test.go，作业见 job_test.go。
//
// 读路径最坏的失败模式不是报错，而是「把查不到表现成 0 值真数据」和「把筛错了表现成
// 全命中」。因此这里的断言普遍分两半：响应本身，以及假件的调用计数——计数才是
// 「该拒的根本没去查库」「该省的第二条 SQL 真的没发出去」的唯一证据。

const (
	readSubj int32 = model.SubjectTypeAid // 读路径夹具统一用 AID 维度
	readAID  int64 = 101
)

func callGetMetric(t *testing.T, d *deps, in *rpc.GetMetricReq) (*rpc.GetMetricReply, error) {
	t.Helper()
	return NewGetMetricLogic(context.Background(), d.ctx).GetMetric(in)
}

func callBatchGet(t *testing.T, d *deps,
	in *rpc.BatchGetMetricsReq) (*rpc.BatchGetMetricsReply, error) {
	t.Helper()
	return NewBatchGetMetricsLogic(context.Background(), d.ctx).BatchGetMetrics(in)
}

func callHot(t *testing.T, d *deps,
	in *rpc.ListHotSubjectsReq) (*rpc.ListHotSubjectsReply, error) {
	t.Helper()
	return NewListHotSubjectsLogic(context.Background(), d.ctx).ListHotSubjects(in)
}

func callConsumer(t *testing.T, d *deps,
	in *rpc.ListConsumerStateReq) (*rpc.ListConsumerStateReply, error) {
	t.Helper()
	return NewListConsumerStateLogic(context.Background(), d.ctx).ListConsumerState(in)
}

func callDeadLetters(t *testing.T, d *deps,
	in *rpc.ListDeadLettersReq) (*rpc.ListDeadLettersReply, error) {
	t.Helper()
	return NewListDeadLettersLogic(context.Background(), d.ctx).ListDeadLetters(in)
}

func callInterest(t *testing.T, d *deps,
	in *rpc.GetUserInterestReq) (*rpc.GetUserInterestReply, error) {
	t.Helper()
	return NewGetUserInterestLogic(context.Background(), d.ctx).GetUserInterest(in)
}

func callRetention(t *testing.T, d *deps,
	in *rpc.GetRetentionReq) (*rpc.GetRetentionReply, error) {
	t.Helper()
	return NewGetRetentionLogic(context.Background(), d.ctx).GetRetention(in)
}

func getMetricReq(metricKey string, version int32, wt rpc.WindowType) *rpc.GetMetricReq {
	return &rpc.GetMetricReq{
		SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: readAID,
		MetricKey: metricKey, MetricVersion: version, WindowType: wt,
	}
}

func batchReq(keys []*rpc.BatchGetMetricsReq_Key, wt rpc.WindowType,
	from int64, count int32) *rpc.BatchGetMetricsReq {
	return &rpc.BatchGetMetricsReq{
		SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: readAID,
		Keys: keys, WindowType: wt, WindowStartFrom: from, WindowCount: count,
	}
}

func batchKey(metricKey string, version int32) *rpc.BatchGetMetricsReq_Key {
	return &rpc.BatchGetMetricsReq_Key{MetricKey: metricKey, MetricVersion: version}
}

// --- 单点读 ---

// TestGetMetricResolvesClosedWindowAndNeverFabricates 钉住 window_start=0 的解析顺序
// （水位优先、极值兜底、极值只认已闭合窗口）、TOTAL 无水位语义，以及「未命中回
// found=false + 身份回显」这条反假成功线。
func TestGetMetricResolvesClosedWindowAndNeverFabricates(t *testing.T) {
	const key = "play_cnt"

	t.Run("window_start=0 优先读水位", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 3, "1,2,3")
		start := alignedStart5Min(0)
		seedWatermark(d, readSubj, key, 3, model.WindowType5Min, start, nowForTest())
		seedWindow(d, readSubj, readAID, key, 3, model.WindowType5Min, start, 8, "req-read")

		reply, err := callGetMetric(t, d, getMetricReq(key, 0, rpc.WindowType_WINDOW_TYPE_5_MIN))
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found {
			t.Fatalf("水位指向的闭合窗口有数据却回 found=false: %+v", reply)
		}
		p := reply.Point
		if p.WindowStart != start || p.MetricKey != key || p.MetricVersion != 3 ||
			p.WindowType != rpc.WindowType_WINDOW_TYPE_5_MIN ||
			p.SubjectType != rpc.SubjectType_SUBJECT_TYPE_AID || p.SubjectId != readAID {
			t.Fatalf("身份/窗口回显错位: %+v", p)
		}
		// seedWindow 由 value 派生分子分母，逐列核对等价于核对 metricPointOf 没漏列。
		if p.Value != 8 || p.Numerator != 8 || p.Denominator != 16 || p.SampleCount != 8 {
			t.Fatalf("取值投影与库里那行不一致: %+v", p)
		}
		if p.EventTime == 0 {
			t.Fatal("event_time 没回带：调用方无从判断这条数据有多新")
		}
		// 水位命中就不该再走极值兜底（那条路径要扫 idx_metric_latest 的区间极值）。
		if d.water.findCalls != 1 || d.wins.latestCalls != 0 || d.wins.findOneCalls != 1 {
			t.Fatalf("解析顺序不对：水位 %d 次、极值 %d 次、点查 %d 次",
				d.water.findCalls, d.wins.latestCalls, d.wins.findOneCalls)
		}
	})

	t.Run("水位缺失才退回极值，且只认已闭合窗口", func(t *testing.T) {
		d := newDeps(t)
		before := nowForTest()
		mustActiveDef(t, d, key, 1, "1")
		closed := alignedStart5Min(-1)
		open := alignedStart5Min(1) // 左边界晚于 now-300，正在写入
		seedWindow(d, readSubj, readAID, key, 1, model.WindowType5Min, closed, 8, "req-closed")
		seedWindow(d, readSubj, readAID, key, 1, model.WindowType5Min, open, 99, "req-open")

		reply, err := callGetMetric(t, d, getMetricReq(key, 1, rpc.WindowType_WINDOW_TYPE_5_MIN))
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found {
			t.Fatalf("兜底极值路径没读到数据: %+v", reply)
		}
		if reply.Point.WindowStart != closed || reply.Point.Value != 8 {
			t.Fatalf("把正在写入的窗口当成了最近闭合窗口: %+v", reply.Point)
		}
		if d.water.findCalls != 1 || d.wins.latestCalls != 1 {
			t.Fatalf("兜底路径没走对：水位 %d 次、极值 %d 次", d.water.findCalls, d.wins.latestCalls)
		}
		// closedBefore 是「只认已闭合窗口」的定义本身：下传成 now - 窗口长度。
		// 这条比上面那个「值必须是 8」的断言稳，因为它不依赖测试时钟与 logic 时钟
		// 之间那几微秒的先后（两者跨过一个 5 分钟边界时上面那条才会误报）。
		seen := d.wins.latestSeen[0]
		if bound := before - minute5; seen < bound || seen > bound+2 {
			t.Fatalf("下传的闭合线 closedBefore=%d，期望 %d（now-300）", seen, bound)
		}
		if model.WindowSeconds(model.WindowType5Min) != minute5 {
			t.Fatal("夹具的 5 分钟常量与 model.WindowSeconds 不一致")
		}
	})

	t.Run("无数据回 found=false 并回显身份，绝不伪造 0 值", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 2, "1")
		start := alignedStart5Min(0)
		seedWatermark(d, readSubj, key, 2, model.WindowType5Min, start, nowForTest())
		// 库里只有别的主体类型的行：本次查询确实没有数据。
		seedWindow(d, model.SubjectTypeZone, readAID, key, 2, model.WindowType5Min, start,
			8, "req-other-subject")

		reply, err := callGetMetric(t, d, getMetricReq(key, 0, rpc.WindowType_WINDOW_TYPE_5_MIN))
		if err != nil {
			t.Fatal(err)
		}
		if reply.Found {
			t.Fatalf("主体类型不匹配却回了 found=true: %+v", reply)
		}
		p := reply.Point
		if p == nil {
			t.Fatal("found=false 也必须回显「按哪个口径、哪个窗口查的」，否则无法复查")
		}
		if p.MetricKey != key || p.MetricVersion != 2 ||
			p.WindowType != rpc.WindowType_WINDOW_TYPE_5_MIN ||
			p.SubjectType != rpc.SubjectType_SUBJECT_TYPE_AID || p.SubjectId != readAID ||
			p.WindowStart != start {
			t.Fatalf("身份回显不全: %+v", p)
		}
		if p.Value != 0 || p.Numerator != 0 || p.Denominator != 0 || p.SampleCount != 0 {
			t.Fatalf("未命中却回了取值，0 会被读成真实指标: %+v", p)
		}
	})

	t.Run("显式 window_start 规整到左边界且不碰水位", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "2")
		h := hourStart(-1)
		seedWindow(d, readSubj, readAID, key, 1, model.WindowTypeHour, h, 8, "req-hour")
		// 水位故意指向另一个窗口：显式给定时必须按调用方那个窗口读。
		seedWatermark(d, readSubj, key, 1, model.WindowTypeHour, hourStart(-5), nowForTest())

		reply, err := callGetMetric(t, d, &rpc.GetMetricReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, SubjectId: readAID, MetricKey: key,
			MetricVersion: 1, WindowType: rpc.WindowType_WINDOW_TYPE_HOUR, WindowStart: h + 137,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Found || reply.Point.WindowStart != h {
			t.Fatalf("window_start 没被规整到左边界: %+v", reply)
		}
		if d.water.findCalls != 0 {
			t.Fatalf("显式窗口仍查了水位 %d 次", d.water.findCalls)
		}
	})

	t.Run("TOTAL 恒读 window_start=0 且不建水位", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "5")
		seedWindow(d, readSubj, readAID, key, 1, model.WindowTypeTotal, 0, 8, "req-total")
		seedWatermark(d, readSubj, key, 1, model.WindowType5Min, alignedStart5Min(0), nowForTest())
		total := rpc.WindowType_WINDOW_TYPE_TOTAL

		for _, reqStart := range []int64{0, 12345} {
			in := getMetricReq(key, 1, total)
			in.WindowStart = reqStart
			reply, err := callGetMetric(t, d, in)
			if err != nil {
				t.Fatalf("window_start=%d: %v", reqStart, err)
			}
			if !reply.Found || reply.Point.WindowStart != 0 {
				t.Fatalf("TOTAL 的 window_start 必须恒为 0（传入 %d）: %+v", reqStart, reply.Point)
			}
		}
		if d.water.findCalls != 0 || d.wins.latestCalls != 0 {
			t.Fatalf("TOTAL 没有水位概念：水位查了 %d 次、极值查了 %d 次",
				d.water.findCalls, d.wins.latestCalls)
		}
	})

	t.Run("命中路径不缓存，每次都回源", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "1")
		start := alignedStart5Min(0)
		seedWatermark(d, readSubj, key, 1, model.WindowType5Min, start, nowForTest())
		seedWindow(d, readSubj, readAID, key, 1, model.WindowType5Min, start, 8, "req-a")
		for i := 0; i < 2; i++ {
			if _, err := callGetMetric(t, d, getMetricReq(key, 1,
				rpc.WindowType_WINDOW_TYPE_5_MIN)); err != nil {
				t.Fatal(err)
			}
		}
		// 「最近闭合窗口」随水位前进，缓存它只会把刚闭合的窗口读成上一窗口的值。
		if d.wins.findOneCalls != 2 {
			t.Fatalf("单点读被缓存了（点查 %d 次 / 2 次调用）", d.wins.findOneCalls)
		}
	})

	t.Run("口径缺失或非 ACTIVE 一律显式报错", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "draft_key", 1, model.DefinitionStateDraft)
		mustActiveDef(t, d, "active_key", 1, "1")
		cases := []struct {
			why     string
			metric  string
			version int32
			want    error
		}{
			{"整键未登记", "ghost_key", 0, model.ErrMetricDefinitionNotFound},
			{"只有 DRAFT 时 version=0 没有 ACTIVE 指针", "draft_key", 0,
				model.ErrMetricDefinitionNotFound},
			{"显式要 DRAFT 版本（不对外可读）", "draft_key", 1, model.ErrMetricNotActive},
			{"显式要未登记的版本", "active_key", 9, model.ErrMetricDefinitionNotFound},
		}
		for _, tc := range cases {
			t.Run(tc.why, func(t *testing.T) {
				reply, err := callGetMetric(t, d, getMetricReq(tc.metric, tc.version,
					rpc.WindowType_WINDOW_TYPE_5_MIN))
				requireErrIs(t, err, tc.want, tc.why)
				if reply != nil {
					t.Fatalf("口径不可用却回了响应（会被当 0 值真数据）: %+v", reply)
				}
			})
		}
		if d.wins.findOneCalls != 0 {
			t.Fatalf("口径不可用仍查了指标表 %d 次", d.wins.findOneCalls)
		}
	})

	t.Run("ACTIVE 指针二义不得随机挑一个", func(t *testing.T) {
		d := newDeps(t)
		seedDef(t, d, "amb_key", 1, model.DefinitionStateActive)
		seedDef(t, d, "amb_key", 2, model.DefinitionStateActive)
		_, err := callGetMetric(t, d, getMetricReq("amb_key", 0, rpc.WindowType_WINDOW_TYPE_5_MIN))
		requireErrIs(t, err, model.ErrMultipleActiveDefinition, "同键多 ACTIVE")
	})

	t.Run("粒度未登记拒绝而不是恒空读", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "3") // 只登记了天级
		_, err := callGetMetric(t, d, getMetricReq(key, 0, rpc.WindowType_WINDOW_TYPE_5_MIN))
		requireErrIs(t, err, model.ErrInvalidWindow, "用 5 分钟读只有天级的口径")
		_, err = callGetMetric(t, d, getMetricReq(key, 0, rpc.WindowType(9)))
		requireErrIs(t, err, model.ErrInvalidWindow, "越界 window_type")
		_, err = callGetMetric(t, d, getMetricReq(key, 0, rpc.WindowType_WINDOW_TYPE_UNSPECIFIED))
		requireErrIs(t, err, model.ErrInvalidWindow, "UNSPECIFIED window_type")
		if d.wins.findOneCalls != 0 || d.water.findCalls != 0 {
			t.Fatalf("粒度被拒后仍查了库（点查 %d、水位 %d）", d.wins.findOneCalls, d.water.findCalls)
		}
	})

	t.Run("入参校验先于口径解析", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "1")
		d.defs.findKeyCalls, d.defs.findActiveCalls = 0, 0

		_, err := callGetMetric(t, d, &rpc.GetMetricReq{SubjectId: readAID, MetricKey: key,
			WindowType: rpc.WindowType_WINDOW_TYPE_5_MIN})
		requireErrIs(t, err, model.ErrInvalidSubject, "UNSPECIFIED 主体类型")
		_, err = callGetMetric(t, d, &rpc.GetMetricReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key,
			WindowType: rpc.WindowType_WINDOW_TYPE_5_MIN})
		requireErrIs(t, err, model.ErrInvalidSubject, "subject_id=0")
		_, err = callGetMetric(t, d, &rpc.GetMetricReq{
			SubjectType: rpc.SubjectType(9), SubjectId: readAID, MetricKey: key,
			WindowType: rpc.WindowType_WINDOW_TYPE_5_MIN})
		requireErrIs(t, err, model.ErrInvalidSubject, "越界主体类型")
		_, err = callGetMetric(t, d, getMetricReq("   ", 1, rpc.WindowType_WINDOW_TYPE_5_MIN))
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "空白 metric_key")
		_, err = callGetMetric(t, d, getMetricReq(strings.Repeat("k", maxMetricKeyBytes+1), 1,
			rpc.WindowType_WINDOW_TYPE_5_MIN))
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "超长 metric_key 按列宽拒绝")

		if d.defs.findKeyCalls != 0 || d.defs.findActiveCalls != 0 ||
			d.wins.findOneCalls != 0 || d.water.findCalls != 0 {
			t.Fatalf("入参不合法时仍解析了口径或查了库：口径 %d/%d、水位 %d、点查 %d",
				d.defs.findKeyCalls, d.defs.findActiveCalls, d.water.findCalls, d.wins.findOneCalls)
		}
	})

	t.Run("水位停摆仍回真实窗口", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "1")
		start := alignedStart5Min(0)
		// 留 60 秒余量：logic 内部自取 time.Now()，本包不能注入时钟，
		// 所以阈值右侧只钉到「明显越线」，不钉到恰好相等。
		laggy := nowForTest() - d.ctx.Config.Spm.WatermarkLagSeconds - 60
		seedWatermark(d, readSubj, key, 1, model.WindowType5Min, start, laggy)
		seedWindow(d, readSubj, readAID, key, 1, model.WindowType5Min, start, 8, "req-laggy")

		reply, err := callGetMetric(t, d, getMetricReq(key, 1, rpc.WindowType_WINDOW_TYPE_5_MIN))
		if err != nil {
			t.Fatalf("聚合器停摆不是读接口的错误：应回真实窗口并留日志证据, %v", err)
		}
		if !reply.Found || reply.Point.WindowStart != start {
			t.Fatalf("停摆时把真实 window_start 改写了: %+v", reply.Point)
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "1")
		d.read.denies = 1
		_, err := callGetMetric(t, d, getMetricReq(key, 1, rpc.WindowType_WINDOW_TYPE_5_MIN))
		requireErrIs(t, err, model.ErrRateLimited, "无令牌必须显式失败而不是回空数据")
		if d.wins.findOneCalls != 0 || d.water.findCalls != 0 {
			t.Fatal("限流后仍然查了库")
		}

		d2 := newDeps(t)
		mustActiveDef(t, d2, key, 1, "1")
		start := alignedStart5Min(0)
		seedWatermark(d2, readSubj, key, 1, model.WindowType5Min, start, nowForTest())
		d2.wins.findOneErr = errStub
		_, err = callGetMetric(t, d2, getMetricReq(key, 1, rpc.WindowType_WINDOW_TYPE_5_MIN))
		if !errors.Is(err, errStub) {
			t.Fatalf("库故障必须原样上抛，实得 %v", err)
		}
	})
}

// --- 批量读 ---

const (
	batchFast = "play_cnt_fast"
	batchSlow = "play_cnt_slow"
)

// batchFixture 备一套「两个口径、水位快慢不一」的夹具。
// fast 的水位指向最近闭合窗口，slow 落后两个窗口：min/max 之争只有这种夹具能分辨。
type batchFixture struct {
	d                          *deps
	s0, s1, s2, s3, s4, sMinus int64
}

func newBatchFixture(t *testing.T) *batchFixture {
	t.Helper()
	d := newDeps(t)
	mustActiveDef(t, d, batchFast, 1, "1,2,3")
	mustActiveDef(t, d, batchSlow, 1, "1,2,3")
	f := &batchFixture{d: d, s0: alignedStart5Min(0), s1: alignedStart5Min(-1),
		s2: alignedStart5Min(-2), s3: alignedStart5Min(-3), s4: alignedStart5Min(-4)}
	seedWatermark(d, readSubj, batchFast, 1, model.WindowType5Min, f.s0, nowForTest())
	seedWatermark(d, readSubj, batchSlow, 1, model.WindowType5Min, f.s2, nowForTest())
	seedWindow(d, readSubj, readAID, batchFast, 1, model.WindowType5Min, f.s0, 100, "req-f0")
	seedWindow(d, readSubj, readAID, batchFast, 1, model.WindowType5Min, f.s1, 50, "req-f1")
	seedWindow(d, readSubj, readAID, batchSlow, 1, model.WindowType5Min, f.s2, 5, "req-s2")
	// s3/s4 故意留空：区间里缺数据的窗口必须从 map 里缺席，而不是补 0。
	return f
}

// TestBatchGetMetricsIntervalEndIsMinimumClosedWindow README「窗口、水位与迟到」与
// 契约缺口之外的实现约定：window_start_from=0 时区间右端取各口径最近闭合窗口的
// **最小值**。取最大值会让水位落后（跑得慢、但数据没错）的口径整段落空，
// 看起来就像「这个指标最近没人看」。
func TestBatchGetMetricsIntervalEndIsMinimumClosedWindow(t *testing.T) {
	fiveMin := rpc.WindowType_WINDOW_TYPE_5_MIN

	t.Run("右端取最小水位，落后的口径不被吞", func(t *testing.T) {
		f := newBatchFixture(t)
		reply, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1),
		}, fiveMin, 0, 3))
		if err != nil {
			t.Fatal(err)
		}
		// end = min(s0, s2) = s2，count=3 -> [s4, s3, s2]。
		wantStarts := fmt.Sprintf("%v", []int64{f.s4, f.s3, f.s2})
		seen := f.d.wins.listKeysSeen
		if len(seen) != 1 || !strings.Contains(seen[0], wantStarts) {
			t.Fatalf("区间起点=%v，期望落在 %s（右端必须是最小水位）", seen, wantStarts)
		}
		wantKeys := fmt.Sprintf("%v", []model.MetricKeyVersion{
			{MetricKey: batchFast, Version: 1}, {MetricKey: batchSlow, Version: 1}})
		if !strings.Contains(seen[0], wantKeys) {
			t.Fatalf("口径集合下传错位：%s", seen[0])
		}
		// map 里只该有 slow@s2：fast 的 s0/s1 都在区间之外。
		only, ok := reply.Points[pointKey(batchSlow, 1, f.s2)]
		if !ok || only.Value != 5 {
			t.Fatalf("落后的口径没读到：%+v", reply.Points)
		}
		if len(reply.Points) != 1 {
			t.Fatalf("map 里有 %d 个点，区间右端取错了值：%+v", len(reply.Points), reply.Points)
		}
		for _, absent := range []string{pointKey(batchFast, 1, f.s0), pointKey(batchFast, 1, f.s1)} {
			if _, has := reply.Points[absent]; has {
				t.Fatalf("%s 不该出现：它的窗口比最小水位更新，读它会与 slow 的旧窗口混在同一区间", absent)
			}
		}
		// 一次批量取水位 + 一次区间查询，绝不按 key 循环打库。
		if f.d.water.listKeysCalls != 1 || f.d.wins.listKeysCalls != 1 || f.d.wins.latestCalls != 0 {
			t.Fatalf("打库次数不对：水位 %d、区间 %d、极值 %d",
				f.d.water.listKeysCalls, f.d.wins.listKeysCalls, f.d.wins.latestCalls)
		}
		// 缺数据的窗口不补 0：区间有 3 个点却只回 1 个。
		if got := len(reply.Points); got != 1 {
			t.Fatalf("缺数据的窗口被补成了 %d 个点", got)
		}
	})

	t.Run("水位缺失时按已落库极值兜底，右端仍取最小", func(t *testing.T) {
		f := newBatchFixture(t)
		for k := range f.d.db.water { // 清掉水位行，逼出兜底路径
			delete(f.d.db.water, k)
		}
		reply, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1),
		}, fiveMin, 0, 3))
		if err != nil {
			t.Fatal(err)
		}
		// 兜底极值：fast -> s0（s1 也闭合但更小）、slow -> s2，右端仍取 s2。
		seen := f.d.wins.listKeysSeen
		wantStarts := fmt.Sprintf("%v", []int64{f.s4, f.s3, f.s2})
		if len(seen) != 1 || !strings.Contains(seen[0], wantStarts) {
			t.Fatalf("兜底路径区间=%v，期望 %s", seen, wantStarts)
		}
		if _, ok := reply.Points[pointKey(batchSlow, 1, f.s2)]; !ok {
			t.Fatalf("兜底路径没读到 slow 的最近闭合窗口: %+v", reply.Points)
		}
		if f.d.water.listKeysCalls != 1 || f.d.wins.latestCalls != 2 {
			t.Fatalf("兜底次数不对：水位批量 %d、逐口径极值 %d",
				f.d.water.listKeysCalls, f.d.wins.latestCalls)
		}
	})

	t.Run("一个闭合窗口都没有时空 map 且不发区间查询", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, batchFast, 1, "1")
		reply, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1)}, fiveMin, 0, 5))
		if err != nil {
			t.Fatal(err)
		}
		if reply.Points == nil || len(reply.Points) != 0 {
			t.Fatalf("应回非 nil 的空 map: %+v", reply)
		}
		if d.wins.listKeysCalls != 0 {
			t.Fatalf("没有可读窗口却发了 %d 次区间查询", d.wins.listKeysCalls)
		}
		if d.water.listKeysCalls != 1 || d.wins.latestCalls != 1 {
			t.Fatalf("解析顺序不对：水位 %d、极值 %d", d.water.listKeysCalls, d.wins.latestCalls)
		}
	})

	t.Run("window_start_from>0 直接按起点数窗口，不查水位", func(t *testing.T) {
		f := newBatchFixture(t)
		reply, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1),
		}, fiveMin, f.s1, 2))
		if err != nil {
			t.Fatal(err)
		}
		wantStarts := fmt.Sprintf("%v", []int64{f.s1, f.s0})
		seen := f.d.wins.listKeysSeen
		if len(seen) != 1 || !strings.HasSuffix(seen[0], wantStarts) {
			t.Fatalf("显式区间=%v，期望 %s", seen, wantStarts)
		}
		if f.d.water.listKeysCalls != 0 || f.d.wins.latestCalls != 0 {
			t.Fatalf("显式起点仍解析了水位（%d/%d 次）", f.d.water.listKeysCalls, f.d.wins.latestCalls)
		}
		if len(reply.Points) != 2 {
			t.Fatalf("显式区间应读到 fast 的两个窗口: %+v", reply.Points)
		}
	})

	t.Run("起点未对齐时规整到左边界", func(t *testing.T) {
		f := newBatchFixture(t)
		if _, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1)}, fiveMin, f.s1+137, 1)); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%v", []int64{f.s1})
		if !strings.HasSuffix(f.d.wins.listKeysSeen[0], want) {
			t.Fatalf("起点 %d 未规整：%s", f.s1+137, f.d.wins.listKeysSeen[0])
		}
	})

	t.Run("TOTAL 只读一个点，count 再大也一样", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, "total_view", 1, "5")
		seedWindow(d, readSubj, readAID, "total_view", 1, model.WindowTypeTotal, 0, 42, "req-t")
		reply, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey("total_view", 1)}, rpc.WindowType_WINDOW_TYPE_TOTAL, 0, 7))
		if err != nil {
			t.Fatal(err)
		}
		seen := d.wins.listKeysSeen
		if len(seen) != 1 || !strings.HasSuffix(seen[0], "[0]") {
			t.Fatalf("TOTAL 的区间必须是单个 window_start=0：%v", seen)
		}
		p, ok := reply.Points[pointKey("total_view", 1, 0)]
		if !ok || p.Value != 42 {
			t.Fatalf("TOTAL 累计值没读到: %+v", reply.Points)
		}
		if d.water.listKeysCalls != 0 || d.wins.latestCalls != 0 {
			t.Fatal("TOTAL 不建水位，不该查水位或极值")
		}
	})

	t.Run("重复口径只解析一次、只下一份", func(t *testing.T) {
		f := newBatchFixture(t)
		if _, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchFast, 1), batchKey(batchFast, 0),
		}, fiveMin, f.s0, 1)); err != nil {
			t.Fatal(err)
		}
		wantSeen := fmt.Sprintf("%d:%d:%d:%v:%v", readSubj, readAID, model.WindowType5Min,
			[]model.MetricKeyVersion{{MetricKey: batchFast, Version: 1}}, []int64{f.s0})
		if len(f.d.wins.listKeysSeen) != 1 || f.d.wins.listKeysSeen[0] != wantSeen {
			t.Fatalf("重复键没去重：\n实得 %v\n期望 %s", f.d.wins.listKeysSeen, wantSeen)
		}
	})

	t.Run("版本按各自口径解析后原样下传", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, "v_key", 7, "1")
		start := alignedStart5Min(0)
		seedWindow(d, readSubj, readAID, "v_key", 7, model.WindowType5Min, start, 8, "req-v")
		if _, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey("v_key", 0)}, fiveMin, start, 1)); err != nil {
			t.Fatal(err)
		}
		wantSeen := fmt.Sprintf("%d:%d:%d:%v:%v", readSubj, readAID, model.WindowType5Min,
			[]model.MetricKeyVersion{{MetricKey: "v_key", Version: 7}}, []int64{start})
		if d.wins.listKeysSeen[0] != wantSeen {
			t.Fatalf("version=0 没解析成 ACTIVE 版本 7：\n实得 %s\n期望 %s",
				d.wins.listKeysSeen[0], wantSeen)
		}
	})
}

// TestBatchGetMetricsRejectsWholeRequestOnUnsafeArgs 批量读的整单拒绝线：
// 部分成功会让「这个 key 为什么不在 map 里」变成无法回答的问题。
func TestBatchGetMetricsRejectsWholeRequestOnUnsafeArgs(t *testing.T) {
	fiveMin := rpc.WindowType_WINDOW_TYPE_5_MIN

	t.Run("任一口径不可用整单拒绝", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, batchFast, 1, "1")
		_, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey("ghost_key", 1)}, fiveMin, 0, 3))
		requireErrIs(t, err, model.ErrMetricDefinitionNotFound, "混了一个不存在的口径")
		if d.wins.listKeysCalls != 0 {
			t.Fatal("整单拒绝却仍发了区间查询")
		}
	})

	t.Run("任一口径未登记粒度整单拒绝", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, batchFast, 1, "1")
		mustActiveDef(t, d, batchSlow, 1, "3") // 只有天级
		_, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1)}, fiveMin, 0, 3))
		requireErrIs(t, err, model.ErrInvalidWindow, "混了一个没有 5 分钟档的口径")
		if d.wins.listKeysCalls != 0 {
			t.Fatal("整单拒绝却仍发了区间查询")
		}
	})

	t.Run("DRAFT 口径混在批量里整单拒绝", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, batchFast, 1, "1")
		seedDef(t, d, "draft_key", 1, model.DefinitionStateDraft)
		_, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey("draft_key", 1)}, fiveMin, 0, 3))
		requireErrIs(t, err, model.ErrMetricNotActive, "DRAFT 不对外可读")
	})

	t.Run("必填与上限", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, batchFast, 1, "1")

		_, err := callBatchGet(t, d, batchReq(nil, fiveMin, 0, 1))
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "keys 为空")
		if d.wins.listKeysCalls != 0 {
			t.Fatal("空 keys 仍查了库")
		}

		many := make([]*rpc.BatchGetMetricsReq_Key, 0, int(d.ctx.Config.Spm.MaxMetricKeysPerRequest)+1)
		for i := 0; i <= int(d.ctx.Config.Spm.MaxMetricKeysPerRequest); i++ {
			many = append(many, batchKey(fmt.Sprintf("k%d", i), 1))
		}
		_, err = callBatchGet(t, d, batchReq(many, fiveMin, 0, 1))
		requireErrIs(t, err, model.ErrTooManyKeys, "keys 超上限必须拒绝而不是截断")

		_, err = callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{batchKey(batchFast, 1)},
			fiveMin, 0, d.ctx.Config.Spm.MaxWindowCount+1))
		requireErrIs(t, err, model.ErrWindowRangeTooLarge, "window_count 超上限")

		_, err = callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{batchKey(batchFast, 1)},
			rpc.WindowType_WINDOW_TYPE_UNSPECIFIED, 0, 1))
		requireErrIs(t, err, model.ErrInvalidWindow, "UNSPECIFIED window_type")

		_, err = callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{batchKey("  ", 1)},
			fiveMin, 0, 1))
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "keys 里的空白键")

		_, err = callBatchGet(t, d, &rpc.BatchGetMetricsReq{SubjectId: readAID,
			Keys: []*rpc.BatchGetMetricsReq_Key{batchKey(batchFast, 1)}, WindowType: fiveMin})
		requireErrIs(t, err, model.ErrInvalidSubject, "UNSPECIFIED subject_type")

		// window_count<=0 归一为 1，而不是当成「不限」。
		start := alignedStart5Min(0)
		if _, err := callBatchGet(t, d, batchReq([]*rpc.BatchGetMetricsReq_Key{batchKey(batchFast, 1)},
			fiveMin, start, -5)); err != nil {
			t.Fatal(err)
		}
		wantSeen := fmt.Sprintf("%d:%d:%d:%v:%v", readSubj, readAID, model.WindowType5Min,
			[]model.MetricKeyVersion{{MetricKey: batchFast, Version: 1}}, []int64{start})
		if len(d.wins.listKeysSeen) != 1 || d.wins.listKeysSeen[0] != wantSeen {
			t.Fatalf("window_count 归一后区间长度不对（应为 1 个点）：\n实得 %v\n期望 %s",
				d.wins.listKeysSeen, wantSeen)
		}
	})

	t.Run("限流优先于一切查库", func(t *testing.T) {
		f := newBatchFixture(t)
		f.d.read.denies = 1
		_, err := callBatchGet(t, f.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1)}, fiveMin, 0, 3))
		requireErrIs(t, err, model.ErrRateLimited, "无令牌必须显式失败而不是回空 map")
		if f.d.wins.listKeysCalls != 0 || f.d.water.listKeysCalls != 0 {
			t.Fatal("限流后仍然查了库")
		}
	})

	t.Run("库故障原样上抛", func(t *testing.T) {
		// 三个查库点各注入一次：任何一个被吞掉都会退化成「空 map = 没有数据」的假成功。
		f1 := newBatchFixture(t)
		f1.d.water.listKeysErr = errStub
		_, err := callBatchGet(t, f1.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1)}, fiveMin, 0, 3))
		if !errors.Is(err, errStub) {
			t.Fatalf("水位批量查询的库故障必须上抛，实得 %v", err)
		}

		f2 := newBatchFixture(t)
		f2.d.wins.listKeysErr = errStub
		_, err = callBatchGet(t, f2.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1), batchKey(batchSlow, 1)}, fiveMin, 0, 3))
		if !errors.Is(err, errStub) {
			t.Fatalf("区间查询的库故障必须上抛，实得 %v", err)
		}

		f3 := newBatchFixture(t)
		f3.d.wins.latestErr = errStub
		for k := range f3.d.db.water {
			delete(f3.d.db.water, k)
		}
		_, err = callBatchGet(t, f3.d, batchReq([]*rpc.BatchGetMetricsReq_Key{
			batchKey(batchFast, 1)}, fiveMin, 0, 3))
		if !errors.Is(err, errStub) {
			t.Fatalf("极值兜底路径的库故障必须上抛，实得 %v", err)
		}
	})
}

// --- 热榜 ---

// TestListHotSubjectsWatermarkAndPaging 钉住出榜主体白名单、水位决定榜单窗口、
// ps 越界拒绝（不是 clamp）、越界页与深翻页只回 total 不发列表 SQL、名次连续编号。
func TestListHotSubjectsWatermarkAndPaging(t *testing.T) {
	const key = "play_cnt"
	fiveMin := rpc.WindowType_WINDOW_TYPE_5_MIN

	// hotSetup：5 行榜、值 10..50，热榜窗口取水位。
	hotSetup := func(t *testing.T) (*deps, int64) {
		t.Helper()
		d := newDeps(t)
		mustActiveDef(t, d, key, 3, "1,2,3")
		start := alignedStart5Min(0)
		seedWatermark(d, readSubj, key, 3, model.WindowType5Min, start, nowForTest())
		d.wins.hotRows = nil
		for i := 0; i < 5; i++ {
			d.wins.hotRows = append(d.wins.hotRows, &model.HotSubject{
				SubjectID: int64(1000 + i), MetricValue: float64(10 * (i + 1)),
				Numerator: int64(i + 1), Denominator: int64(2 * (i + 1)),
			})
		}
		return d, start
	}

	t.Run("出榜主体白名单", func(t *testing.T) {
		d, _ := hotSetup(t)
		for _, st := range []rpc.SubjectType{rpc.SubjectType_SUBJECT_TYPE_UNSPECIFIED,
			rpc.SubjectType_SUBJECT_TYPE_MID, rpc.SubjectType(9)} {
			_, err := callHot(t, d, &rpc.ListHotSubjectsReq{SubjectType: st, MetricKey: key,
				WindowType: fiveMin})
			requireErrIs(t, err, model.ErrInvalidSubject, fmt.Sprintf("subject_type=%d 不出榜", st))
		}
		for _, st := range []rpc.SubjectType{rpc.SubjectType_SUBJECT_TYPE_AID,
			rpc.SubjectType_SUBJECT_TYPE_ZONE, rpc.SubjectType_SUBJECT_TYPE_CATALOG_ITEM} {
			if _, err := callHot(t, d, &rpc.ListHotSubjectsReq{SubjectType: st, MetricKey: key,
				WindowType: fiveMin, WindowStart: alignedStart5Min(0)}); err != nil {
				t.Fatalf("subject_type=%d 应可出榜: %v", st, err)
			}
		}
	})

	t.Run("榜单窗口取水位且名次按整榜连续编号", func(t *testing.T) {
		d, start := hotSetup(t)
		reply, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key,
			WindowType: fiveMin, Pn: 2, Ps: 2})
		if err != nil {
			t.Fatal(err)
		}
		if reply.WindowStart != start || reply.MetricVersion != 3 || reply.Total != 5 {
			t.Fatalf("回带的窗口/版本/total 不对: %+v", reply)
		}
		if len(reply.Subjects) != 2 {
			t.Fatalf("本页行数=%d: %+v", len(reply.Subjects), reply.Subjects)
		}
		for i, s := range reply.Subjects {
			wantRow := d.wins.hotRows[2+i]
			if s.SubjectId != wantRow.SubjectID || s.Value != wantRow.MetricValue ||
				s.Numerator != wantRow.Numerator || s.Denominator != wantRow.Denominator {
				t.Fatalf("第 %d 行投影与库里不一致: %+v", i, s)
			}
			// 名次 = 本页在整榜中的位置，调用方翻页不需要自己累加。
			if s.Rank != int32(3+i) {
				t.Fatalf("rank=%d，期望 %d", s.Rank, 3+i)
			}
		}
		want := fmt.Sprintf("%d:%s@v%d:%d:%d:zone=%d", readSubj, key, 3,
			model.WindowType5Min, start, 0)
		if len(d.wins.countHotSeen) != 1 || d.wins.countHotSeen[0] != want {
			t.Fatalf("COUNT 下传条件=%v，期望 %s", d.wins.countHotSeen, want)
		}
	})

	t.Run("zone_id 进榜条件", func(t *testing.T) {
		d, start := hotSetup(t)
		d.wins.countHotSeen = nil
		if _, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start, ZoneId: 1009}); err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(d.wins.countHotSeen[0], "zone=1009") {
			t.Fatalf("分区过滤没下传：%s", d.wins.countHotSeen[0])
		}
	})

	t.Run("显式 window_start 规整且不查水位", func(t *testing.T) {
		d, start := hotSetup(t)
		if _, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start + 7}); err != nil {
			t.Fatal(err)
		}
		if d.water.findCalls != 0 {
			t.Fatalf("显式窗口仍查了水位 %d 次", d.water.findCalls)
		}
		// 下传条件的拼接口径见夹具 CountHot（fakes_test.go 的 "%d:%s@v%d:%d:%d:zone=%d"），
		// 与「榜单窗口取水位且名次按整榜连续编号」子用例同源。
		// 整串比对而非后缀：既要钉 start+7 被 model.AlignWindow 规整回 start，
		// 也要钉主体/键/版本/粒度/zone 一个都没漏。
		want := fmt.Sprintf("%d:%s@v%d:%d:%d:zone=%d", readSubj, key, 3,
			model.WindowType5Min, start, 0)
		if len(d.wins.countHotSeen) != 1 || d.wins.countHotSeen[0] != want {
			t.Fatalf("显式窗口未规整：%v，期望 %s", d.wins.countHotSeen, want)
		}
	})

	t.Run("还没有任何闭合窗口时空榜回显 0，而不是全 0 榜", func(t *testing.T) {
		d := newDeps(t)
		mustActiveDef(t, d, key, 1, "1")
		reply, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin})
		if err != nil {
			t.Fatal(err)
		}
		if reply.WindowStart != 0 || reply.Total != 0 || len(reply.Subjects) != 0 {
			t.Fatalf("应回「榜还没开始」: %+v", reply)
		}
		if reply.MetricVersion != 1 {
			t.Fatalf("空榜也要回显解析出的口径版本: %+v", reply)
		}
		if d.wins.countHotCalls != 0 || d.wins.listHotCalls != 0 {
			t.Fatalf("无闭合窗口仍查了库（COUNT %d、LIST %d）",
				d.wins.countHotCalls, d.wins.listHotCalls)
		}
		if d.wins.latestCalls != 1 {
			t.Fatal("水位缺失时应退到极值兜底确认「确实没有」")
		}
	})

	t.Run("ps 越界拒绝而不是 clamp", func(t *testing.T) {
		d, _ := hotSetup(t)
		_, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: alignedStart5Min(0), Ps: d.ctx.Config.Spm.MaxPageSize + 1})
		requireErrIs(t, err, model.ErrPsTooLarge, "ps>MaxPageSize 必须拒绝：clamp 会让调用方以为取了更多行")
		_, err = callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: alignedStart5Min(0), Ps: -1})
		requireErrIs(t, err, model.ErrPsTooLarge, "负 ps 同样拒绝")
		if d.wins.countHotCalls != 0 || d.wins.listHotCalls != 0 {
			t.Fatal("越界 ps 仍查了库")
		}
		// ps=0 走配置默认页大小。
		d.wins.countHotSeen = nil
		if _, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: alignedStart5Min(0)}); err != nil {
			t.Fatal(err)
		}
		if got := d.wins.listHotCalls; got != 1 {
			t.Fatalf("默认页大小路径没查库（%d 次）", got)
		}
	})

	t.Run("越界页只回 total 不再发列表 SQL", func(t *testing.T) {
		d, start := hotSetup(t)
		total := int64(3)
		d.wins.hotTotal = &total
		d.wins.countHotCalls, d.wins.listHotCalls = 0, 0
		reply, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start, Pn: 2, Ps: 10})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != total || len(reply.Subjects) != 0 {
			t.Fatalf("越界页响应不对: %+v", reply)
		}
		if d.wins.countHotCalls != 1 || d.wins.listHotCalls != 0 {
			t.Fatalf("越界页发了 %d 次列表查询（应只有 COUNT）", d.wins.listHotCalls)
		}
	})

	t.Run("深翻页守卫与 total 无关", func(t *testing.T) {
		d, start := hotSetup(t)
		big := int64(20_000_000)
		d.wins.hotTotal = &big
		d.wins.countHotCalls, d.wins.listHotCalls = 0, 0
		reply, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start, Pn: 100_001, Ps: 100})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != big {
			t.Fatalf("total=%d", reply.Total)
		}
		if d.wins.countHotCalls != 1 || d.wins.listHotCalls != 0 {
			t.Fatalf("OFFSET 已达 1e7 量级仍查列表（%d 次）", d.wins.listHotCalls)
		}
	})

	t.Run("缓存关闭时每页都回源，键含窗口与分区", func(t *testing.T) {
		d, start := hotSetup(t)
		for i := 0; i < 2; i++ {
			if _, err := callHot(t, d, &rpc.ListHotSubjectsReq{
				SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
				WindowStart: start, Pn: 2, Ps: 2}); err != nil {
				t.Fatal(err)
			}
		}
		// svc.Cache 为 nil -> shortCacheTTL()==0 -> 缓存整体关闭（README 契约缺口第 7 条）。
		// 真值必须每次现查，否则「缓存命中」与「确实只有这些行」在测试里长得一样。
		if d.wins.countHotCalls != 2 || d.wins.listHotCalls != 2 {
			t.Fatalf("缓存关闭时仍少查了库：COUNT %d、LIST %d",
				d.wins.countHotCalls, d.wins.listHotCalls)
		}
		if shortCacheTTL(d.ctx) != 0 {
			t.Fatal("夹具里的 Cache 配置应让短缓存整体关闭")
		}
		base := hotListCacheKey(readSubj, key, 3, model.WindowType5Min, start, 0, 2, 2)
		for name, other := range map[string]string{
			"分区不同": hotListCacheKey(readSubj, key, 3, model.WindowType5Min, start, 1009, 2, 2),
			"页码不同": hotListCacheKey(readSubj, key, 3, model.WindowType5Min, start, 0, 4, 2),
			"窗口不同": hotListCacheKey(readSubj, key, 3, model.WindowType5Min, start+minute5, 0, 2, 2),
			"版本不同": hotListCacheKey(readSubj, key, 4, model.WindowType5Min, start, 0, 2, 2),
			"粒度不同": hotListCacheKey(readSubj, key, 3, model.WindowTypeHour, start, 0, 2, 2),
			"主体不同": hotListCacheKey(model.SubjectTypeZone, key, 3, model.WindowType5Min, start, 0, 2, 2),
		} {
			if other == base {
				t.Fatalf("%s 却得到同一个榜单缓存键 %s（会串页）", name, base)
			}
		}
		if !strings.HasPrefix(base, hotListCachePrefix) || !strings.Contains(base, key+"@v3") {
			t.Fatalf("榜单缓存键必须落在本服务命名空间并带口径版本: %s", base)
		}
	})

	t.Run("口径与粒度先于分页", func(t *testing.T) {
		d, _ := hotSetup(t)
		_, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: "ghost_key",
			WindowType: fiveMin, Ps: 999})
		requireErrIs(t, err, model.ErrMetricDefinitionNotFound, "口径错误优先于分页错误")
		_, err = callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key,
			WindowType: rpc.WindowType_WINDOW_TYPE_WEEK, Ps: 999})
		requireErrIs(t, err, model.ErrInvalidWindow, "粒度错误优先于分页错误")
		if d.wins.countHotCalls != 0 {
			t.Fatal("校验未通过却查了库")
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d, start := hotSetup(t)
		d.read.denies = 1
		_, err := callHot(t, d, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌必须显式失败而不是回一张空榜")
		if d.wins.countHotCalls != 0 || d.wins.listHotCalls != 0 {
			t.Fatal("限流后仍查了库")
		}

		d2, start2 := hotSetup(t)
		d2.wins.hotErr = errStub
		if _, err = callHot(t, d2, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start2}); !errors.Is(err, errStub) {
			t.Fatalf("COUNT 的库故障必须上抛，实得 %v", err)
		}

		// COUNT 已成功、只有列表查询失败时，绝不能回一张「total=5、榜是空的」响应。
		d3, start3 := hotSetup(t)
		d3.wins.listHotErr = errStub
		if _, err = callHot(t, d3, &rpc.ListHotSubjectsReq{
			SubjectType: rpc.SubjectType_SUBJECT_TYPE_AID, MetricKey: key, WindowType: fiveMin,
			WindowStart: start3, Pn: 1, Ps: 2}); !errors.Is(err, errStub) {
			t.Fatalf("LIST 的库故障必须上抛，实得 %v", err)
		}
		if d3.wins.countHotCalls != 1 || d3.wins.listHotCalls != 1 {
			t.Fatalf("榜单查询次数不对：COUNT %d、LIST %d",
				d3.wins.countHotCalls, d3.wins.listHotCalls)
		}
	})
}

// --- 消费状态 ---

// TestListConsumerStateBoundedAggregate 钉住 README「数据保留策略」与契约缺口：
// 契约里 ListConsumerStateReq 没有 since 字段，有界聚合的时间下界只能由服务端按
// BehaviorRetentionDays 给；过滤条件写错时必须拒绝而不是当「不限」。
func TestListConsumerStateBoundedAggregate(t *testing.T) {
	rows := []*model.ConsumerSummary{{
		Topic: "behavior.play.v1", State: model.ConsumerStateSucceeded, RowCount: 7,
		OldestCtime: 111, LastMsgOffset: 999, LastEventTime: 888,
	}}

	t.Run("条件与分页逐列下传", func(t *testing.T) {
		d := newDeps(t)
		d.offs.rows = rows
		// total 必须覆盖 pn=4/ps=5 的 offset=15：否则 logic 按 README「越界页与深翻页保护线
		// 只回 total、不发查询 SQL」短路，Summarize 根本不会被调用，
		// 「COUNT 与 SUMMARIZE 同条件」就无从观测。total 是「topic × 状态」汇总行数。
		total := int64(64)
		d.offs.total = &total
		before := nowForTest()
		reply, err := callConsumer(t, d, &rpc.ListConsumerStateReq{Topic: " behavior.play.v1 ",
			State: rpc.ConsumerState_CONSUMER_STATE_SUCCEEDED, Pn: 4, Ps: 5})
		if err != nil {
			t.Fatal(err)
		}
		lower := before - int64(d.ctx.Config.Spm.BehaviorRetentionDays)*dayUnix
		if len(d.offs.seenSince) != 2 {
			t.Fatalf("COUNT 与 SUMMARIZE 应各记一次时间下界：%v", d.offs.seenSince)
		}
		if reply.Total != total {
			t.Fatalf("total=%d，应等于 COUNT 下传后回带的汇总行数 %d", reply.Total, total)
		}
		for _, saw := range d.offs.seenSince {
			// 下界 = now - BehaviorRetentionDays*86400；logic 自取时钟，只留 2 秒余量。
			if saw < lower || saw > lower+2 {
				t.Fatalf("时间下界 %d 不在保留期边界 %d 附近", saw, lower)
			}
		}
		if d.offs.seenTopic[0] != "behavior.play.v1" || d.offs.seenTopic[1] != "behavior.play.v1" {
			t.Fatalf("topic 裁剪后没原样进两条 SQL：%v", d.offs.seenTopic)
		}
		for _, s := range d.offs.seenStates {
			if s != model.ConsumerStateSucceeded {
				t.Fatalf("状态过滤下传成 %q", s)
			}
		}
		if d.offs.seenOffset[0] != 15 || d.offs.seenLimit[0] != 5 {
			t.Fatalf("分页未下传：offset=%d limit=%d", d.offs.seenOffset[0], d.offs.seenLimit[0])
		}
		if len(reply.Rows) != 1 {
			t.Fatalf("行数=%d", len(reply.Rows))
		}
		r := reply.Rows[0]
		if r.Topic != "behavior.play.v1" || r.State != rpc.ConsumerState_CONSUMER_STATE_SUCCEEDED ||
			r.Count != 7 || r.OldestCtime != 111 || r.LastMsgOffset != 999 || r.LastEventTime != 888 {
			t.Fatalf("汇总列没逐列回带: %+v", r)
		}
	})

	t.Run("UNSPECIFIED 是「不限」，未知枚举是错误", func(t *testing.T) {
		d := newDeps(t)
		d.offs.rows = rows
		if _, err := callConsumer(t, d, &rpc.ListConsumerStateReq{}); err != nil {
			t.Fatal(err)
		}
		for _, s := range d.offs.seenStates {
			if s != "" {
				t.Fatalf("未指定状态时不该带 state 条件，实得 %q", s)
			}
		}
		if d.offs.seenTopic[0] != "" {
			t.Fatalf("空 topic = 全部，实得 %q", d.offs.seenTopic[0])
		}

		d2 := newDeps(t)
		for _, st := range []rpc.ConsumerState{rpc.ConsumerState(9), rpc.ConsumerState(-1),
			rpc.ConsumerState(6)} {
			_, err := callConsumer(t, d2, &rpc.ListConsumerStateReq{State: st})
			requireErrIs(t, err, model.ErrInvalidConsumerState, fmt.Sprintf("越界 state=%d", st))
		}
		if d2.offs.countCalls != 0 || d2.offs.summarizeCalls != 0 {
			t.Fatal("非法过滤条件必须拒绝，而不是当「不限」去查全表")
		}
	})

	t.Run("库里脏状态映射成 UNSPECIFIED", func(t *testing.T) {
		d := newDeps(t)
		d.offs.rows = []*model.ConsumerSummary{
			{Topic: "t", State: "bogus_state", RowCount: 1},
			{Topic: "t", State: model.ConsumerStateDeadLetter, RowCount: 1},
		}
		reply, err := callConsumer(t, d, &rpc.ListConsumerStateReq{})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Rows[0].State != rpc.ConsumerState_CONSUMER_STATE_UNSPECIFIED {
			t.Fatalf("解释不了的状态被悄悄归到了 %v", reply.Rows[0].State)
		}
		if reply.Rows[1].State != rpc.ConsumerState_CONSUMER_STATE_DEAD_LETTER {
			t.Fatalf("已知状态映射错了: %v", reply.Rows[1].State)
		}
	})

	t.Run("ps 越界先拒，再查 topic", func(t *testing.T) {
		d := newDeps(t)
		d.offs.rows = rows
		_, err := callConsumer(t, d, &rpc.ListConsumerStateReq{
			Topic: strings.Repeat("t", maxTopicBytes+1), Ps: d.ctx.Config.Spm.MaxPageSize + 1})
		requireErrIs(t, err, model.ErrPsTooLarge, "分页校验在 topic 之前")
		// 超长 topic：当前实现复用 ErrMetricKeyEmpty（model 里没有 topic 专用哨兵，
		// 换错误码属契约可见改动且 README 无依据），这里钉住现状。
		_, err = callConsumer(t, d, &rpc.ListConsumerStateReq{Topic: strings.Repeat("t", maxTopicBytes+1)})
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "超长 topic 目前报 ErrMetricKeyEmpty")
		if d.offs.countCalls != 0 {
			t.Fatal("入参未通过却查了库")
		}
	})

	t.Run("越界页只回 total 不发聚合", func(t *testing.T) {
		d := newDeps(t)
		d.offs.rows = rows
		total := int64(2)
		d.offs.total = &total
		reply, err := callConsumer(t, d, &rpc.ListConsumerStateReq{Pn: 3, Ps: 10})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != total || len(reply.Rows) != 0 {
			t.Fatalf("越界页: %+v", reply)
		}
		if d.offs.countCalls != 1 || d.offs.summarizeCalls != 0 {
			t.Fatalf("越界页仍做了聚合（COUNT %d、SUMMARIZE %d）",
				d.offs.countCalls, d.offs.summarizeCalls)
		}
	})

	t.Run("深翻页守卫", func(t *testing.T) {
		d := newDeps(t)
		big := int64(20_000_000)
		d.offs.total = &big
		if _, err := callConsumer(t, d, &rpc.ListConsumerStateReq{Pn: 100_001, Ps: 100}); err != nil {
			t.Fatal(err)
		}
		if d.offs.countCalls != 1 || d.offs.summarizeCalls != 0 {
			t.Fatalf("OFFSET 达 1e7 量级仍做聚合（SUMMARIZE %d 次）", d.offs.summarizeCalls)
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d := newDeps(t)
		d.read.denies = 1
		_, err := callConsumer(t, d, &rpc.ListConsumerStateReq{})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌必须显式失败而不是回空汇总")
		if d.offs.countCalls != 0 {
			t.Fatal("限流后仍查了库")
		}
		d2 := newDeps(t)
		d2.offs.err = errStub
		_, err = callConsumer(t, d2, &rpc.ListConsumerStateReq{})
		if !errors.Is(err, errStub) {
			t.Fatalf("COUNT 故障实得 %v", err)
		}
		d3 := newDeps(t)
		d3.offs.rows = rows
		d3.offs.err = errStub
		d3.offs.total = nil
		if _, err = callConsumer(t, d3, &rpc.ListConsumerStateReq{}); !errors.Is(err, errStub) {
			t.Fatalf("SUMMARIZE 故障实得 %v", err)
		}
	})
}

// --- 死信留档 ---

// TestListDeadLettersFilterAndMaskedPreview 钉住隐私边界与筛选语义：
// payload_preview 在写入侧已脱敏，读侧原样回带、不二次加工，也不提供取原文的入口。
func TestListDeadLettersFilterAndMaskedPreview(t *testing.T) {
	const preview = `{"mid":"***","aid":123,"action":"like"}`

	dlFixture := func(t *testing.T) *deps {
		t.Helper()
		d := newDeps(t)
		d.dls.rows = []*model.DeadLetter{{
			ID: 9, EventID: "", EventType: "behavior.play", Topic: "behavior.play.v1",
			PayloadDigest: "sha256:abc", PayloadPreview: preview, Reason: "unsupported_event_type",
			State: model.DeadLetterStateOpen, Ctime: 555,
		}}
		return d
	}

	t.Run("过滤条件同进 COUNT 与 LIST", func(t *testing.T) {
		d := dlFixture(t)
		// total 必须覆盖 pn=4/ps=5 的 offset=15，否则 logic 按 README「越界页只回 total、
		// 不发查询 SQL」短路，List 不会被调用，「同一份过滤条件同进两条 SQL」就无从观测。
		total := int64(64)
		d.dls.total = &total
		reply, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{
			Topic: " behavior.play.v1 ", State: " open ", Since: 1234, Pn: 4, Ps: 5})
		if err != nil {
			t.Fatal(err)
		}
		want := model.DeadLetterFilter{Topic: "behavior.play.v1", State: model.DeadLetterStateOpen,
			Since: 1234, Offset: 15, Limit: 5}
		if len(d.dls.seen) != 2 || d.dls.seen[0] != want || d.dls.seen[1] != want {
			t.Fatalf("下传条件=%v，两次都应为 %+v", d.dls.seen, want)
		}
		if reply.Total != total {
			t.Fatalf("total=%d，应等于 COUNT 回带的 %d", reply.Total, total)
		}
		if len(reply.Items) != 1 {
			t.Fatalf("行数=%d", len(reply.Items))
		}
		it := reply.Items[0]
		if it.PayloadPreview != preview {
			t.Fatalf("payload_preview 被读侧改写了: %q", it.PayloadPreview)
		}
		if it.EventId != "" {
			t.Fatalf("信封不可解析时 event_id 就该是空串，实得 %q", it.EventId)
		}
		if it.Id != 9 || it.State != model.DeadLetterStateOpen || it.Ctime != 555 ||
			it.PayloadDigest != "sha256:abc" || it.Reason != "unsupported_event_type" ||
			it.Topic != "behavior.play.v1" || it.EventType != "behavior.play" {
			t.Fatalf("留档字段未逐列回带: %+v", it)
		}
		// 响应里根本没有 payload 原文字段（契约只有 payload_preview）——这条是结构性的，
		// 用 preview 之外的明文标识符反查一遍，确保没有别处偷偷带出。
		if strings.Contains(fmt.Sprintf("%+v", reply), "10.0.0.5") {
			t.Fatal("响应里出现了未脱敏的网络标识")
		}
	})

	t.Run("state 白名单拒绝而不是当不限", func(t *testing.T) {
		d := dlFixture(t)
		callsBefore := d.dls.listCalls
		for _, s := range []string{"bogus", "OPEN", "replay", "已处理", "open ed"} {
			_, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{State: s})
			requireErrIs(t, err, model.ErrInvalidDeadLetterState, fmt.Sprintf("state=%q", s))
		}
		for _, s := range []string{"", "open", "replayed", "ignored", " open "} {
			if _, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{State: s}); err != nil {
				t.Fatalf("state=%q 应合法（空 = 不限，空白先裁剪）: %v", s, err)
			}
		}
		// 5 次非法 + 5 次合法：非法那 5 次一次 SQL 都不该发（合法 5 次各发 COUNT+LIST 两条，
		// 记在同一个 listCalls 上，所以总数恰为 10）。
		if got := d.dls.listCalls - callsBefore; got != 10 {
			t.Fatalf("非法 state 仍查了库：listCalls 增量 %d，期望 10", got)
		}
	})

	t.Run("since 负值与 ps 越界拒绝", func(t *testing.T) {
		d := dlFixture(t)
		_, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{Since: -1})
		requireErrIs(t, err, model.ErrInvalidDeadLetterState, "since 不能为负")
		_, err = callDeadLetters(t, d, &rpc.ListDeadLettersReq{
			Ps: d.ctx.Config.Spm.MaxPageSize + 1})
		requireErrIs(t, err, model.ErrPsTooLarge, "ps 越界必须拒绝而不是 clamp")
		_, err = callDeadLetters(t, d, &rpc.ListDeadLettersReq{Topic: strings.Repeat("t", maxTopicBytes+1)})
		requireErrIs(t, err, model.ErrMetricKeyEmpty, "超长 topic 目前报 ErrMetricKeyEmpty")
		if d.dls.listCalls != 0 {
			t.Fatal("入参未通过却查了库")
		}
	})

	t.Run("越界页与深翻页只回 total", func(t *testing.T) {
		d := dlFixture(t)
		total := int64(1)
		d.dls.total = &total
		d.dls.listCalls = 0
		reply, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{Pn: 2, Ps: 10})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Total != total || len(reply.Items) != 0 || d.dls.listCalls != 1 {
			t.Fatalf("越界页仍查了列表（%d 次查询）: %+v", d.dls.listCalls, reply)
		}
		big := int64(20_000_000)
		d.dls.total = &big
		d.dls.listCalls = 0
		if _, err = callDeadLetters(t, d, &rpc.ListDeadLettersReq{Pn: 100_001, Ps: 100}); err != nil {
			t.Fatal(err)
		}
		if d.dls.listCalls != 1 {
			t.Fatalf("OFFSET 达 1e7 量级仍查列表（%d 次）", d.dls.listCalls)
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d := dlFixture(t)
		d.read.denies = 1
		_, err := callDeadLetters(t, d, &rpc.ListDeadLettersReq{})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌必须显式失败而不是回空清单")
		if d.dls.listCalls != 0 {
			t.Fatal("限流后仍查了库")
		}
		d2 := dlFixture(t)
		d2.dls.err = errStub
		if _, err = callDeadLetters(t, d2, &rpc.ListDeadLettersReq{}); !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
	})
}

// --- 用户兴趣画像 ---

// TestGetUserInterestBoundsAndStale 钉住 README「契约缺口」第 1 条（画像表没有
// metric_key，version=0 无从解析，只能显式给定）与隐私边界（只回受控兴趣键）。
func TestGetUserInterestBoundsAndStale(t *testing.T) {
	t.Run("必填与上限", func(t *testing.T) {
		d := newDeps(t)
		cases := []struct {
			why  string
			in   *rpc.GetUserInterestReq
			want error
		}{
			{"mid=0", &rpc.GetUserInterestReq{}, model.ErrInvalidMid},
			{"mid 为负", &rpc.GetUserInterestReq{Mid: -1, MetricVersion: 1}, model.ErrInvalidMid},
			{"version=0 无从解析 ACTIVE 指针", &rpc.GetUserInterestReq{Mid: 101},
				model.ErrMetricVersionRequired},
			{"version 为负", &rpc.GetUserInterestReq{Mid: 101, MetricVersion: -1},
				model.ErrMetricVersionRequired},
			{"top_n 超上限", &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1,
				TopN: d.ctx.Config.Spm.MaxInterestTopN + 1}, model.ErrTopNTooLarge},
		}
		for _, tc := range cases {
			_, err := callInterest(t, d, tc.in)
			requireErrIs(t, err, tc.want, tc.why)
		}
		if d.ints.listTopCalls != 0 {
			t.Fatalf("入参未通过却查了画像表 %d 次", d.ints.listTopCalls)
		}
	})

	t.Run("top_n 默认取配置值并原样下传", func(t *testing.T) {
		d := newDeps(t)
		for _, tc := range []struct{ in, want int32 }{{0, d.ctx.Config.Spm.InterestTopN},
			{-5, d.ctx.Config.Spm.InterestTopN}, {100, 100}} {
			d.ints.listTopCalls = 0
			d.ints.seenTopN = nil
			if _, err := callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101,
				MetricVersion: 1, TopN: tc.in}); err != nil {
				t.Fatal(err)
			}
			if d.ints.seenTopN[0] != tc.want {
				t.Fatalf("top_n=%d 下传成 %d，期望 %d", tc.in, d.ints.seenTopN[0], tc.want)
			}
		}
	})

	t.Run("脏兴趣键丢弃，只回受控键", func(t *testing.T) {
		d := newDeps(t)
		fresh := nowForTest()
		d.ints.rows = []*model.UserInterest{
			{InterestKey: "zone:1009", Weight: 0.4, SampleCount: 40, EventTime: fresh},
			{InterestKey: "tag:88", Weight: 0.3, SampleCount: 30, EventTime: fresh},
			{InterestKey: "catalog:7", Weight: 0.2, SampleCount: 20, EventTime: fresh - 60},
			{InterestKey: "up:500", Weight: 0.1, SampleCount: 10, EventTime: fresh - 60},
			// 以下都是写侧本已拒绝、库里不该出现但必须被读侧兜住的自由文本。
			{InterestKey: "免费看电影", Weight: 0.5, SampleCount: 1, EventTime: fresh},
			{InterestKey: "device:abc", Weight: 0.5, SampleCount: 1, EventTime: fresh},
			{InterestKey: "10.0.0.5", Weight: 0.5, SampleCount: 1, EventTime: fresh},
			{InterestKey: "zone:01", Weight: 0.5, SampleCount: 1, EventTime: fresh},
			{InterestKey: "author:5", Weight: 0.5, SampleCount: 1, EventTime: fresh},
			{InterestKey: "", Weight: 0.5, SampleCount: 1, EventTime: fresh},
		}
		reply, err := callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 4})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Interests) != 4 {
			t.Fatalf("只该回 4 条受控键，实得 %+v", reply.Interests)
		}
		joined := fmt.Sprintf("%+v", reply)
		for _, leak := range []string{"免费看电影", "device:", "10.0.0.5", "zone:01", "author:"} {
			if strings.Contains(joined, leak) {
				t.Fatalf("响应里出现了 %q：自由文本进响应就是行为明细外泄", leak)
			}
		}
		for _, it := range reply.Interests {
			if !model.ValidInterestKey(it.InterestKey) {
				t.Fatalf("回带了非法兴趣键 %q", it.InterestKey)
			}
		}
		if reply.MetricVersion != 4 {
			t.Fatalf("版本回显: %+v", reply)
		}
		if reply.Stale {
			t.Fatal("刚推进过的画像不该标 stale")
		}
	})

	t.Run("空画像与过期画像都置 stale，让召回侧走冷启动", func(t *testing.T) {
		d := newDeps(t)
		reply, err := callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Interests) != 0 || !reply.Stale {
			t.Fatalf("「没有这个版本的重算结果」必须与「画像很旧」同动作: %+v", reply)
		}
		// stale=false + 空列表会被读成「这个人确实没有任何兴趣」。

		fresh := nowForTest()
		// stale 的判定基准是「组内最大 event_time」，且比较是严格大于
		// Spm.InterestStaleAfterSeconds（logic 自取 time.Now()，本包不能注入时钟），
		// 所以阈值两侧各留 300 秒余量：恰好等于阈值那个点不在可稳定钉住的范围内。
		d.ints.rows = []*model.UserInterest{
			{InterestKey: "zone:1", Weight: 0.5, EventTime: fresh - dayUnix - 300},
			{InterestKey: "tag:2", Weight: 0.5, EventTime: fresh - 2*dayUnix},
		}
		reply, err = callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if !reply.Stale {
			t.Fatalf("最大 event_time 已越过阈值却没标 stale: %+v", reply)
		}
		if len(reply.Interests) != 2 {
			t.Fatalf("过期画像仍要原样回带内容（调用方自己决定用不用）: %+v", reply.Interests)
		}

		// 阈值之内：同一个夹具把最大 event_time 挪回「刚刚推进」，stale 必须翻回 false。
		d.ints.rows = []*model.UserInterest{
			{InterestKey: "zone:1", Weight: 0.5, EventTime: fresh},
			{InterestKey: "tag:2", Weight: 0.5, EventTime: fresh - 2*dayUnix},
		}
		reply, err = callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if reply.Stale {
			t.Fatalf("最大 event_time 就是刚才，却仍标 stale（召回侧会永久走冷启动）: %+v", reply)
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d := newDeps(t)
		d.read.denies = 1
		_, err := callInterest(t, d, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1})
		requireErrIs(t, err, model.ErrRateLimited, "无令牌")
		if d.ints.listTopCalls != 0 {
			t.Fatal("限流后仍查了画像")
		}
		d2 := newDeps(t)
		d2.ints.err = errStub
		_, err = callInterest(t, d2, &rpc.GetUserInterestReq{Mid: 101, MetricVersion: 1})
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
	})
}

// --- 留存曲线 ---

// TestGetRetentionRecomputesRate 钉住 README「数据保留策略」与契约缺口：
// 留存点必须按天边界分桶、max_day 收敛到 1..90、rate 由 retained/cohort_size 现算。
func TestGetRetentionRecomputesRate(t *testing.T) {
	setup := func(t *testing.T) *deps {
		t.Helper()
		d := newDeps(t)
		d.rets.rows = []*model.RetentionCohort{
			// rate 列故意写成一个「当时算出来」的旧值：读侧必须重算，不能照抄。
			{DayOffset: 0, CohortSize: 100, Retained: 100, Rate: 0.111, EventTime: nowForTest()},
			{DayOffset: 1, CohortSize: 100, Retained: 45, Rate: 0.9, EventTime: nowForTest()},
			{DayOffset: 2, CohortSize: 0, Retained: 0, Rate: 0.9, EventTime: nowForTest()},
		}
		return d
	}
	valid := func(d *deps) *rpc.GetRetentionReq {
		return &rpc.GetRetentionReq{CohortType: rpc.GetRetentionReq_COHORT_TYPE_FIRST_PLAY_DAY,
			CohortDate: dayStart(-30), MetricVersion: 1, ZoneId: 0}
	}

	t.Run("必填与边界一律显式拒绝", func(t *testing.T) {
		d := setup(t)
		cases := []struct {
			why  string
			mut  func(in *rpc.GetRetentionReq)
			want error
		}{
			{"cohort_type 未指定", func(in *rpc.GetRetentionReq) {
				in.CohortType = rpc.GetRetentionReq_COHORT_TYPE_UNSPECIFIED
			}, model.ErrInvalidCohort},
			{"cohort_type 越界", func(in *rpc.GetRetentionReq) {
				in.CohortType = rpc.GetRetentionReq_CohortType(7)
			}, model.ErrInvalidCohort},
			{"cohort_date 缺失", func(in *rpc.GetRetentionReq) { in.CohortDate = 0 },
				model.ErrInvalidCohort},
			{"cohort_date 为负", func(in *rpc.GetRetentionReq) { in.CohortDate = -1 },
				model.ErrInvalidCohort},
			{"zone_id 为负", func(in *rpc.GetRetentionReq) { in.ZoneId = -1 },
				model.ErrInvalidCohort},
			{"max_day 越界", func(in *rpc.GetRetentionReq) {
				in.MaxDay = d.ctx.Config.Spm.MaxRetentionDay + 1
			}, model.ErrMaxDayTooLarge},
			{"version=0 无从解析 ACTIVE 指针", func(in *rpc.GetRetentionReq) { in.MetricVersion = 0 },
				model.ErrMetricVersionRequired},
			{"version 为负", func(in *rpc.GetRetentionReq) { in.MetricVersion = -1 },
				model.ErrMetricVersionRequired},
		}
		for _, tc := range cases {
			in := valid(d) // 每次新建一份请求：复制 protobuf 消息会连带复制其内部锁
			tc.mut(in)
			_, err := callRetention(t, d, in)
			requireErrIs(t, err, tc.want, tc.why)
		}
		if d.rets.listCurveCalls != 0 {
			t.Fatalf("入参未通过却查了曲线 %d 次", d.rets.listCurveCalls)
		}
	})

	t.Run("max_day 默认取配置上限并原样下传", func(t *testing.T) {
		d := setup(t)
		for _, in := range []int32{0, -3} {
			req := valid(d)
			req.MaxDay = in
			if _, err := callRetention(t, d, req); err != nil {
				t.Fatal(err)
			}
			want := d.ctx.Config.Spm.MaxRetentionDay
			if got := d.rets.seenMaxDay[len(d.rets.seenMaxDay)-1]; got != want {
				t.Fatalf("max_day=%d 下传成 %d，期望默认 %d", in, got, want)
			}
		}
	})

	t.Run("cohort_date 规整到天边界，回显即入参", func(t *testing.T) {
		d := setup(t)
		day := dayStart(-30)
		req := valid(d)
		req.CohortDate = day + 9*hourUnix // 同一天内的任意时刻
		reply, err := callRetention(t, d, req)
		if err != nil {
			t.Fatal(err)
		}
		if d.rets.seenDates[0] != day {
			t.Fatalf("下传给 model 的分桶日未对齐 UTC 零点：%d", d.rets.seenDates[0])
		}
		// 回显必须与下传同一个值：调用方拿它原样传回来复查，才不会再落到另一个桶里。
		if reply.CohortDate != day || reply.CohortDate != d.rets.seenDates[0] {
			t.Fatalf("回显 cohort_date=%d，model 收到 %d", reply.CohortDate, d.rets.seenDates[0])
		}
		if reply.MetricVersion != req.MetricVersion {
			t.Fatalf("版本回显: %+v", reply)
		}
	})

	t.Run("rate 由分子分母现算，不照抄落库比率", func(t *testing.T) {
		d := setup(t)
		reply, err := callRetention(t, d, valid(d))
		if err != nil {
			t.Fatal(err)
		}
		if len(reply.Points) != 3 {
			t.Fatalf("曲线点数=%d", len(reply.Points))
		}
		wantRate := []float64{1.0, 0.45, 0}
		wantSize := []int64{100, 100, 0}
		for i, p := range reply.Points {
			if p.DayOffset != int32(i) {
				t.Fatalf("第 %d 个点的 day_offset=%d", i, p.DayOffset)
			}
			if p.CohortSize != wantSize[i] {
				t.Fatalf("第 %d 个点的 cohort_size=%d，期望 %d", i, p.CohortSize, wantSize[i])
			}
			if p.Rate != wantRate[i] {
				t.Fatalf("第 %d 日 rate=%v，期望 retained/cohort_size=%v（照抄旧比率会得到「人数变了留存率没变」的曲线）",
					i, p.Rate, wantRate[i])
			}
		}
		if reply.Points[0].Rate == 0.111 {
			t.Fatal("第 0 日照抄了落库的旧比率")
		}
		if reply.Points[2].CohortSize != 0 {
			t.Fatalf("cohort_size=0 的行被改写: %+v", reply.Points[2])
		}
	})

	t.Run("限流与库故障", func(t *testing.T) {
		d := setup(t)
		d.read.denies = 1
		_, err := callRetention(t, d, valid(d))
		requireErrIs(t, err, model.ErrRateLimited, "无令牌")
		if d.rets.listCurveCalls != 0 {
			t.Fatal("限流后仍查了曲线")
		}
		d2 := setup(t)
		d2.rets.err = errStub
		_, err = callRetention(t, d2, valid(d2))
		if !errors.Is(err, errStub) {
			t.Fatalf("实得 %v", err)
		}
	})
}
