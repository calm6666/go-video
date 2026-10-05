// Package consumer 是 inbox 的领域事件消费侧（AGENTS.md §3：MQ 消费者放在
// 拥有写入权的服务里）。它消费 docs/api-and-events.md §5 中的三个事件 topic，
// 把互动、内容、直播事件转成站内信：
//
//	engagement.action.v1  -> 分类 2（互动）：点赞/收藏/分享/关注/评论/弹幕
//	content.published.v1  -> 分类 3（内容）：下架/过期/删除通知作者
//	live.state.v1         -> 分类 4（直播）：断流/结束/封禁通知主播
//
// 处理流程固定为：解析信封 -> event_id 去重领取 -> 构造站内信 -> 事务投递 ->
// 写回 succeeded / retry / dead_letter（AGENTS.md §5、docs §6）。
//
// 依赖边界：本包只依赖 internal/repository 暴露的小接口 Store，不直接写 SQL，
// 也不在单测里连接 Kafka/MySQL/Redis；Kafka 客户端只出现在 kafkaruntime_kafka.go
// （-tags inbox_kafka）里，默认构建由 kafkaruntime_stub.go 显式声明运行时未链接。
package consumer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/inbox/model"
)

// 本服务消费的事件类型（docs/api-and-events.md §5）。
const (
	EventTypeEngagementAction = "engagement.action"
	EventTypeContentPublished = "content.published"
	EventTypeLiveState        = "live.state"
)

// 事件默认 topic（版本号由信封推导，见 eventenvelope.Topic）。
const (
	TopicEngagementAction = "engagement.action.v1"
	TopicContentPublished = "content.published.v1"
	TopicLiveState        = "live.state.v1"
)

// idempotencyKeyPrefix 是事件投递的幂等键前缀。
// 站内信主体按 evt:<event_id> 唯一，因此即使清空 inbox_consumer_offset 也不会重复投递。
const idempotencyKeyPrefix = "evt:"

// 标题/正文的 rune 上限：事件里的内容标题长度不可控，落库前统一收敛，
// 避免超长标题撑坏列表页（中文按 1 rune 计）。
const (
	maxTitleRunes   = 64
	maxContentRunes = 512
)

// 信封或 payload 无法满足契约时的永久错误：直接判死，不消耗重试配额。
var (
	// ErrUnsupportedEventType 事件类型不在本服务消费契约内。
	ErrUnsupportedEventType = errors.New("inbox/consumer: unsupported event_type")
	// ErrNilEvent 信封为空。
	ErrNilEvent = errors.New("inbox/consumer: nil event")
	// ErrEmptyPayload payload 缺失或为空对象。
	ErrEmptyPayload = errors.New("inbox/consumer: empty payload")
)

// EngagementActionPayload 是 engagement.action.v1 中 inbox 需要消费的字段。
//
// 只声明用到的字段，未声明的字段在反序列化时被忽略，因此上游即使误投递
// 手机号、IP、Token 等敏感字段也不会进入站内信（AGENTS.md §6、§7）。
type EngagementActionPayload struct {
	Action       string  `json:"action"`        // like/cancel_like/favorite/share/follow/comment/danmaku
	ContentID    int64   `json:"content_id"`    // 被互动的内容
	ContentType  int32   `json:"content_type"`  // 1 UGC、2 PGC、3 直播
	ContentTitle string  `json:"content_title"` // 用于文案展示
	Mid          int64   `json:"mid"`           // 触发者（点赞/关注的人）
	TargetMid    int64   `json:"target_mid"`    // 被通知人：内容作者或被关注者
	AuthorMid    int64   `json:"author_mid"`    // target_mid 缺失时的回退
	Recipients   []int64 `json:"recipients"`    // 上游显式指定的收件人，优先级最高
	Link         string  `json:"link"`          // 客户端跳转路由，可空
}

