// eventaction.go 是 engagement.action.v1 的 payload 与信封装配（本服务唯一产出的事件）。
//
// 单独成文件而不是写进 repository.go：payload 的字段清单是跨服务契约
// （docs/api-and-events.md §5 登记的 engagement.action.v1，消费方是 search-indexer 与 inbox），
// 改字段就是改契约，必须和「互动写库」这段数据访问代码分开放，才看得清哪一处需要递增 schema_version。
package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/core/trace"

	"go-video/common/eventenvelope"
	"go-video/common/timeutil"
	"go-video/services/engagement/model"
)

// heat 快照里 engagement 拥有并可以声明的字段名（payload 的 snapshot_fields 取值）。
//
// 这三个名字是契约的一部分：消费方（search-indexer）只覆盖这里声明的字段，
// 其余 heat 子字段（view_count/comment_count/danmaku_count/heat_score）由别的生产者拥有，
// 必须从索引现值带过来。所以改名或加字段要同步改 consumer 的合并逻辑，不能只动生产侧。
const (
	SnapshotFieldLikeCount     = "like_count"
	SnapshotFieldFavoriteCount = "favorite_count"
	SnapshotFieldShareCount    = "share_count"
)

// heatCounters 是事件里的绝对计数快照（不是增量）。
//
// 用绝对值的原因与消费方一致：增量事件在重放、并发消费或退避重试下会双计，
// 绝对快照让「同一事件投两次」与「投一次」结果相同。
// 没被 snapshot_fields 声明的字段留 0，消费方不会用它覆盖索引现值。
type heatCounters struct {
	LikeCount     int64 `json:"like_count"`
	FavoriteCount int64 `json:"favorite_count"`
	ShareCount    int64 `json:"share_count"`
	// HeatRevision 快照版本（Unix 毫秒）：与信封 occurred_at 同源（秒 × 1000）。
	HeatRevision int64 `json:"heat_revision"`
}

// engagementActionPayload 是 engagement.action.v1 的 payload（schema_version=1）。
//
// 只放 engagement 拥有的事实：动作名、被互动的对象、操作者、本域坐标，以及被声明的计数快照。
// 刻意不含的字段与理由（AGENTS.md §5 数据所有权 + §6 隐私，改动前先读这段）：
//   - content_type：索引文档主键的一部分（esclient.DocID(content_type, content_id)），
//     命名空间属于 search-indexer。本服务手里只有 business 字符串与 tp 整数
//     （obc 收藏类型：2 视频、11 ugv 视频、12 音频），把它们映射成 1 UGC/2 PGC/3 直播
//     是消费方的坐标注册表职责，服务端不发明别人的口径；
//   - target_mid / author_mid / recipients：inbox 用来定位收件人。本服务唯一可能的来源是
//     LikeReq.up_mid，而它是客户端任意传入、未经校验的字段（gateway/app 原样透传），
//     拿它当收件人等于让任何客户端给任意 mid 发站内信，因此一个都不填；
//     inbox 侧走 resolveRecipients 的「无收件人 ⇒ skip」分支，不会报错也不会误投；
//   - content_title / link：正文与跳转分别由 video/catalog 与客户端拥有；
//   - dislike_number：索引的 heat 里没有这个字段，声明它只会让消费方拿 0 覆盖别人的计数；
//   - view / comment / danmaku 计数与 heat_score：owner 是 playback / comment / danmaku / spm，
//     本事件不带，因此 snapshot_fields 也只声明本服务真正改过的那几列。
//
// 字段只增不改：删除或改名会让已发布的 v1 事件在消费侧解错，必须递增 schema_version。
type engagementActionPayload struct {
	Action    string `json:"action"` // like/cancel_like/favorite/cancel_favorite/share
	ContentID int64  `json:"content_id"`
	Mid       int64  `json:"mid"`
	// Business 与 OriginID 是点赞路径的本域坐标（thumbup_stat 的唯一键列）；
	// Tp 与 Otype 是收藏/分享路径的坐标（favorite_item/share_log 的列）。
	// 消费方按 (business, tp) 注册表判定这条互动落在哪一类内容上，未登记的坐标会被跳过。
	Business string `json:"business,omitempty"`
	OriginID int64  `json:"origin_id,omitempty"`
	Tp       int32  `json:"tp,omitempty"`
	Otype    int32  `json:"otype,omitempty"`
	// Counters 一定是对象而不是 null：消费方用「counters 缺失」判定契约违反，
	// 一个只发动作不发动作快照的事件对它没有意义。
	Counters heatCounters `json:"counters"`
	// SnapshotFields 声明 Counters 里哪些字段是本次事件的权威值。
	// 空数组不会出现在产出的事件里（那种情况根本不产事件）。
	SnapshotFields []string `json:"snapshot_fields"`
	HeatRevision   int64    `json:"heat_revision"`
}

// actionNameForLike 把一次点赞状态转换翻译成动作名。
// 返回值只用于人读与审计：消费方判热度用的是 counters + snapshot_fields，不是动作名。
func actionNameForLike(oldState, newState int32) string {
	switch newState {
	case 1:
		return model.ActionLike
	case 2:
		return model.ActionDislike
	case 0:
		// 取消动作的名字取被撤销的那一侧：从点赞撤销才是 cancel_like。
		if oldState == 1 {
			return model.ActionCancelLike
		}
		return model.ActionCancelDislike
	}
	return ""
}

