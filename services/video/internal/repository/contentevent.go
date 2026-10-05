// contentevent.go 是 content.published.v1 的 payload 与信封装配（本服务唯一产出的事件）。
//
// 为什么单独成文件而不是写进 repository.go：payload 的字段清单是跨服务契约
// （docs/api-and-events.md §5 登记的 content.published.v1，消费方是 search-indexer 与 inbox），
// 改字段就是改契约，必须和「状态推进」这段数据访问代码分开放，才看得清哪一处需要递增 schema_version。
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zeromicro/go-zero/core/trace"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/video/model"
)

// contentPublishedPayload 是 content.published.v1 的 payload（schema_version=1）。
//
// 只放 video 拥有的事实：可见性动作、内容主键与归属、正文展示四件套、分区与标签、
// 版本时间戳，以及下架/过期/删除时的审计原因。
// 刻意不含的字段与理由（AGENTS.md §5 数据所有权，改动前先读这段）：
//   - duration_sec / language / subtitle_langs：媒资事实，owner 是 asset/transcode；
//   - author_name：可变主资料，owner 是 user-profile；
//   - type_name：分区名，owner 是 catalog；
//   - rights_expire_at：版权窗口，owner 是 rights（UGC 也没有这个概念）；
//   - heat / heat_score / heat_revision：互动计数与打分，走 engagement.action.v1，
//     由 search-indexer 的 HeatFromEngagementEvent 单独打补丁，本事件不代答；
//   - sensitive：审核结论，owner 是 moderation-orchestrator；
//   - recipients / link：收件人与跳转分别由 inbox、客户端负责，服务端不发明 fan-out 与 UI 路由。
//
// 字段只增不改：删除或改名会让已发布的 v1 事件在消费侧解错，必须递增 schema_version。
type contentPublishedPayload struct {
	Action      string   `json:"action"` // publish/offline/expired/delete
	ContentID   int64    `json:"content_id"`
	ContentType int32    `json:"content_type"` // 恒 1（UGC）
	Title       string   `json:"title"`
	Description string   `json:"description"`
	CoverURL    string   `json:"cover_url"`
	AuthorMid   int64    `json:"author_mid"`
	Typeid      int32    `json:"typeid"`
	Tags        []string `json:"tags"`
	// PublishAt 本次转为对外可见的时间（Unix 秒）。
	// 只有 publish 动作赋值：下架/过期事件不声明发布时间，0 是「本事件不含这个事实」，
	// 不是字段缺失（消费侧的 takedown 路径不重建整篇，因此不会把索引里的发布时间抹成 0）。
	PublishAt int64 `json:"publish_at,omitempty"`
	// Ctime 稿件创建时间（Unix 秒），取自 video_submission.ctime，不伪造。
	Ctime int64 `json:"ctime"`
	// DocRevision 事实版本（Unix 毫秒）：与同事务写入的 video_audit_log.ctime 同源（秒 × 1000），
	// 消费侧据此做 last-write-wins，运维也能用一个数字反查到那条审计行。
	DocRevision int64  `json:"doc_revision"`
	Reason      string `json:"reason,omitempty"`
}

// contentActionFor 把一条状态转换翻译成对外可见性动作。
// ok=false 表示该转换不改变「谁能看到这条稿件」，不产事件。
//
// DELETED 只在稿件曾经公开过时才产 delete：草稿、上传中、被驳回的稿件从未进过索引，
// 给作者发「你的作品《x》已被删除」既失真也没意义。
func contentActionFor(fromState, toState int32) (string, bool) {
	switch toState {
	case model.StatePublished:
		return model.ActionPublish, true
	case model.StateOffline:
		return model.ActionOffline, true
	case model.StateExpired:
		return model.ActionExpired, true
	case model.StateDeleted:
		if wasPubliclyKnown(fromState) {
			return model.ActionDelete, true
		}
	}
	return "", false
}

// wasPubliclyKnown 判断稿件在转换前是否处于「对外可见过」的状态。
// 含 EXPIRED：那是公开过后的终态，将来若放开 EXPIRED → DELETED，
// 这条转换必须继续清索引，而不是因为少列一个状态就静默漏投。
func wasPubliclyKnown(state int32) bool {
	switch state {
	case model.StatePublished, model.StateOffline, model.StateExpired:
		return true
	}
	return false
}

