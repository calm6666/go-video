package esclient

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// _bulk 动作类型。
const (
	BulkActionIndex  = "index"  // 覆盖写（带显式 _id 时幂等）
	BulkActionCreate = "create" // 仅新建（重复会报 version_conflict）
	BulkActionUpdate = "update" // 部分字段合并
	BulkActionDelete = "delete" // 删除（幂等）
)

// BulkOp 一条批量写操作。
// ID 必填：只有携带显式主键的批量写才是幂等的，才允许在超时/5xx 时重试。
type BulkOp struct {
	Action string      // index/create/update/delete
	Index  string      // 目标物理索引；为空由 Bulk 的默认索引补齐
	ID     string      // 文档主键（ContentDoc.ID() 形式 <content_type>_<content_id>）
	Doc    interface{} // delete 时忽略；update 时应为 {"doc": {...}} 包裹体
}

// NewIndexOp 构造覆盖写操作。
func NewIndexOp(index, id string, doc *ContentDoc) BulkOp {
	return BulkOp{Action: BulkActionIndex, Index: index, ID: id, Doc: doc}
}

// NewHeatUpdateOp 构造热度部分更新操作（不重建整篇文档）。
func NewHeatUpdateOp(index, id string, heat Heat) BulkOp {
	return BulkOp{
		Action: BulkActionUpdate,
		Index:  index,
		ID:     id,
		Doc:    map[string]interface{}{"doc": map[string]interface{}{"heat": heat}},
	}
}

// NewStateUpdateOp 构造状态降级操作（下架/过期时保留投影但不命中检索）。
func NewStateUpdateOp(index, id string, state int32, revision int64) BulkOp {
	return BulkOp{
		Action: BulkActionUpdate,
		Index:  index,
		ID:     id,
		Doc:    map[string]interface{}{"doc": StatePatchBody(state, revision)},
	}
}

// NewDeleteOp 构造删除操作。
func NewDeleteOp(index, id string) BulkOp {
	return BulkOp{Action: BulkActionDelete, Index: index, ID: id}
}

// BuildBulkNDJSON 按 OpenSearch _bulk 协议序列化请求体。
//
// 格式要求（易错点，单测断言）：
//   - 每行一个 JSON 对象，动作行与数据行成对出现；
//   - delete 只有动作行；
//   - 整个请求体必须以换行符结尾，否则服务端返回 400。
func BuildBulkNDJSON(defaultIndex string, ops []BulkOp) ([]byte, error) {
	if len(ops) == 0 {
		return nil, fmt.Errorf("esclient: empty bulk ops")
	}
	var buf bytes.Buffer
	for i, op := range ops {
		if op.ID == "" {
			return nil, fmt.Errorf("esclient: bulk op %d missing _id", i)
		}
		idx := op.Index
		if idx == "" {
			idx = defaultIndex
		}
		if idx == "" {
			return nil, fmt.Errorf("esclient: bulk op %d missing index", i)
		}
		action := op.Action
		if action == "" {
			action = BulkActionIndex
		}
		switch action {
		case BulkActionIndex, BulkActionCreate, BulkActionUpdate, BulkActionDelete:
		default:
			return nil, fmt.Errorf("esclient: bulk op %d invalid action %q", i, action)
		}

		meta, err := json.Marshal(map[string]interface{}{
			action: map[string]string{"_index": idx, "_id": op.ID},
		})
		if err != nil {
			return nil, fmt.Errorf("esclient: marshal bulk meta %d: %w", i, err)
		}
		buf.Write(meta)
		buf.WriteByte('\n')

		if action == BulkActionDelete {
			continue
		}
		if op.Doc == nil {
			return nil, fmt.Errorf("esclient: bulk op %d action %s missing doc", i, action)
		}
		// encoding/json 默认不转义 HTML，避免标题里的 & < > 被改写成 \u0026 等。
		src, err := marshalNoHTMLEscape(op.Doc)
		if err != nil {
			return nil, fmt.Errorf("esclient: marshal bulk doc %d: %w", i, err)
		}
		buf.Write(src)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// marshalNoHTMLEscape 序列化 JSON 且保留原始字符（不转义 <、>、&）。
func marshalNoHTMLEscape(v interface{}) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encode 自带结尾换行，去掉后由调用方统一补 '\n'。
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// BulkItem 逐项写结果。
type BulkItem struct {
	Action string `json:"-"` // 动作类型
	Index  string `json:"-"` // 物理索引
	ID     string `json:"-"` // 文档主键
	Status int    `json:"-"` // HTTP 状态语义（200/201 成功，404 未命中，409 版本冲突）
	Error  string `json:"-"` // 失败原因摘要（脱敏，不含堆栈）
}

// Succeeded 判断逐项是否成功（404 视为幂等成功：删除不存在的文档）。
func (i BulkItem) Succeeded() bool {
	return i.Status == 200 || i.Status == 201 || i.Status == 404
}

// BulkResult _bulk 响应。
type BulkResult struct {
	Took   int64      `json:"took"`   // 服务端耗时（毫秒）
	Errors bool       `json:"errors"` // 是否有任何逐项失败
	Items  []BulkItem `json:"-"`      // 逐项结果
}

// Failed 返回失败的文档主键（供上层把对应事件转入重试/死信）。
func (r *BulkResult) Failed() []BulkItem {
	var bad []BulkItem
	if r == nil {
		return bad
	}
	for _, it := range r.Items {
		if !it.Succeeded() {
			bad = append(bad, it)
		}
	}
	return bad
}

// bulkResponse 是 _bulk 原始响应的中间结构，用于把 items 展平成 BulkItem。
type bulkResponse struct {
	Took   int  `json:"took"`
	Errors bool `json:"errors"`
	Items  []map[string]struct {
		ID     string `json:"_id"`
		Index  string `json:"_index"`
		Status int    `json:"status"`
		Error  struct {
			Type   string `json:"type"`
			Reason string `json:"reason"`
		} `json:"error"`
	} `json:"items"`
}

func parseBulkResponse(body []byte) (*BulkResult, error) {
	var resp bulkResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("esclient: unmarshal bulk response: %w", err)
	}
	out := &BulkResult{Took: int64(resp.Took), Errors: resp.Errors, Items: make([]BulkItem, 0, len(resp.Items))}
	for _, item := range resp.Items {
		for action, f := range item {
			reason := ""
			if f.Error.Type != "" {
				reason = truncate(f.Error.Type+": "+f.Error.Reason, 500)
			}
			out.Items = append(out.Items, BulkItem{
				Action: action,
				Index:  f.Index,
				ID:     f.ID,
				Status: f.Status,
				Error:  reason,
			})
		}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
