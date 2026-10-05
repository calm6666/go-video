// 本文件覆盖健康采样上报入口 ReportStreamHealth 的四条契约：
//
//  1. 阈值判定：CRITICAL 优先于 DEGRADED，未配置（<=0）的维度不参与判定；
//  2. 「连续越界才断流」：单次抖动不许把主播踢下线，且「连续」受采样窗口约束；
//  3. report_id 幂等回放先于任何流表读取：重放既不新增采样也不改投影；
//  4. 采样落库 + 投影回写 + 断流迁移同事务：任一步失败必须整笔回滚，
//     不留「有采样没投影」或「有事件没采样」的半截事实。
//
// 时间敏感用例沿用 healthquery_test.go 的口径：采样点偏移按「窗口内留 >=2 秒余量、
// 窗口外至少早 40 秒」排布，钉的是分支归属而不是边界那一秒（nowUnix() 会跳秒）。
package logic

import (
	"fmt"
	"strings"
	"testing"

	"go-video/services/live-ingest/model"
	"go-video/services/live-ingest/rpc"
)

// healthReq 构造一份「阈值内、幂等键齐」的健康上报，mut 用来制造越界与畸形入参。
func healthReq(streamID, reportID string, mut ...func(*rpc.ReportStreamHealthReq)) *rpc.ReportStreamHealthReq {
	in := &rpc.ReportStreamHealthReq{
		StreamId: streamID, ReportId: reportID, NodeId: "node-h",
		VideoBitrateBps: 1_000_000, AudioBitrateBps: 128_000,
		FpsX100: 3000, PacketLossPpm: 0, RttMs: 20,
	}
	for _, m := range mut {
		m(in)
	}
	return in
}

func (e *testEnv) reportHealth(t *testing.T, in *rpc.ReportStreamHealthReq) (*rpc.ReportStreamHealthReply, error) {
	t.Helper()
	return NewReportStreamHealthLogic(bg(), e.svc).ReportStreamHealth(in)
}

// mustReportHealth 是 reportHealth 的「必须成功」形态（与 mustReport/mustRevokeKey 同口径）：
// 本文件多数用例只关心落库结果，失败时直接 Fatalf 并带上 report_id，
// 免得用 `reply, _ := ...` 把错误咽掉、再在 nil 指针上炸出无信息现场。
func (e *testEnv) mustReportHealth(t *testing.T, in *rpc.ReportStreamHealthReq) *rpc.ReportStreamHealthReply {
	t.Helper()
	reply, err := e.reportHealth(t, in)
	if err != nil {
		t.Fatalf("ReportStreamHealth(%s/%s)：%v", in.StreamId, in.ReportId, err)
	}
	return reply
}

// seedPublishingStream 铺一条「正在推流、心跳已旧、健康列仍是建档默认 NO_DATA」的流：
// 这样「投影有没有真的被这次上报推进」才是可证伪的（初值与落值不同）。
func (e *testEnv) seedPublishingStream(t *testing.T, id string) *model.Stream {
	t.Helper()
	return e.seedStream(t, &model.Stream{
		StreamID: id, RoomID: 51, AnchorMid: 1001, Protocol: 1, NodeID: "node-h",
		State: model.StreamStatePublishing, Seq: 1,
		LastHeartbeatAt: nowUnix() - 3600,
	})
}

// seedHealthSamples 铺 n 条历史采样点（occurred_at = now-ago，判定态由用例指定）。
// OccurredAt 必须显式给：替身在 0 时拿 Ctime 兜底（1700000000），落在任何窗口之外，
// 「连续越界」的输入就会静默为空，用例变成自证。
// report_id 带上 ago：uniq_report_id 是唯一索引，按「一次一条、状态各异」逐次调用本
// helper 时（见 Confirm 用例），只带循环下标会让每次都是 -0 而互相撞键。
func (e *testEnv) seedHealthSamples(t *testing.T, streamID string, ago []int64, healthState int32) {
	t.Helper()
	for i, a := range ago {
		if _, err := e.repo.StreamHealthReport.Insert(bg(), nil, &model.StreamHealthReport{
			ReportID: fmt.Sprintf("seed-h-%s-%d-%d", streamID, i, a), StreamID: streamID, NodeID: "node-h",
			VideoBitrateBps: 1_000_000, AudioBitrateBps: 128_000, FpsX100: 3000,
			SampleWindowSeconds: 10, HealthState: healthState, OccurredAt: nowUnix() - a,
		}); err != nil {
			t.Fatalf("seed 采样 %s ago=%d：%v", streamID, a, err)
		}
	}
}

