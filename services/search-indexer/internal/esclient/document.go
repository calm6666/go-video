// Package esclient 是 OpenSearch/Elasticsearch 的薄 HTTP 客户端。
//
// 为什么手写：本仓库不允许新增第三方依赖（go.mod/go.sum 只读），
// 官方 opensearch-go / olivere/elastic 均未引入，因此这里只用标准库
// net/http + encoding/json 封装本服务真正需要的最小 API 面：
// _bulk（NDJSON 批量写）、_doc（单文档读写/删）、_update（部分字段合并）、
// _aliases（别名原子切换）、_refresh、_count、_reindex（切片重建）、indices 建索引。
//
// 设计约束：
//   - 所有请求带 context 超时（OpenSearch.TimeoutMs），不无限等待；
//   - 只对幂等操作重试（带显式 _id 的 PUT/DELETE/GET、含显式 _id 的 _bulk）；
//     _aliases、_reindex、_refresh 不自动重试，避免重复副作用；
//   - 错误一律向上返回，绝不吞掉后返回成功（AGENTS.md §9）；
//   - 凭据缺失时写操作直接失败（ErrWriteGuarded），不静默成功。
package esclient

import (
	"fmt"
	"strings"
)

// 文档标识与取值边界。
const (
	// DefaultSchemaVersion 当前索引 mapping 版本，变更字段结构时递增。
	DefaultSchemaVersion = "v1"
	// MinContentID content_id 合法下界。
	MinContentID = int64(1)
)

// Heat 热度快照字段（部分更新时使用，字段名与索引 mapping 一致）。
type Heat struct {
	ViewCount     int64 `json:"view_count"`     // 播放量快照
	LikeCount     int64 `json:"like_count"`     // 点赞数快照
	FavoriteCount int64 `json:"favorite_count"` // 收藏数快照
	ShareCount    int64 `json:"share_count"`    // 分享数快照
	CommentCount  int64 `json:"comment_count"`  // 评论数快照
	DanmakuCount  int64 `json:"danmaku_count"`  // 弹幕数快照
	HeatScore     int32 `json:"heat_score"`     // 归一化热度 [0,1000]
	HeatRevision  int64 `json:"heat_revision"`  // 热度版本（Unix 毫秒）
}

// ContentDoc 内容投影文档，json tag 即索引字段名。
// 注意：不包含手机号、身份证、IP、Token 等敏感字段（AGENTS.md §7）。
type ContentDoc struct {
	ContentID      int64    `json:"content_id"`       // 内容主键
	ContentType    int32    `json:"content_type"`     // 内容类型：1 UGC、2 PGC、3 直播
	Title          string   `json:"title"`            // 标题
	Description    string   `json:"description"`      // 简介
	CoverURL       string   `json:"cover_url"`        // 封面地址
	AuthorMid      int64    `json:"author_mid"`       // 作者 ID
	AuthorName     string   `json:"author_name"`      // 作者昵称快照
	Typeid         int32    `json:"typeid"`           // 分区 ID
	TypeName       string   `json:"type_name"`        // 分区名快照
	Tags           []string `json:"tags,omitempty"`   // 标签
	DurationSec    int64    `json:"duration_sec"`     // 时长（秒）
	PublishAt      int64    `json:"publish_at"`       // 发布时间（Unix 秒）
	Ctime          int64    `json:"ctime"`            // 创建时间（Unix 秒）
	State          int32    `json:"state"`            // 可见状态：见 ContentState*
	DocRevision    int64    `json:"doc_revision"`     // 事实版本（Unix 毫秒），防旧覆盖新
	Heat           Heat     `json:"heat"`             // 热度快照
	RightsExpireAt int64    `json:"rights_expire_at"` // 版权窗口结束（0 表示不适用）
	Language       string   `json:"language,omitempty"`
	SubtitleLangs  []string `json:"subtitle_langs,omitempty"`
	Sensitive      bool     `json:"sensitive"`                // 审核敏感标记
	SchemaVersion  int      `json:"schema_version,omitempty"` // 文档结构版本
}