// ContentPublishedPayload 是 content.published.v1 的消费字段。
// 生产者是 video/catalog/rights，动作为发布、下架、过期（docs §5）。
type ContentPublishedPayload struct {
	Action      string  `json:"action"` // publish/update/offline/expired/delete
	ContentID   int64   `json:"content_id"`
	ContentType int32   `json:"content_type"`
	Title       string  `json:"title"`
	AuthorMid   int64   `json:"author_mid"`
	Reason      string  `json:"reason"` // 下架/过期原因，可为空
	Recipients  []int64 `json:"recipients"`
	Link        string  `json:"link"`
}

// LiveStatePayload 是 live.state.v1 的消费字段。
//
// 唯一生产者是 live-ingest（docs/api-and-events.md §5「一个事件只有一个写入者」），
// 它发的是状态迁移事实，不带 action：stream_state = 1 IDLE、2 PUBLISHING、
// 3 INTERRUPTED、4 STOPPED。因此动作名在本服务里由 stream_state 推导，
// 见 liveActionFromStreamState；action 只作为上游显式指定语义时的覆盖入口。
type LiveStatePayload struct {
	Action      string  `json:"action"` // start/stop/interrupt/ban/unban，生产侧可空
	StreamState int32   `json:"stream_state"`
	RoomID      int64   `json:"room_id"`
	AnchorMid   int64   `json:"anchor_mid"`
	Title       string  `json:"title"`
	Reason      string  `json:"reason"`
	Recipients  []int64 `json:"recipients"`
	Link        string  `json:"link"`
}

// live_stream 状态取值，与 live-ingest 的 model.StreamState* 一致。
// 不 import 那个包：跨服务直连别人的 model 违反 AGENTS.md §5，
// 这里只需要数值口径，改动必须同步 docs/api-and-events.md §5 的 payload 表。
const (
	liveStreamStateInterrupted int32 = 3
	liveStreamStateStopped     int32 = 4
)

// liveActionFromStreamState 把状态事实翻译成站内信动作。
// IDLE 与 PUBLISHING（开播）返回空串：开播面向粉丝，本服务没有粉丝关系，
// 在这里编一个动作名只会产生一条注定跳过的记录。
func liveActionFromStreamState(state int32) string {
	switch state {
	case liveStreamStateInterrupted:
		return "interrupt"
	case liveStreamStateStopped:
		return "stop"
	default:
		return ""
	}
}

// template 一条事件的标题与正文模板（正文含一个 %s 占位内容标题）。
type template struct {
	title   string
	content string
}

// ParseEnvelope 解析并校验事件信封。
// eventenvelope 的 UnmarshalJSON 自带 Validate，因此格式错误在这里就返回，
// 调用方不必再手工校验必填字段（AGENTS.md §5）。
func ParseEnvelope(raw []byte) (*eventenvelope.Envelope, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyPayload
	}
	var env eventenvelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("inbox/consumer: %w", err)
	}
	return &env, nil
}

// SupportedEventType 判断事件类型是否会被转成站内信。
func SupportedEventType(eventType string) bool {
	switch eventType {
	case EventTypeEngagementAction, EventTypeContentPublished, EventTypeLiveState:
		return true
	default:
		return false
	}
}

// TopicFor 返回事件类型对应的标准 topic 名，未知类型返回空串。
func TopicFor(eventType string) string {
	switch eventType {
	case EventTypeEngagementAction:
		return TopicEngagementAction
	case EventTypeContentPublished:
		return TopicContentPublished
	case EventTypeLiveState:
		return TopicLiveState
	default:
		return ""
	}
}

// BuildResult 一个事件构造出的站内信及其收件人。
type BuildResult struct {
	Message    *model.InboxMessage
	Recipients []int64
	// Skip 为 true 表示按契约该事件不产生站内信（例如取消点赞、面向粉丝的群发）。
	// 这是正常结论，不是错误：调用方应把事件标记为 succeeded。
	Skip bool
	// Reason 说明跳过原因，只进日志。
	Reason string
}