// healthRow 回库里指定 report_id 的采样行（副本：断言「没被改动」必须读副本）。
func (e *testEnv) healthRow(t *testing.T, reportID string) *model.StreamHealthReport {
	t.Helper()
	for _, row := range e.db.reports {
		if row.ReportID == reportID {
			cp := *row
			return &cp
		}
	}
	t.Fatalf("库里没有采样 %s", reportID)
	return nil
}

// --- 依赖缺席 ---

func TestReportStreamHealth_NoRepositoryIsError(t *testing.T) {
	_, err := NewReportStreamHealthLogic(bg(), withoutRepoEnv(t)).ReportStreamHealth(healthReq("S-ANY", "rep-any"))
	wantFail(t, err, errNoRepository, "未装配仓储时必须报错")
}

// --- 入参校验先于任何一次查询 ---

func TestReportStreamHealth_MalformedInputNeverTouchesDB(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ReportStreamHealthReq
		want error
	}{
		{"空 stream_id", healthReq("", "rep-1"), model.ErrInvalidStreamId},
		{"stream_id 含斜杠", healthReq("S/BAD", "rep-1"), model.ErrInvalidStreamId},
		{"stream_id 超列宽", healthReq(strings.Repeat("S", 65), "rep-1"), model.ErrInvalidStreamId},
		{"空 report_id", healthReq("S-OK", "   "), model.ErrIdempotencyKeyRequired},
		{"report_id 超列宽", healthReq("S-OK", strings.Repeat("r", 65)), model.ErrIdempotencyKeyRequired},
		{"node_id 含空白", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.NodeId = "node h"
		}), model.ErrNodeNotFound},
		{"视频码率为负", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.VideoBitrateBps = -1
		}), model.ErrInvalidSampleMetrics},
		{"视频码率超物理上限", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.VideoBitrateBps = maxVideoBitrateBps + 1
		}), model.ErrInvalidSampleMetrics},
		{"音频码率超上限", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.AudioBitrateBps = maxAudioBitrateBps + 1
		}), model.ErrInvalidSampleMetrics},
		{"帧率为负", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.FpsX100 = -1
		}), model.ErrInvalidSampleMetrics},
		{"丢包为负", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.PacketLossPpm = -1
		}), model.ErrInvalidSampleMetrics},
		{"丢包超 100%", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.PacketLossPpm = maxPacketLossPpm + 1
		}), model.ErrInvalidSampleMetrics},
		{"rtt 超上限", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.RttMs = maxRttMs + 1
		}), model.ErrInvalidSampleMetrics},
		{"采样窗口为负", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.SampleWindowSeconds = -1
		}), model.ErrInvalidSampleMetrics},
		{"采样窗口超上限", healthReq("S-OK", "rep-1", func(in *rpc.ReportStreamHealthReq) {
			in.SampleWindowSeconds = maxSampleWindowSeconds + 1
		}), model.ErrInvalidSampleMetrics},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedPublishingStream(t, "S-OK")
			before := e.effects()
			reportHits, streamHits := e.call("StreamHealthReport.FindByReportID"), e.call("Stream.FindOne")

			_, err := e.reportHealth(t, c.in)

			wantFail(t, err, c.want, c.name)
			e.requireSameEffects(t, before, c.name)
			e.requireNoTransaction(t, before, c.name)
			if got := e.call("StreamHealthReport.FindByReportID"); got != reportHits {
				t.Fatalf("%s：入参未校验就查了采样表（幂等预检 %d→%d 次）", c.name, reportHits, got)
			}
			if got := e.call("Stream.FindOne"); got != streamHits {
				t.Fatalf("%s：入参未校验就查了流表（%d→%d 次）", c.name, streamHits, got)
			}
		})
	}
}

