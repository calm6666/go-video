// 本文件覆盖读侧「健康与事件位点」的两条入口：GetStreamHealth（排障 / 运营判健康）与
// ListStreamEvents（live-room、live-media 的补偿对账游标）。
//
// 这两条读路径各自有一个别处测不到的核心契约：
//
//	GetStreamHealth —— 「窗口是夹取语义而不是校验语义」：客户端只能收窄聚合窗口，
//	  放宽必须被夹回配置值（否则一个 window_seconds=10 年的入参就能把健康检查变成全表扫描）；
//	  以及「NO_DATA 是视图不是事实」：读路径判定无数据，绝不能把最后一次真实判定抹进库里；
//	ListStreamEvents —— 「游标必须诚实」：max_seq 与 has_more 是消费方判断「我追平了吗」
//	  的唯一依据，谎报 has_more 让对账任务永远空转，谎报 max_seq 让它永久漏事件。
//
// 时间敏感用例的口径：窗口边界值（10 秒 / 30 秒）本身不留用例——nowUnix() 在铺数据与调用
// 之间会跳秒，边界断言必然偶发红。所有采样点的偏移都按「窗口内留 >=2 秒余量、窗口外至少
// 早 4 秒」排布，钉的是「在不在窗口内」这个分支，而不是边界那一秒的归属。
//
// 副作用口径沿用 streamquery_test.go：读路径九张表行数、事务开合、以及**具体写方法是否被调用**
// 都要钉住（健康读路径最容易偷偷补一刀 MarkStopped / 投影写回）。

package logic

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/internal/repository"
	"go-video/services/live-ingest/internal/svc"
	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// getStreamHealth / listEvents 走真 logic，参数缺省形态与客户端省略字段时一致。
func getStreamHealth(e *testEnv, mut ...func(*rpc.GetStreamHealthReq)) (*rpc.GetStreamHealthReply, error) {
	in := &rpc.GetStreamHealthReq{}
	for _, m := range mut {
		m(in)
	}
	return NewGetStreamHealthLogic(bg(), e.svc).GetStreamHealth(in)
}

func listEvents(e *testEnv, mut ...func(*rpc.ListStreamEventsReq)) (*rpc.ListStreamEventsReply, error) {
	in := &rpc.ListStreamEventsReq{}
	for _, m := range mut {
		m(in)
	}
	return NewListStreamEventsLogic(bg(), e.svc).ListStreamEvents(in)
}

// seedStreamHealth 把流铺成「已经上报过健康」的形态（reportedAt 是 health_reported_at）。
//
// 说明：真 Insert 不落 health_state / health_reported_at 两列（它们由 ApplyHealthProjection
// 写），本函数借 seedStream 直接铺行，钉的是读侧对「库里既有健康列」的投影与超时判定，
// 不主张任何写入语义。
func (e *testEnv) seedStreamHealth(t *testing.T, id string, state, healthState int32, reportedAt int64) *model.Stream {
	t.Helper()
	return e.seedStream(t, &model.Stream{
		StreamID: id, RoomID: 31, AnchorMid: 1001, Protocol: 1, NodeID: "node-health",
		State: state, Seq: 1, LastHeartbeatAt: reportedAt,
		HealthState: healthState, HealthReportedAt: reportedAt,
	})
}

// seedSample 铺一个 occurred_at = now-ago 的健康采样点，回该点落库的绝对时间。
//
// OccurredAt 必须显式给：替身的 Insert 在 OccurredAt==0 时拿 Ctime 兜底，而 Ctime 缺省是
// 1700000000 —— 落在任何窗口之外，种子会静默不参与聚合，用例就成了自证。
// report_id 按当前行数派生而不是按 ago：同一秒铺多个点是 limit 夹取用例的正常形态。
func (e *testEnv) seedSample(t *testing.T, streamID string, ago, videoBps int64, lossPpm int32) int64 {
	t.Helper()
	at := nowUnix() - ago
	_, err := e.repo.StreamHealthReport.Insert(bg(), nil, &model.StreamHealthReport{
		ReportID:            fmt.Sprintf("seed-h-%d", len(e.db.reports)),
		StreamID:            streamID,
		NodeID:              "node-health",
		VideoBitrateBps:     videoBps,
		AudioBitrateBps:     128_000,
		FpsX100:             3000,
		PacketLossPpm:       lossPpm,
		RttMs:               20,
		SampleWindowSeconds: 10,
		HealthState:         model.HealthStateHealthy,
		OccurredAt:          at,
	})
	if err != nil {
		t.Fatalf("seed 采样点 %s ago=%d：%v", streamID, ago, err)
	}
	return at
}

// seedEventChain 用真状态机入口把一条 IDLE 流推到 STOPPED，产生 seq=1..4 四条事件。
// 走静默入口（mustReport）而不是手工 Insert：事件的 seq 只有经合法迁移分配才有意义，
// 游标语义在「测试自己造的 seq」上成立不等于在真链路上成立。
func (e *testEnv) seedEventChain(t *testing.T, id string) {
	t.Helper()
	e.seedStream(t, &model.Stream{StreamID: id, RoomID: 31, Protocol: 1})
	path := []rpc.StreamState{
		rpc.StreamState_STREAM_STATE_PUBLISHING,
		rpc.StreamState_STREAM_STATE_INTERRUPTED,
		rpc.StreamState_STREAM_STATE_PUBLISHING,
		rpc.StreamState_STREAM_STATE_STOPPED,
	}
	for i, target := range path {
		e.mustReport(t, reportReq(id, fmt.Sprintf("rep-chain-%s-%d", id, i), target))
	}
}

