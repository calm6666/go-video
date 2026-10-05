// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 search-query 服务的配置结构。
//
// 只读依赖：OpenSearch 查询别名由 search-indexer 写入/切换，本服务不创建索引；
// DataSource 指向本服务自有库 go_video_search_query（AGENTS.md §5）。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 业务缓存连接：结果短缓存、热词缓存、联想词典（ZSET）、计数器。
	// 不能命名为 Redis —— zrpc.RpcServerConf 内嵌同名字段（鉴权拦截用），
	// 同名会让配置加载直接失败（与仓库其它服务一致的写法）。
	CacheRedis redis.RedisConf

	// MySQL 主库连接 DSN（库：go_video_search_query）。
	DataSource string

	// OpenSearch 查询端配置。
	OpenSearch OpenSearchConf

	// Search 查询侧限制与降级策略。
	Search SearchConf
}

// OpenSearchConf OpenSearch 访问配置。
// Endpoints 或 Alias 为空时视为“未配置引擎”：Search/Suggest 回源一律返回
// 明确的降级错误（model.ErrSearchUnavailable），不会返回伪造的空成功结果。
type OpenSearchConf struct {
	// Endpoints 引擎地址列表，例：http://127.0.0.1:9200。
	// 多个地址时客户端按轮询选择（只读查询，无写放大）。
	Endpoints []string `json:",optional"`

	// Alias 查询别名，与 search-indexer 的读写别名切换约定一致
	// （例 go_video_search_read 指向当前生效的索引版本）。
	Alias string `json:",optional"`

	// Username 引擎基础认证用户名（未开启安全插件时留空）。
	Username string `json:",optional"`

	// Password 引擎基础认证密码。示例配置必须留空，生产从 Secret/Vault 注入。
	Password string `json:",optional"`

	// TimeoutMs 单次引擎请求超时（毫秒）。同时受 gRPC 调用 deadline 约束。
	TimeoutMs int64 `json:",default=1500"`

	// MaxResultWindow 引擎 index.max_result_window，from+size 不得超过该值。
	// 与 Search.MaxOffset 取较小者生效，超过即返回 ErrDeepPage。
	MaxResultWindow int64 `json:",default=10000"`
}

// SearchConf 查询侧限制、缓存与降级策略。
type SearchConf struct {
	// PsDefault 未传 ps 时的每页大小。
	PsDefault int32 `json:",default=30"`

	// PsLimit 每页大小上限（超过直接截断，不报错，便于老客户端兼容）。
	PsLimit int32 `json:",default=50"`

	// MaxOffset 允许的最大起始 offset（深分页保护，返回 ErrDeepPage）。
	MaxOffset int32 `json:",default=900"`

	// KeywordMaxLen 关键词最大字符数（按 rune 计）。
	KeywordMaxLen int `json:",default=64"`

	// CacheTTLSeconds 搜索结果短缓存秒数，0 表示关闭结果缓存。
	CacheTTLSeconds int `json:",default=30"`

	// DegradeEnabled 引擎已配置但暂时不可用（熔断打开、查询失败）时，是否允许用仍在
	// TTL 内的结果缓存兜底（返回结果会带 Degraded 标记，网关据此不写客户端缓存）。
	// false 表示任何引擎故障都直接返回 ErrSearchUnavailable。
	// 注意：引擎“未配置”（Endpoints/Alias 缺失）时本开关不参与判定，一律报错，
	// 不用历史缓存掩盖配置错误（见 repository.Search 的判定顺序）。
	DegradeEnabled bool `json:",default=true"`

	// SuggestLimit 联想返回条数默认值。
	SuggestLimit int32 `json:",default=10"`

	// SuggestCandidateWindow 联想在 Redis ZSET 中取回的候选窗口大小，
	// 前缀过滤在该窗口内完成（详见 README 的选型说明）。
	SuggestCandidateWindow int `json:",default=500"`

	// HotKeywordLimit 热词返回条数上限。
	HotKeywordLimit int32 `json:",default=20"`

	// HistoryLimit 搜索历史单页条数上限。
	HistoryLimit int32 `json:",default=30"`

	// HistoryEnabled 是否在 Search 成功后为登录用户写入搜索历史。
	HistoryEnabled bool `json:",default=true"`

	// BlockWordCacheTTLSeconds 屏蔽词判定结果缓存秒数。
	BlockWordCacheTTLSeconds int `json:",default=60"`

	// HighlightPreTag 高亮前标签，默认 <em>。仅作为文本标记返回，
	// 具体渲染由客户端决定（服务端不写死 UI 行为）。
	HighlightPreTag string `json:",optional"`

	// HighlightPostTag 高亮后标签，默认 </em>。
	HighlightPostTag string `json:",optional"`

	// DefaultSort 各端默认排序（rpc.SortMode 数值），GetSearchConfig 返回。
	DefaultSort int32 `json:",default=1"`
}

// HighlightTags 返回高亮标签，未配置时使用 <em>/</em>。
func (s SearchConf) HighlightTags() (string, string) {
	pre, post := s.HighlightPreTag, s.HighlightPostTag
	if pre == "" {
		pre = "<em>"
	}
	if post == "" {
		post = "</em>"
	}
	return pre, post
}