// --- 未知 stream_id / 终态流 ---

func TestReportStreamHealth_UnknownStreamIsNotFoundAndWritesNothing(t *testing.T) {
	e := newTestEnv(t)
	before := e.effects()

	_, err := e.reportHealth(t, healthReq("S-NOT-EXIST", "rep-miss"))

	wantFail(t, err, model.ErrStreamNotFound, "未知 stream_id")
	e.requireSameEffects(t, before, "未知 stream_id")
	e.requireNoTransaction(t, before, "未知 stream_id：采样表都没有主键可挂")
}

func TestReportStreamHealth_TerminalStreamRejectedBeforeTransaction(t *testing.T) {
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-TERMINAL", RoomID: 51, AnchorMid: 1001,
		Protocol: 1, NodeID: "node-h", State: model.StreamStateStopped, Seq: 4})
	before := e.effects()

	_, err := e.reportHealth(t, healthReq("S-TERMINAL", "rep-after-stop"))

	wantFail(t, err, model.ErrTerminalStream, "终态流不接受健康上报")
	e.requireSameEffects(t, before, "终态流")
	e.requireNoTransaction(t, before, "终态流：预检就该拒，别占事务连接")
}

// --- 幂等回放 ---

func TestReportStreamHealth_ReplayIsJudgedBeforeReadingTheStream(t *testing.T) {
	e := newTestEnv(t)
	// 采样属于一条现在已经 STOPPED 的流：若回放判定排在流读取之后，这里会回 ErrTerminalStream，
	// 节点侧就永远拿不到「上一次上报成功了」的确认，只能无限重试。
	e.seedStream(t, &model.Stream{StreamID: "S-REPLAY", RoomID: 51, AnchorMid: 1001,
		Protocol: 1, NodeID: "node-h", State: model.StreamStateStopped, Seq: 4})
	if _, err := e.repo.StreamHealthReport.Insert(bg(), nil, &model.StreamHealthReport{
		ReportID: "rep-replay", StreamID: "S-REPLAY", NodeID: "node-h", VideoBitrateBps: 900_000,
		AudioBitrateBps: 128_000, FpsX100: 3000, SampleWindowSeconds: 10,
		HealthState: model.HealthStateDegraded, OccurredAt: nowUnix() - 5,
	}); err != nil {
		t.Fatalf("seed 待回放的采样：%v", err)
	}
	before := e.effects()
	streamHits := e.call("Stream.FindOne")

	reply, err := e.reportHealth(t, healthReq("S-REPLAY", "rep-replay"))

	wantOK(t, reply, err, "同 report_id 重放")
	mustTrue(t, reply.Replayed, "重放位")
	if reply.HealthState != rpc.HealthState_HEALTH_STATE_DEGRADED {
		t.Fatalf("重放回显的是判定态而不是库里那条采样的态：got=%v", reply.HealthState)
	}
	if reply.Seq != 0 || reply.EventId != "" || reply.TriggeredInterrupt {
		t.Fatalf("首次上报没触发迁移，重放却回带了事件：%+v", reply)
	}
	if reply.Message != "相同 report_id 的采样已记录；本次未重复入库" {
		t.Fatalf("重放文案不符：%q", reply.Message)
	}
	e.requireSameEffects(t, before, "重放不得产生第二次采样")
	e.requireNoTransaction(t, before, "重放")
	if got := e.call("Stream.FindOne"); got != streamHits {
		t.Fatalf("回放分支仍读了流表（%d→%d 次）：幂等锚点应当是采样行本身", streamHits, got)
	}
}

