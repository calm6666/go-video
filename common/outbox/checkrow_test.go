package outbox

import (
	"encoding/json"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
)

// envelopePayload 造一份与列同源的正确信封 JSON。
// 用结构体而不是手写字面量：字段名一旦改，本文件会编译失败，
// 而不是留下一条永远「通过」的字符串比对。
func envelopePayload(t *testing.T, eventID, aggregateID string) string {
	t.Helper()
	b, err := json.Marshal(&eventenvelope.Envelope{
		EventID:       eventID,
		EventType:     "playback.granted",
		SchemaVersion: 1,
		OccurredAt:    "2026-10-04T00:00:00Z",
		Producer:      "playback",
		AggregateType: "play_auth",
		AggregateID:   aggregateID,
		Payload:       json.RawMessage(`{"kind":"hls"}`),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return string(b)
}

func validRow(t *testing.T) *Row {
	t.Helper()
	return &Row{
		ID:      1,
		EventID: "01EVENTID",
		Topic:   "playback.granted.v1",
		Key:     "auth-9",
		Payload: envelopePayload(t, "01EVENTID", "auth-9"),
	}
}

func TestCheckRowAcceptsConsistentRow(t *testing.T) {
	row := validRow(t)
	if got := CheckRow(row, []string{"playback.granted.v1"}); got != "" {
		t.Fatalf("自洽的行不该有缺陷，实得 %q", got)
	}
	// 多个 topic 时按集合判定，不要求顺序。
	if got := CheckRow(row, []string{"other.thing.v1", "", "playback.granted.v1", "playback.granted.v1"}); got != "" {
		t.Fatalf("空串与重复项不该影响判定，实得 %q", got)
	}
}

// TestCheckRowRejectsEachDefect 逐条钉住判据。
// 每条都必须给出结论而不是返回错误：判死是这一行的结果，循环要继续处理别的行。
func TestCheckRowRejectsEachDefect(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*Row)
		allowedTopics []string
		wantSubstr    string
	}{
		{"nil 行", nil, []string{"t.v1"}, "row is nil"},
		{"没声明产出 topic", func(r *Row) {}, nil, "没声明本服务产出的 topic"},
		{"只声明了空串", func(r *Row) {}, []string{""}, "没声明本服务产出的 topic"},
		{"topic 不属于本服务", func(r *Row) { r.Topic = "content.published.v1" },
			[]string{"playback.granted.v1"}, "topic \"content.published.v1\" 不属于本服务"},
		{"分区键为空", func(r *Row) { r.Key = "" }, []string{"playback.granted.v1"}, "分区键为空"},
		{"event_id 列为空", func(r *Row) { r.EventID = "" }, []string{"playback.granted.v1"}, "event_id 列为空"},
		{"payload 不是 JSON", func(r *Row) { r.Payload = "not-json" },
			[]string{"playback.granted.v1"}, "payload 不是合法事件信封"},
		{"payload 是空串", func(r *Row) { r.Payload = "" },
			[]string{"playback.granted.v1"}, "payload 不是合法事件信封"},
		{"payload 缺 schema_version", func(r *Row) {
			r.Payload = `{"event_id":"01EVENTID","event_type":"playback.granted","occurred_at":"2026-10-04T00:00:00Z","producer":"playback","aggregate_type":"play_auth","aggregate_id":"auth-9","payload":{}}`
		}, []string{"playback.granted.v1"}, "schema_version"},
		{"payload event_id 与列不同源", func(r *Row) { r.Payload = envelopePayload(t, "01OTHER", "auth-9") },
			[]string{"playback.granted.v1"}, "payload event_id=\"01OTHER\" 与列 event_id=\"01EVENTID\" 不一致"},
		{"payload aggregate_id 与分区键不同源", func(r *Row) { r.Payload = envelopePayload(t, "01EVENTID", "auth-8") },
			[]string{"playback.granted.v1"}, "payload aggregate_id=\"auth-8\" 与列分区键=\"auth-9\" 不一致"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var row *Row
			if tc.mutate == nil {
				row = nil
			} else {
				row = validRow(t)
				tc.mutate(row)
			}
			got := CheckRow(row, tc.allowedTopics)
			if got == "" {
				t.Fatalf("必须给出缺陷结论")
			}
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("缺陷原因应含 %q，实得 %q", tc.wantSubstr, got)
			}
		})
	}
}

// TestCheckRowNamesAllowedTopics 钉住判据里带上本服务声明的 topic：
// 运维看到「topic 不属于本服务」时，要能立刻知道本表到底该产哪些事件，
// 而不是再去翻 model 常量。
func TestCheckRowNamesAllowedTopics(t *testing.T) {
	row := validRow(t)
	row.Topic = "playback.granted.v2"
	got := CheckRow(row, []string{"playback.granted.v1", "playback.revoked.v1"})
	if !strings.Contains(got, "[playback.granted.v1 playback.revoked.v1]") {
		t.Fatalf("缺陷原因要列出声明的 topic，实得 %q", got)
	}
}

// TestCheckRowPayloadKeepsRowIntact 确认判据只读不写：
// 缺陷由 Store 适配写进 Row.Defect，CheckRow 不该顺手改行内容。
func TestCheckRowPayloadKeepsRowIntact(t *testing.T) {
	row := validRow(t)
	before := *row
	row.Topic = "wrong.topic.v1"
	_ = CheckRow(row, []string{"playback.granted.v1"})
	if row.Payload != before.Payload || row.EventID != before.EventID || row.Key != before.Key {
		t.Fatalf("CheckRow 不得修改行内容")
	}
}
