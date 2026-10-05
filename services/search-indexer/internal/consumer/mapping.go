// Package consumer 承载 search-indexer 的事件消费与后台执行循环。
//
// 目录说明（AGENTS.md §3）：MQ 消费者放在拥有写入权的服务里，本包即消费
// `content.published.v1` / `engagement.action.v1` 并写入本服务的索引投影；
// 重建任务执行循环（RebuildRunner）与重试清扫（RetrySweeper）同属本服务 worker 侧，
// 不额外新增 jobs/ 顶层目录。
package consumer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/search-indexer/internal/esclient"
)

// 本服务消费的事件类型（docs/api-and-events.md §5）。
const (
	EventTypeContentPublished = "content.published"
	EventTypeEngagementAction = "engagement.action"
)

// content.published 的 action 取值。
const (
	ActionPublish = "publish"
	ActionUpdate  = "update"
	ActionOffline = "offline"
	ActionExpired = "expired"
	ActionDelete  = "delete"
)

// HeatPayload 互动计数快照（绝对值，避免事件重放导致重复累加）。
// 字段与 rpc.HeatSnapshot 一一对应；计数或分值缺失时留 0，本服务不做「读-改-写累加」。
type HeatPayload struct {
	ViewCount     int64 `json:"view_count"`
	LikeCount     int64 `json:"like_count"`
	FavoriteCount int64 `json:"favorite_count"`
	ShareCount    int64 `json:"share_count"`
	CommentCount  int64 `json:"comment_count"`
	DanmakuCount  int64 `json:"danmaku_count"`
	// HeatScore 归一化热度分值，由上游（SPM/推荐）计算后推送，本服务不自行打分。
	HeatScore int32 `json:"heat_score"`
	// HeatRevision 快照版本（Unix 毫秒），热度字段按 last-write-wins 覆盖；
	// 嵌套值优先，缺失时回退到 payload 顶层的同名字段。
	HeatRevision int64 `json:"heat_revision"`
}

// ContentPublishedPayload 是 `content.published.v1` 的 payload 契约。
//
// 生产者是 video / catalog / rights（发布、下架、过期）；本结构只声明 search-indexer
// 需要消费的字段，未声明的字段在反序列化时被忽略，因此敏感字段（IP、手机号、身份证、
// Token）即使被误投递也不会进入索引（AGENTS.md §7）。
type ContentPublishedPayload struct {
	Action         string       `json:"action"`       // publish/update/offline/expired/delete
	ContentID      int64        `json:"content_id"`   // 内容主键；为 0 时回退用 envelope.aggregate_id
	ContentType    int32        `json:"content_type"` // 1 UGC、2 PGC、3 直播
	Title          string       `json:"title"`
	Description    string       `json:"description"`
	CoverURL       string       `json:"cover_url"`
	AuthorMid      int64        `json:"author_mid"`
	AuthorName     string       `json:"author_name"`
	Typeid         int32        `json:"typeid"`
	TypeName       string       `json:"type_name"`
	Tags           []string     `json:"tags"`
	DurationSec    int64        `json:"duration_sec"`
	PublishAt      int64        `json:"publish_at"`
	Ctime          int64        `json:"ctime"`
	DocRevision    int64        `json:"doc_revision"`     // 事实版本（Unix 毫秒）；为 0 时回退 occurred_at
	RightsExpireAt int64        `json:"rights_expire_at"` // PGC 版权窗口结束
	Language       string       `json:"language"`
	SubtitleLangs  []string     `json:"subtitle_langs"`
	Sensitive      bool         `json:"sensitive"`
	Heat           *HeatPayload `json:"heat"`
	HeatScore      int32        `json:"heat_score"`
	HeatRevision   int64        `json:"heat_revision"`
}

// EngagementActionPayload 是 `engagement.action.v1` 的 payload 契约。
// 互动事件只更新热度字段，不重建整篇文档。
type EngagementActionPayload struct {
	ContentID   int64        `json:"content_id"`
	ContentType int32        `json:"content_type"`
	Action      string       `json:"action"` // like/cancel_like/favorite/share/comment/danmaku/play
	Mid         int64        `json:"mid"`    // 操作者用户 ID，只用于审计，不写入索引
	Counters    *HeatPayload `json:"counters"`
	HeatScore   int32        `json:"heat_score"`
	// HeatRevision 热度快照版本（Unix 毫秒）。缺失时无法做防旧覆盖新判定，判为永久错误。
	HeatRevision int64 `json:"heat_revision"`
}