// seedRawEvents 直接铺 n 条事件（seq 1..n）。
// 存在的唯一理由：状态机一条流最多 4 条事件，够不到 limit 的夹取区间（20/120/200），
// 「夹取到底有没有下推」只能靠超过一页的事件量暴露。
func (e *testEnv) seedRawEvents(t *testing.T, id string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if _, err := e.repo.StreamEvent.Insert(bg(), nil, &model.StreamEvent{
			EventID: fmt.Sprintf("EVRAW%05d", i), StreamID: id, RoomID: 31, Seq: int64(i),
			FromState: model.StreamStatePublishing, ToState: model.StreamStatePublishing,
			Reason: "铺数据", OccurredAt: nowUnix() - int64(n-i),
		}); err != nil {
			t.Fatalf("seed 事件 %s seq=%d：%v", id, i, err)
		}
	}
}

func seqsOf(rows []*rpc.StreamEventInfo) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetSeq())
	}
	return out
}

func requireSeqs(t *testing.T, rows []*rpc.StreamEventInfo, want []int64, label string) {
	t.Helper()
	got := seqsOf(rows)
	if len(got) != len(want) {
		t.Fatalf("%s：seq 序列不符\n  got =%v\n want=%v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s：第 %d 条不符\n  got =%v\n want=%v", label, i, got, want)
		}
	}
}

// requireSeqRange 断言 rows 恰好是 [from..to] 这一段连续 seq，
// 用于几百条规模的夹取用例——整列打印会把现场淹掉。
func requireSeqRange(t *testing.T, rows []*rpc.StreamEventInfo, from, to int64, label string) {
	t.Helper()
	if from > to {
		t.Fatalf("%s：用例写错了，from=%d > to=%d", label, from, to)
	}
	want := to - from + 1
	if int64(len(rows)) != want {
		t.Fatalf("%s：条数不符 got=%d want=%d（%d..%d）", label, len(rows), want, from, to)
	}
	for seq := from; seq <= to; seq++ {
		if rows[seq-from].GetSeq() != seq {
			t.Fatalf("%s：第 %d 条 seq=%d，应为 %d（连续升序承诺破了）",
				label, seq-from, rows[seq-from].GetSeq(), seq)
		}
	}
}

// --- GetStreamHealth：窗口聚合 + 采样投影 ---

func TestGetStreamHealth_ProjectsWindowAggregateAndAscendingSamples(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-OK", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	// 配置窗口 10 秒：4 点在窗口内，1 点在窗口外（码率/丢包都刻意取极值，越界就必须看得见）
	atOld := e.seedSample(t, "S-H-OK", 6, 900_000, 100)
	atMid := e.seedSample(t, "S-H-OK", 5, 1_500_000, 400)
	atNew := e.seedSample(t, "S-H-OK", 2, 600_000, 200)
	atLast := e.seedSample(t, "S-H-OK", 1, 1_200_000, 50)
	e.seedSample(t, "S-H-OK", 30, 5_000_000, 999_999)

	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-OK" })
	wantOK(t, reply, err, "健康检查")

	if reply.GetStreamId() != "S-H-OK" || reply.GetState() != rpc.StreamState_STREAM_STATE_PUBLISHING {
		t.Fatalf("流身份/状态投影不符：%s %v", reply.GetStreamId(), reply.GetState())
	}
	if reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_HEALTHY {
		t.Fatalf("刚上报过的流不该被判 NO_DATA：%v", reply.GetHealthState())
	}
	// 聚合只算窗口内 4 点：avg=(900k+1500k+600k+1200k)/4，min=600k，max loss=400。
	// 窗口外那条的 5_000_000 / 999_999 一旦出现在这里，就是「窗口条件下推漏了」。
	if got := reply.GetSampleCount(); got != 4 {
		t.Fatalf("窗口内采样点数=%d，应为 4（窗口外那条被算进来了）", got)
	}
	if got := reply.GetAvgVideoBitrateBps(); got != 1_050_000 {
		t.Fatalf("窗口均值=%d，应为 1050000", got)
	}
	if got := reply.GetMinVideoBitrateBps(); got != 600_000 {
		t.Fatalf("窗口最低码率=%d，应为 600000", got)
	}
	if got := reply.GetMaxPacketLossPpm(); got != 400 {
		t.Fatalf("窗口最高丢包=%d，应为 400", got)
	}

	// 采样点必须按 occurred_at 升序（前端直接画折线；真 SQL 先 DESC 取最近 N 条再翻转）。
	samples := reply.GetSamples()
	if len(samples) != 4 {
		t.Fatalf("窗口内应返回 4 个采样点，实得 %d", len(samples))
	}
	got := seqsOfTime(samples)
	for i, want := range []int64{atOld, atMid, atNew, atLast} {
		if got[i] != want {
			t.Fatalf("第 %d 个采样点时间错（升序承诺破了）：got=%v", i, got)
		}
	}
	s := samples[2]
	for _, c := range []struct {
		field     string
		got, want int64
	}{
		{"video_bitrate_bps", s.GetVideoBitrateBps(), 600_000},
		{"audio_bitrate_bps", s.GetAudioBitrateBps(), 128_000},
		{"rtt_ms", s.GetRttMs(), 20},
	} {
		if c.got != c.want {
			t.Fatalf("采样点字段 %s 投影不符：got=%d want=%d", c.field, c.got, c.want)
		}
	}
	for _, c := range []struct {
		field     string
		got, want int32
	}{
		{"fps_x100", s.GetFpsX100(), 3000},
		{"packet_loss_ppm", s.GetPacketLossPpm(), 200},
	} {
		if c.got != c.want {
			t.Fatalf("采样点字段 %s 投影不符：got=%d want=%d", c.field, c.got, c.want)
		}
	}
}

