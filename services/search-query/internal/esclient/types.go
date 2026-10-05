package esclient

import (
	"encoding/json"
	"fmt"
)

// SearchResponse _search 响应（只解析查询侧需要的字段）。
type SearchResponse struct {
	Took     int64      `json:"took"` // 引擎耗时（毫秒）
	TimedOut bool       `json:"timed_out"`
	Shards   ShardStats `json:"_shards"`
	Hits     HitsBlock  `json:"hits"`
}

// ShardStats _shards 统计；failed>0 表示结果不可信，客户端按异常处理。
type ShardStats struct {
	Total      int `json:"total"`
	Successful int `json:"successful"`
	Failed     int `json:"failed"`
}

// HitsBlock hits 区块。
type HitsBlock struct {
	Total TotalBlock `json:"total"`
	Hits  []Hit      `json:"hits"`
}

// TotalBlock 命中总数。
// OpenSearch/ES 6.x 返回数字，7.x+ 返回 {"value":n,"relation":"eq|gte"}；
// 两种格式都要能解析，否则版本升级会直接导致查询失败。
type TotalBlock struct {
	Value    int64  `json:"value"`
	Relation string `json:"relation"` // eq 精确、gte 达到 track_total_hits 上限
}

// UnmarshalJSON 兼容 number 与 object 两种 total 表示。
func (t *TotalBlock) UnmarshalJSON(b []byte) error {
	trimmed := string(b)
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	// 数字形式（ES 6.x）。
	var n int64
	if err := json.Unmarshal(b, &n); err == nil {
		t.Value = n
		t.Relation = "eq"
		return nil
	}
	type alias TotalBlock
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return fmt.Errorf("esclient: parse hits.total: %w", err)
	}
	*t = TotalBlock(a)
	if t.Relation == "" {
		t.Relation = "eq"
	}
	return nil
}

// Hit 单条命中。
type Hit struct {
	Index      string              `json:"_index"`  // 命中的物理索引名（别名切换排查用）
	ID         json.RawMessage     `json:"_id"`     // 引擎文档 ID（字符串或数字都可能出现）
	Score      float64             `json:"_score"`  // 引擎打分
	Source     SourceDoc           `json:"_source"` // 文档投影（schema 与 search-indexer 约定，见 README）
	Highlights map[string][]string `json:"highlight"`
}

// SourceDoc 索引文档投影字段。
//
// ⚠ 命名与 search-indexer 的当前 mapping **不一致**（本服务从 openbilibili 移植时沿用了
// 旧的 video/user/pgc 索引词汇），真实往返前必须先做契约对齐，见服务 README「索引契约」
// 与 docs/roadmap.md 搜索待办；缺失字段解码为零值。
// 本服务不解释 doc_id 的业务含义（aid / mid / 作品 ID 由 doc_type 决定），
// 也不补全主资料：昵称、头像等只是索引快照，展示时以 user-profile 为准。
type SourceDoc struct {
	DocType      string   `json:"doc_type"`      // video / user / pgc
	DocID        int64    `json:"doc_id"`        // aid / mid / 作品 ID
	Title        string   `json:"title"`         // 标题（video 稿件标题、user 昵称、pgc 作品名）
	Intro        string   `json:"intro"`         // 简介/签名摘要
	AuthorMid    int64    `json:"author_mid"`    // UP 主 mid（user 文档为自身 mid）
	AuthorName   string   `json:"author_name"`   // UP 主昵称快照
	ZoneID       int32    `json:"zone_id"`       // 分区 ID（pgc 为 catalog 分区投影）
	ZoneName     string   `json:"zone_name"`     // 分区名快照
	CoverURL     string   `json:"cover_url"`     // 封面引用（非播放签名）
	ViewCount    int64    `json:"view_count"`    // 播放数快照
	LikeCount    int64    `json:"like_count"`    // 点赞数快照（事实源 engagement）
	DanmakuCount int64    `json:"danmaku_count"` // 弹幕数快照（事实源 danmaku）
	FansCount    int64    `json:"fans_count"`    // 粉丝数快照（事实源 social-graph）
	DurationSec  int32    `json:"duration_sec"`  // 时长（video/pgc）
	PubTime      int64    `json:"pub_time"`      // 发布/上线时间（Unix 秒）
	HotScore     float64  `json:"hot_score"`     // 热度分（SPM/离线写入的排序因子）
	State        string   `json:"state"`         // 文档状态快照（published/offline/expired）
	Tags         []string `json:"tags"`          // 标签（供上层展示，不参与本服务过滤）
}