// clampCounter 把计数快照的非正值归零。
//
// 只作用于事件，不作用于返回给 RPC 调用方的值（那边必须继续看到库里的真实数字，
// 否则缺陷就永远浮不出来）。原因：thumbup_stat 缺行时取消点赞会把 like_number 写成 -1
// （README 已知缺口 4），而这个绝对快照会被消费方 last-write-wins 写进索引，
// 「搜索结果里这条视频有 -1 个赞」比「暂时少一个赞」糟糕得多。
// 根因仍在 Incr 没有 GREATEST(0, ...) 兜底，那条修好之后这里可以直接透传。
func clampCounter(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// pendingEvent 是一期待落库的事件：信封 + 已序列化的 payload + outbox 列上的 occurred_at。
type pendingEvent struct {
	env        *eventenvelope.Envelope
	payload    string
	occurredAt int64
}

// buildActionEvent 组装 engagement.action.v1。
//
// in.snapshot 为空时返回 (nil, nil)：本次互动没有改变任何「本服务在事件契约里拥有的计数」
// （典型：纯点踩、幂等重复请求），不产事件，而不是发一条空快照让消费方猜。
// 返回 error 一律让整笔事务回滚：
//   - outboxMd 为 nil 却有事件要写：装配错误，静默跳过会让「点了赞但索引热度永远不更新」
//     变成生产现场的常态；
//   - content_id 非正、序列化或信封校验失败：不合契约的事件发出去也只会进死信。
func (r *Repository) buildActionEvent(ctx context.Context, action string, contentID, mid int64,
	business string, originID int64, tp, otype int32, counters heatCounters, snapshot []string, occurredAt int64) (*pendingEvent, error) {
	if len(snapshot) == 0 {
		return nil, nil
	}
	if contentID <= 0 {
		return nil, fmt.Errorf("engagement/repository: %s 事件的 content_id 非法（%d），不能组装 engagement.action.v1", action, contentID)
	}
	if r.outboxMd == nil {
		return nil, fmt.Errorf("engagement/repository: %s 事件需要注入 EngagementOutboxModel（content_id=%d）", action, contentID)
	}
	revision := occurredAt * 1000
	if revision <= 0 {
		return nil, fmt.Errorf("engagement/repository: %s 事件的 occurred_at 非法（%d），heat_revision 会缺版本", action, occurredAt)
	}
	counters.LikeCount = clampCounter(counters.LikeCount)
	counters.FavoriteCount = clampCounter(counters.FavoriteCount)
	counters.ShareCount = clampCounter(counters.ShareCount)
	counters.HeatRevision = revision
	body, err := json.Marshal(engagementActionPayload{
		Action:         action,
		ContentID:      contentID,
		Mid:            mid,
		Business:       business,
		OriginID:       originID,
		Tp:             tp,
		Otype:          otype,
		Counters:       counters,
		SnapshotFields: snapshot,
		HeatRevision:   revision,
	})
	if err != nil {
		return nil, fmt.Errorf("engagement/repository: 序列化 engagement.action.v1 payload: %w", err)
	}
	env, err := eventenvelope.New(model.Producer, model.EventEngagementAction,
		model.AggregateTypeAction, strconv.FormatInt(contentID, 10), model.EventSchemaVersion,
		body, trace.TraceIDFromContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("engagement/repository: 组装 engagement.action.v1 信封: %w", err)
	}
	payload, err := marshalEnvelope(env)
	if err != nil {
		return nil, err
	}
	// 列上的 occurred_at 从信封解析：信封是契约真源，列只是可检索的副本，两者不同源就没法对账。
	occurredAtFromEnv, err := parseOccurredAt(env.OccurredAt)
	if err != nil {
		return nil, err
	}
	return &pendingEvent{env: env, payload: payload, occurredAt: occurredAtFromEnv}, nil
}

// insertEvent 把一期待发布事件写进当前事务；evt 为 nil 表示本次不产事件，直接成功。
// 与 buildActionEvent 分在两个文件：这里只谈「落库」，那里的注释谈「契约」。
func (r *Repository) insertEvent(ctx context.Context, session sqlx.Session, evt *pendingEvent) error {
	if evt == nil {
		return nil
	}
	return r.outboxMd.Insert(ctx, session, &model.EngagementOutbox{
		EventID:   evt.env.EventID,
		EventType: evt.env.EventType,
		// 信封的 schema_version 是 int，列是 INT（int32）：值由 model.EventSchemaVersion 单一来源给出，
		// 这里只做窄化，不引入第二个版本真相。
		SchemaVersion: int32(evt.env.SchemaVersion),
		AggregateType: evt.env.AggregateType,
		AggregateID:   evt.env.AggregateID,
		Payload:       evt.payload,
		OccurredAt:    evt.occurredAt,
	})
}

// marshalEnvelope 序列化信封。Envelope.MarshalJSON 在序列化阶段再跑一次 Validate，
// 因此残缺信封在这里就报错，不会写出一行「业务列齐全但 payload 不合法」的 outbox。
func marshalEnvelope(env *eventenvelope.Envelope) (string, error) {
	raw, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("engagement/repository: 序列化 engagement.action.v1 信封: %w", err)
	}
	return string(raw), nil
}

// parseOccurredAt 把信封的 RFC3339 occurred_at 转成 Unix 秒。
func parseOccurredAt(occurredAt string) (int64, error) {
	t, err := timeutil.ParseRFC3339(occurredAt)
	if err != nil {
		return 0, fmt.Errorf("engagement/repository occurred_at %q: %w", occurredAt, err)
	}
	return t.Unix(), nil
}