func TestReportStreamHealth_ReplayCarriesTheInterruptItCaused(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-REPLAY-INT")
	in := healthReq("S-REPLAY-INT", "rep-int-once", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 100_000
	})
	e.seedHealthSamples(t, "S-REPLAY-INT", []int64{5, 3}, model.HealthStateCritical)
	first := e.mustReportHealth(t, in)
	if !first.TriggeredInterrupt {
		t.Fatalf("前置条件破了：连续第三次危险采样就该断流，实得 %+v", first)
	}
	before := e.effects()

	second, err := e.reportHealth(t, in)

	wantOK(t, second, err, "断流那次上报的重放")
	mustTrue(t, second.Replayed, "重放位")
	if !second.TriggeredInterrupt {
		t.Fatalf("首次触发了断流，重放却说没触发：%+v", second)
	}
	if second.Seq != first.Seq || second.EventId != first.EventId {
		t.Fatalf("重放必须回带首次迁移的 seq/event_id：first=%d/%s second=%d/%s",
			first.Seq, first.EventId, second.Seq, second.EventId)
	}
	if second.HealthState != rpc.HealthState_HEALTH_STATE_CRITICAL {
		t.Fatalf("重放回显的判定态应是首次那条采样的 CRITICAL，实得 %v", second.HealthState)
	}
	e.requireSameEffects(t, before, "重放不产生第二个采样点、也不重复断流")
}

// --- 正常写入路径：采样 + 投影一起推进 ---

func TestReportStreamHealth_LegalSampleAdvancesProjectionAndKeepsReportTime(t *testing.T) {
	e := newTestEnv(t)
	s := e.seedPublishingStream(t, "S-HEALTHY")
	before := e.effects()
	staleHeartbeat := s.LastHeartbeatAt

	reply := e.mustReportHealth(t, healthReq("S-HEALTHY", "rep-ok", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 900_000
		in.PacketLossPpm = 12_000
		in.RttMs = 33
	}))

	if reply.HealthState != rpc.HealthState_HEALTH_STATE_HEALTHY {
		t.Fatalf("900k 码率在 degraded 阈值之上，应判 HEALTHY：%+v", reply)
	}
	if reply.Message != "采样已记录" {
		t.Fatalf("正常采样文案不符：%q", reply.Message)
	}
	mustFalse(t, reply.TriggeredInterrupt, "一次正常采样不许动状态机")
	// 增量只有采样行：事件/Outbox/断流区间都必须为 0。
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "正常采样的写入面")

	row := e.streamRow(t, "S-HEALTHY")
	if row.State != model.StreamStatePublishing || row.Seq != 1 {
		t.Fatalf("正常采样改动了状态位：state=%d seq=%d", row.State, row.Seq)
	}
	if row.HealthState != model.HealthStateHealthy {
		t.Fatalf("投影没写回：health_state=%d", row.HealthState)
	}
	if row.VideoBitrateBps != 900_000 || row.PacketLossPpm != 12_000 ||
		row.AudioBitrateBps != 128_000 || row.FpsX100 != 3000 {
		t.Fatalf("投影字段与本次采样不一致：%+v", row)
	}
	at := e.healthRow(t, "rep-ok").OccurredAt
	if row.HealthReportedAt != at {
		t.Fatalf("health_reported_at 没跟进本次采样：%d vs %d", row.HealthReportedAt, at)
	}
	if row.LastHeartbeatAt != at {
		t.Fatalf("GREATEST 没把陈旧心跳推到本次采样时间：%d（旧 %d，期望 %d）", row.LastHeartbeatAt, staleHeartbeat, at)
	}
	if must := e.healthRow(t, "rep-ok"); must.StreamID != "S-HEALTHY" || must.NodeID != "node-h" ||
		must.HealthState != model.HealthStateHealthy || must.RttMs != 33 {
		t.Fatalf("采样行留痕不完整（rtt 只留在采样表，投影列没有该列）：%+v", must)
	}
}