// 内容可见状态常量（与 rpc.ContentState 对齐，避免 model 依赖 rpc 包）。
const (
	StatePending   = int32(1) // 待审：不参与公共检索
	StatePublished = int32(2) // 已发布：可检索
	StateOffline   = int32(3) // 下架
	StateExpired   = int32(4) // 版权过期
	StateDeleted   = int32(5) // 删除：应移除投影
)

// IsRetrievable 判断该状态是否应被查询命中。
func IsRetrievable(state int32) bool {
	return state == StatePublished
}

// DocID 返回文档主键：<content_type>_<content_id>。
// 不同内容类型的主键由各自服务独立发号，可能重号，因此必须加类型前缀。
func DocID(contentType int32, contentID int64) string {
	return fmt.Sprintf("%d_%d", contentType, contentID)
}

// ID 文档主键。
func (d *ContentDoc) ID() string {
	return DocID(d.ContentType, d.ContentID)
}

// Validate 校验写入索引所需的最小字段集。
// 错误消息不包含字段值全文，避免把标题等内容写进日志。
func (d *ContentDoc) Validate() error {
	if d == nil {
		return fmt.Errorf("esclient: nil doc")
	}
	if d.ContentID < MinContentID {
		return fmt.Errorf("esclient: invalid content_id %d", d.ContentID)
	}
	if d.ContentType < 1 || d.ContentType > 3 {
		return fmt.Errorf("esclient: invalid content_type %d", d.ContentType)
	}
	if d.DocRevision <= 0 {
		return fmt.Errorf("esclient: doc %s missing doc_revision", d.ID())
	}
	if d.State < 1 || d.State > 5 {
		return fmt.Errorf("esclient: invalid state %d for doc %s", d.State, d.ID())
	}
	if strings.TrimSpace(d.Title) == "" && d.State == StatePublished {
		return fmt.Errorf("esclient: empty title for published doc %s", d.ID())
	}
	return nil
}

// ShouldOverwrite 判断 incoming 是否允许覆盖 existing（last-write-wins）。
//
// 规则（README「迟到/乱序」章节）：
//   - existing == nil（文档不存在）→ 允许写入；
//   - incoming.DocRevision > existing.DocRevision → 允许；
//   - 相等 → 允许（同一版本的幂等重写，内容一致，ES PUT 天然幂等）；
//   - 更小 → 拒绝，旧事实永远不能覆盖新事实。
func ShouldOverwrite(existing, incoming *ContentDoc) bool {
	if existing == nil {
		return true
	}
	return incoming.DocRevision >= existing.DocRevision
}

// ShouldPatchHeat 判断热度快照是否比索引中的更新（等号放行，保证幂等重放）。
func ShouldPatchHeat(existingRevision, incomingRevision int64) bool {
	return incomingRevision >= existingRevision
}

// HeatPatch 热度部分更新体：只包含 heat.* 字段，不重建整篇文档
// （engagement.action.v1 只影响热度，重建整篇会放大上游读压力）。
type HeatPatch struct {
	ContentID   int64 `json:"-"` // 仅用于定位文档，不写入 _source
	ContentType int32 `json:"-"`
	Heat        Heat  `json:"heat"`
}

// ID 文档主键。
func (p *HeatPatch) ID() string {
	return DocID(p.ContentType, p.ContentID)
}

// PartialBody 生成 _update API 的 {"doc": {...}} 请求体。
func (p *HeatPatch) PartialBody() map[string]interface{} {
	return map[string]interface{}{"heat": p.Heat}
}

// StatePatchBody 生成只改可见状态的部分更新体（下架/过期时保留投影但不命中检索）。
func StatePatchBody(state int32, revision int64) map[string]interface{} {
	return map[string]interface{}{
		"state":        state,
		"doc_revision": revision,
	}
}
