package logic

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"go-video/services/playback/model"
	rpc "go-video/services/playback/rpc"
)

const hbSessionID = "01HEARTBEATSESSION0000000EE"

// hbEnvelope 是 playback_outbox.payload 的解码视图（只取本用例要断言的字段）。
type hbEnvelope struct {
	EventID       string `json:"event_id"`
	EventType     string `json:"event_type"`
	SchemaVersion int    `json:"schema_version"`
	OccurredAt    string `json:"occurred_at"`
	Producer      string `json:"producer"`
	TraceID       string `json:"trace_id"`
	AggregateType string `json:"aggregate_type"`
	AggregateID   string `json:"aggregate_id"`
	Payload       struct {
		SessionID      string  `json:"session_id"`
		ContentType    int32   `json:"content_type"`
		ContentID      int64   `json:"content_id"`
		MidHash        string  `json:"mid_hash"`
		Guest          bool    `json:"guest"`
		PositionMs     int64   `json:"position_ms"`
		DurationMs     int64   `json:"duration_ms"`
		MaxPositionMs  int64   `json:"max_position_ms"`
		BufferCount    int32   `json:"buffer_count"`
		Completion     float64 `json:"completion"`
		SessionExpired bool    `json:"session_expired"`
	} `json:"payload"`
}

// seedHBSession 布一个可播的 UGC 会话；heartbeat 只读会话事实，不校验签名。
func seedHBSession(t *testing.T, e *env, expireAt int64) *model.PlaybackSession {
	t.Helper()
	return seedSession(t, e.st, &model.PlaybackSession{
		SessionId: hbSessionID, ContentType: model.ContentTypeUGC, ContentId: 700, Vid: "BV700",
		Mid: 4242, Platform: model.PlatformDesktop, AppVersion: "9.9.9", Region: "CN",
		Uri: ugcURI, ObjectKey: "ugc/12/34/700.m3u8", RequestId: "req-hb-1",
		ExpireAt: expireAt, State: model.SessionStateActive, Ctime: expireAt - 600,
	})
}

func hbReq(pos, dur int64) *rpc.ReportHeartbeatReq {
	return &rpc.ReportHeartbeatReq{
		SessionId: hbSessionID, PositionMs: pos, DurationMs: dur,
		BufferCount: 2, AvgBitrate: 4_000_000, TraceId: "trace-hb",
	}
}

func reportHB(t *testing.T, e *env, in *rpc.ReportHeartbeatReq) (*rpc.ReportHeartbeatReply, error) {
	t.Helper()
	return NewReportHeartbeatLogic(context.Background(), e.svcCtx).ReportHeartbeat(in)
}

// decodeOutbox 取出 outbox 唯一那一行并解码信封。
func decodeOutbox(t *testing.T, e *env) (model.PlaybackOutbox, hbEnvelope) {
	t.Helper()
	row := e.st.outbox.only()
	var env hbEnvelope
	if err := json.Unmarshal([]byte(row.Payload), &env); err != nil {
		t.Fatalf("outbox payload 不是合法信封 JSON：%v（原文 %s）", err, row.Payload)
	}
	return *row, env
}

// TestReportHeartbeatRejectsInvalidRequests 心跳入参守卫必须先看参数再看会话：
// 一个字段非法的客户端不该能通过刷 session_id 消耗数据库读。
func TestReportHeartbeatRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		in   *rpc.ReportHeartbeatReq
		want error
	}{
		{"缺少 session_id", &rpc.ReportHeartbeatReq{PositionMs: 1000, DurationMs: 2000}, model.ErrMissingSessionID},
		{"位置为负", hbReq(-1, 2000), model.ErrInvalidHeartbeat},
		{"时长为负", hbReq(1000, -1), model.ErrInvalidHeartbeat},
		{"时长超过 48 小时上限", hbReq(1000, model.MaxDurationMS+1), model.ErrInvalidHeartbeat},
		{"位置超过时长", hbReq(2001, 2000), model.ErrInvalidHeartbeat},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seedHBSession(t, e, nowPlus(600))
			before := e.st.log.snapshot()

			reply, err := reportHB(t, e, tc.in)

			wantErrIs(t, tc.name, err, tc.want)
			if reply != nil {
				t.Errorf("%s：非法心跳仍被受理 %+v", tc.name, reply)
			}
			wantNoCall(t, tc.name, e.st, before)
			if len(e.st.outbox.rows) != 0 {
				t.Errorf("%s：非法心跳仍写入了事件", tc.name)
			}
		})
	}
}

