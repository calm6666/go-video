package logic

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"go-video/services/spm/model"
	"go-video/services/spm/rpc"
)

// 本文件钉住 README「窗口、水位与迟到」对 WriteMetricWindow 的全部承诺：
// 幂等覆盖（绝不累加）、重放回放首次计数、迟到行按水位 + 容忍线拒绝、
// 指标行与水位同事务提交。

func writeReq(points []*rpc.MetricPoint, reqID string, allowLate bool) *rpc.WriteMetricWindowReq {
	return &rpc.WriteMetricWindowReq{
		Points: points, Source: rpc.MetricSource_METRIC_SOURCE_REALTIME,
		RequestId: reqID, AllowLateWrite: allowLate,
	}
}

func callWrite(t *testing.T, d *deps, in *rpc.WriteMetricWindowReq) (*rpc.WriteMetricWindowReply, error) {
	t.Helper()
	return NewWriteMetricWindowLogic(context.Background(), d.ctx).WriteMetricWindow(in)
}

// TestWriteMetricWindowConvergesNeverAccumulates 同一自然键（uniq_metric 六列）重复写回
// 必须收敛到最后一次的值：本表是投影，重放把计数加两遍就是「榜上的数只会变大」。
func TestWriteMetricWindowConvergesNeverAccumulates(t *testing.T) {
	d := newDeps(t)
	key := "play_finish_rate"
	mustActiveDef(t, d, key, 3, "1,2,3")
	start := alignedStart5Min(0)

	first := validPoint(key, 3, model.WindowType5Min, start)
	first.Value, first.Numerator, first.Denominator, first.SampleCount = 0.4, 4, 10, 10
	if _, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{first}, "req-1", false)); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	row := storedWindow(t, d, model.SubjectTypeAid, 101, key, 3, model.WindowType5Min, start)
	if row.MetricValue != 0.4 || row.Numerator != 4 || row.SampleCount != 10 {
		t.Fatalf("首次写入值不对: %+v", row)
	}
	firstClosed := d.db.txRuns

	second := validPoint(key, 3, model.WindowType5Min, start)
	second.Value, second.Numerator, second.Denominator, second.SampleCount = 0.9, 9, 10, 10
	reply, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{second}, "req-2", false))
	if err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}
	if reply.Written != 1 {
		t.Fatalf("written=%d，期望 1（受理并落库的行数，不是 affected）", reply.Written)
	}
	if len(d.db.windows) != 1 {
		t.Fatalf("同自然键产生了 %d 行投影：重复回写在累加而不是覆盖", len(d.db.windows))
	}
	row = storedWindow(t, d, model.SubjectTypeAid, 101, key, 3, model.WindowType5Min, start)
	if row.MetricValue != 0.9 || row.Numerator != 9 || row.Denominator != 10 ||
		row.SampleCount != 10 {
		t.Fatalf("窗口未收敛到最后一次写入: %+v", row)
	}
	if row.WriteReqID != "req-2" {
		t.Fatalf("write_request_id 未更新为本次幂等键: %s", row.WriteReqID)
	}
	if got := d.wins.upsertLate; len(got) != 2 || got[0] || got[1] {
		t.Fatalf("allow_late_write=false 必须透传给 UpsertBatch: %v", got)
	}
	if d.db.txRuns != firstClosed+1 {
		t.Fatalf("事务数=%d，期望 %d", d.db.txRuns, firstClosed+1)
	}
}

