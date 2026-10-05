package model

import "errors"

// search-query 域错误。
//
// 约定（AGENTS.md §6）：gRPC 侧不使用 HTTP 信封，logic 直接返回这些哨兵错误，
// 由网关映射为稳定的业务 code。降级类错误必须是显式错误，不能返回空结果冒充成功。
var (
	// ErrInvalidKeyword 关键词为空、超长或含控制字符。
	ErrInvalidKeyword = errors.New("search-query: invalid keyword")
	// ErrInvalidMid 需要登录态但 mid 非法。
	ErrInvalidMid = errors.New("search-query: invalid mid")
	// ErrInvalidPage pn/ps/cursor 组合非法。
	ErrInvalidPage = errors.New("search-query: invalid page parameters")
	// ErrDeepPage 起始 offset 超过 MaxOffset 或引擎 max_result_window，拒绝深分页。
	ErrDeepPage = errors.New("search-query: page offset exceeds the allowed window")
	// ErrSearchUnavailable 引擎未配置或不可用（熔断/超时/连接失败）：明确降级，不伪造结果。
	ErrSearchUnavailable = errors.New("search-query: search engine temporarily unavailable, please retry later")
	// ErrAliasMissing 查询别名或索引不存在（重建中/别名未切换），属于运维可见故障。
	ErrAliasMissing = errors.New("search-query: search index alias not found")
	// ErrQueryRejected 引擎拒绝查询（DSL 语法、字段缺失等），需查日志修复。
	ErrQueryRejected = errors.New("search-query: query rejected by search engine")
	// ErrKeywordBlocked 关键词命中屏蔽词，不查询引擎，返回无结果语义。
	ErrKeywordBlocked = errors.New("search-query: keyword is blocked")
	// ErrInvalidSearchType 不支持的搜索类型。
	ErrInvalidSearchType = errors.New("search-query: invalid search type")
	// ErrInvalidSort 排序方式与搜索类型不匹配。
	ErrInvalidSort = errors.New("search-query: invalid sort for this search type")
	// ErrInvalidCursor 游标格式非法或版本不匹配。
	ErrInvalidCursor = errors.New("search-query: invalid cursor")
	// ErrCursorMismatch 游标不属于当前查询条件（翻页过程中改了筛选条件）。
	ErrCursorMismatch = errors.New("search-query: cursor does not match current query")
	// ErrConfirmRequired 删除类操作缺少 confirm=true（二次确认语义）。
	ErrConfirmRequired = errors.New("search-query: confirm=true is required to delete history")
	// ErrInvalidQueryID query_id 为空或超长（幂等键）。
	ErrInvalidQueryID = errors.New("search-query: invalid query_id")
	// ErrInvalidScope scope 非法（应为 global 或 zone:<id>）。
	ErrInvalidScope = errors.New("search-query: invalid scope")
	// ErrInvalidPlatform 端标识不在白名单（android/ios/harmony/desktop/web）。
	ErrInvalidPlatform = errors.New("search-query: invalid platform")
	// ErrHistoryNotFound 搜索历史行不存在（删除时返回 0 行而非报错，保留给内部判定）。
	ErrHistoryNotFound = errors.New("search-query: search history not found")
	// ErrOutboxNotFound outbox 行不存在。
	ErrOutboxNotFound = errors.New("search-query: outbox record not found")
)

// 搜索历史状态（search_history.state）。
// 用户主动删除走物理 DELETE（隐私要求），state 只用于运营标记待清理行。
const (
	HistoryStateNormal    = 0 // 正常
	HistoryStateFlagged   = 1 // 运营标记（仍可见，等待清理）
	HistoryStateTombstone = 2 // 待清理（列表不再返回）
)

// 查询日志结果状态（search_query_log.result_state，与 rpc.QueryResultState 对应）。
const (
	ResultStateOK       = "ok"       // 引擎返回且有命中
	ResultStateEmpty    = "empty"    // 引擎返回但零命中
	ResultStateDegraded = "degraded" // 引擎不可用/熔断/超时，未返回结果
	ResultStateBlocked  = "blocked"  // 命中屏蔽词，未查询引擎
)

// 屏蔽词状态（search_block_word.state）。
const (
	BlockWordStateActive   = 0 // 生效
	BlockWordStateInactive = 1 // 停用（保留审计，不再生效）
)

// Outbox 发布状态（search_outbox.state）。
const (
	OutboxStatePending   = 0 // 待发布
	OutboxStatePublished = 1 // 已发布
	OutboxStateFailed    = 2 // 超过最大重试，人工处理
)

// 事件契约：搜索行为只服务分析与推荐链路（AGENTS.md §7），不含任何广告语义。
const (
	// EventQueryReported 事件类型（topic = search.query.v1）。
	EventQueryReported = "search.query"
	// EventSchemaVersion 当前 payload schema 版本。
	EventSchemaVersion = 1
	// AggregateTypeQuery 聚合根类型。
	AggregateTypeQuery = "search_query"
	// ProducerName 事件生产者标识。
	ProducerName = "search-query"
)

// 文档类型（本服务对客户端暴露的检索对象类型；对引擎需翻译成索引的 content_type，
// 该翻译尚未实现 —— 索引侧只有 1 UGC/2 PGC/3 直播，没有 user 文档，见 README「索引契约」）。
const (
	DocTypeVideo = "video"
	DocTypeUser  = "user"
	DocTypePGC   = "pgc"
)

// 热词作用域（search_hot_keyword.scope）。
const (
	ScopeGlobal = "global"
)