// TestReportHeartbeatAcceptsUnknownDuration 时长未知（0）时不能把正常心跳判成非法：
// 直播与流式场景下客户端拿不到 duration，位置只带自身。
func TestReportHeartbeatAcceptsUnknownDuration(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(600))

	reply, err := reportHB(t, e, hbReq(45_000, 0))

	wantNoErr(t, "时长未知", err)
	wantEQ(t, "时长未知", "max_position_ms", reply.MaxPositionMs, int64(45_000))
}

// TestReportHeartbeatWritesProgressAndOutboxTogether 是 AGENTS.md §5 的 Outbox 口径：
// 业务写入与事件写入在同一个事务里，事件状态是待发布，索引列与信封一致。
func TestReportHeartbeatWritesProgressAndOutboxTogether(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(600))
	before := e.st.log.snapshot()
	calledAt := time.Now().Unix()

	reply, err := reportHB(t, e, hbReq(30_000, 60_000))
	wantNoErr(t, "首次心跳", err)

	// 1) 进度落库
	p := e.st.progress.only()
	wantEQ(t, "进度落库", "session_id", p.SessionId, hbSessionID)
	wantEQ(t, "进度落库", "position_ms", p.PositionMs, int64(30_000))
	wantEQ(t, "进度落库", "duration_ms", p.DurationMs, int64(60_000))
	wantEQ(t, "进度落库", "buffer_count", p.BufferCount, int32(2))
	wantEQ(t, "进度落库", "avg_bitrate", p.AvgBitrate, int64(4_000_000))
	wantEQ(t, "进度落库", "内容主键取自会话而非入参", p.ContentId, int64(700))
	wantEQ(t, "进度落库", "mid 取自会话", p.Mid, int64(4242))
	wantEQ(t, "进度落库", "vid 取自会话", p.Vid, "BV700")

	// 2) 事件落库
	row, env := decodeOutbox(t, e)
	wantEQ(t, "事件落库", "event_type", row.EventType, model.EventPlaybackHeartbeat)
	wantEQ(t, "事件落库", "schema_version", row.SchemaVersion, int32(model.EventSchemaVersion))
	wantEQ(t, "事件落库", "aggregate_type", row.AggregateType, model.AggregateTypeSession)
	wantEQ(t, "事件落库", "aggregate_id", row.AggregateID, hbSessionID)
	wantEQ(t, "事件落库", "state 必须是待发布", row.State, int32(model.OutboxStatePending))
	wantEQ(t, "事件落库", "event_id 与响应一致", row.EventID, reply.EventId)
	if row.EventID == "" {
		t.Errorf("事件落库：event_id 为空，消费者无法按它幂等去重")
	}
	if row.OccurredAt < calledAt-2 || row.OccurredAt > calledAt+2 {
		t.Errorf("事件落库：occurred_at = %d, want ≈%d（表列必须与信封 occurred_at 同源）", row.OccurredAt, calledAt)
	}
	wantEQ(t, "信封", "event_type", env.EventType, model.EventPlaybackHeartbeat)
	wantEQ(t, "信封", "producer", env.Producer, model.Producer)
	wantEQ(t, "信封", "aggregate_id", env.AggregateID, hbSessionID)
	wantEQ(t, "信封", "trace_id 透传", env.TraceID, "trace-hb")
	wantEQ(t, "信封负载", "max_position_ms", env.Payload.MaxPositionMs, int64(30_000))
	wantEQ(t, "信封负载", "session_id", env.Payload.SessionID, hbSessionID)
	wantEQ(t, "信封负载", "completion（半程）", env.Payload.Completion, float64(0.5))
	wantEQ(t, "信封负载", "session_expired", env.Payload.SessionExpired, false)

	// 3) 两次写入必须在同一个事务会话里
	wantEQ(t, "事务边界", "开启的事务数", e.st.conn.transactions, 1)
	if !e.st.progress.gotTx[hbSessionID] || !e.st.outbox.gotTx {
		t.Errorf("进度或事件未走事务会话（gotTx progress=%v outbox=%v），Outbox 与业务写入可能分离",
			e.st.progress.gotTx[hbSessionID], e.st.outbox.gotTx)
	}
	wantEQ(t, "响应", "accepted_at ≈ 调用时刻", reply.AcceptedAt >= calledAt && reply.AcceptedAt <= time.Now().Unix()+1, true)
	wantOps(t, "首次心跳的调用链", e.st.log.opsFrom(before), []string{
		"cache.GetSession:" + keySession(hbSessionID),
		"session.FindOne:" + hbSessionID,
		"cache.SetSession:" + keySession(hbSessionID),
		"progress.FindOne:" + hbSessionID,
		"progress.Upsert:" + hbSessionID + "/30000",
		"outbox.Insert:" + model.EventPlaybackHeartbeat + "/" + hbSessionID,
	})
}

