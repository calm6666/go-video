package consumer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go-video/common/eventenvelope"
	"go-video/services/inbox/model"
)

// mapping_test.go 覆盖「事件 payload -> 站内信」映射：收件人解析、分类与文案、
// 幂等键派生、长度收敛，以及哪些事件按契约应当跳过（不是错误）。
// 全程不连接 Kafka/MySQL/Redis。

const testOccurredAt = "2026-01-02T03:04:05Z"

// envelopeJSON 构造一个能通过 eventenvelope.Validate 的信封原文。
func envelopeJSON(t *testing.T, eventID, eventType, payload string) string {
	t.Helper()
	raw, err := json.Marshal(eventenvelope.Envelope{
		EventID:       eventID,
		EventType:     eventType,
		SchemaVersion: 1,
		OccurredAt:    testOccurredAt,
		Producer:      "engagement.v1.rpc",
		TraceID:       "trace-1",
		AggregateType: "content",
		AggregateID:   "8899",
		Payload:       json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("构造信封失败: %v", err)
	}
	return string(raw)
}

func mustParse(t *testing.T, raw string) *eventenvelope.Envelope {
	t.Helper()
	env, err := ParseEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("ParseEnvelope 失败: %v", err)
	}
	return env
}

// --- 信封解析 ---

func TestParseEnvelopeRejectsBadInput(t *testing.T) {
	if _, err := ParseEnvelope(nil); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("空原文期望 ErrEmptyPayload，得到 %v", err)
	}
	if _, err := ParseEnvelope([]byte("{")); err == nil {
		t.Fatal("非法 JSON 必须报错")
	}
	// 缺 producer：信封契约不满足，ParseEnvelope 走 UnmarshalJSON -> Validate。
	if _, err := ParseEnvelope([]byte(`{"event_id":"x","event_type":"engagement.action",` +
		`"schema_version":1,"occurred_at":"2026-01-02T03:04:05Z","aggregate_type":"content",` +
		`"aggregate_id":"1","payload":{}}`)); err == nil {
		t.Fatal("缺 producer 的信封必须被 Validate 拒绝")
	}
}

func TestParseEnvelopeIgnoresUnknownFields(t *testing.T) {
	// 生产者新增字段不得打挂消费者（docs §2 兼容窗口）。
	raw := envelopeJSON(t, "01EVENTIDAAA", EventTypeEngagementAction,
		`{"action":"like","target_mid":200,"mid":100,"content_title":"标题","future_field":{"a":1}}`)
	env := mustParse(t, raw)
	res, err := BuildMessage(env)
	if err != nil {
		t.Fatalf("未知字段不应影响映射: %v", err)
	}
	if res.Skip || len(res.Recipients) != 1 || res.Recipients[0] != 200 {
		t.Fatalf("收件人解析异常：%+v", res.Recipients)
	}
}

// --- 互动事件 ---

func TestBuildEngagementLike(t *testing.T) {
	raw := envelopeJSON(t, "evt-like-1", EventTypeEngagementAction,
		`{"action":"like","mid":100,"target_mid":200,"content_id":55,"content_title":"我的第一次投稿","link":"/video/55"}`)
	res, err := BuildMessage(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	if res.Skip {
		t.Fatal("点赞必须产生站内信")
	}
	msg := res.Message
	if msg.Category != model.CategoryEngagement {
		t.Fatalf("点赞消息分类期望 %d，得到 %d", model.CategoryEngagement, msg.Category)
	}
	if msg.Title != "收到新的赞" {
		t.Fatalf("标题不符：%q", msg.Title)
	}
	if msg.Content != "你的作品《我的第一次投稿》收到了新的赞" {
		t.Fatalf("正文不符：%q", msg.Content)
	}
	if msg.MsgType != model.MsgTypeLink {
		t.Fatalf("带 link 的消息期望 MsgTypeLink，得到 %d", msg.MsgType)
	}
	if msg.SenderMid != 100 {
		t.Fatalf("发送方应为触发者 100，得到 %d", msg.SenderMid)
	}
	if msg.IdempotencyKey != "evt:evt-like-1" {
		t.Fatalf("幂等键必须由 event_id 派生，得到 %q", msg.IdempotencyKey)
	}
	if msg.BizType != EventTypeEngagementAction || msg.BizID != "8899" {
		t.Fatalf("业务标识不符：%q/%q", msg.BizType, msg.BizID)
	}
	wantTime, err := time.Parse(time.RFC3339, testOccurredAt)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Ctime != wantTime.Unix() {
		t.Fatalf("ctime 应取信封 occurred_at，期望 %d 得到 %d", wantTime.Unix(), msg.Ctime)
	}
	if len(res.Recipients) != 1 || res.Recipients[0] != 200 {
		t.Fatalf("收件人期望 [200]，得到 %v", res.Recipients)
	}

	var extra extraJSON
	if err := json.Unmarshal([]byte(msg.Extra), &extra); err != nil {
		t.Fatalf("extra 必须是合法 JSON: %v", err)
	}
	if extra.EventID != "evt-like-1" || extra.Topic != TopicEngagementAction ||
		extra.Action != "like" || extra.Link != "/video/55" || extra.TraceID != "trace-1" {
		t.Fatalf("extra 字段不完整：%+v", extra)
	}
}

func TestBuildEngagementAuthorFallbackAndSelfLike(t *testing.T) {
	// target_mid 缺失时回退 author_mid。
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-fb", EventTypeEngagementAction,
		`{"action":"favorite","mid":100,"author_mid":300}`)))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	if len(res.Recipients) != 1 || res.Recipients[0] != 300 {
		t.Fatalf("期望回退到 author_mid=300，得到 %v", res.Recipients)
	}
	if res.Message.MsgType != model.MsgTypeText {
		t.Fatalf("无 link 时期望 MsgTypeText，得到 %d", res.Message.MsgType)
	}

	// 自己给自己点赞：不产生站内信，但不是错误。
	res, err = BuildMessage(mustParse(t, envelopeJSON(t, "evt-self", EventTypeEngagementAction,
		`{"action":"like","mid":100,"target_mid":100}`)))
	if err != nil {
		t.Fatalf("自点赞不应报错: %v", err)
	}
	if !res.Skip {
		t.Fatal("自己给自己互动不应投递站内信")
	}
	if res.Reason == "" {
		t.Fatal("跳过必须给出原因，便于日志排查")
	}
}