// buildContentPublishedEvent 用「事务内刚读到的稿件行 + 本次审计时间」组装
// content.published.v1 信封。
//
// sub 是 TransitionState 在事务内二次校验时读到的行：payload 的正文四件套必须来自它，
// 而不是 logic 层更早的那次读（两者之间稿件可能已被改过）。
// auditCtime 与同批写入的 video_audit_log.ctime 取同一个值，doc_revision 由此派生。
//
// trace_id 取自 ctx 的链路上下文：go-zero 在没有 trace 时返回空串，
// 信封的 trace_id 是 omitempty 字段，因此既不会污染契约也不会丢事件。
func buildContentPublishedEvent(ctx context.Context, sub *model.VideoSubmission,
	fromState, toState int32, reason string, auditCtime int64) (*eventenvelope.Envelope, error) {
	if sub == nil {
		return nil, fmt.Errorf("video/repository: content.published.v1 需要稿件行")
	}
	action, ok := contentActionFor(fromState, toState)
	if !ok {
		return nil, fmt.Errorf("video/repository: aid=%d 的 %d→%d 转换不是可见性变更，不应组装事件",
			sub.Aid, fromState, toState)
	}
	if auditCtime <= 0 {
		return nil, fmt.Errorf("video/repository: aid=%d 的审计时间非法（%d），事件会缺 doc_revision", sub.Aid, auditCtime)
	}
	publishAt := int64(0)
	if action == model.ActionPublish {
		publishAt = auditCtime
	}
	body, err := json.Marshal(contentPublishedPayload{
		Action:      action,
		ContentID:   sub.Aid,
		ContentType: model.ContentTypeUGC,
		Title:       sub.Title,
		Description: sub.Desc,
		CoverURL:    sub.Cover,
		AuthorMid:   sub.Mid,
		Typeid:      sub.Typeid,
		Tags:        splitTags(sub.Tag),
		PublishAt:   publishAt,
		Ctime:       sub.Ctime,
		DocRevision: auditCtime * 1000,
		Reason:      reason,
	})
	if err != nil {
		return nil, fmt.Errorf("video/repository: 序列化 content.published.v1 payload: %w", err)
	}
	env, err := eventenvelope.New(model.Producer, model.EventContentPublished,
		model.AggregateTypeSubmission, strconv.FormatInt(sub.Aid, 10), model.EventSchemaVersion,
		body, trace.TraceIDFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("video/repository: 组装 content.published.v1 信封: %w", err)
	}
	return env, nil
}

// splitTags 把 video_submission.tag 的逗号分隔串切成数组。
// 空串、纯空白段一律丢掉，因此无标签稿件的 tags 是 []（而不是 [""]，那会让索引多一个空标签）。
func splitTags(tag string) []string {
	parts := strings.Split(tag, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// pendingEvent 是「本次转换要落库的 Outbox 行」：信封 + 已序列化的 payload + 列上的 occurred_at。
type pendingEvent struct {
	env        *eventenvelope.Envelope
	payload    string
	occurredAt int64
}

// buildVisibilityEvent 把可见性转换翻译成一期待发布的事件；非可见性转换返回 nil（不产事件）。
//
// 返回 error 的两种情况都必须让整笔事务回滚，而不是跳过事件：
//   - outboxMd 为 nil（NewWithDeps 没注入）却要写事件行：装配错误，静默跳过会让
//     「已发布的稿件永远不进索引」变成生产现场的常态；
//   - 信封组装或序列化失败：payload 不合契约的事件发出去也只会进死信。
func (r *Repository) buildVisibilityEvent(ctx context.Context, sub *model.VideoSubmission,
	fromState, toState int32, reason string, auditCtime int64) (*pendingEvent, error) {
	if _, ok := contentActionFor(fromState, toState); !ok {
		return nil, nil
	}
	if r.outboxMd == nil {
		return nil, fmt.Errorf("video/repository: aid=%d 的 %d→%d 是可见性转换，必须注入 VideoOutboxModel",
			sub.Aid, fromState, toState)
	}
	env, err := buildContentPublishedEvent(ctx, sub, fromState, toState, reason, auditCtime)
	if err != nil {
		return nil, err
	}
	payload, err := marshalEnvelope(env)
	if err != nil {
		return nil, err
	}
	occurredAt, err := parseOccurredAt(env.OccurredAt)
	if err != nil {
		return nil, err
	}
	return &pendingEvent{env: env, payload: payload, occurredAt: occurredAt}, nil
}

// marshalEnvelope 序列化信封。Envelope.MarshalJSON 在序列化阶段再跑一次 Validate，
// 因此残缺信封在这里就报错，不会写出一行「六个业务列齐全但 payload 不合法」的 outbox。
func marshalEnvelope(env *eventenvelope.Envelope) (string, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("video/repository: 序列化 content.published.v1 信封: %w", err)
	}
	return string(raw), nil
}

// parseOccurredAt 把信封的 RFC3339 occurred_at 转成 outbox 表存的 Unix 秒，
// 保证「列的 occurred_at」与「信封的 occurred_at」同源（信封是契约真源，列只是检索索引）。
func parseOccurredAt(occurredAt string) (int64, error) {
	t, err := timeutil.ParseRFC3339(occurredAt)
	if err != nil {
		return 0, fmt.Errorf("video/repository occurred_at %q: %w", occurredAt, err)
	}
	return t.Unix(), nil
}