// TestReportHeartbeatPositionNeverGoesBackwards 断点是用户资产：弱网重试与客户端乱序
// 上报不能把已记录的进度往回拽；事件里的 max_position_ms 必须与库里一致。
func TestReportHeartbeatPositionNeverGoesBackwards(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(600))
	seedProgress(t, e.st, &model.PlaybackProgress{
		SessionId: hbSessionID, ContentType: model.ContentTypeUGC, ContentId: 700, Mid: 4242,
		PositionMs: 90_000, DurationMs: 120_000, BufferCount: 3, AvgBitrate: 1_000,
		LastError: 7, Ctime: nowPlus(-500), Mtime: nowPlus(-400),
	})

	reply, err := reportHB(t, e, &rpc.ReportHeartbeatReq{
		SessionId: hbSessionID, PositionMs: 10_000, DurationMs: 130_000, BufferCount: 2,
	})
	wantNoErr(t, "回退心跳", err)

	wantEQ(t, "回退心跳", "响应 max_position_ms", reply.MaxPositionMs, int64(90_000))
	p := e.st.progress.only()
	wantEQ(t, "回退心跳", "库里位置不回退", p.PositionMs, int64(90_000))
	wantEQ(t, "回退心跳", "卡顿次数累加", p.BufferCount, int32(5))
	wantEQ(t, "回退心跳", "时长以最新上报为准", p.DurationMs, int64(130_000))

	_, env := decodeOutbox(t, e)
	wantEQ(t, "回退心跳", "事件里的 max 与库里一致", env.Payload.MaxPositionMs, p.PositionMs)
	wantEQ(t, "回退心跳", "事件里的本次位置如实上报", env.Payload.PositionMs, int64(10_000))
}

// TestReportHeartbeatLateHeartbeatKeepsProgress 会话过期后的迟到心跳仍然记账（断点属于用户），
// 但事件必须带 session_expired，下游据此过滤质量统计。
func TestReportHeartbeatLateHeartbeatKeepsProgress(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(-60))

	reply, err := reportHB(t, e, hbReq(50_000, 60_000))
	wantNoErr(t, "迟到心跳", err)
	wantEQ(t, "迟到心跳", "进度仍被记录", reply.MaxPositionMs, int64(50_000))

	_, env := decodeOutbox(t, e)
	wantEQ(t, "迟到心跳", "session_expired 标记", env.Payload.SessionExpired, true)
	// 记账不等于放行：本方法不产生任何播放授权。
	wantCount(t, "迟到心跳", e.st.log, "cache.Incr", 0)
	wantCount(t, "迟到心跳", e.st.log, "cache.MarkVerified", 0)
}