// TestRecomputeAndWriteReplayReturnFirstCount 重放判定：六列全部带同一 write_request_id
// 才算「这批已写过」，此时回放首次写入计数、不再发起覆盖写。
func TestRecomputeAndWriteReplayReturnFirstCount(t *testing.T) {
	d := newDeps(t)
	key := "hot_score"
	mustActiveDef(t, d, key, 1, "1")
	start := alignedStart5Min(0)
	pt := validPoint(key, 1, model.WindowType5Min, start)

	if _, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{pt}, "req-replay", false)); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	upsertsAfterFirst := d.wins.upsertCalls

	again, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{pt}, "req-replay", false))
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if again.Written != 1 || again.Rejected != 0 {
		t.Fatalf("重放应回放首次写入计数：written=%d rejected=%d", again.Written, again.Rejected)
	}
	if d.wins.upsertCalls != upsertsAfterFirst {
		t.Fatalf("重放又发起了一次覆盖写（%d 次），幂等键没起作用", d.wins.upsertCalls)
	}
	if d.water.advanceCalls != 1 {
		t.Fatalf("重放又推进了一次水位（%d 次）", d.water.advanceCalls)
	}
	if d.wins.naturalCalls != 2 {
		t.Fatalf("重放判定应走 ListByNaturalKeys（write_request_id 无索引），实得 %d 次",
			d.wins.naturalCalls)
	}

	// 部分命中（批里混进了没写过的行）不是重放：覆盖写本身幂等，重写只会继续收敛。
	other := validPoint(key, 1, model.WindowType5Min, start)
	other.SubjectId = 202
	if _, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{pt, other}, "req-replay", false)); err != nil {
		t.Fatalf("部分命中的批次失败: %v", err)
	}
	if d.wins.upsertCalls != upsertsAfterFirst+1 {
		t.Fatalf("部分命中应继续覆盖写，实得 upsert 次数 %d", d.wins.upsertCalls)
	}
	// 库里已有的行换了幂等键再写：不是重放，值必须被新值覆盖。
	changed := validPoint(key, 1, model.WindowType5Min, start)
	changed.Value, changed.SampleCount = 0.11, 3
	if _, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{changed}, "req-replay-3", false)); err != nil {
		t.Fatal(err)
	}
	if row := storedWindow(t, d, model.SubjectTypeAid, 101, key, 1, model.WindowType5Min, start); row.MetricValue != 0.11 || row.SampleCount != 3 {
		t.Fatalf("换 request_id 重写未收敛: %+v", row)
	}
}

// TestLateWindowToleranceBoundaryIsStrict lateWindow 的边界：容忍线本身仍算「可写」。
func TestLateWindowToleranceBoundaryIsStrict(t *testing.T) {
	wm := &model.WindowWatermark{LastClosedStart: 10_000, WindowType: model.WindowType5Min}
	if lateWindow(wm, tolerSecs, 10_000-tolerSecs) {
		t.Fatal("start == 水位 - 容忍线 被判迟到，等于容忍线白配")
	}
	if !lateWindow(wm, tolerSecs, 10_000-tolerSecs-1) {
		t.Fatal("越过容忍线 1 秒仍放行，已闭合窗口会被无限改写")
	}
	if lateWindow(nil, tolerSecs, 0) {
		t.Fatal("水位还不存在（口径首日）时没有任何已闭合窗口能被改写坏，不该判迟到")
	}
	if lateWindow(&model.WindowWatermark{LastClosedStart: 0}, tolerSecs, 100) {
		t.Fatal("水位为 0 等同「未建立」，不该判迟到")
	}
}

// TestWriteMetricWindowRejectsLateRowBeyondTolerance 迟到判定基准是水位：
// 早于「本组水位 - LateToleranceSeconds」的行计入 rejected / rejected_keys，只能显式回填。
func TestWriteMetricWindowRejectsLateRowBeyondTolerance(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	wmStart := alignedStart5Min(0)
	seedWatermark(d, model.SubjectTypeAid, key, 1, model.WindowType5Min, wmStart, wmStart+60)

	fresh := validPoint(key, 1, model.WindowType5Min, wmStart)
	tooLate := validPoint(key, 1, model.WindowType5Min, wmStart-minute5)
	tooLate.SubjectId = 303

	reply, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{fresh, tooLate}, "req-late", false))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if reply.Written != 1 || reply.Rejected != 1 {
		t.Fatalf("written=%d rejected=%d，期望 1/1", reply.Written, reply.Rejected)
	}
	wantKey := fmt.Sprintf("%d:%d:%s", model.SubjectTypeAid, 303, key)
	if len(reply.RejectedKeys) != 1 || reply.RejectedKeys[0] != wantKey {
		t.Fatalf("rejected_keys=%v，期望 [%q]（契约 <subject_type>:<subject_id>:<metric_key>）",
			reply.RejectedKeys, wantKey)
	}
	lateKey := winKey(model.SubjectTypeAid, 303, key, 1, model.WindowType5Min, wmStart-minute5)
	if _, ok := d.db.windows[lateKey]; ok {
		t.Fatal("越线迟到行仍被写进投影表")
	}
	if d.db.txRuns != 1 {
		t.Fatalf("事务数=%d，期望 1（只有 1 行受理）", d.db.txRuns)
	}
	// 已闭合窗口的值不因被拒的迟到行而改变。
	if row := storedWindow(t, d, model.SubjectTypeAid, 101, key, 1, model.WindowType5Min, wmStart); row.SampleCount != 10 || row.MetricValue != 0.8 {
		t.Fatalf("已闭合窗口被越线迟到行改写: %+v", row)
	}

	// 显式回填同一条迟到数据：允许写，且不查水位（调用方已承认在改历史）。
	before := d.water.findCalls
	rep2, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{tooLate}, "req-late-2", true))
	if err != nil {
		t.Fatalf("显式回填失败: %v", err)
	}
	if rep2.Written != 1 || rep2.Rejected != 0 {
		t.Fatalf("回填应受理: %+v", rep2)
	}
	if d.water.findCalls != before {
		t.Fatalf("allow_late_write=true 仍去查水位（%d 次）", d.water.findCalls-before)
	}
	if _, ok := d.db.windows[lateKey]; !ok {
		t.Fatal("显式回填的迟到行没落库")
	}
}