func TestReportStreamHealth_LateButLegalSampleRewindsTheReportTime(t *testing.T) {
	// TODO(缺陷 #11)：迟到的合法采样会把 health_reported_at 倒退，
	// 于是仍在推流的流在 GetStreamHealth 里被读成 NO_DATA（见 README「已知缺口」第 11 条）。
	// 本用例钉的是**当前真实行为**，不是应然行为：修好之后这条必须改断言而不是删掉。
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-LATE")
	fresh := e.mustReportHealth(t, healthReq("S-LATE", "rep-fresh"))
	mustFalse(t, fresh.TriggeredInterrupt, "正常采样")
	before := e.effects()
	atBefore := e.streamRow(t, "S-LATE").HealthReportedAt
	heartbeatBefore := e.streamRow(t, "S-LATE").LastHeartbeatAt

	// 补报一条 100 秒前的采样：occurred_at 合法（未超 skew 与下限），必须被接受。
	reply := e.mustReportHealth(t, healthReq("S-LATE", "rep-backfill", func(in *rpc.ReportStreamHealthReq) {
		in.OccurredAt = nowUnix() - 100
	}))
	mustFalse(t, reply.Replayed, "新 report_id 不是重放")
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "补报仍只多一条采样")

	row := e.streamRow(t, "S-LATE")
	if row.LastHeartbeatAt != heartbeatBefore {
		t.Fatalf("GREATEST 失守：心跳被迟到的采样拉回 %d（原 %d）", row.LastHeartbeatAt, heartbeatBefore)
	}
	if row.HealthReportedAt >= atBefore {
		t.Fatalf("缺陷 #11 的行为变了（health_reported_at 不再被迟到采样改写）：%d -> %d，"+
			"请把本用例改成断言新语义并同步 README", row.HealthReportedAt, atBefore)
	}
	if row.HealthReportedAt != nowUnix()-100 {
		t.Fatalf("health_reported_at 应等于迟到采样的 occurred_at：%d", row.HealthReportedAt)
	}
	// 后果必须可观测：读侧把「刚上报过的流」显示成无数据。
	h, err := getStreamHealth(e, func(in *rpc.GetStreamHealthReq) { in.StreamId = "S-LATE" })
	wantOK(t, h, err, "补报后读健康")
	if h.HealthState != rpc.HealthState_HEALTH_STATE_NO_DATA {
		t.Fatalf("缺陷 #11 的下游后果没复现：读侧应回 NO_DATA，实得 %v", h.HealthState)
	}
	if h.State != rpc.StreamState_STREAM_STATE_PUBLISHING {
		t.Fatalf("流本身还在 PUBLISHING，只有健康视图被抹成无数据：state=%v", h.State)
	}
}

// --- 阈值判定 ---

func TestReportStreamHealth_JudgeHealthThresholdTable(t *testing.T) {
	cfg := healthThresholds{
		degradedMinVideoBitrate: 800_000, criticalMinVideoBitrate: 200_000,
		criticalMinFpsX100: 500, criticalMaxPacketLossPpm: 50_000,
	}
	cases := []struct {
		name                 string
		th                   healthThresholds
		video                int64
		fps, loss, wantState int32
	}{
		{"全阈值内", cfg, 1_000_000, 3000, 0, model.HealthStateHealthy},
		{"码率等于危险线不算越界", cfg, 200_000, 3000, 0, model.HealthStateDegraded},
		{"码率刚低于危险线", cfg, 199_999, 3000, 0, model.HealthStateCritical},
		{"帧率等于危险线不算越界", cfg, 1_000_000, 500, 0, model.HealthStateHealthy},
		{"帧率低于危险线", cfg, 1_000_000, 499, 0, model.HealthStateCritical},
		{"丢包等于上限不算越界", cfg, 1_000_000, 3000, 50_000, model.HealthStateHealthy},
		{"丢包超上限", cfg, 1_000_000, 3000, 50_001, model.HealthStateCritical},
		{"码率落在劣化区间", cfg, 799_999, 3000, 0, model.HealthStateDegraded},
		{"CRITICAL 优先于 DEGRADED", cfg, 100_000, 10, 900_000, model.HealthStateCritical},
		{"阈值未配置则该维度不参与判定", healthThresholds{}, 1, 1, 900_000, model.HealthStateHealthy},
		{"只配了劣化线时码率再低也只判劣化",
			healthThresholds{degradedMinVideoBitrate: 800_000}, 10, 3000, 0, model.HealthStateDegraded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := judgeHealth(c.th, c.video, c.fps, c.loss); got != c.wantState {
				t.Fatalf("判定不符：got=%d want=%d", got, c.wantState)
			}
		})
	}
}

