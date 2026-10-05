package logic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
	"go-video/services/playback/model"
	rpc "go-video/services/playback/rpc"
)

const (
	testSessionID = "01HZX0000000000000000000PB" // 26 位，合法 ULID 形状
	testMid       = int64(987654321)
)

// heartbeatTestSession 构造一个用于 payload 断言的会话。
func heartbeatTestSession() *model.PlaybackSession {
	return &model.PlaybackSession{
		SessionId:   testSessionID,
		ContentType: model.ContentTypeUGC,
		ContentId:   700,
		Vid:         "BV700",
		Mid:         testMid,
		Platform:    model.PlatformAndroid,
		AppVersion:  "1.2.3",
		Region:      "cn-zhejiang-hangzhou",
		ObjectKey:   "/ugc/12/34/700.m3u8",
		Uri:         "/ugc/12/34/700.m3u8",
		RequestId:   "req-1",
		ExpireAt:    2000,
		State:       model.SessionStateActive,
	}
}

// TestBuildHeartbeatPayloadFields 校验心跳事件 payload 的业务字段与幂等字段组装正确。
func TestBuildHeartbeatPayloadFields(t *testing.T) {
	s := heartbeatTestSession()
	in := &rpc.ReportHeartbeatReq{
		SessionId:   s.SessionId,
		PositionMs:  120000,
		DurationMs:  240000,
		BufferCount: 5,
		AvgBitrate:  1800,
		LastError:   7,
		TraceId:     "trace-1",
	}

	raw, err := buildHeartbeatPayload(s, in, 150000, 1000)
	if err != nil {
		t.Fatalf("buildHeartbeatPayload err = %v", err)
	}

	var got heartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("payload 不是合法 JSON: %v", err)
	}
	if got.SessionID != s.SessionId {
		t.Errorf("session_id = %q, want %q", got.SessionID, s.SessionId)
	}
	if got.ContentType != model.ContentTypeUGC || got.ContentID != 700 {
		t.Errorf("内容标识 = (%d,%d), want (%d,700)", got.ContentType, got.ContentID, model.ContentTypeUGC)
	}
	if got.Vid != "BV700" {
		t.Errorf("vid = %q, want BV700", got.Vid)
	}
	if got.Platform != model.PlatformAndroid {
		t.Errorf("platform = %d, want %d", got.Platform, model.PlatformAndroid)
	}
	if got.AppVersion != "1.2.3" || got.Region != "cn-zhejiang-hangzhou" {
		t.Errorf("app_version/region = %q/%q", got.AppVersion, got.Region)
	}
	// max_position_ms 必须来自服务端（DB 单调最大值），不能是客户端上报值
	if got.MaxPositionMs != 150000 {
		t.Errorf("max_position_ms = %d, want 150000（服务端单调最大值）", got.MaxPositionMs)
	}
	if got.PositionMs != 120000 || got.DurationMs != 240000 {
		t.Errorf("position/duration = %d/%d, want 120000/240000", got.PositionMs, got.DurationMs)
	}
	if got.BufferCount != 5 || got.AvgBitrate != 1800 || got.LastError != 7 {
		t.Errorf("质量指标 = %d/%d/%d, want 5/1800/7", got.BufferCount, got.AvgBitrate, got.LastError)
	}
	if got.Completion != 0.5 {
		t.Errorf("completion = %v, want 0.5", got.Completion)
	}
	if got.Guest {
		t.Errorf("登录用户 guest = true, want false")
	}
	if got.SessionExpired {
		t.Errorf("expire_at=2000 > now=1000，session_expired 应为 false")
	}
}

// TestBuildHeartbeatPayloadSessionExpiredFlag 迟到心跳必须带 session_expired 标记，
// 但进度仍照常上报，供下游过滤质量统计。
func TestBuildHeartbeatPayloadSessionExpiredFlag(t *testing.T) {
	s := heartbeatTestSession()
	s.ExpireAt = 1000
	raw, err := buildHeartbeatPayload(s, &rpc.ReportHeartbeatReq{SessionId: s.SessionId}, 0, 1001)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got heartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if !got.SessionExpired {
		t.Errorf("session_expired = false, want true")
	}
}

// TestHeartbeatPayloadHasNoPII 校验 AGENTS.md §6：事件 payload 不含明文 IP、
// Token/签名串、播放地址与明文 mid，用户标识只能是摘要。
func TestHeartbeatPayloadHasNoPII(t *testing.T) {
	s := heartbeatTestSession()
	in := &rpc.ReportHeartbeatReq{SessionId: s.SessionId, PositionMs: 10, DurationMs: 100}

	raw, err := buildHeartbeatPayload(s, in, 10, 1000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	env, err := eventenvelope.New(model.Producer, model.EventPlaybackHeartbeat,
		model.AggregateTypeSession, s.SessionId, model.EventSchemaVersion, raw, in.TraceId)
	if err != nil {
		t.Fatalf("eventenvelope.New err = %v", err)
	}
	blob, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope err = %v", err)
	}
	text := string(blob)
	// 字段名断言针对 payload 本身：信封里 payload 是转义后的字符串，键名会被转义成 \" 而检查不到
	for _, forbidden := range []string{"auth_key", "private_key", "play_url", "\"ip\"", "\"uri\"", "\"object_key\"", "\"request_id\""} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("payload 含敏感字段 %q: %s", forbidden, raw)
		}
	}
	// 明文 mid 与签名目标路径（含文件名）都不得出现在序列化后的事件里
	for _, leak := range []string{"987654321", s.ObjectKey} {
		if strings.Contains(text, leak) {
			t.Errorf("envelope 泄漏敏感值 %q: %s", leak, text)
		}
	}

	var got heartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if len(got.MidHash) != 32 {
		t.Errorf("mid_hash 长度 = %d, want 32", len(got.MidHash))
	}
	if !isHex(got.MidHash) {
		t.Errorf("mid_hash 不是小写十六进制: %q", got.MidHash)
	}
}

