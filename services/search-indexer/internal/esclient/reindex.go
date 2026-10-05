package esclient

import (
	"context"
	"fmt"
	"net/http"
)

// ReindexSliceReq 一次切片重建请求。
//
// 重建不读任何上游数据库（AGENTS.md §5），而是在 OpenSearch 内部将
// 「当前 active 索引」的既有投影按 content_id 区间搬到新索引：
//   - From/To 构成 [From, To) 的半开区间，To 由上层按 slice span 推进；
//   - Filter 追加 scope 过滤（如 {"term": {"content_type": 2}}）；
//   - Size 控制服务端每批 scroll 文档数，避免一次拉全量打爆堆内存。
//
// 目标文档沿用源 _id，因此同一区间重复执行是幂等覆盖，进程重启后可从
// cursor_value 断点续跑。
type ReindexSliceReq struct {
	Source string
	Dest   string
	From   int64
	To     int64
	Size   int
	Filter map[string]interface{}
}

// ReindexResult _reindex 响应。
type ReindexResult struct {
	Took             int64            `json:"took"`
	TimedOut         bool             `json:"timed_out"`
	Total            int64            `json:"total"`
	Updated          int64            `json:"updated"`
	Created          int64            `json:"created"`
	Deleted          int64            `json:"deleted"`
	VersionConflicts int64            `json:"version_conflicts"`
	Noops            int64            `json:"noops"`
	Failures         []ReindexFailure `json:"failures"`
}

// ReindexFailure 单条失败明细。
type ReindexFailure struct {
	Index  string `json:"_index"`
	ID     string `json:"_id"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

// ErrReindexFailed 表示 _reindex 返回了 failures 或超时，任务必须转入 failed，
// 不能当作成功（否则新索引会静默缺文档）。
var ErrReindexFailed = fmt.Errorf("esclient: reindex reported failures")

// BuildReindexBody 生成 _reindex 请求体（导出以便单测断言区间与过滤条件）。
func BuildReindexBody(req ReindexSliceReq) map[string]interface{} {
	must := []interface{}{
		map[string]interface{}{
			"range": map[string]interface{}{
				"content_id": map[string]interface{}{"gte": req.From, "lt": req.To},
			},
		},
	}
	if len(req.Filter) > 0 {
		must = append(must, req.Filter)
	}
	source := map[string]interface{}{
		"index": req.Source,
		"query": map[string]interface{}{"bool": map[string]interface{}{"must": must}},
	}
	if req.Size > 0 {
		source["size"] = req.Size
	}
	return map[string]interface{}{
		"source": source,
		"dest":   map[string]interface{}{"index": req.Dest},
		// 冲突多为「同一文档被并发事件更新」，proceed 后由 revision 守卫保证终态；
		// 计数仍然返回给上层，超过阈值时任务判失败。
		"conflicts": "proceed",
	}
}

// ReindexSlice 执行一次切片重建。不自动重试：重试语义由任务的 cursor 决定。
func (c *httpClient) ReindexSlice(ctx context.Context, req ReindexSliceReq) (*ReindexResult, error) {
	if err := c.guardWrite(); err != nil {
		return nil, err
	}
	if req.Source == "" || req.Dest == "" {
		return nil, fmt.Errorf("esclient: reindex requires source and dest")
	}
	if req.To <= req.From {
		return nil, fmt.Errorf("esclient: reindex slice %d-%d is empty", req.From, req.To)
	}
	body, err := marshalNoHTMLEscape(BuildReindexBody(req))
	if err != nil {
		return nil, fmt.Errorf("esclient: marshal reindex body: %w", err)
	}

	var out ReindexResult
	if err := c.request(ctx, http.MethodPost, "/_reindex", nil, body, "application/json", false, &out); err != nil {
		return nil, err
	}
	if out.TimedOut || len(out.Failures) > 0 {
		return &out, fmt.Errorf("%w: slice %d-%d failures=%d timed_out=%v",
			ErrReindexFailed, req.From, req.To, len(out.Failures), out.TimedOut)
	}
	return &out, nil
}