// BuildMessage 把事件映射成站内信写入。
//
// 收件人规则（重要，避免与其它服务的通知职责重复）：
//  1. payload.recipients 非空时以其为准——上游已经算好该通知谁；
//  2. 否则只通知事件里明确给出的单个对象（互动目标 / 内容作者 / 主播）；
//  3. 两者都没有时跳过并记 succeeded，绝不自行发明 fan-out：
//     粉丝列表属于 social-graph、多通道触达属于 notification，
//     站内信只负责「已经确定收件人的那一条消息落库」。
//
// 自己对自己互动（自己点赞自己的作品）不产生站内信。
func BuildMessage(env *eventenvelope.Envelope) (*BuildResult, error) {
	if env == nil {
		return nil, ErrNilEvent
	}
	switch env.EventType {
	case EventTypeEngagementAction:
		return buildEngagementMessage(env)
	case EventTypeContentPublished:
		return buildContentMessage(env)
	case EventTypeLiveState:
		return buildLiveMessage(env)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedEventType, env.EventType)
	}
}

// buildEngagementMessage 互动事件 -> 分类 2。
func buildEngagementMessage(env *eventenvelope.Envelope) (*BuildResult, error) {
	var p EngagementActionPayload
	if err := unmarshalPayload(env.Payload, &p); err != nil {
		return nil, err
	}
	tpl, ok := engagementTemplate(p.Action)
	if !ok {
		return skipResult("action " + quoteOrEmpty(p.Action) + " 不需要站内信"), nil
	}

	target := p.TargetMid
	if target <= 0 {
		target = p.AuthorMid
	}
	recipients, reason := resolveRecipients(p.Recipients, target, p.Mid)
	if recipients == nil {
		return skipResult(reason), nil
	}

	title := firstNonEmpty(p.ContentTitle, fmt.Sprintf("作品 %d", contentIDOf(p.ContentID, env.AggregateID)))
	content := clipRune(render(tpl, clipRune(title, maxTitleRunes)), maxContentRunes)
	return &BuildResult{
		Message: baseMessage(env, model.CategoryEngagement, p.Action, p.Link,
			clipRune(tpl.title, maxTitleRunes), content, p.Mid),
		Recipients: recipients,
	}, nil
}

// buildContentMessage 内容事件 -> 分类 3。
// publish/update 的收件人是粉丝群体，inbox 拿不到粉丝关系，按规则跳过；
// offline/expired/delete 是作者需要知道的状态变更，通知作者本人。
func buildContentMessage(env *eventenvelope.Envelope) (*BuildResult, error) {
	var p ContentPublishedPayload
	if err := unmarshalPayload(env.Payload, &p); err != nil {
		return nil, err
	}
	tpl, ok := contentTemplate(p.Action)
	if !ok {
		return skipResult("action " + quoteOrEmpty(p.Action) + " 不需要站内信"), nil
	}

	recipients, reason := resolveRecipients(p.Recipients, p.AuthorMid, 0)
	if recipients == nil {
		return skipResult(reason), nil
	}

	contentID := contentIDOf(p.ContentID, env.AggregateID)
	name := firstNonEmpty(p.Title, fmt.Sprintf("作品 %d", contentID))
	content := withReason(render(tpl, clipRune(name, maxTitleRunes)), p.Reason)
	return &BuildResult{
		Message: baseMessage(env, model.CategoryContent, p.Action, p.Link,
			clipRune(tpl.title, maxTitleRunes), clipRune(content, maxContentRunes), 0),
		Recipients: recipients,
	}, nil
}