func seqsOfTime(rows []*rpc.HealthSample) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetOccurredAt())
	}
	return out
}

func TestGetStreamHealth_ClientWindowOnlyNarrowsNeverWidens(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-WIN", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	// 配置窗口 10 秒：ago 1/2/5/6 在窗口内（各留 >=2 秒余量），ago 14 稳定在窗口外。
	for _, ago := range []int64{1, 2, 5, 6} {
		e.seedSample(t, "S-H-WIN", ago, 1_000_000, 10)
	}
	e.seedSample(t, "S-H-WIN", 14, 1_000_000, 10)

	cases := []struct {
		name    string
		window  int32
		wantPts int32
	}{
		// 不给窗口 → 取配置默认 10 秒
		{"缺省走配置", 0, 4},
		// 显式收窄到 4 秒：只剩 ago 1/2
		{"客户端收窄到 4 秒", 4, 2},
		// 放宽必须被夹回配置值：客户端无权要一个更大的窗口
		{"客户端放宽被夹回配置", 600, 4},
		// 非正数按「未提供」处理，交给配置默认，而不是报错也不是「全量」
		{"负窗口按缺省处理", -5, 4},
	}
	for _, c := range cases {
		reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) {
			r.StreamId = "S-H-WIN"
			r.WindowSeconds = c.window
		})
		wantOK(t, reply, err, c.name)
		if reply.GetSampleCount() != c.wantPts || int32(len(reply.GetSamples())) != c.wantPts {
			t.Fatalf("%s：聚合 %d 点 / 采样 %d 条，期望各 %d 点（窗口夹取失效）",
				c.name, reply.GetSampleCount(), len(reply.GetSamples()), c.wantPts)
		}
	}
}

func TestGetStreamHealth_WindowFallsBackWhenConfigMissing(t *testing.T) {
	e := newTestEnv(t)
	// 配置缺失（0）时的兜底路径：positiveOrDefault 回 0 → 不夹取 → window<=0 → 10 秒。
	// 若这层兜底没了，since=now 会让窗口里一个点都不剩：「配不到窗口就返回空健康」
	// 正是它要挡的静默故障。
	e.svc.Config.LiveIngest.HealthSampleWindowSeconds = 0
	e.seedStreamHealth(t, "S-H-NOCFG", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	e.seedSample(t, "S-H-NOCFG", 2, 1_000_000, 10)
	e.seedSample(t, "S-H-NOCFG", 30, 1_000_000, 10)

	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-NOCFG" })
	wantOK(t, reply, err, "配置缺失时的窗口兜底")
	if reply.GetSampleCount() != 1 {
		t.Fatalf("兜底窗口未生效：采样点数=%d，期望 1（defaultHealthWindowSeconds=10 只该收进 ago=2 那个点）",
			reply.GetSampleCount())
	}

	// 顺带钉住一处「配置缺失连带关掉上限」的既有形态（README 已登记为观察项）：
	// 夹取分支的条件是 cfg>0，配置为 0 时客户端给的窗口原样生效。
	wide, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) {
		r.StreamId = "S-H-NOCFG"
		r.WindowSeconds = 99
	})
	wantOK(t, wide, err, "配置缺失时客户端窗口不被夹取")
	if wide.GetSampleCount() != 2 {
		t.Fatalf("配置为 0 时应原样采用客户端窗口：采样点数=%d，期望 2", wide.GetSampleCount())
	}
}