func TestEngagementRevokeActionsAndUnknownActionSkip(t *testing.T) {
	for _, payload := range []string{
		`{"action":"cancel_like","mid":100,"target_mid":200}`,
		`{"action":"unfollow","mid":100,"target_mid":200}`,
		`{"action":"","mid":100,"target_mid":200}`,
		`{"action":"whatever_new","mid":100,"target_mid":200}`,
	} {
		res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-skip", EventTypeEngagementAction, payload)))
		if err != nil {
			t.Fatalf("%s 不应报错: %v", payload, err)
		}
		if !res.Skip {
			t.Fatalf("%s 应跳过投递", payload)
		}
	}
}

func TestFollowMessageHasNoUnrenderedPlaceholder(t *testing.T) {
	// 「有人关注了你」没有 %s 占位：直接 Sprintf 会在正文尾部留下 %!(EXTRA ...)。
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-follow", EventTypeEngagementAction,
		`{"action":"follow","mid":100,"target_mid":200}`)))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	if strings.ContainsAny(res.Message.Content, "%!") {
		t.Fatalf("正文残留格式化占位符：%q", res.Message.Content)
	}
	if res.Message.Content != "有人关注了你" {
		t.Fatalf("正文不符：%q", res.Message.Content)
	}
}

// --- 内容与直播事件 ---

func TestBuildContentOfflineAppendsReason(t *testing.T) {
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-offline", EventTypeContentPublished,
		`{"action":"offline","content_id":77,"title":"被下架的视频","author_mid":400,"reason":"版权投诉"}`)))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	if res.Message.Category != model.CategoryContent {
		t.Fatalf("期望分类 %d，得到 %d", model.CategoryContent, res.Message.Category)
	}
	if res.Message.Content != "你的作品《被下架的视频》已下架：版权投诉" {
		t.Fatalf("正文应带上游原因，得到 %q", res.Message.Content)
	}
	if res.Recipients[0] != 400 {
		t.Fatalf("应通知作者本人，得到 %v", res.Recipients)
	}
}

func TestContentPublishAndLiveStartSkipWithoutFanout(t *testing.T) {
	// 面向粉丝的群发不由 inbox 决定：拿不到收件人就跳过，绝不自行 fan-out。
	cases := []struct{ eventType, payload string }{
		{EventTypeContentPublished, `{"action":"publish","content_id":1,"author_mid":400}`},
		{EventTypeContentPublished, `{"action":"update","content_id":1,"author_mid":400}`},
		{EventTypeLiveState, `{"action":"start","room_id":9,"anchor_mid":500}`},
		{EventTypeContentPublished, `{"action":"delete","content_id":1}`}, // 没有收件人
	}
	for _, c := range cases {
		res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-nofanout", c.eventType, c.payload)))
		if err != nil {
			t.Fatalf("%s 不应报错: %v", c.payload, err)
		}
		if !res.Skip {
			t.Fatalf("%s %s 应跳过而不是自创收件人", c.eventType, c.payload)
		}
	}
}