// ErrHeatSnapshotRequired 事件只给了动作没给计数快照。
// 索引侧不做「读-改-写累加」，因为并发下会双计，重放也会双计。
var ErrHeatSnapshotRequired = errors.New("search-indexer: engagement.action 事件必须携带 counters 绝对快照")

// ErrUnknownAction content.published 的 action 不在契约内。
var ErrUnknownAction = errors.New("search-indexer: unknown content.published action")

// ParseEnvelope 解析并校验事件信封（eventenvelope 自带结构校验）。
func ParseEnvelope(raw []byte) (*eventenvelope.Envelope, error) {
	var env eventenvelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("consumer: unmarshal envelope: %w", err)
	}
	return &env, nil
}

// SupportedEventType 判断事件类型是否由本服务消费。
func SupportedEventType(eventType string) bool {
	switch eventType {
	case EventTypeContentPublished, EventTypeEngagementAction:
		return true
	default:
		return false
	}
}

// aggregateIDToInt64 在 payload 未显式给 content_id 时回退使用聚合根 ID。
func aggregateIDToInt64(aggregateID string) (int64, error) {
	v := strings.TrimSpace(aggregateID)
	if v == "" {
		return 0, fmt.Errorf("consumer: content_id missing and aggregate_id empty")
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("consumer: aggregate_id %q is not numeric content_id: %w", v, err)
	}
	return id, nil
}

// occurredAtMillis 把信封 occurred_at 转成 Unix 毫秒。
func occurredAtMillis(s string) int64 {
	t, err := timeutil.ParseRFC3339(s)
	if err != nil {
		return 0
	}
	return t.UnixNano() / int64(time.Millisecond)
}