func TestGetStreamHealth_SampleLimitTruncatesToTheNewestPoints(t *testing.T) {
	e := newTestEnv(t)
	// 把窗口拉到 60 秒，好让 25 个点都落在窗口内——limit 夹取只有在「候选多于一页」时才看得见。
	e.svc.Config.LiveIngest.HealthSampleWindowSeconds = 60
	e.seedStreamHealth(t, "S-H-LIMIT", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	// 相邻采样点间隔 2 秒而不是 1 秒：nowUnix() 在铺数据过程中最多跳 1 秒，
	// 间隔 1 秒会让相邻两点在某些时刻同秒，排序与截断断言就变成掷骰子。
	var ats []int64
	for i := 25; i >= 1; i-- {
		ats = append(ats, e.seedSample(t, "S-H-LIMIT", int64(2*i), 1_000_000, 10))
	}

	cases := []struct {
		name    string
		limit   int32
		wantPts int
	}{
		// 未给 limit → defaultListPageSize=20（不是「不限量」）
		{"缺省 limit 归一到 20", 0, 20},
		{"显式 limit 生效", 3, 3},
		// 超 MaxSamplePoints(120) 被夹到 120：这里 25 条全返回，而不是拒绝也不是只给 20 条
		{"超上限被夹取而非拒绝", 100_000, 25},
	}
	for _, c := range cases {
		reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) {
			r.StreamId = "S-H-LIMIT"
			r.SampleLimit = c.limit
		})
		wantOK(t, reply, err, c.name)
		if len(reply.GetSamples()) != c.wantPts {
			t.Fatalf("%s：返回 %d 个采样点，期望 %d", c.name, len(reply.GetSamples()), c.wantPts)
		}
		// 截断必须丢最旧的而不是最新的：折线图右端（当前状态）被切掉是最没用的降级。
		// 比对的是铺数据时拿到的绝对时间，不做「now 减一减」——那会把秒级抖动引进断言。
		want := ats[len(ats)-c.wantPts:]
		got := seqsOfTime(reply.GetSamples())
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s：返回的不是最近 %d 个点（截断方向反了）\n  got =%v\n want=%v", c.name, c.wantPts, got, want)
			}
		}
		// 聚合与采样条数解耦：limit 只影响折线密度，不该动摇判定用的样本量。
		if reply.GetSampleCount() != 25 {
			t.Fatalf("%s：sample_count 被 sample_limit 影响了：%d（应为窗口内全量 25）", c.name, reply.GetSampleCount())
		}
	}
}

// --- GetStreamHealth：NO_DATA 是视图，不是写回 ---

func TestGetStreamHealth_StaleReportReadsAsNoDataWithoutPersisting(t *testing.T) {
	e := newTestEnv(t)
	staleAt := nowUnix() - 120 // 远超 cfg.HealthNoDataSeconds=30
	e.seedStreamHealth(t, "S-H-STALE", model.StreamStatePublishing, model.HealthStateHealthy, staleAt)
	before := e.effects()

	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-STALE" })
	wantOK(t, reply, err, "无数据降级")
	if reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_NO_DATA {
		t.Fatalf("停止上报的流仍回 %v，应为 NO_DATA", reply.GetHealthState())
	}
	if reply.GetHealthReportedAt() != staleAt {
		t.Fatalf("health_reported_at 被改写了：%d（原值 %d）", reply.GetHealthReportedAt(), staleAt)
	}
	// 关键：NO_DATA 只活在这一次响应里。写进库里就抹掉了最后一次真实判定，
	// 节点恢复上报前的这段时间再没人知道它曾经是 HEALTHY 还是 CRITICAL。
	if row := e.streamRow(t, "S-H-STALE"); row.HealthState != model.HealthStateHealthy {
		t.Fatalf("读路径把视图判定写回了库：health_state=%d（库里应仍是 HEALTHY）", row.HealthState)
	}
	e.requireSameEffects(t, before, "NO_DATA 降级不得写库")
	e.requireNoTransaction(t, before, "NO_DATA 降级不该开事务")
	if got := e.call("Stream.MarkStopped"); got != 0 {
		t.Fatalf("读路径调用了 Stream.MarkStopped %d 次（停流是写侧职责）", got)
	}
}

func TestGetStreamHealth_StoppedStreamKeepsItsLastVerdict(t *testing.T) {
	e := newTestEnv(t)
	// 超时条件的第一项是 state != STOPPED：流已停，就再也不会有心跳，
	// 此时「很久没上报」是事实而不是数据缺失，最后一次判定必须留着。
	e.seedStreamHealth(t, "S-H-STOPPED", model.StreamStateStopped, model.HealthStateCritical, nowUnix()-99_999)

	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-STOPPED" })
	wantOK(t, reply, err, "已停流的健康视图")
	if reply.GetState() != rpc.StreamState_STREAM_STATE_STOPPED {
		t.Fatalf("状态投影：%v", reply.GetState())
	}
	if reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_CRITICAL {
		t.Fatalf("已停流被改判为 %v，应保留最后一次真实判定 CRITICAL", reply.GetHealthState())
	}
}

// --- GetStreamHealth：厂商探测是补偿路径，不参与判定也不阻塞 ---

func TestGetStreamHealth_ProbeFailureDegradesAndLogsDistinctReason(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-PROBE", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	e.seedSample(t, "S-H-PROBE", 2, 1_000_000, 10)
	logs := captureLogs(t)

	// 生产真形态：repository 的 stub 恒回 ErrCdnNotConfigured。
	// 「读不到厂商侧」不等于「流不健康」，所以既不失败也不改判。
	e.gw.probeErr = repository.ErrCdnNotConfigured
	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-PROBE" })
	wantOK(t, reply, err, "适配器未接入时的降级")
	if reply.GetSampleCount() != 1 || reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_HEALTHY {
		t.Fatalf("降级后 DB 视图不完整：samples=%d health=%v", reply.GetSampleCount(), reply.GetHealthState())
	}
	if e.gw.probeCall != 1 {
		t.Fatalf("探测调用次数=%d，应为 1", e.gw.probeCall)
	}
	text := logs.joined()
	mustTrue(t, strings.Contains(text, "cdn probe unavailable"), "未接入应记 unavailable 一条")
	mustTrue(t, strings.Contains(text, "gateway_not_configured"), "降级原因要能被日志检索定位")
	mustFalse(t, strings.Contains(text, "probe_failed"), "未接入不该被记成探测失败（两种运维动作不同）")

	// 另一种失败：CDN 已接入但调用本身失败——必须落到不同 reason，
	// 否则运维看到一条「不可用」会以为只是没配。
	e.gw.probeErr = errors.New("cdn 502")
	again, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-PROBE" })
	wantOK(t, again, err, "探测失败时的降级")
	text = logs.joined()
	mustTrue(t, strings.Contains(text, "cdn probe failed"), "真实失败应记 probe_failed")
	mustTrue(t, strings.Contains(text, "reason=probe_failed"), "失败原因要可检索")
	if e.gw.probeCall != 2 {
		t.Fatalf("两次健康检查的探测次数=%d，应为 2", e.gw.probeCall)
	}
}