// TestWriteMetricWindowLateWriteCannotMoveWatermarkBack 已闭合窗口的水位不能被迟到回填拉回：
// Advance 单调拒绝（applied=false）不是错误，指标行仍按自然键覆盖写生效。
func TestWriteMetricWindowLateWriteCannotMoveWatermarkBack(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	wmStart := alignedStart5Min(0)
	seedWatermark(d, model.SubjectTypeAid, key, 1, model.WindowType5Min, wmStart, wmStart+60)

	old := validPoint(key, 1, model.WindowType5Min, wmStart-2*minute5)
	old.Value, old.SampleCount = 5, 5
	reply, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{old}, "req-backfill", true))
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if reply.Written != 1 || reply.Rejected != 0 {
		t.Fatalf("回填应受理：%+v", reply)
	}
	wm := d.db.water[wmKey(model.SubjectTypeAid, key, 1, model.WindowType5Min)]
	if wm.LastClosedStart != wmStart {
		t.Fatalf("水位被迟到回填拉回：%d -> %d", wmStart, wm.LastClosedStart)
	}
	if len(wm.UpdateRequestID) == 0 || wm.UpdateRequestID != "seed" {
		t.Fatalf("被拒的回退把水位的幂等键也改了: %s", wm.UpdateRequestID)
	}
	if n := len(d.water.advanceApplied); n == 0 || d.water.advanceApplied[n-1] {
		t.Fatal("回退推进必须记为 applied=false（幂等拒绝，不是错误）")
	}
	if row := storedWindow(t, d, model.SubjectTypeAid, 101, key, 1, model.WindowType5Min,
		wmStart-2*minute5); row.SampleCount != 5 {
		t.Fatalf("指标行未覆盖写：%+v", row)
	}
}

// TestWriteMetricWindowCommitsRowAndWatermarkTogether 指标行与它描述的水位必须同事务提交，
// 否则 window_start=0 会解析到一个还没有数据的窗口。
func TestWriteMetricWindowCommitsRowAndWatermarkTogether(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	start := alignedStart5Min(0)

	if _, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{
		validPoint(key, 1, model.WindowType5Min, start),
	}, "req-tx", false)); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if d.db.txRuns != 1 {
		t.Fatalf("事务次数=%d，期望 1", d.db.txRuns)
	}
	if d.wins.outsideTx != 0 {
		t.Fatalf("有 %d 次覆盖写没走 WithSession", d.wins.outsideTx)
	}
	if d.water.advanceCalls != 1 {
		t.Fatalf("水位推进=%d 次", d.water.advanceCalls)
	}

	// 覆盖写失败 -> 整笔事务回滚，水位不得单独前进。
	d2 := newDeps(t)
	mustActiveDef(t, d2, key, 1, "1")
	d2.wins.upsertErr = errors.New("boom")
	if _, err := callWrite(t, d2, writeReq([]*rpc.MetricPoint{
		validPoint(key, 1, model.WindowType5Min, start),
	}, "req-tx-2", false)); err == nil {
		t.Fatal("覆盖写失败必须整单报错")
	}
	if len(d2.db.windows) != 0 {
		t.Fatal("写失败后不该留下指标行")
	}
	if len(d2.db.water) != 0 {
		t.Fatal("指标行写失败后水位却单独前进了（会出现空窗口读）")
	}

	// 水位推进失败同样整单失败：只写指标不推水位，榜会永远停在上一个窗口。
	d3 := newDeps(t)
	mustActiveDef(t, d3, key, 1, "1")
	d3.water.advErr = errStub
	if _, err := callWrite(t, d3, writeReq([]*rpc.MetricPoint{
		validPoint(key, 1, model.WindowType5Min, start),
	}, "req-tx-3", false)); !errors.Is(err, errStub) {
		t.Fatalf("水位失败必须原样上抛，实得 %v", err)
	}
	if len(d3.db.windows) != 0 {
		t.Fatalf("水位失败后指标行未回滚：%+v", d3.db.windows)
	}
}