// DocFromContentEvent 由 content.published 事件构造内容投影（publish/update 路径）。
//
// 字段映射规则：
//   - content_id：payload.content_id 优先，缺失时回退 envelope.aggregate_id；
//   - doc_revision：payload.doc_revision 优先，缺失时回退 occurred_at（毫秒）；
//   - state：publish → PUBLISHED、update → PUBLISHED（由上游保证已过审）；
//   - heat：payload.heat + heat_revision，缺省时全零并沿用 doc_revision 作为热度版本。
func DocFromContentEvent(env *eventenvelope.Envelope, p *ContentPublishedPayload) (*esclient.ContentDoc, error) {
	if env == nil || p == nil {
		return nil, fmt.Errorf("consumer: nil event or payload")
	}
	contentID := p.ContentID
	if contentID <= 0 {
		v, err := aggregateIDToInt64(env.AggregateID)
		if err != nil {
			return nil, err
		}
		contentID = v
	}
	revision := p.DocRevision
	if revision <= 0 {
		revision = occurredAtMillis(env.OccurredAt)
	}
	if revision <= 0 {
		return nil, fmt.Errorf("consumer: content_id=%d 缺少 doc_revision（事件必须携带事实版本以便防旧覆盖新）", contentID)
	}

	doc := &esclient.ContentDoc{
		ContentID:      contentID,
		ContentType:    p.ContentType,
		Title:          p.Title,
		Description:    p.Description,
		CoverURL:       p.CoverURL,
		AuthorMid:      p.AuthorMid,
		AuthorName:     p.AuthorName,
		Typeid:         p.Typeid,
		TypeName:       p.TypeName,
		Tags:           normalizeTags(p.Tags),
		DurationSec:    p.DurationSec,
		PublishAt:      p.PublishAt,
		Ctime:          p.Ctime,
		State:          esclient.StatePublished,
		DocRevision:    revision,
		RightsExpireAt: p.RightsExpireAt,
		Language:       p.Language,
		SubtitleLangs:  p.SubtitleLangs,
		Sensitive:      p.Sensitive,
		SchemaVersion:  1,
	}
	if p.Heat != nil {
		doc.Heat = esclient.Heat{
			ViewCount:     p.Heat.ViewCount,
			LikeCount:     p.Heat.LikeCount,
			FavoriteCount: p.Heat.FavoriteCount,
			ShareCount:    p.Heat.ShareCount,
			CommentCount:  p.Heat.CommentCount,
			DanmakuCount:  p.Heat.DanmakuCount,
			HeatScore:     p.Heat.HeatScore,
			HeatRevision:  p.Heat.HeatRevision,
		}
		// 顶层 heat_revision 兼容只把版本放在 payload 顶层的生产者。
		if doc.Heat.HeatRevision == 0 {
			doc.Heat.HeatRevision = p.HeatRevision
		}
		if doc.Heat.HeatScore == 0 {
			doc.Heat.HeatScore = p.HeatScore
		}
		if doc.Heat.HeatRevision == 0 {
			// 事件里没单独给热度版本时，热度随正文版本一起单调。
			doc.Heat.HeatRevision = revision
		}
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return doc, nil
}

// HeatFromEngagementEvent 由 engagement.action 事件构造热度快照。
// 缺少绝对计数快照或版本时返回永久错误（见 ErrHeatSnapshotRequired）。
func HeatFromEngagementEvent(env *eventenvelope.Envelope, p *EngagementActionPayload) (contentID int64, contentType int32, heat esclient.Heat, err error) {
	if env == nil || p == nil {
		return 0, 0, heat, fmt.Errorf("consumer: nil event or payload")
	}
	contentID = p.ContentID
	if contentID <= 0 {
		v, cerr := aggregateIDToInt64(env.AggregateID)
		if cerr != nil {
			return 0, 0, heat, cerr
		}
		contentID = v
	}
	if p.Counters == nil {
		return 0, 0, heat, ErrHeatSnapshotRequired
	}
	// 版本可写在 counters 里或 payload 顶层，但必须有一个，否则无法判定新旧。
	revision := p.Counters.HeatRevision
	if revision <= 0 {
		revision = p.HeatRevision
	}
	if revision <= 0 {
		return 0, 0, heat, fmt.Errorf("%w: content_id=%d 缺少 heat_revision", ErrHeatSnapshotRequired, contentID)
	}
	score := p.Counters.HeatScore
	if score == 0 {
		score = p.HeatScore
	}
	heat = esclient.Heat{
		ViewCount:     p.Counters.ViewCount,
		LikeCount:     p.Counters.LikeCount,
		FavoriteCount: p.Counters.FavoriteCount,
		ShareCount:    p.Counters.ShareCount,
		CommentCount:  p.Counters.CommentCount,
		DanmakuCount:  p.Counters.DanmakuCount,
		HeatScore:     score,
		HeatRevision:  revision,
	}
	return contentID, p.ContentType, heat, nil
}

// ClassifyAction 把 content.published 的 action 归类：
//   - kind=ActionKindUpsert：写入/更新整篇投影；
//   - kind=ActionKindTakedown：只降级或移除投影（不重建整篇，避免部分 payload 抹掉字段）。
//
// 未知 action 返回 ErrUnknownAction，由上层判为永久错误进死信，绝不「猜测着写索引」。
func ClassifyAction(action string) (kind string, purge bool, err error) {
	switch action {
	case ActionPublish, ActionUpdate:
		return ActionKindUpsert, false, nil
	case ActionOffline, ActionExpired:
		return ActionKindTakedown, false, nil
	case ActionDelete:
		return ActionKindTakedown, true, nil
	default:
		return "", false, fmt.Errorf("%w: %q", ErrUnknownAction, action)
	}
}

// action 分类结果。
const (
	ActionKindUpsert   = "upsert"
	ActionKindTakedown = "takedown"
)

// normalizeTags 去空白、去重、限制数量，避免脏标签污染 keyword 字段。
func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	const maxTags = 32
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// unmarshalPayload 把信封 payload 解到具体结构。
// 使用严格模式会因生产者新增字段而报错，这里保持前向兼容（§2 兼容窗口）。
func unmarshalPayload(raw json.RawMessage, target interface{}) error {
	if len(raw) == 0 {
		return fmt.Errorf("consumer: empty payload")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("consumer: unmarshal payload: %w", err)
	}
	return nil
}