func TestGetStreamHealth_SuccessfulProbeChangesNothing(t *testing.T) {
	// TODO(缺陷 #2)：ProbeStream 的返回值在 getstreamhealthlogic.go:89 被 `if _, err :=`
	// 直接丢弃。厂商侧说「这条流已经不在了、码率 0」也不会改动响应里的任何一个字段，
	// 于是每次健康检查都白付一次外部管控面 RTT：既没补偿 DB 的滞后，也没留下可比对的痕迹。
	// 修好后本用例应改成「Alive=false 时 health_state 至少被标 stale/降级」，
	// 届时请与 README 缺陷 #2 一起收口。
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-DISCARD", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	at := e.seedSample(t, "S-H-DISCARD", 2, 1_000_000, 10)
	logs := captureLogs(t)
	e.gw.probe = &repository.StreamProbe{Alive: false, VideoBitrateBps: 0, FpsX100: 0, ProbedAt: nowUnix()}
	e.gw.probeErr = nil

	reply, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "S-H-DISCARD" })
	wantOK(t, reply, err, "探测成功的健康检查")
	if e.gw.probeCall != 1 {
		t.Fatalf("探测调用次数=%d，应为 1", e.gw.probeCall)
	}
	// 探针确实打了，且参数是 (node_id, stream_id) 这个顺序：两个入参同为 string，
	// 交换后编译期与返回值都看不出问题，只能靠实参留痕证伪。
	if e.gw.probeNodeID != "node-health" || e.gw.probeStreamID != "S-H-DISCARD" {
		t.Fatalf("ProbeStream 实参顺序/内容不符：node=%q stream=%q", e.gw.probeNodeID, e.gw.probeStreamID)
	}
	// 现状钉桩：响应完全等于纯 DB 视图。
	if reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_HEALTHY ||
		reply.GetSampleCount() != 1 || reply.GetAvgVideoBitrateBps() != 1_000_000 ||
		len(reply.GetSamples()) != 1 || reply.GetSamples()[0].GetOccurredAt() != at {
		t.Fatalf("探针结果竟改动了 DB 视图（本用例钉的是「被丢弃」）：%+v", reply)
	}
	requireAbsent(t, logs.joined(), "cdn probe", "探测成功不该留降级日志")
}

func TestGetStreamHealth_SkipsProbeWhenGatewayAbsent(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-NOGW", model.StreamStatePublishing, model.HealthStateDegraded, nowUnix())
	e.seedSample(t, "S-H-NOGW", 2, 1_000_000, 10)
	logs := captureLogs(t)

	// 生产 NewServiceContext 恒注入非 nil 的 stub，这一格钉的是防御分支：
	// 网关缺席时既不能报错，也不能伪造一条「探测失败」的告警（理由根本不存在）。
	svcNoGw := &svc.ServiceContext{Config: e.svc.Config, Repository: e.repo}
	reply, err := NewGetStreamHealthLogic(bg(), svcNoGw).GetStreamHealth(&rpc.GetStreamHealthReq{StreamId: "S-H-NOGW"})
	wantOK(t, reply, err, "无网关的健康检查")
	if e.gw.probeCall != 0 {
		t.Fatalf("网关缺席却仍尝试探测：%d 次", e.gw.probeCall)
	}
	if reply.GetHealthState() != rpc.HealthState_HEALTH_STATE_DEGRADED {
		t.Fatalf("DB 判定丢了：%v", reply.GetHealthState())
	}
	requireAbsent(t, logs.joined(), "cdn probe", "没探测就不该写探测日志")
}

// --- GetStreamHealth：入参与零副作用 ---

func TestGetStreamHealth_InputValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-VALID", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	before := e.effects()
	findOneBefore := e.call("Stream.FindOne")

	cases := []struct {
		name     string
		streamID string
		want     error
	}{
		{"空 stream_id", "", model.ErrInvalidStreamId},
		{"只有空白", "   ", model.ErrInvalidStreamId},
		{"含斜杠", "S/H", model.ErrInvalidStreamId},
		{"含空白", "S H", model.ErrInvalidStreamId},
		{"超列宽 64", strings.Repeat("S", maxStreamRefBytes+1), model.ErrInvalidStreamId},
		{"合法但不存在", "S-NOT-EXIST", model.ErrStreamNotFound},
	}
	for _, c := range cases {
		_, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = c.streamID })
		wantFail(t, err, c.want, c.name)
	}
	// 优先级：先判引用形态再查库——否则非法入参也要占一次 SELECT。
	_, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) { r.StreamId = "bad id" })
	wantErr(t, err, model.ErrInvalidStreamId, "非法引用不被降级成 not_found")
	// 上面 8 次调用里只有「合法但不存在」那一格该走到 SELECT。
	if got := e.call("Stream.FindOne"); got != findOneBefore+1 {
		t.Fatalf("Stream.FindOne 调用增量=%d，期望 1（非法引用不该消耗一次 SELECT）", got-findOneBefore)
	}
	if got := e.gw.probeCall; got != 0 {
		t.Fatalf("入参非法却探测了厂商：%d 次", got)
	}
	e.requireSameEffects(t, before, "健康检查入参校验必须零副作用")
	e.requireNoTransaction(t, before, "健康检查不该开事务")
}