// buildLiveMessage 直播事件 -> 分类 4。
// 开播（start）面向粉丝，inbox 无粉丝关系按规则跳过；断流/结束/封禁只影响主播本人，通知主播。
// 动作名优先取上游显式 action，缺失时由 stream_state 推导（live-ingest 只发状态事实）。
func buildLiveMessage(env *eventenvelope.Envelope) (*BuildResult, error) {
	var p LiveStatePayload
	if err := unmarshalPayload(env.Payload, &p); err != nil {
		return nil, err
	}
	action := p.Action
	if action == "" {
		action = liveActionFromStreamState(p.StreamState)
	}
	tpl, ok := liveTemplate(action)
	if !ok {
		return skipResult(fmt.Sprintf("action %s（stream_state=%d）不需要站内信",
			quoteOrEmpty(action), p.StreamState)), nil
	}

	recipients, reason := resolveRecipients(p.Recipients, p.AnchorMid, 0)
	if recipients == nil {
		return skipResult(reason), nil
	}

	roomID := p.RoomID
	if roomID <= 0 {
		roomID = aggregateIntID(env.AggregateID)
	}
	name := firstNonEmpty(p.Title, fmt.Sprintf("直播间 %d", roomID))
	content := withReason(render(tpl, clipRune(name, maxTitleRunes)), p.Reason)
	return &BuildResult{
		Message: baseMessage(env, model.CategoryLive, action, p.Link,
			clipRune(tpl.title, maxTitleRunes), clipRune(content, maxContentRunes), 0),
		Recipients: recipients,
	}, nil
}

// baseMessage 组装消息主体：幂等键、分类、业务标识与扩展 JSON 在此统一，
// 保证三个事件来源的字段口径一致。
func baseMessage(
	env *eventenvelope.Envelope, category int32, action, link, title, content string, senderMid int64,
) *model.InboxMessage {
	msgType := model.MsgTypeText
	if link != "" {
		msgType = model.MsgTypeLink
	}
	return &model.InboxMessage{
		Category:       category,
		MsgType:        msgType,
		Title:          title,
		Content:        content,
		SenderMid:      senderMid,
		BizType:        env.EventType,
		BizID:          clipRune(env.AggregateID, 64),
		Extra:          marshalExtra(env, action, link),
		IdempotencyKey: idempotencyKeyPrefix + env.EventID,
		Ctime:          occurredAtSeconds(env.OccurredAt),
	}
}

// extraJSON 是站内信 extra 列的结构：只保留展示与追溯需要的字段。
type extraJSON struct {
	EventID   string `json:"event_id"`
	EventType string `json:"event_type"`
	Topic     string `json:"topic"`
	Action    string `json:"action,omitempty"`
	Link      string `json:"link,omitempty"`
	TraceID   string `json:"trace_id,omitempty"`
}

// marshalExtra 生成 extra JSON。字段全是字符串，marshal 理论上不会失败；
// 真失败时退化成 {}，不影响投递，只丢失跳转信息。
func marshalExtra(env *eventenvelope.Envelope, action, link string) string {
	raw, err := json.Marshal(extraJSON{
		EventID:   env.EventID,
		EventType: env.EventType,
		Topic:     derivedTopic(env),
		Action:    action,
		Link:      link,
		TraceID:   env.TraceID,
	})
	if err != nil {
		return "{}"
	}
	return string(raw)
}

// derivedTopic 优先使用信封推导出的版本化 topic，回退到事件类型默认 topic。
func derivedTopic(env *eventenvelope.Envelope) string {
	if t := eventenvelope.Topic(env.EventType, env.SchemaVersion); t != "" {
		return t
	}
	return TopicFor(env.EventType)
}

// engagementTemplate 返回动作对应的文案；false 表示该动作不需要站内信。
func engagementTemplate(action string) (template, bool) {
	switch action {
	case "like":
		return template{"收到新的赞", "你的作品《%s》收到了新的赞"}, true
	case "favorite":
		return template{"作品被收藏", "你的作品《%s》被收藏了"}, true
	case "share":
		return template{"作品被分享", "你的作品《%s》被分享了"}, true
	case "follow":
		return template{"新的关注", "有人关注了你"}, true
	case "comment":
		return template{"收到新评论", "你的作品《%s》收到了新评论"}, true
	case "danmaku":
		return template{"收到新弹幕", "你的作品《%s》收到了新弹幕"}, true
	default:
		// 含 cancel_like / unfollow 等撤销动作：不补发也不撤销已投递的站内信。
		return template{}, false
	}
}