func TestReportStreamHealth_SampleWindowNormalizationReachesTheRow(t *testing.T) {
	cases := []struct {
		name      string
		requested int32
		want      int32
	}{
		{"省略时取配置默认 HealthSampleWindowSeconds", 0, 10},
		{"显式值原样落库", 7, 7},
		{"物理上限本身可落", maxSampleWindowSeconds, maxSampleWindowSeconds},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedPublishingStream(t, "S-WIN")
			e.mustReportHealth(t, healthReq("S-WIN", "rep-win", func(in *rpc.ReportStreamHealthReq) {
				in.SampleWindowSeconds = c.requested
			}))
			if got := e.healthRow(t, "rep-win").SampleWindowSeconds; got != c.want {
				t.Fatalf("采样窗口没按口径落库：requested=%d got=%d want=%d", c.requested, got, c.want)
			}
		})
	}
}

// --- 连续越界才断流 ---

func TestReportStreamHealth_SingleCriticalDoesNotInterrupt(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-ONE")
	before := e.effects()

	reply := e.mustReportHealth(t, healthReq("S-ONE", "rep-crit-1", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 50_000
	}))

	if reply.HealthState != rpc.HealthState_HEALTH_STATE_CRITICAL {
		t.Fatalf("单次越界就该判 CRITICAL：%+v", reply)
	}
	mustFalse(t, reply.TriggeredInterrupt, "单次抖动不许断流")
	if reply.Seq != 0 || reply.EventId != "" {
		t.Fatalf("没发生迁移却回带了事件：%+v", reply)
	}
	if reply.Message != "本次采样危险，但未达连续次数，暂不断流" {
		t.Fatalf("文案不符：%q", reply.Message)
	}
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "单次危险采样只多一行")
	if row := e.streamRow(t, "S-ONE"); row.State != model.StreamStatePublishing ||
		row.HealthState != model.HealthStateCritical {
		t.Fatalf("状态没动但投影也没记下危险态：%+v", row)
	}
}

func TestReportStreamHealth_ConfirmNeedsTwoCriticalInsideTheWindow(t *testing.T) {
	cases := []struct {
		name     string
		agos     []int64
		states   []int32
		wantRead int32
	}{
		{"两次危险但都在窗口外", []int64{60, 40},
			[]int32{model.HealthStateCritical, model.HealthStateCritical}, 0},
		{"一次在窗口内一次在窗口外", []int64{40, 3},
			[]int32{model.HealthStateCritical, model.HealthStateCritical}, 0},
		{"窗口内两次但早先那次正常", []int64{5, 3},
			[]int32{model.HealthStateHealthy, model.HealthStateCritical}, 0},
		{"窗口内两次危险", []int64{5, 3},
			[]int32{model.HealthStateCritical, model.HealthStateCritical}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedPublishingStream(t, "S-CONFIRM")
			for i := range c.agos {
				e.seedHealthSamples(t, "S-CONFIRM", []int64{c.agos[i]}, c.states[i])
			}
			before := e.effects()

			reply, err := e.reportHealth(t, healthReq("S-CONFIRM", "rep-judge", func(in *rpc.ReportStreamHealthReq) {
				in.PacketLossPpm = 900_000
			}))
			wantOK(t, reply, err, "越界上报")

			if c.wantRead == 1 {
				mustTrue(t, reply.TriggeredInterrupt, "窗口内连续两次危险 + 本次 = 第三次，必须断流")
				e.requireDelta(t, before, [9]int{0, 0, 0, 0, 1, 1, 0, 1, 1}, "断流要留下事件+Outbox+断流区间+采样")
				return
			}
			mustFalse(t, reply.TriggeredInterrupt, "不满足「窗口内连续」就不该断流")
			e.requireDelta(t, before, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "未断流时只多一行采样")
		})
	}
}