func TestGetStreamHealth_ReadPathWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.seedStreamHealth(t, "S-H-READ", model.StreamStatePublishing, model.HealthStateHealthy, nowUnix())
	e.seedSample(t, "S-H-READ", 2, 1_000_000, 10)
	before := e.effects()
	writes := func() [4]int {
		return [4]int{e.call("StreamHealthReport.Insert"), e.call("StreamEvent.Insert"),
			e.call("Stream.ApplyTransition"), e.call("Outbox.Insert")}
	}
	beforeWrites := writes()

	for _, limit := range []int32{0, 5, 1000} {
		_, err := getStreamHealth(e, func(r *rpc.GetStreamHealthReq) {
			r.StreamId = "S-H-READ"
			r.SampleLimit = limit
		})
		if err != nil {
			t.Fatalf("健康检查读路径失败（limit=%d）：%v", limit, err)
		}
	}
	e.requireSameEffects(t, before, "健康检查不得改任何表")
	e.requireNoTransaction(t, before, "健康检查不该开事务")
	if got := writes(); got != beforeWrites {
		t.Fatalf("读路径调用了写方法：前 %v 后 %v", beforeWrites, got)
	}
}

func TestGetStreamHealth_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewGetStreamHealthLogic(bg(), svcCtx).GetStreamHealth(&rpc.GetStreamHealthReq{StreamId: "S-H"})
	wantFail(t, err, errNoRepository, "未装配 Repository 的健康检查")
}

// --- ListStreamEvents：游标 / 位点 / 夹取 ---

func TestListStreamEvents_FreshChainProjectsFromZeroInAscendingOrder(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-CHAIN")

	reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) { r.StreamId = "S-EV-CHAIN" })
	wantOK(t, reply, err, "从头拉全链")
	requireSeqs(t, reply.GetEvents(), []int64{1, 2, 3, 4}, "seq 1..4 全序列")
	if reply.GetMaxSeq() != 4 {
		t.Fatalf("max_seq=%d，应为 4", reply.GetMaxSeq())
	}
	mustFalse(t, reply.GetHasMore(), "全部事件已返回却声称还有（对账任务会空转）")

	wantStates := []struct {
		from, to   rpc.StreamState
		stopReason rpc.StopReason
	}{
		{rpc.StreamState_STREAM_STATE_IDLE, rpc.StreamState_STREAM_STATE_PUBLISHING, rpc.StopReason_STOP_REASON_UNSPECIFIED},
		{rpc.StreamState_STREAM_STATE_PUBLISHING, rpc.StreamState_STREAM_STATE_INTERRUPTED, rpc.StopReason_STOP_REASON_UNSPECIFIED},
		{rpc.StreamState_STREAM_STATE_INTERRUPTED, rpc.StreamState_STREAM_STATE_PUBLISHING, rpc.StopReason_STOP_REASON_UNSPECIFIED},
		{rpc.StreamState_STREAM_STATE_PUBLISHING, rpc.StreamState_STREAM_STATE_STOPPED, rpc.StopReason_STOP_REASON_ANCHOR_STOP},
	}
	seen := map[string]bool{}
	var interruptionOfResume int64
	for i, ev := range reply.GetEvents() {
		w := wantStates[i]
		if ev.GetFromState() != w.from || ev.GetToState() != w.to {
			t.Fatalf("事件 %d 状态迁移投影：%v -> %v，期望 %v -> %v", i+1,
				ev.GetFromState(), ev.GetToState(), w.from, w.to)
		}
		if ev.GetStopReason() != w.stopReason {
			t.Fatalf("事件 %d stop_reason=%v，期望 %v（只有停流事件带原因）", i+1, ev.GetStopReason(), w.stopReason)
		}
		if ev.GetStreamId() != "S-EV-CHAIN" || ev.GetRoomId() != 31 {
			t.Fatalf("事件 %d 归属投影：%s room=%d", i+1, ev.GetStreamId(), ev.GetRoomId())
		}
		if ev.GetEventId() == "" || seen[ev.GetEventId()] {
			t.Fatalf("事件 %d 的 event_id 缺失或重复：%q", i+1, ev.GetEventId())
		}
		seen[ev.GetEventId()] = true
		if ev.GetOccurredAt() <= 0 {
			t.Fatalf("事件 %d 没有发生时间：%d", i+1, ev.GetOccurredAt())
		}
		// 断流归因必须自带：消费方据此把中断时长挂到对应区间上，不必再查别的表。
		switch i {
		case 1:
			if ev.GetInterruptionId() == 0 {
				t.Fatalf("进 INTERRUPTED 的事件没带 interruption_id，断流区间与事件脱钩了")
			}
			interruptionOfResume = ev.GetInterruptionId()
		case 2:
			if ev.GetInterruptionId() != interruptionOfResume {
				t.Fatalf("重连事件引用了另一段断流：%d，期望 %d", ev.GetInterruptionId(), interruptionOfResume)
			}
		default:
			if ev.GetInterruptionId() != 0 {
				t.Fatalf("事件 %d 不该带断流引用：%d", i+1, ev.GetInterruptionId())
			}
		}
	}
}