// TestReportHeartbeatFailsWhenSessionMissingOrStoreDown 会话读不到时必须报错：
// 既不能凭空造一条进度，也不能"当作没发生"返回成功。
func TestReportHeartbeatFailsWhenSessionMissingOrStoreDown(t *testing.T) {
	t.Run("会话不存在", func(t *testing.T) {
		e := newEnv(t)

		reply, err := reportHB(t, e, hbReq(1000, 2000))

		wantErrIs(t, "会话不存在", err, model.ErrSessionNotFound)
		if reply != nil {
			t.Errorf("会话不存在却受理 %+v", reply)
		}
		wantCount(t, "会话不存在", e.st.log, "outbox.Insert", 0)
	})

	t.Run("会话查询失败", func(t *testing.T) {
		e := newEnv(t)
		boom := errors.New("playback: mysql is gone")
		e.st.sessions.failWith("FindOne", boom)

		_, err := reportHB(t, e, hbReq(1000, 2000))

		wantErrIs(t, "会话查询失败", err, boom)
		wantCount(t, "会话查询失败", e.st.log, "outbox.Insert", 0)
	})

	t.Run("既有进度读取失败", func(t *testing.T) {
		e := newEnv(t)
		seedHBSession(t, e, nowPlus(600))
		boom := errors.New("playback: mysql is gone")
		e.st.progress.failWith("FindOne", boom)

		_, err := reportHB(t, e, hbReq(1000, 2000))

		wantErrIs(t, "进度读取失败", err, boom)
		wantCount(t, "进度读取失败", e.st.log, "progress.Upsert", 0)
	})
}

// TestReportHeartbeatRollsBackOutboxWithProgress 事件写入失败时整个事务必须失败：
// 进度已写入也不能对客户端报成功，否则下游会永久缺这一条心跳事件。
func TestReportHeartbeatRollsBackOutboxWithProgress(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(600))
	boom := errors.New("playback: outbox insert rejected")
	e.st.outbox.failWith("Insert", boom)

	reply, err := reportHB(t, e, hbReq(30_000, 60_000))

	wantErrIs(t, "事件写入失败", err, boom)
	if reply != nil {
		t.Errorf("事件写入失败却返回了成功响应 %+v", reply)
	}
	wantEQ(t, "事件写入失败", "事务以失败收场", e.st.conn.rolledBack, 1)
	wantCount(t, "事件写入失败", e.st.log, "progress.Upsert", 1)
	// 注意：真实 MySQL 会连同进度一起回滚；替身不做回滚，见 fakes_test.go 覆盖边界。
}

// TestReportHeartbeatProgressFailureWritesNoEvent 进度写失败时必须短路，
// 绝不能只把事件写进去（那会让下游收到一条库里没有对应断点的"幽灵心跳"）。
func TestReportHeartbeatProgressFailureWritesNoEvent(t *testing.T) {
	e := newEnv(t)
	seedHBSession(t, e, nowPlus(600))
	boom := errors.New("playback: progress write rejected")
	e.st.progress.failWith("Upsert", boom)

	_, err := reportHB(t, e, hbReq(30_000, 60_000))

	wantErrIs(t, "进度写失败", err, boom)
	wantCount(t, "进度写失败", e.st.log, "outbox.Insert", 0)
	wantEQ(t, "进度写失败", "事务标记回滚", e.st.conn.rolledBack, 1)
	if len(e.st.outbox.rows) != 0 {
		t.Errorf("进度写失败仍落了 %d 行事件", len(e.st.outbox.rows))
	}
}

// TestReportHeartbeatDoesNotReplayCacheErrors 心跳只读会话，不碰签名、不碰版权、不碰计数。
func TestReportHeartbeatDoesNotReplayCacheErrors(t *testing.T) {
	e := newEnv(t)
	row := seedHBSession(t, e, nowPlus(600))
	e.st.cache.warm(row)

	_, err := reportHB(t, e, hbReq(30_000, 60_000))
	wantNoErr(t, "心跳走缓存", err)

	wantCount(t, "心跳走缓存", e.st.log, "session.FindOne", 0)
	wantCount(t, "心跳走缓存", e.st.log, "rights.CheckPlayable", 0)
	wantCount(t, "心跳走缓存", e.st.log, "cache.MarkVerified", 0)
	wantCount(t, "心跳走缓存", e.st.log, "cache.Incr", 0)
}