func TestReportStreamHealth_ConsecutiveCriticalInterruptsExactlyOnce(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-TRIP")
	e.seedHealthSamples(t, "S-TRIP", []int64{5, 3}, model.HealthStateCritical)
	before := e.effects()

	reply := e.mustReportHealth(t, healthReq("S-TRIP", "rep-trip", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 50_000
	}))

	mustTrue(t, reply.TriggeredInterrupt, "第三次连续危险采样必须断流")
	if reply.Message != "连续危险采样，已按状态机进入断流并产生事件" {
		t.Fatalf("文案不符：%q", reply.Message)
	}
	if reply.Seq != 2 || reply.EventId == "" {
		t.Fatalf("seq/event_id 没回带（迁移后 seq 应为 2）：%+v", reply)
	}
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 1, 1, 0, 1, 1}, "断流的一次性副作用面")

	row := e.streamRow(t, "S-TRIP")
	if row.State != model.StreamStateInterrupted || row.Seq != 2 {
		t.Fatalf("流没进入 INTERRUPTED：%+v", row)
	}
	if row.HealthState != model.HealthStateCritical {
		t.Fatalf("触发断流的采样必须留下 CRITICAL 痕迹：health_state=%d", row.HealthState)
	}
	open, err := e.repo.StreamInterruption.FindOpenByStream(bg(), "S-TRIP")
	wantOK(t, open, err, "断流区间已开")
	if open == nil {
		t.Fatalf("断流后没有留下未结束的区间行")
	}
	if open.EndedAt != 0 {
		t.Fatalf("断流区间没按「未结束」形态留下：ended_at=%d", open.EndedAt)
	}
	if open.EpisodeNo != 1 || open.RoomID != 51 {
		t.Fatalf("断流区间的序号/房间归属不符：%+v", open)
	}
	if open.StartEventID != reply.EventId {
		t.Fatalf("断流区间的 start_event_id 与事件不同源：%s vs %s", open.StartEventID, reply.EventId)
	}
	// 事件源必须是 health：运营据此区分「健康越界自动断流」与「心跳超时」。
	ev, err := e.repo.StreamEvent.FindByReportID(bg(), "rep-trip")
	wantOK(t, ev, err, "健康触发的迁移按同一 report_id 可查")
	if ev == nil {
		t.Fatalf("健康触发的迁移没按 report_id 留下事件，重放就查不到 seq")
	}
	if ev.Source != model.EventSourceHealth || ev.ToState != model.StreamStateInterrupted {
		t.Fatalf("事件来源/目标态不符：source=%s to=%d", ev.Source, ev.ToState)
	}

	// 继续危险也只断一次：INTERRUPTED 不再满足 confirm 的 state==PUBLISHING 前置。
	after := e.effects()
	again := e.mustReportHealth(t, healthReq("S-TRIP", "rep-trip-2", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 50_000
	}))
	mustFalse(t, again.TriggeredInterrupt, "已在 INTERRUPTED 时不该再断一次")
	e.requireDelta(t, after, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "断流后继续上报只多一行采样，不开第二个区间")
}

func TestReportStreamHealth_CriticalOnIdleStreamNeverConfirms(t *testing.T) {
	// confirm 的前置是「流正在推流」：IDLE（已建档但没推流）的流不该被健康采样踢成 INTERRUPTED。
	e := newTestEnv(t)
	e.seedStream(t, &model.Stream{StreamID: "S-IDLE", RoomID: 51, AnchorMid: 1001,
		Protocol: 1, NodeID: "node-h", State: model.StreamStateIdle})
	e.seedHealthSamples(t, "S-IDLE", []int64{5, 3}, model.HealthStateCritical)
	before := e.effects()

	reply := e.mustReportHealth(t, healthReq("S-IDLE", "rep-idle-crit", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 50_000
	}))

	mustFalse(t, reply.TriggeredInterrupt, "IDLE 流不该因危险采样进入断流")
	e.requireDelta(t, before, [9]int{0, 0, 0, 0, 0, 0, 0, 0, 1}, "IDLE 只留采样")
	if row := e.streamRow(t, "S-IDLE"); row.State != model.StreamStateIdle || row.Seq != 0 {
		t.Fatalf("IDLE 流被健康上报推进了状态：%+v", row)
	}
}

// --- 时间戳归一 ---