func TestLiveStateMessagesUseAnchorAsRecipient(t *testing.T) {
	for _, action := range []string{"interrupt", "stop", "ban"} {
		res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-live", EventTypeLiveState,
			`{"action":"`+action+`","room_id":1234,"anchor_mid":500,"title":"深夜电台"}`)))
		if err != nil {
			t.Fatalf("%s 不应报错: %v", action, err)
		}
		if res.Skip || res.Message.Category != model.CategoryLive {
			t.Fatalf("%s 应产生分类 %d 的站内信：%+v", action, model.CategoryLive, res)
		}
		if !strings.Contains(res.Message.Content, "深夜电台") {
			t.Fatalf("%s 正文缺少直播标题：%q", action, res.Message.Content)
		}
		if len(res.Recipients) != 1 || res.Recipients[0] != 500 {
			t.Fatalf("%s 应只通知主播：%v", action, res.Recipients)
		}
	}
	// unban 等不产生站内信的动作走 skip。
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-unban", EventTypeLiveState,
		`{"action":"unban","room_id":1,"anchor_mid":500}`)))
	if err != nil || !res.Skip {
		t.Fatalf("unban 应跳过：%+v err=%v", res, err)
	}
}

// TestLiveStateFromLiveIngestPayload 消费 live-ingest 实际发出的 payload 形态。
//
// 这份 JSON 逐字抄自 live-ingest 的 stateEventPayload
// （services/live-ingest/internal/logic/streamstate.go，见 docs/api-and-events.md §5 的
// live.state.v1 payload 表）：生产者是「状态迁移事实」而不是动作名。
// 本用例是那条链路两侧字段名的锚点，缺它就会出现「生产端在发、消费端永远 skip」。
func TestLiveStateFromLiveIngestPayload(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantSkip  bool
		wantTitle string
	}{
		{
			name:      "INTERRUPTED 通知主播",
			payload:   `{"stream_id":"st_1F4","room_id":1234,"anchor_mid":500,"session_id":77,"stream_state":3,"stream_seq":4,"occurred_at":1750000000,"interrupted_seconds":65,"reason":"cdn callback"}`,
			wantTitle: "直播已中断",
		},
		{
			name:      "STOPPED 通知主播",
			payload:   `{"stream_id":"st_1F4","room_id":1234,"anchor_mid":500,"session_id":77,"stream_state":4,"stream_seq":5,"occurred_at":1750000100,"reason":"closed by operator"}`,
			wantTitle: "直播已结束",
		},
		{
			name:     "PUBLISHING 开播不群发",
			payload:  `{"stream_id":"st_1F4","room_id":1234,"anchor_mid":500,"session_id":77,"stream_state":2,"stream_seq":1,"occurred_at":1750000000}`,
			wantSkip: true,
		},
		{
			name:     "IDLE 不通知",
			payload:  `{"stream_id":"st_1F4","room_id":1234,"anchor_mid":500,"session_id":77,"stream_state":1,"stream_seq":0,"occurred_at":1750000000}`,
			wantSkip: true,
		},
		{
			name:     "没有 anchor_mid 时不自行找收件人",
			payload:  `{"stream_id":"st_1F4","room_id":1234,"anchor_mid":0,"session_id":77,"stream_state":4,"stream_seq":6,"occurred_at":1750000200}`,
			wantSkip: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-ingest", EventTypeLiveState, tc.payload)))
			if err != nil {
				t.Fatalf("BuildMessage 失败: %v", err)
			}
			if tc.wantSkip {
				if !res.Skip {
					t.Fatalf("应跳过而不是自建收件人：%+v", res.Message)
				}
				return
			}
			if res.Skip {
				t.Fatalf("live-ingest 的真实 payload 应能生成站内信，却跳过了：%s", tc.payload)
			}
			if res.Message.Category != model.CategoryLive {
				t.Errorf("分类=%d，期望 %d", res.Message.Category, model.CategoryLive)
			}
			if res.Message.Title != tc.wantTitle {
				t.Errorf("标题=%q，期望 %q（动作推导失败会退到默认文案）", res.Message.Title, tc.wantTitle)
			}
			if len(res.Recipients) != 1 || res.Recipients[0] != 500 {
				t.Fatalf("只应通知 anchor_mid=500：%v", res.Recipients)
			}
			// 上游没给 title，房间名回退到「直播间 <room_id>」，正文里必须能看到。
			if !strings.Contains(res.Message.Content, "直播间 1234") {
				t.Errorf("正文缺少房间回退名：%q", res.Message.Content)
			}
		})
	}
}

// --- 收件人与长度收敛 ---

func TestExplicitRecipientsWinAndAreDeduplicated(t *testing.T) {
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-multi", EventTypeEngagementAction,
		`{"action":"comment","mid":100,"target_mid":200,"recipients":[300,300,0,-1,400]}`)))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	if got := res.Recipients; len(got) != 2 || got[0] != 300 || got[1] != 400 {
		t.Fatalf("显式 recipients 应优先且去重去非法，得到 %v", got)
	}
}