// TestClosedWindowWatermarks 水位归并：按 (主体类型, 口径, 版本, 粒度) 分组，
// last_closed_start 取组内最大、rows_written 只数落在该边界上的行，TOTAL 不建水位，
// 也不生成跨主体汇总位（subject_type=0）。
func TestClosedWindowWatermarks(t *testing.T) {
	rows := []*model.MetricWindow{
		{SubjectType: model.SubjectTypeAid, MetricKey: "a", MetricVersion: 1,
			WindowType: model.WindowType5Min, WindowStart: 300, EventTime: 301},
		{SubjectType: model.SubjectTypeAid, MetricKey: "a", MetricVersion: 1,
			WindowType: model.WindowType5Min, WindowStart: 600, EventTime: 601},
		{SubjectType: model.SubjectTypeAid, MetricKey: "a", MetricVersion: 1,
			WindowType: model.WindowType5Min, WindowStart: 600, EventTime: 500},
		{SubjectType: model.SubjectTypeAid, MetricKey: "a", MetricVersion: 2,
			WindowType: model.WindowType5Min, WindowStart: 900, EventTime: 901},
		{SubjectType: model.SubjectTypeZone, MetricKey: "a", MetricVersion: 1,
			WindowType: model.WindowType5Min, WindowStart: 900, EventTime: 901},
		{SubjectType: model.SubjectTypeAid, MetricKey: "a", MetricVersion: 1,
			WindowType: model.WindowTypeTotal, WindowStart: 0, EventTime: 999},
	}
	got := closedWindowWatermarks(rows, "req-x")
	if len(got) != 3 {
		t.Fatalf("水位组数=%d，期望 3（TOTAL 不建水位；主体与口径版本都不跨组）：%+v", len(got), got)
	}
	for _, wm := range got {
		if wm.SubjectType == model.SubjectTypeUnspecified {
			t.Fatal("写回批生成了跨主体汇总水位：这批是否覆盖全部主体只有聚合器知道")
		}
		if wm.UpdateRequestID != "req-x" {
			t.Fatalf("水位没带本次幂等键：%+v", wm)
		}
		if wm.WindowType == model.WindowTypeTotal {
			t.Fatal("TOTAL 被建了水位")
		}
	}
	aidV1 := got[0]
	if aidV1.MetricVersion != 1 || aidV1.LastClosedStart != 600 || aidV1.RowsWritten != 2 ||
		aidV1.LastEventTime != 601 {
		t.Fatalf("AID/v1 组归并错：%+v（rows_written 只应数落在最大边界上的行）", aidV1)
	}
	if got[1].MetricVersion != 2 || got[1].LastClosedStart != 900 {
		t.Fatalf("v2 与 v1 混组：%+v", got[1])
	}
	if got[2].SubjectType != model.SubjectTypeZone || got[2].RowsWritten != 1 {
		t.Fatalf("ZONE 组归并错：%+v", got[2])
	}
	if len(closedWindowWatermarks(nil, "req")) != 0 {
		t.Fatal("空批不该产水位")
	}
}