func TestListStreamEvents_AfterSeqIsExclusiveLowerBound(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-CURSOR")

	cases := []struct {
		afterSeq int64
		want     []int64
	}{
		{0, []int64{1, 2, 3, 4}},
		{1, []int64{2, 3, 4}},
		{2, []int64{3, 4}},
		// 游标是开区间：已应用 seq=3 的调用方不该再收到 seq=3
		{3, []int64{4}},
		{4, []int64{}},
		// 游标超前（该流已回退过？不可能，但消费方可能带着旧位点重来）：空而不是报错
		{99, []int64{}},
	}
	for _, c := range cases {
		reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
			r.StreamId = "S-EV-CURSOR"
			r.AfterSeq = c.afterSeq
		})
		label := fmt.Sprintf("after_seq=%d", c.afterSeq)
		wantOK(t, reply, err, label)
		requireSeqs(t, reply.GetEvents(), c.want, label)
		// max_seq 与游标无关：它是「该流到哪了」的位点，翻页时抖动就等于让消费方误判追平。
		if reply.GetMaxSeq() != 4 {
			t.Fatalf("%s：max_seq=%d，应为 4", label, reply.GetMaxSeq())
		}
		mustFalse(t, reply.GetHasMore(), label+" 之后已无事件")
	}
}

func TestListStreamEvents_DescYieldsNewestFirstAboveCursor(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-DESC")

	reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
		r.StreamId = "S-EV-DESC"
		r.Desc = true
	})
	wantOK(t, reply, err, "倒序全量")
	requireSeqs(t, reply.GetEvents(), []int64{4, 3, 2, 1}, "desc 下 seq 倒排")
	mustFalse(t, reply.GetHasMore(), "倒序未装满一页不该说还有")

	// 倒序 + 游标：取「after_seq 之上」的最新一批，而不是全局最新一批——
	// 否则带着旧位点的消费方会以为自己没落后再收一遍旧事件。
	page, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
		r.StreamId = "S-EV-DESC"
		r.Desc = true
		r.AfterSeq = 1
		r.Limit = 2
	})
	wantOK(t, page, err, "倒序带游标")
	requireSeqs(t, page.GetEvents(), []int64{4, 3}, "倒序游标之上取最新两条")
	mustTrue(t, page.GetHasMore(), "装满一页时应有 more（seq=2 还在游标之上）")
	for _, ev := range page.GetEvents() {
		if ev.GetSeq() <= 1 {
			t.Fatalf("倒序把游标以下的事件也返回了：%d", ev.GetSeq())
		}
	}
}

func TestListStreamEvents_LimitIsClampedToMaxEventPageSize(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-EV-BIG", RoomID: 31, Protocol: 1})
	e.seedRawEvents(t, "S-EV-BIG", 205)

	cases := []struct {
		name    string
		limit   int32
		wantRow int64
	}{
		// 未给 limit → defaultListPageSize=20，而不是「不给就不限」
		{"缺省归一到 20", 0, 20},
		{"显式 limit 生效", 7, 7},
		// 超 MaxEventPageSize(200) 夹到 200：不夹取就等于让调用方要多少拿多少
		{"超上限被夹到 200", 100_000, 200},
	}
	for _, c := range cases {
		reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
			r.StreamId = "S-EV-BIG"
			r.Limit = c.limit
		})
		wantOK(t, reply, err, c.name)
		if int64(len(reply.GetEvents())) != c.wantRow {
			t.Fatalf("%s：返回 %d 条，期望 %d 条", c.name, len(reply.GetEvents()), c.wantRow)
		}
		requireSeqRange(t, reply.GetEvents(), 1, c.wantRow, c.name)
		if reply.GetMaxSeq() != 205 {
			t.Fatalf("%s：max_seq=%d，应为 205", c.name, reply.GetMaxSeq())
		}
		mustTrue(t, reply.GetHasMore(), c.name+" 后面确实还有事件")
	}

	// 最后一页：游标推到 200 之后只剩 5 条，此时必须诚实说没有更多，
	// 对账任务才有一个可终止的条件。
	tail, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
		r.StreamId = "S-EV-BIG"
		r.AfterSeq = 200
		r.Limit = 200
	})
	wantOK(t, tail, err, "最后一页")
	requireSeqRange(t, tail.GetEvents(), 201, 205, "最后一页")
	mustFalse(t, tail.GetHasMore(), "已到尾部却说还有")
}

