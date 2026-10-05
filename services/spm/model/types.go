// Package model 是 spm 服务的数据库访问层，只操作 go_video_spm 库自身的表
// （AGENTS.md §5：服务只能写自己的 schema）。
//
// 表清单与 deploy/migrations/spm/*.sql 严格一致（10 张表，库名 go_video_spm）：
//
//	spm_behavior_event      脱敏后的行为事件事实（幂等真值 event_id，指标重算的事实源）
//	spm_metric_definition   指标口径注册表（口径版本化的落点）
//	spm_metric_window       窗口指标投影（可从事实重算，不是唯一事实源）
//	spm_window_watermark    窗口闭合水位投影（window_start=0 的解析依据，可重算）
//	spm_user_interest       用户兴趣画像投影（可从事实重算）
//	spm_retention_cohort    留存投影（可从事实重算）
//	spm_aggregation_job     实时/离线/重算作业与租约
//	spm_content_projection  content.published.v1 的本地只读投影（榜单过滤必需的最小列）
//	spm_consumer_offset     事件消费状态机（按 event_id 幂等 + 可重放位点）
//	spm_dead_letter         死信留档（只存摘要与脱敏预览）
//
// 上游字段对齐（本包列名与生产者字段的关系，逐字取自生产者契约）：
//   - 内容主键：上游事件给的是 content_id + content_type，没有 aid/zone_id 这一对。
//     UGC 的 content_id 就是 aid，PGC 的 content_id 是 catalog 的 episode_id
//     （见 services/playback/rpc/playback.proto:47 与 CONTENT_TYPE_* 注释）。
//   - 分区：video 侧字段名是 typeid、catalog 侧是 zoneid；behavior.* 与 playback.heartbeat
//     都不带分区，只能由消费者 mapping 阶段查 spm_content_projection 补齐（补不到就是 0）。
//   - content_type 的类型在上游就不一致：event-collector 的 BehaviorEvent.content_type 是
//     字符串枚举（ugc/pgc/live/keyword），playback/engagement/content.published 是整数
//     1/2/3。本包统一存整数，字符串 -> 整数的转换见 ContentTypeFromName。
//
// 隐私边界（AGENTS.md §7、docs/data-design.md §6）：本包不接受、不保存明文设备号、
// 手机号与原始 IP；写入路径只允许哈希摘要列（spm_behavior_event.pseudonym/pseudonym_kind）。
package model

import (
	"strconv"
	"strings"

	"go-video/common/eventenvelope"
)

// 聚合主体类型，与 rpc.SubjectType 逐值对齐（proto 是契约源，这里是落库形态）。
const (
	SubjectTypeUnspecified int32 = 0
	SubjectTypeAid         int32 = 1 // 稿件 aid
	SubjectTypeZone        int32 = 2 // 分区 zone_id
	SubjectTypeMid         int32 = 3 // 用户 mid
	SubjectTypeCatalogItem int32 = 4 // 版权内容条目 item_id
)

// ValidSubjectType 判断主体类型是否可作为指标主体。
func ValidSubjectType(t int32) bool { return t >= SubjectTypeAid && t <= SubjectTypeCatalogItem }

// 窗口粒度，与 rpc.WindowType 对齐。数值即「窗口秒数」的档位编号，
// 真正的边界规整由 WindowSeconds / AlignWindow 完成。
const (
	WindowTypeUnspecified int32 = 0
	WindowType5Min        int32 = 1
	WindowTypeHour        int32 = 2
	WindowTypeDay         int32 = 3
	WindowTypeWeek        int32 = 4
	WindowTypeTotal       int32 = 5
)

// ValidWindowType 判断窗口粒度是否合法。
func ValidWindowType(t int32) bool { return t >= WindowType5Min && t <= WindowTypeTotal }

// WindowSeconds 返回窗口粒度对应的秒数；TOTAL 返回 0（window_start 固定 0）。
//
// 这些常量是口径的一部分：改动等价于口径变更，必须同步新增 metric_version。
func WindowSeconds(t int32) int64 {
	switch t {
	case WindowType5Min:
		return 300
	case WindowTypeHour:
		return 3600
	case WindowTypeDay:
		return 86400
	case WindowTypeWeek:
		return 604800
	case WindowTypeTotal:
		return 0
	default:
		return -1
	}
}

// AlignWindow 把任意时间戳规整到窗口左边界（Unix 秒）。
// 落库与查询都必须先规整，否则同一分钟的数据会散落到多个 window_start 上。
// ts<0 返回 0，TOTAL 恒为 0。
func AlignWindow(ts int64, windowType int32) int64 { return alignWindowUnix(ts, windowType) }