func TestLongTitlesAreClippedOnRuneBoundaries(t *testing.T) {
	long := strings.Repeat("站内信标题很长", 60) // 240 个汉字，远超 64 rune
	res, err := BuildMessage(mustParse(t, envelopeJSON(t, "evt-long", EventTypeEngagementAction,
		`{"action":"like","mid":1,"target_mid":2,"content_title":"`+long+`"}`)))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	title := res.Message.Title
	if len([]rune(title)) > maxTitleRunes {
		t.Fatalf("标题应按 rune 收敛到 %d，得到 %d", maxTitleRunes, len([]rune(title)))
	}
	if !utf8.ValidString(res.Message.Content) || !utf8.ValidString(title) {
		t.Fatalf("标题/正文被截断成非法 UTF-8，utf8mb4 会拒绝写入：%q / %q", title, res.Message.Content)
	}
	if len([]rune(res.Message.Content)) > maxContentRunes+maxTitleRunes+16 {
		t.Fatalf("正文过长：%d rune", len([]rune(res.Message.Content)))
	}
}

func TestPayloadMustNotBeEmpty(t *testing.T) {
	env := func(payload string) *eventenvelope.Envelope {
		// 直接构造信封：`  ` 这类原文连 json.Marshal 都过不去，但队列里确实可能出现残缺投递。
		return &eventenvelope.Envelope{
			EventID: "evt-empty", EventType: EventTypeEngagementAction, SchemaVersion: 1,
			OccurredAt: testOccurredAt, Producer: "engagement.v1.rpc",
			AggregateType: "content", AggregateID: "1", Payload: json.RawMessage(payload),
		}
	}
	if _, err := BuildMessage(env(`{}`)); !errors.Is(err, ErrEmptyPayload) {
		t.Fatalf("空对象 payload 期望 ErrEmptyPayload，得到 %v", err)
	}
	if _, err := BuildMessage(env(`  `)); err == nil {
		t.Fatal("残缺 payload 必须报错（判死），不能当成有效事件")
	}
	// payload=null 解出来是全零值：没有 action 就没有可投递的站内信，按跳过处理。
	res, err := BuildMessage(env(`null`))
	if err != nil {
		t.Fatalf("payload=null 应跳过而不是报错: %v", err)
	}
	if !res.Skip {
		t.Fatalf("payload=null 应跳过，得到 %+v", res)
	}
}

func TestBuildMessageRejectsUnsupportedAndNil(t *testing.T) {
	if _, err := BuildMessage(nil); !errors.Is(err, ErrNilEvent) {
		t.Fatalf("nil 信封期望 ErrNilEvent，得到 %v", err)
	}
	env := mustParse(t, envelopeJSON(t, "evt-x", "moderation.decision", `{"action":"approve"}`))
	if _, err := BuildMessage(env); !errors.Is(err, ErrUnsupportedEventType) {
		t.Fatalf("未知类型期望 ErrUnsupportedEventType，得到 %v", err)
	}
	if SupportedEventType("moderation.decision") {
		t.Fatal("moderation.decision 不属于本服务消费契约")
	}
	if TopicFor("moderation.decision") != "" {
		t.Fatal("未知类型不应有默认 topic")
	}
}

func TestSensitivePayloadFieldsDoNotLeak(t *testing.T) {
	// 上游误投递手机号/IP/Token：未声明的字段在反序列化时被丢弃，不进标题、正文与 extra。
	raw := envelopeJSON(t, "evt-pii", EventTypeEngagementAction,
		`{"action":"like","mid":1,"target_mid":2,"content_title":"标题","phone":"13800001111",`+
			`"ip":"10.1.2.3","access_token":"secret-token"}`)
	res, err := BuildMessage(mustParse(t, raw))
	if err != nil {
		t.Fatalf("BuildMessage 失败: %v", err)
	}
	joined := res.Message.Title + res.Message.Content + res.Message.Extra + res.Message.BizID
	for _, leak := range []string{"13800001111", "10.1.2.3", "secret-token"} {
		if strings.Contains(joined, leak) {
			t.Fatalf("敏感字段 %s 泄漏进站内信：%s", leak, joined)
		}
	}
}

func TestDerivedTopicUsesSchemaVersion(t *testing.T) {
	env := mustParse(t, envelopeJSON(t, "evt-topic", EventTypeEngagementAction,
		`{"action":"like","mid":1,"target_mid":2}`))
	if got := derivedTopic(env); got != TopicEngagementAction {
		t.Fatalf("v1 信封应推导为 %s，得到 %s", TopicEngagementAction, got)
	}
	env.SchemaVersion = 2
	if got := derivedTopic(env); got != "engagement.action.v2" {
		t.Fatalf("topic 应随 schema_version 演进，得到 %s", got)
	}
}
