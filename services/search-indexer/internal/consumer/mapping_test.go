package consumer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go-video/common/eventenvelope"
	"go-video/services/search-indexer/internal/esclient"
	"go-video/services/search-indexer/model"
)

func envelopeOf(t *testing.T, eventType, aggregateID, occurredAt string, payload interface{}) *eventenvelope.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	env := &eventenvelope.Envelope{
		EventID:       "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		EventType:     eventType,
		SchemaVersion: 1,
		OccurredAt:    occurredAt,
		Producer:      "search-indexer-test",
		AggregateType: "content",
		AggregateID:   aggregateID,
		Payload:       raw,
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("测试用信封不合法: %v", err)
	}
	return env
}

func TestParseEnvelope_RejectsMalformed(t *testing.T) {
	if _, err := ParseEnvelope([]byte(`{`)); err == nil {
		t.Fatal("残缺 JSON 必须报错")
	}
	// 缺 event_id / schema_version<=0 都由 Envelope.UnmarshalJSON 拦截。
	if _, err := ParseEnvelope([]byte(`{"event_type":"content.published","schema_version":1}`)); err == nil {
		t.Fatal("字段不完整的信封必须报错，不能进入索引写入")
	}
	env, err := ParseEnvelope([]byte(`{"event_id":"e1","event_type":"content.published","schema_version":1,
	  "occurred_at":"2026-01-02T03:04:05Z","producer":"video","aggregate_type":"content",
	  "aggregate_id":"123","payload":{"action":"publish"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if env.AggregateID != "123" || env.SchemaVersion != 1 {
		t.Fatalf("信封解析结果异常: %+v", env)
	}
}

func TestSupportedEventType(t *testing.T) {
	for _, et := range []string{EventTypeContentPublished, EventTypeEngagementAction} {
		if !SupportedEventType(et) {
			t.Errorf("%s 应受支持", et)
		}
	}
	for _, et := range []string{"feed.publish.v1", "", "content.published.v1"} {
		if SupportedEventType(et) {
			t.Errorf("%s 不应受支持", et)
		}
	}
}

func TestDocFromContentEvent_FieldMapping(t *testing.T) {
	env := envelopeOf(t, EventTypeContentPublished, "900", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionPublish, "content_id": 900, "content_type": 1,
		"title": "标题", "description": "简介", "cover_url": "https://cdn/x.jpg",
		"author_mid": 42, "author_name": "UP主", "typeid": 29, "type_name": "音乐",
		"tags": []string{" 原创 ", "原创", "", "live"}, "duration_sec": 120,
		"publish_at": 1767322845, "ctime": 1767322000, "doc_revision": 555,
		"rights_expire_at": 1799999999, "language": "zh-CN",
		"subtitle_langs": []string{"zh-CN", "en"}, "sensitive": true,
		"heat": map[string]interface{}{"view_count": 10, "like_count": 2, "heat_score": 88, "heat_revision": 500},
	})
	p := &ContentPublishedPayload{}
	if err := unmarshalPayload(env.Payload, p); err != nil {
		t.Fatal(err)
	}
	doc, err := DocFromContentEvent(env, p)
	if err != nil {
		t.Fatal(err)
	}

	// 逐字段核对，避免投影写串字段（例如 cover_url 落到 description）。
	if doc.ContentID != 900 || doc.ContentType != 1 || doc.Title != "标题" || doc.Description != "简介" ||
		doc.CoverURL != "https://cdn/x.jpg" || doc.AuthorMid != 42 || doc.AuthorName != "UP主" ||
		doc.Typeid != 29 || doc.TypeName != "音乐" || doc.DurationSec != 120 || doc.PublishAt != 1767322845 ||
		doc.Ctime != 1767322000 || doc.RightsExpireAt != 1799999999 || doc.Language != "zh-CN" || !doc.Sensitive {
		t.Fatalf("字段映射不完整: %+v", doc)
	}
	if doc.State != esclient.StatePublished {
		t.Fatalf("publish 事件 state = %d, want %d", doc.State, esclient.StatePublished)
	}
	if doc.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", doc.SchemaVersion)
	}
	// 标签去空白 + 去重（" 原创 " 与 "原创" 归一后是同一个）。
	if strings.Join(doc.Tags, "|") != "原创|live" {
		t.Fatalf("tags 归一化结果 = %v", doc.Tags)
	}
	if doc.Heat.ViewCount != 10 || doc.Heat.LikeCount != 2 || doc.Heat.HeatScore != 88 || doc.Heat.HeatRevision != 500 {
		t.Fatalf("heat 映射异常: %+v", doc.Heat)
	}
	if doc.ID() != "1_900" {
		t.Fatalf("文档主键 = %s, want 1_900", doc.ID())
	}
}

// TestDocFromContentEvent_RevisionFallback 校验「防旧覆盖新」的两个回退：
// payload 未给 doc_revision 时用事件发生时间；两者都没有则拒绝写入。
func TestDocFromContentEvent_RevisionFallback(t *testing.T) {
	env := envelopeOf(t, EventTypeContentPublished, "777", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionUpdate, "content_type": 1, "title": "T",
	})
	p := &ContentPublishedPayload{}
	if err := unmarshalPayload(env.Payload, p); err != nil {
		t.Fatal(err)
	}
	doc, err := DocFromContentEvent(env, p)
	if err != nil {
		t.Fatal(err)
	}
	if doc.ContentID != 777 {
		t.Fatalf("content_id 应回退到 aggregate_id，实际 %d", doc.ContentID)
	}
	want := occurredAtMillis(env.OccurredAt)
	if doc.DocRevision != want || want <= 0 {
		t.Fatalf("doc_revision 回退值 = %d, occurred_at 毫秒 = %d", doc.DocRevision, want)
	}
	// 没有 heat 时热度版本跟随正文版本，保证首次投影的热度也可比较。
	if doc.Heat.HeatRevision != 0 {
		t.Fatalf("payload 无 heat 时不应凭空造热度版本: %+v", doc.Heat)
	}

	// occurred_at 不可解析且没给 doc_revision → 必须拒绝，而不是写一个 0 版本。
	bad := envelopeOf(t, EventTypeContentPublished, "777", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionPublish, "content_type": 1, "title": "T",
	})
	bad.OccurredAt = ""
	p2 := &ContentPublishedPayload{}
	if err := unmarshalPayload(bad.Payload, p2); err != nil {
		t.Fatal(err)
	}
	if _, err := DocFromContentEvent(bad, p2); err == nil {
		t.Fatal("缺 doc_revision 且 occurred_at 无效时必须拒绝写入")
	} else if !errors.Is(err, model.ErrInvalidDocRevision) && !strings.Contains(err.Error(), "doc_revision") {
		t.Fatalf("错误应指向 doc_revision，实际 %v", err)
	}
}

func TestDocFromContentEvent_HeatVersionFallbacks(t *testing.T) {
	// 生产者只把版本放在 payload 顶层（部分上游实现）。
	env := envelopeOf(t, EventTypeContentPublished, "1", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionPublish, "content_id": 1, "content_type": 1, "title": "T", "doc_revision": 1000,
		"heat":          map[string]interface{}{"view_count": 3},
		"heat_score":    77,
		"heat_revision": 999,
	})
	p := &ContentPublishedPayload{}
	if err := unmarshalPayload(env.Payload, p); err != nil {
		t.Fatal(err)
	}
	doc, err := DocFromContentEvent(env, p)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Heat.ViewCount != 3 || doc.Heat.HeatRevision != 999 || doc.Heat.HeatScore != 77 {
		t.Fatalf("顶层 heat 字段未回退: %+v", doc.Heat)
	}

	// 完全没给热度版本时，热度版本跟随正文版本，避免 0 版本永远无法被更新。
	env2 := envelopeOf(t, EventTypeContentPublished, "1", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionPublish, "content_id": 1, "content_type": 1, "title": "T", "doc_revision": 2000,
		"heat": map[string]interface{}{"view_count": 3},
	})
	p2 := &ContentPublishedPayload{}
	if err := unmarshalPayload(env2.Payload, p2); err != nil {
		t.Fatal(err)
	}
	doc2, err := DocFromContentEvent(env2, p2)
	if err != nil {
		t.Fatal(err)
	}
	if doc2.Heat.HeatRevision != 2000 {
		t.Fatalf("热度版本应回退到 doc_revision=2000，实际 %d", doc2.Heat.HeatRevision)
	}
}

// TestDocFromContentEvent_DropsSensitiveFields 是本服务的隐私边界：
// 生产者误投敏感字段时，未声明字段在反序列化阶段被丢弃，绝不进入索引。
func TestDocFromContentEvent_DropsSensitiveFields(t *testing.T) {
	env := envelopeOf(t, EventTypeContentPublished, "5", "2026-01-02T03:04:05Z", map[string]interface{}{
		"action": ActionPublish, "content_id": 5, "content_type": 1, "title": "T", "doc_revision": 1,
		// 以下都属越界投递，必须被忽略
		"mobile": "13800001111", "id_card": "110101199001011234", "ip": "10.1.2.3",
		"real_name": "张三", "token": "eyJhbGciOi...", "phone_country": "86",
	})
	p := &ContentPublishedPayload{}
	if err := unmarshalPayload(env.Payload, p); err != nil {
		t.Fatal(err)
	}
	doc, err := DocFromContentEvent(env, p)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, forbidden := range []string{"13800001111", "110101199001011234", "10.1.2.3", "张三", "eyJhbGciOi", "mobile", "id_card", "token"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("投影泄漏敏感字段 %q: %s", forbidden, body)
		}
	}
}

func TestDocFromContentEvent_NilAndInvalidInput(t *testing.T) {
	env := envelopeOf(t, EventTypeContentPublished, "1", "2026-01-02T03:04:05Z", map[string]interface{}{"action": ActionPublish})
	p := &ContentPublishedPayload{Action: ActionPublish, ContentType: 1, Title: "T", DocRevision: 1}
	if _, err := DocFromContentEvent(nil, p); err == nil {
		t.Fatal("nil 信封必须报错")
	}
	if _, err := DocFromContentEvent(env, nil); err == nil {
		t.Fatal("nil payload 必须报错")
	}
	// content_id 与 aggregate_id 都拿不到 → 无法定位文档。
	noID := envelopeOf(t, EventTypeContentPublished, "abc", "2026-01-02T03:04:05Z", map[string]interface{}{"action": ActionPublish})
	if _, err := DocFromContentEvent(noID, &ContentPublishedPayload{DocRevision: 1}); err == nil {
		t.Fatal("aggregate_id 非数字时必须报错")
	}
}

func TestNormalizeTags(t *testing.T) {
	if got := normalizeTags(nil); got != nil {
		t.Fatalf("nil 输入应返回 nil，实际 %v", got)
	}
	in := make([]string, 0, 50)
	for i := 0; i < 50; i++ {
		in = append(in, "tag")
	}
	if got := normalizeTags(in); len(got) != 1 {
		t.Fatalf("重复标签应合并，实际 %v", got)
	}
	var many []string
	for i := 0; i < 100; i++ {
		many = append(many, "t"+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if got := normalizeTags(many); len(got) != 32 {
		t.Fatalf("标签数量应限制在 32，实际 %d", len(got))
	}
	if got := normalizeTags([]string{"", "  ", "a"}); len(got) != 1 || got[0] != "a" {
		t.Fatalf("空标签未过滤: %v", got)
	}
}

func TestClassifyAction(t *testing.T) {
	cases := map[string]struct {
		kind  string
		purge bool
	}{
		ActionPublish: {ActionKindUpsert, false},
		ActionUpdate:  {ActionKindUpsert, false},
		ActionOffline: {ActionKindTakedown, false},
		ActionExpired: {ActionKindTakedown, false},
		ActionDelete:  {ActionKindTakedown, true},
	}
	for action, want := range cases {
		kind, purge, err := ClassifyAction(action)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if kind != want.kind || purge != want.purge {
			t.Errorf("ClassifyAction(%s) = %s/%v, want %s/%v", action, kind, purge, want.kind, want.purge)
		}
	}
	// 未知 action 必须判永久错误：猜一个语义去写索引比不写更糟。
	if _, _, err := ClassifyAction("unpublished"); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("未知 action 应返回 ErrUnknownAction，实际 %v", err)
	}
	if _, _, err := ClassifyAction(""); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("空 action 应返回 ErrUnknownAction，实际 %v", err)
	}
}

func TestHeatFromEngagementEvent(t *testing.T) {
	env := envelopeOf(t, EventTypeEngagementAction, "88", "2026-01-02T03:04:05Z", map[string]interface{}{
		"content_id": 88, "content_type": 1, "action": "like",
		"counters": map[string]interface{}{"like_count": 31, "danmaku_count": 4, "heat_revision": 111},
	})
	p := &EngagementActionPayload{}
	if err := unmarshalPayload(env.Payload, p); err != nil {
		t.Fatal(err)
	}
	contentID, contentType, heat, err := HeatFromEngagementEvent(env, p)
	if err != nil {
		t.Fatal(err)
	}
	if contentID != 88 || contentType != 1 {
		t.Fatalf("定位字段异常: %d/%d", contentID, contentType)
	}
	if heat.LikeCount != 31 || heat.DanmakuCount != 4 || heat.HeatRevision != 111 {
		t.Fatalf("快照字段异常: %+v", heat)
	}

	// 没给绝对快照就必须报错：索引侧做「读-改-写累加」在并发与重放下都会双计。
	noCounters := &EngagementActionPayload{ContentID: 1, ContentType: 1, Action: "like"}
	if _, _, _, err := HeatFromEngagementEvent(env, noCounters); !errors.Is(err, ErrHeatSnapshotRequired) {
		t.Fatalf("缺计数快照应返回 ErrHeatSnapshotRequired，实际 %v", err)
	}
	// 有计数但没版本 → 无法判定新旧，同样判永久错误。
	noRevision := &EngagementActionPayload{ContentID: 1, ContentType: 1, Counters: &HeatPayload{LikeCount: 5}}
	if _, _, _, err := HeatFromEngagementEvent(env, noRevision); !errors.Is(err, ErrHeatSnapshotRequired) {
		t.Fatalf("缺 heat_revision 应返回 ErrHeatSnapshotRequired，实际 %v", err)
	}
	// 顶层 heat_revision 也算合法版本。
	topLevel := &EngagementActionPayload{ContentID: 1, ContentType: 1, Counters: &HeatPayload{LikeCount: 5}, HeatRevision: 222}
	if _, _, heat2, err := HeatFromEngagementEvent(env, topLevel); err != nil || heat2.HeatRevision != 222 {
		t.Fatalf("顶层 heat_revision 未被采纳: %+v %v", heat2, err)
	}
	if _, _, _, err := HeatFromEngagementEvent(nil, topLevel); err == nil {
		t.Fatal("nil 信封必须报错")
	}
}

func TestUnmarshalPayload_Errors(t *testing.T) {
	var p ContentPublishedPayload
	if err := unmarshalPayload(nil, &p); err == nil {
		t.Fatal("空 payload 必须报错")
	}
	if err := unmarshalPayload([]byte(`{"action":`), &p); err == nil {
		t.Fatal("坏 JSON 必须报错")
	}
	// 前向兼容：生产者新增字段不能让消费端报错（§2 兼容窗口）。
	if err := unmarshalPayload([]byte(`{"action":"publish","future_field":{"a":1}}`), &p); err != nil {
		t.Fatalf("未知字段应保持兼容: %v", err)
	}
}

func TestTopicAndOccurredAtHelpers(t *testing.T) {
	env := envelopeOf(t, EventTypeContentPublished, "1", "2026-01-02T03:04:05Z", map[string]interface{}{"action": ActionPublish})
	if got := topicOf(env, "fallback"); got != "content.published.v1" {
		t.Fatalf("topicOf = %q", got)
	}
	if got := topicOf(&eventenvelope.Envelope{}, "fallback"); got != "fallback" {
		t.Fatalf("topicOf 回退异常: %q", got)
	}
	if occurredAtSeconds(env.OccurredAt) <= 0 {
		t.Fatal("occurredAtSeconds 应能解析合法时间")
	}
	if occurredAtSeconds("bad") < 2020 {
		t.Fatal("非法时间应回退到当前时间而不是 0")
	}
	if occurredAtMillis("bad") != 0 {
		t.Fatal("毫秒解析失败应回退 0，由上层决定是否拒绝")
	}
	if _, err := aggregateIDToInt64(""); err == nil {
		t.Fatal("空 aggregate_id 必须报错")
	}
	if v, err := aggregateIDToInt64(" 42 "); err != nil || v != 42 {
		t.Fatalf("aggregateIDToInt64 = %d/%v", v, err)
	}
}