func alignWindowUnix(ts int64, windowType int32) int64 {
	if ts <= 0 {
		return 0
	}
	sec := WindowSeconds(windowType)
	if sec <= 0 {
		return 0
	}
	return ts - ts%sec
}

// 指标写入来源，与 rpc.MetricSource 对齐。只有计算链路可以写指标，
// 结构上不存在「运营手工改指标」这一档（AGENTS.md §7 第 3 条）。
const (
	MetricSourceUnspecified int32 = 0
	MetricSourceRealtime    int32 = 1
	MetricSourceOffline     int32 = 2
	MetricSourceRecompute   int32 = 3
)

// ValidMetricSource 判断写入来源是否属于允许的计算链路。
func ValidMetricSource(s int32) bool { return s >= MetricSourceRealtime && s <= MetricSourceRecompute }

// 口径版本状态，与 rpc.DefinitionState 对齐。
const (
	DefinitionStateUnspecified int32 = 0
	DefinitionStateDraft       int32 = 1
	DefinitionStateActive      int32 = 2
	DefinitionStateRetired     int32 = 3
)

// 作业类型与状态，与 rpc.JobType / rpc.JobState 对齐。
const (
	JobTypeUnspecified     int32 = 0
	JobTypeRealtime        int32 = 1
	JobTypeOfflineBackfill int32 = 2
	JobTypeRecompute       int32 = 3
)

const (
	JobStateUnspecified int32 = 0
	JobStatePending     int32 = 1
	JobStateRunning     int32 = 2
	JobStateSucceeded   int32 = 3
	JobStateFailed      int32 = 4
	JobStateCancelled   int32 = 5
)

// ValidJobType 判断作业类型是否合法（拒绝 UNSPECIFIED）。
func ValidJobType(t int32) bool { return t >= JobTypeRealtime && t <= JobTypeRecompute }

// 事件消费状态机（docs/api-and-events.md §6 要求的最小状态集合）。
const (
	ConsumerStateReceived   = "received"
	ConsumerStateProcessing = "processing"
	ConsumerStateSucceeded  = "succeeded"
	ConsumerStateRetry      = "retry"
	ConsumerStateDeadLetter = "dead_letter"
)

// 死信留档状态。
const (
	DeadLetterStateOpen     = "open"
	DeadLetterStateReplayed = "replayed"
	DeadLetterStateIgnored  = "ignored"
)

// 行为事件类型白名单（AGENTS.md §7 第 1 条列出的链路）。
//
// 两条来源通道，event_type 名字直接取自上游契约，不自行发明：
//  1. event-collector 归一化后的行为事件（rpc.BehaviorCategory -> behavior.<category>.v1）；
//  2. 领域服务自己发的原始事件（playback / engagement / search-query / video）。
//
// 不在白名单里的 event_type 一律跳过并记 succeeded，不写事实表：
// 「订阅面比消费能力宽」不是故障，全刷死信会淹没真实问题（docs/api-and-events.md §6）。
const (
	// 通道一：event-collector 归一化行为事件。
	EventBehaviorPlay     = "behavior.play"     // 播放开始/恢复
	EventBehaviorClick    = "behavior.click"    // 点击
	EventBehaviorSearch   = "behavior.search"   // 搜索提交/结果点击
	EventBehaviorSkip     = "behavior.skip"     // 跳过/关闭/不感兴趣
	EventBehaviorLike     = "behavior.like"     // 点赞
	EventBehaviorFavorite = "behavior.favorite" // 收藏
	EventBehaviorFollow   = "behavior.follow"   // 关注
	EventBehaviorShare    = "behavior.share"    // 分享
	EventBehaviorQuality  = "behavior.quality"  // 播放质量（卡顿/首帧/错误码）
	EventBehaviorExposure = "behavior.exposure" // 曝光

	// 通道二：领域服务原始事件（字段更丰富，但覆盖面与通道一重叠）。
	EventPlaybackHeartbeat = "playback.heartbeat" // playback：进度/完播/质量
	EventEngagementAction  = "engagement.action"  // engagement：点赞/收藏/关注/分享/评论/弹幕
	EventSearchQuery       = "search.query"       // search-query：检索行为
	EventContentPublished  = "content.published"  // video/catalog/rights：内容状态变更投影
)

// DefaultSchemaVersion 是当前订阅的事件结构版本。
// 生产者发布 v2 时消费者要先同时兼容 v1/v2（docs/api-and-events.md §2 兼容窗口），
// 所以这里只登记「默认」而不是唯一值：topic 名一律由 TopicFor 按信封实际版本派生。
const DefaultSchemaVersion = 1