func TestListStreamEvents_FullPageAtTheTailStillSaysHasMore(t *testing.T) {
	// TODO(缺陷 #6)：eventsHaveMore（liststreameventslogic.go:77）先判
	// `len(rows) >= limit` 再判「最后一条是否已到 max_seq」，装满一页就短路返回 true。
	// 于是「恰好一整页且已经到尾部」这一格谎报 has_more：消费方按 has_more 继续拉，
	// 下一趟拿到空列表才停——多一次无谓往返（对账任务放大成全量重扫）。
	// 修好后本用例的期望值应改为 false，并与 README 缺陷 #6 一起收口。
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-TAIL")

	for _, desc := range []bool{false, true} {
		label := "asc 装满一页"
		if desc {
			label = "desc 装满一页"
		}
		reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
			r.StreamId = "S-EV-TAIL"
			r.Limit = 4
			r.Desc = desc
		})
		wantOK(t, reply, err, label)
		if len(reply.GetEvents()) != 4 {
			t.Fatalf("%s：返回 %d 条，期望 4 条", label, len(reply.GetEvents()))
		}
		if reply.GetMaxSeq() != 4 {
			t.Fatalf("%s：max_seq=%d，应为 4", label, reply.GetMaxSeq())
		}
		mustTrue(t, reply.GetHasMore(), label+"（现状就是会谎报，此处钉的是已实现行为）")
	}
}

func TestListStreamEvents_EmptyResultNeverClaimsMore(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-EV-EMPTY", RoomID: 31, Protocol: 1})

	reply, err := listEvents(e, func(r *rpc.ListStreamEventsReq) { r.StreamId = "S-EV-EMPTY" })
	wantOK(t, reply, err, "无事件的流")
	if len(reply.GetEvents()) != 0 {
		t.Fatalf("新建流竟有事件：%d 条", len(reply.GetEvents()))
	}
	if reply.GetMaxSeq() != 0 {
		t.Fatalf("max_seq=%d，应为 0（消费方据此判断这条流一条都没应用过）", reply.GetMaxSeq())
	}
	mustFalse(t, reply.GetHasMore(), "空列表 + has_more=true 会让对账任务永远追不平")
}

func TestListStreamEvents_UnknownStreamIsNotFound(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-OTHER")

	_, err := listEvents(e, func(r *rpc.ListStreamEventsReq) { r.StreamId = "S-EV-MISSING" })
	wantFail(t, err, model.ErrStreamNotFound, "查无此流")
	// 「查无此流」与「事件已全部应用」必须是两回事：把前者当后者返回空列表，
	// 对账任务会把一条根本不存在的流标记为已追平，事件缺口就永久静默了。
}

func TestListStreamEvents_InputValidationIsZeroSideEffect(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-CHECK")
	before := e.effects()
	listBefore := e.call("StreamEvent.ListAfterSeq")

	cases := []struct {
		name     string
		streamID string
		afterSeq int64
		want     error
	}{
		{"空 stream_id", "", 0, model.ErrInvalidStreamId},
		{"超列宽", strings.Repeat("E", maxStreamRefBytes+1), 0, model.ErrInvalidStreamId},
		{"含制表符", "S\tEV", 0, model.ErrInvalidStreamId},
		{"负游标", "S-EV-CHECK", -1, model.ErrInvalidStreamId},
		{"极值负游标", "S-EV-CHECK", -9223372036854775807, model.ErrInvalidStreamId},
		// 优先级：游标校验在查库之前——非法位点不该消耗一次 SELECT。
		{"不存在的流带负游标仍先判游标", "S-EV-MISSING", -1, model.ErrInvalidStreamId},
		{"不存在的流", "S-EV-MISSING", 0, model.ErrStreamNotFound},
	}
	for _, c := range cases {
		_, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
			r.StreamId = c.streamID
			r.AfterSeq = c.afterSeq
		})
		wantFail(t, err, c.want, c.name)
	}
	if got := e.call("StreamEvent.ListAfterSeq"); got != listBefore {
		t.Fatalf("入参非法/流不存在却查了事件表：%d 次增量", got-listBefore)
	}
	e.requireSameEffects(t, before, "事件位点校验必须零副作用")
	e.requireNoTransaction(t, before, "事件位点不该开事务")
}

func TestListStreamEvents_ReadPathWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	e.seedEventChain(t, "S-EV-READ")
	before := e.effects()
	writes := func() [3]int {
		return [3]int{e.call("StreamEvent.Insert"), e.call("Outbox.Insert"), e.call("Stream.ApplyTransition")}
	}
	beforeWrites := writes()

	for _, after := range []int64{0, 2, 4} {
		_, err := listEvents(e, func(r *rpc.ListStreamEventsReq) {
			r.StreamId = "S-EV-READ"
			r.AfterSeq = after
		})
		if err != nil {
			t.Fatalf("事件读取失败（after_seq=%d）：%v", after, err)
		}
	}
	e.requireSameEffects(t, before, "拉事件不得改任何表")
	e.requireNoTransaction(t, before, "拉事件不该开事务")
	if got := writes(); got != beforeWrites {
		t.Fatalf("读路径调用了写方法：前 %v 后 %v", beforeWrites, got)
	}
}

func TestListStreamEvents_RejectsMissingRepository(t *testing.T) {
	svcCtx := withoutRepoEnv(t)
	_, err := NewListStreamEventsLogic(bg(), svcCtx).ListStreamEvents(&rpc.ListStreamEventsReq{StreamId: "S-EV"})
	wantFail(t, err, errNoRepository, "未装配 Repository 的事件位点")
}