// TestHeartbeatPayloadGuestWhenAnonymous 游客（mid<=0）时 guest=true 且 mid_hash 为空串。
func TestHeartbeatPayloadGuestWhenAnonymous(t *testing.T) {
	s := heartbeatTestSession()
	s.Mid = 0
	raw, err := buildHeartbeatPayload(s, &rpc.ReportHeartbeatReq{SessionId: s.SessionId}, 0, 1000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got heartbeatPayload
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal err = %v", err)
	}
	if !got.Guest {
		t.Errorf("guest = false, want true")
	}
	if got.MidHash != "" {
		t.Errorf("游客 mid_hash = %q, want 空串", got.MidHash)
	}
}

// TestHashMidStableAndDomainSeparated hash 必须稳定、可跨进程复现，且不同 mid 不相等。
func TestHashMidStableAndDomainSeparated(t *testing.T) {
	if hashMid(testMid) != hashMid(testMid) {
		t.Errorf("同一 mid 两次 hash 不同")
	}
	if hashMid(testMid) == hashMid(testMid+1) {
		t.Errorf("不同 mid 的 hash 相同")
	}
	if hashMid(0) != "" || hashMid(-1) != "" {
		t.Errorf("游客 mid 的 hash 应为空串")
	}
	// 域分离：与"裸 mid 十进制的 sha256 前 32 位"不同，避免跨链路通用假名被反查表命中
	naked := sha256.Sum256([]byte(strconv.FormatInt(testMid, 10)))
	if hashMid(testMid) == hex.EncodeToString(naked[:])[:32] {
		t.Errorf("mid_hash 未做域分离，可能与其他链路假名通用")
	}
	// 锁定摘要形状：下游用它做用户级去重，格式变化属于破坏性契约变更
	if len(hashMid(testMid)) != 32 || !isHex(hashMid(testMid)) {
		t.Errorf("mid_hash 形状变化: %q", hashMid(testMid))
	}
}

// TestCompletionRatio 完播比例边界：总时长非正、位置超过总时长都要收敛到 [0,1]。
func TestCompletionRatio(t *testing.T) {
	cases := []struct {
		name     string
		position int64
		duration int64
		want     float64
	}{
		{"正常半程", 5000, 10000, 0.5},
		{"零时长", 1000, 0, 0},
		{"负时长", 1000, -1, 0},
		{"零位置", 0, 10000, 0},
		{"超出时长收敛为 1", 20000, 10000, 1},
		{"刚好播完", 10000, 10000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := completionRatio(tc.position, tc.duration); got != tc.want {
				t.Errorf("completionRatio(%d,%d) = %v, want %v", tc.position, tc.duration, got, tc.want)
			}
		})
	}
}

// TestHeartbeatEnvelopeTopicAndContract 事件信封必须符合 docs/api-and-events.md §4/§5：
// topic 为 playback.heartbeat.v1，payload 原样入库供消费者按 event_id 去重。
func TestHeartbeatEnvelopeTopicAndContract(t *testing.T) {
	s := heartbeatTestSession()
	raw, err := buildHeartbeatPayload(s, &rpc.ReportHeartbeatReq{
		SessionId: s.SessionId, PositionMs: 1000, DurationMs: 2000,
	}, 1000, 1000)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	env, err := eventenvelope.New(model.Producer, model.EventPlaybackHeartbeat,
		model.AggregateTypeSession, s.SessionId, model.EventSchemaVersion, raw, "trace-9")
	if err != nil {
		t.Fatalf("eventenvelope.New err = %v", err)
	}
	if topic := eventenvelope.Topic(env.EventType, env.SchemaVersion); topic != "playback.heartbeat.v1" {
		t.Errorf("topic = %q, want playback.heartbeat.v1", topic)
	}
	if env.Producer != model.Producer || env.AggregateType != model.AggregateTypeSession {
		t.Errorf("producer/aggregate_type = %q/%q", env.Producer, env.AggregateType)
	}
	if env.AggregateID != s.SessionId {
		t.Errorf("aggregate_id = %q, want %q", env.AggregateID, s.SessionId)
	}
	if env.TraceID != "trace-9" {
		t.Errorf("trace_id = %q, want trace-9", env.TraceID)
	}
	if string(env.Payload) != string(raw) {
		t.Errorf("envelope.payload 与 buildHeartbeatPayload 输出不一致")
	}
	// 消费侧按 event_id 幂等：event_id 必须非空且为 26 位 ULID
	if len(env.EventID) != 26 {
		t.Errorf("event_id 长度 = %d, want 26", len(env.EventID))
	}
}

func isHex(s string) bool {
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return len(s) > 0
}