// TopicFor 派生事件 topic 名（event_type + ".v" + schema_version）。
//
// 实现只有一处来源：common/eventenvelope.Topic。本服务不再自己拼一遍字符串，
// 否则「spm 认为的 topic」与「event-collector 实际投递的 topic」会静默分叉，
// 表现为消费者空转、spm_consumer_offset 里没有一行，而告警还显示一切正常。
// 白名单外的 event_type 返回空串，调用方按坏消息处理。
func TopicFor(eventType string, schemaVersion int) string {
	if !SupportedEventType(eventType) {
		return ""
	}
	return eventenvelope.Topic(eventType, schemaVersion)
}

// 事实行的来源通道。跨通道同源事件（例如 engagement.action 与 behavior.like 都表达点赞）
// 的重复计数由「口径登记里的 source_event_types 唯一指定来源」消除，
// 而不是在写入时猜测哪条是「真的」——那会让同一指标在两套通道下漂移。
const (
	SourceChannelUnspecified int32 = 0
	SourceChannelCollector   int32 = 1 // event-collector 投递的 behavior.*
	SourceChannelDomain      int32 = 2 // 领域服务原始事件
	SourceChannelContent     int32 = 3 // content.published：只服务投影，不参与行为计数
)

// ValidSourceChannel 判断来源通道是否合法。
func ValidSourceChannel(c int32) bool {
	return c >= SourceChannelCollector && c <= SourceChannelContent
}

// SupportedEventType 判断 event_type 是否在 spm 的处理范围内。
func SupportedEventType(t string) bool {
	switch t {
	case EventBehaviorPlay, EventBehaviorClick, EventBehaviorSearch, EventBehaviorSkip,
		EventBehaviorLike, EventBehaviorFavorite, EventBehaviorFollow, EventBehaviorShare,
		EventBehaviorQuality, EventBehaviorExposure,
		EventPlaybackHeartbeat, EventEngagementAction, EventSearchQuery, EventContentPublished:
		return true
	default:
		return false
	}
}

// ChannelOf 返回 event_type 的来源通道，未知类型返回 UNSPECIFIED。
func ChannelOf(eventType string) int32 {
	switch eventType {
	case EventBehaviorPlay, EventBehaviorClick, EventBehaviorSearch, EventBehaviorSkip,
		EventBehaviorLike, EventBehaviorFavorite, EventBehaviorFollow, EventBehaviorShare,
		EventBehaviorQuality, EventBehaviorExposure:
		return SourceChannelCollector
	case EventPlaybackHeartbeat, EventEngagementAction, EventSearchQuery:
		return SourceChannelDomain
	case EventContentPublished:
		return SourceChannelContent
	default:
		return SourceChannelUnspecified
	}
}

// 归一化行为动作键（受控词表）。interest 画像与热度口径都以它为准，
// 这样「同一动作来自哪条通道」不会改变指标语义。
const (
	ActionPlay     = "play"
	ActionFinish   = "finish"
	ActionSkip     = "skip"
	ActionClick    = "click"
	ActionExposure = "exposure"
	ActionSearch   = "search"
	ActionLike     = "like"
	ActionFavorite = "favorite"
	ActionFollow   = "follow"
	ActionShare    = "share"
	ActionComment  = "comment"
	ActionDanmaku  = "danmaku"
	ActionQuality  = "quality"
)

// 上游 payload.action 的取值（逐字取自生产者，不是本服务发明的词）：
//   - engagement.action.v1：services/inbox/internal/consumer/mapping.go 与
//     engagement 服务给出的 like/cancel_like/favorite/share/follow/comment/danmaku；
//   - content.published.v1：publish/update/offline/expired/delete。
//
// 撤销类动作（cancel_like/unfollow）生产者已经定义，本期没有「负样本」口径，
// 因此不映射成动作键；将来要支持时属于**口径变更**，必须新增 metric_version。
const (
	PayloadActionLike       = "like"
	PayloadActionCancelLike = "cancel_like"
	PayloadActionFavorite   = "favorite"
	PayloadActionShare      = "share"
	PayloadActionFollow     = "follow"
	PayloadActionUnfollow   = "unfollow"
	PayloadActionComment    = "comment"
	PayloadActionDanmaku    = "danmaku"

	PayloadActionPublish = "publish"
	PayloadActionUpdate  = "update"
	PayloadActionOffline = "offline"
	PayloadActionExpired = "expired"
	PayloadActionDelete  = "delete"
)