// TestWriteMetricWindowRejectsWholeRequestOnUnsafeArgs 整单级拒绝：非计算链路来源、
// 超批量上限、缺幂等键、限流，都不该退化成「部分写入」。
func TestWriteMetricWindowRejectsWholeRequestOnUnsafeArgs(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	start := alignedStart5Min(0)
	pt := validPoint(key, 1, model.WindowType5Min, start)

	t.Run("非计算链路来源", func(t *testing.T) {
		for _, src := range []rpc.MetricSource{rpc.MetricSource_METRIC_SOURCE_UNSPECIFIED,
			rpc.MetricSource(9), rpc.MetricSource(-1)} {
			_, err := callWrite(t, d, &rpc.WriteMetricWindowReq{
				Points: []*rpc.MetricPoint{pt}, Source: src, RequestId: "req-src"})
			requireErrIs(t, err, model.ErrInvalidMetricSource, fmt.Sprintf("source=%d", src))
		}
	})
	t.Run("批量超上限", func(t *testing.T) {
		pts := make([]*rpc.MetricPoint, 0, d.ctx.Config.Spm.MaxWritePoints+1)
		for i := int32(0); i <= d.ctx.Config.Spm.MaxWritePoints; i++ {
			p := validPoint(key, 1, model.WindowType5Min, start)
			p.SubjectId = int64(1000 + i)
			pts = append(pts, p)
		}
		_, err := callWrite(t, d, writeReq(pts, "req-big", false))
		requireErrIs(t, err, model.ErrTooManyPoints, "points>MaxWritePoints")
	})
	t.Run("缺幂等键", func(t *testing.T) {
		_, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{pt}, "   ", false))
		requireErrIs(t, err, model.ErrRequestIdRequired, "request_id 空白")
	})
	t.Run("写侧限流", func(t *testing.T) {
		d.write.denies = 1
		_, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{pt}, "req-rl", false))
		requireErrIs(t, err, model.ErrRateLimited, "写侧无令牌")
	})
	if d.wins.upsertCalls != 0 || d.db.txRuns != 0 {
		t.Fatalf("以上拒绝路径不该动库：upsert=%d tx=%d", d.wins.upsertCalls, d.db.txRuns)
	}
	if len(d.db.windows) != 0 {
		t.Fatalf("以上拒绝路径留下了 %d 行投影", len(d.db.windows))
	}
}

// TestWriteMetricWindowPerRowRejections 逐行拒绝：单行的问题只拒这一行，
// 整批其它主体仍要落库（一条坏数据不能拖垮同批几百个主体）。
func TestWriteMetricWindowPerRowRejections(t *testing.T) {
	d := newDeps(t)
	const active = "act_key"
	mustActiveDef(t, d, active, 2, "1,3")
	if _, err := d.defs.InsertIfAbsent(context.Background(), &model.MetricDefinition{
		MetricKey: "draft_key", MetricVersion: 1, Name: "草稿", Formula: "f", Unit: "count",
		SupportedWindows: "1", SourceEventTypes: "behavior.play",
		State: model.DefinitionStateDraft, CreatedBy: "t", RequestID: "r-draft",
	}); err != nil {
		t.Fatal(err)
	}
	start := alignedStart5Min(0)

	cases := []struct {
		name   string
		mutate func(*rpc.MetricPoint)
	}{
		{"口径未登记", func(p *rpc.MetricPoint) { p.MetricKey = "ghost" }},
		{"口径非 ACTIVE", func(p *rpc.MetricPoint) { p.MetricKey = "draft_key" }},
		{"写侧不接受 version=0", func(p *rpc.MetricPoint) { p.MetricVersion = 0 }},
		{"粒度未登记", func(p *rpc.MetricPoint) { p.WindowType = rpc.WindowType_WINDOW_TYPE_HOUR }},
		{"粒度 UNSPECIFIED", func(p *rpc.MetricPoint) { p.WindowType = rpc.WindowType_WINDOW_TYPE_UNSPECIFIED }},
		{"分子为负", func(p *rpc.MetricPoint) { p.Numerator = -1 }},
		{"样本为负", func(p *rpc.MetricPoint) { p.SampleCount = -3 }},
		{"分母为负", func(p *rpc.MetricPoint) { p.Denominator = -1 }},
		{"缺 event_time", func(p *rpc.MetricPoint) { p.EventTime = 0 }},
		{"主体未指定", func(p *rpc.MetricPoint) { p.SubjectType = rpc.SubjectType_SUBJECT_TYPE_UNSPECIFIED }},
		{"主体主键非正", func(p *rpc.MetricPoint) { p.SubjectId = 0 }},
		{"指标键为空", func(p *rpc.MetricPoint) { p.MetricKey = "  " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := validPoint(active, 2, model.WindowType5Min, start)
			tc.mutate(bad)
			good := validPoint(active, 2, model.WindowType5Min, start)
			good.SubjectId = 777

			reply, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{bad, good}, "req-"+tc.name, false))
			if err != nil {
				t.Fatalf("单行问题不应整单失败: %v", err)
			}
			if reply.Written != 1 || reply.Rejected != 1 {
				t.Fatalf("written=%d rejected=%d，期望 1/1", reply.Written, reply.Rejected)
			}
			if len(reply.RejectedKeys) != 1 {
				t.Fatalf("rejected_keys=%v", reply.RejectedKeys)
			}
			if _, ok := d.db.windows[winKey(model.SubjectTypeAid, 777, active, 2,
				model.WindowType5Min, start)]; !ok {
				t.Fatal("同批的合法行被坏数据拖垮，没有落库")
			}
			for k := range d.db.windows {
				if k == winKey(model.SubjectTypeAid, 777, active, 2, model.WindowType5Min, start) {
					continue
				}
				t.Fatalf("被拒行仍落库：%s", k)
			}
			delete(d.db.windows, winKey(model.SubjectTypeAid, 777, active, 2, model.WindowType5Min, start))
		})
	}

	t.Run("批内重复自然键只拒后来那条", func(t *testing.T) {
		a := validPoint(active, 2, model.WindowType5Min, start)
		b := validPoint(active, 2, model.WindowType5Min, start+120) // 规整后与 a 同窗口
		reply, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{a, b}, "req-dup", false))
		if err != nil {
			t.Fatal(err)
		}
		if reply.Written != 1 || reply.Rejected != 1 {
			t.Fatalf("written=%d rejected=%d，期望 1/1", reply.Written, reply.Rejected)
		}
		if len(d.db.windows) != 1 {
			t.Fatalf("批内重复产生了 %d 行", len(d.db.windows))
		}
	})
}