// contentTemplate 内容事件文案：publish/update 面向粉丝，这里不产生站内信。
func contentTemplate(action string) (template, bool) {
	switch action {
	case "offline":
		return template{"作品已下架", "你的作品《%s》已下架"}, true
	case "expired":
		return template{"作品版权到期", "你的作品《%s》因版权到期已停止播放"}, true
	case "delete":
		return template{"作品已删除", "你的作品《%s》已被删除"}, true
	default:
		return template{}, false
	}
}

// liveTemplate 直播事件文案：start 面向粉丝，这里不产生站内信。
func liveTemplate(action string) (template, bool) {
	switch action {
	case "interrupt":
		return template{"直播已中断", "你的直播《%s》出现断流，已自动暂停"}, true
	case "stop":
		return template{"直播已结束", "你的直播《%s》已结束"}, true
	case "ban":
		return template{"直播间被封禁", "你的直播《%s》已被封禁"}, true
	default:
		return template{}, false
	}
}

// resolveRecipients 按「显式列表 > 单点对象」确定收件人，并剔除触发者本人。
// 返回 nil 表示无可投递收件人，reason 说明原因（只进日志，不进死信）。
func resolveRecipients(explicit []int64, fallback, selfMid int64) ([]int64, string) {
	src, note := explicit, "事件未给出收件人，且本服务不自行 fan-out"
	if len(explicit) == 0 {
		if fallback > 0 {
			src, note = []int64{fallback}, "收件人非法或缺失"
		} else {
			return nil, note
		}
	}

	seen := make(map[int64]struct{}, len(src))
	out := make([]int64, 0, len(src))
	for _, mid := range src {
		if mid <= 0 {
			continue
		}
		if selfMid > 0 && mid == selfMid {
			continue // 自己给自己发通知没有意义
		}
		if _, dup := seen[mid]; dup {
			continue
		}
		seen[mid] = struct{}{}
		out = append(out, mid)
	}
	if len(out) == 0 {
		if selfMid > 0 && fallback == selfMid {
			return nil, "收件人只有触发者本人，不产生站内信"
		}
		return nil, note
	}
	return out, note
}

// skipResult 构造「按契约跳过」的结果。
func skipResult(reason string) *BuildResult {
	return &BuildResult{Skip: true, Reason: reason}
}

// contentIDOf payload 未给数值 ID 时回退用聚合根 ID（只影响文案，不参与定位）。
func contentIDOf(contentID int64, aggregateID string) int64 {
	if contentID > 0 {
		return contentID
	}
	return aggregateIntID(aggregateID)
}

// render 填充正文模板。部分文案（如「有人关注了你」）不含占位符，
// 因此按是否含 %s 分支：直接 Sprintf 会在无占位时追加 %!(EXTRA ...) 污染正文。
func render(tpl template, subject string) string {
	if !strings.Contains(tpl.content, "%s") {
		return tpl.content
	}
	return fmt.Sprintf(tpl.content, subject)
}

// withReason 把上游原因拼到正文尾部，长度受控。
func withReason(content, reason string) string {
	if reason == "" {
		return content
	}
	return content + "：" + clipRune(reason, 128)
}

// unmarshalPayload 解 payload 到具体结构。
// 不用严格模式，生产者新增字段不会打挂消费者（docs §2 兼容窗口）。
func unmarshalPayload(raw json.RawMessage, target any) error {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "{}" {
		return ErrEmptyPayload
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("inbox/consumer: unmarshal payload: %w", err)
	}
	return nil
}

// aggregateIntID 在 payload 未给出数值 ID 时回退用聚合根 ID；非数字返回 0（只影响文案）。
func aggregateIntID(aggregateID string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(aggregateID), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// occurredAtSeconds 把信封时间转成 Unix 秒；解析失败返回 0，由 model 层兜当前时间。
func occurredAtSeconds(s string) int64 {
	t, err := timeutil.ParseRFC3339(s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// quoteOrEmpty 日志里区分「action 缺失」与「action 为空串」。
func quoteOrEmpty(s string) string {
	if s == "" {
		return "<空>"
	}
	return s
}

// clipRune 按 rune 截断，避免切断 UTF-8 多字节汉字导致 utf8mb4 拒绝写入。
func clipRune(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimSpace(string(r[:max]))
}