// NormalizeAction 把 (event_type, payload.action) 归一化成受控动作键。
// 返回 false 表示该组合没有分析语义（例如 cancel_like / unfollow 这类撤销动作，
// 以及 content.published 这类不参与行为计数的内容事件），调用方应记 succeeded 后跳过。
//
// behavior.* 通道的事件类型本身就等于动作，不看 payload.action；
// playback.heartbeat 同理，但心跳 payload 自带 completion（[0,1] 完播比例），
// play 与 finish 的区分由 mapping 阶段按口径登记的阈值判定，不在这里写死。
func NormalizeAction(eventType, payloadAction string) (string, bool) {
	switch eventType {
	case EventBehaviorPlay, EventPlaybackHeartbeat:
		return ActionPlay, true
	case EventBehaviorClick:
		return ActionClick, true
	case EventBehaviorExposure:
		return ActionExposure, true
	case EventBehaviorSearch, EventSearchQuery:
		return ActionSearch, true
	case EventBehaviorSkip:
		return ActionSkip, true
	case EventBehaviorQuality:
		return ActionQuality, true
	case EventBehaviorLike:
		return ActionLike, true
	case EventBehaviorFavorite:
		return ActionFavorite, true
	case EventBehaviorFollow:
		return ActionFollow, true
	case EventBehaviorShare:
		return ActionShare, true
	case EventEngagementAction:
		switch payloadAction {
		case PayloadActionLike:
			return ActionLike, true
		case PayloadActionFavorite:
			return ActionFavorite, true
		case PayloadActionShare:
			return ActionShare, true
		case PayloadActionFollow:
			return ActionFollow, true
		case PayloadActionComment:
			return ActionComment, true
		case PayloadActionDanmaku:
			return ActionDanmaku, true
		default:
			// cancel_like / unfollow 等撤销动作：本期不做「负样本」口径，直接跳过。
			return "", false
		}
	default:
		return "", false
	}
}

// 稿件投影状态：content.published.v1 带来的上下架状态，
// 决定该 aid 是否还能出现在热度榜上（下架内容不能被推荐位复用）。
const (
	ContentStateNormal int32 = 0 // 可见
	ContentStateHidden int32 = 1 // 下架/过期/删除，不再出榜
)

// ContentStateOfAction 把 content.published.v1 的 payload.action 映射成投影状态。
// 返回 false 表示该动作不改变可见性（本期只有 update：改标题/简介不影响出榜资格），
// 调用方应跳过而不是把状态重置成 normal。
//
// 这条映射是榜单正确性的关键：offline/expired/delete 必须把内容压出榜单，
// 而 publish 只恢复可见性——绝不因为「收到事件」就默认 normal。
func ContentStateOfAction(action string) (int32, bool) {
	switch action {
	case PayloadActionPublish:
		return ContentStateNormal, true
	case PayloadActionOffline, PayloadActionExpired, PayloadActionDelete:
		return ContentStateHidden, true
	default:
		return ContentStateUnchanged, false
	}
}

// ContentStateUnchanged 表示事件不改变投影可见性（区别于 normal：不得顺手放开出榜）。
const ContentStateUnchanged int32 = -1

// 兴趣键的受控词表（spm_user_interest.interest_key 的前缀集合）。
//
// 每条都是「<前缀>:<跨服务主键>」：兴趣画像因此可以被解释成
// 「对分区 1009 的兴趣」而不是一串没人认识的自由文本。
// 前缀集合与 rpc.GetUserInterestReply.Interest.interest_key 的注释逐字对齐
// （zone / tag / catalog / up），契约里写的是 up:<mid> 而不是 author:<mid>。
// 搜索词原文、标题、UP 昵称等自由文本一律禁止作为兴趣键——
// 那等价于把用户输入原文长期保存在画像里（AGENTS.md §7 隐私边界）。
const (
	InterestKeyPrefixZone    = "zone"
	InterestKeyPrefixTag     = "tag"
	InterestKeyPrefixCatalog = "catalog"
	InterestKeyPrefixUp      = "up"
)

// ValidInterestKey 判断兴趣键是否为「<受控前缀>:<正整数主键>」形态。
// 显式拒绝空键、未知前缀、负数/非数字 ID 与含空白的自由文本。
func ValidInterestKey(key string) bool {
	colon := strings.IndexByte(key, ':')
	if colon <= 0 || colon == len(key)-1 {
		return false
	}
	switch key[:colon] {
	case InterestKeyPrefixZone, InterestKeyPrefixTag, InterestKeyPrefixCatalog, InterestKeyPrefixUp:
	default:
		return false
	}
	id := key[colon+1:]
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return false
		}
	}
	// 前导零会让 zone:01 与 zone:1 成为两个兴趣键，实际指向同一个分区。
	return id[0] != '0' || len(id) == 1
}

// InterestKey 构造一个受控兴趣键；ID 非正或前缀未知时返回空串，
// 调用方必须把空串当错误处理（不要写进画像）。
func InterestKey(prefix string, id int64) string {
	if id <= 0 {
		return ""
	}
	switch prefix {
	case InterestKeyPrefixZone, InterestKeyPrefixTag, InterestKeyPrefixCatalog, InterestKeyPrefixUp:
		return prefix + ":" + strconv.FormatInt(id, 10)
	default:
		return ""
	}
}