func TestReportStreamHealth_OccurredAtOutOfRangeRejectedBeforeStreamRead(t *testing.T) {
	cases := []struct {
		name string
		at   int64
	}{
		{"超前服务端 400 秒", 0},
		{"早于可信下限", 1500000000},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.seedPublishingStream(t, "S-SKEW")
			offset := int64(400)
			if i == 1 {
				offset = 0
			}
			at := c.at
			if i == 0 {
				at = nowUnix() + offset
			}
			before := e.effects()
			streamHits := e.call("Stream.FindOne")

			_, err := e.reportHealth(t, healthReq("S-SKEW", fmt.Sprintf("rep-skew-%d", i), func(in *rpc.ReportStreamHealthReq) {
				in.OccurredAt = at
			}))

			wantFail(t, err, model.ErrCallbackTimestampSkew, c.name)
			e.requireSameEffects(t, before, c.name)
			e.requireNoTransaction(t, before, c.name)
			if got := e.call("Stream.FindOne"); got != streamHits {
				t.Fatalf("%s：时间戳还没归一就查了流表（%d→%d）", c.name, streamHits, got)
			}
		})
	}
	// 反向对照：在 skew 之内的超前量必须被接受，否则「允许时钟偏差」这条口径就成了永假断言。
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-SKEW")
	e.mustReportHealth(t, healthReq("S-SKEW", "rep-skew-ok", func(in *rpc.ReportStreamHealthReq) {
		in.OccurredAt = nowUnix() + 120
	}))
	if got := e.healthRow(t, "rep-skew-ok").OccurredAt; got < nowUnix() {
		t.Fatalf("未超 skew 的超前时间戳被服务端时间覆盖了：%d", got)
	}
}

// --- 事务内的两种失败：残留形态必须干净 ---

func TestReportStreamHealth_StreamStoppedMidTransactionRollsBackTheSample(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-RACE")
	before := e.effects()
	e.db.forceHealthApplyMiss = true
	t.Cleanup(func() { e.db.forceHealthApplyMiss = false })

	_, err := e.reportHealth(t, healthReq("S-RACE", "rep-race"))

	wantFail(t, err, model.ErrTerminalStream, "事务内发现流已被停掉")
	e.requireSameEffects(t, before, "半截采样是最坏的一种失败：宁可让节点侧重试")
	if got := e.effects().txs; got != before.txs+1 {
		t.Fatalf("应当开过一笔事务再回滚（tx %d→%d）", before.txs, got)
	}
	if n := e.call("StreamHealthReport.Insert"); n == 0 {
		t.Fatalf("没走到采样写入就说明这条分支根本没被测到（Insert %d 次）", n)
	}
}

func TestReportStreamHealth_ConcurrentDuplicateBecomesConcurrentUpdate(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-DUP")
	before := e.effects()
	e.db.forceReportDuplicate = true
	t.Cleanup(func() { e.db.forceReportDuplicate = false })

	_, err := e.reportHealth(t, healthReq("S-DUP", "rep-dup"))

	wantFail(t, err, model.ErrConcurrentUpdate, "与并发上报撞 uniq_report_id")
	if !strings.Contains(err.Error(), "duplicate health report id") {
		t.Fatalf("错误里没留下「duplicate health report id」线索，运维无法与别的并发失败区分：%v", err)
	}
	e.requireSameEffects(t, before, "撞唯一键后整笔回滚，不留半截事件")
}

// --- 隐私 ---

func TestReportStreamHealth_LogsNoMetricValuesAndKeepsReplyFreeOfKeyMaterial(t *testing.T) {
	e := newTestEnv(t)
	e.seedPublishingStream(t, "S-PRIV")
	logs := captureLogs(t)
	_, err := e.reportHealth(t, healthReq("S-PRIV", "rep-priv", func(in *rpc.ReportStreamHealthReq) {
		in.VideoBitrateBps = 50_000
	}))
	wantOK(t, true, err, "危险采样本身是成功的")
	joined := logs.joined()
	requireAbsent(t, joined, "vault:secret/data/live-ingest", "健康上报不碰密钥引用")
	requireAbsent(t, joined, "rep-priv", "幂等键不进日志（它标识一次上报，不是排障线索）")
	if strings.Contains(joined, "50000") || strings.Contains(joined, "100000") {
		t.Fatalf("健康上报把逐条指标值刷进了日志（会淹没日志聚合）：\n%s", joined)
	}
}