// TestWriteMetricWindowEmptyAndTotalWindow 空批次与 TOTAL 粒度：
// 空批次不写库不推水位；TOTAL 恒为 window_start=0 且不建水位。
func TestWriteMetricWindowEmptyAndTotalWindow(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1,5")

	empty, err := callWrite(t, d, writeReq(nil, "req-empty", false))
	if err != nil {
		t.Fatalf("空批次应成功返回 0/0: %v", err)
	}
	if empty.Written != 0 || empty.Rejected != 0 || len(empty.RejectedKeys) != 0 {
		t.Fatalf("空批次响应 %+v", empty)
	}
	if d.wins.upsertCalls != 0 || d.water.advanceCalls != 0 || d.db.txRuns != 0 {
		t.Fatal("空批次动了库或水位")
	}

	total := validPoint(key, 1, model.WindowTypeTotal, 0)
	rep, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{total}, "req-total", false))
	if err != nil {
		t.Fatal(err)
	}
	if rep.Written != 1 {
		t.Fatalf("TOTAL 行未受理: %+v", rep)
	}
	if len(d.db.windows) != 1 {
		t.Fatalf("投影行数=%d", len(d.db.windows))
	}
	if row := storedWindow(t, d, model.SubjectTypeAid, 101, key, 1, model.WindowTypeTotal, 0); row.WindowStart != 0 {
		t.Fatalf("TOTAL 的 window_start 必须恒为 0：%+v", row)
	}
	if d.water.advanceCalls != 0 || len(d.db.water) != 0 {
		t.Fatal("TOTAL 不该建水位（model.Advance 对它返回 ErrInvalidWindow）")
	}
}

// TestWriteMetricWindowWatermarkLookupFailureFailsClosed 水位查询真失败必须整单报错，
// 而不是把这一行当成「没有水位」放行。
func TestWriteMetricWindowWatermarkLookupFailureFailsClosed(t *testing.T) {
	d := newDeps(t)
	key := "play_cnt"
	mustActiveDef(t, d, key, 1, "1")
	d.water.findErr = errStub
	_, err := callWrite(t, d, writeReq([]*rpc.MetricPoint{
		validPoint(key, 1, model.WindowType5Min, alignedStart5Min(0)),
	}, "req-wm-err", false))
	if !errors.Is(err, errStub) {
		t.Fatalf("水位查询失败必须原样上抛，实得 %v", err)
	}
	if len(d.db.windows) != 0 || d.wins.upsertCalls != 0 {
		t.Fatal("读不到水位时不该写任何行（fail closed）")
	}
}
