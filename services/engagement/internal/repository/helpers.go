package repository

import (
	"encoding/json"
	"time"
)

// nowUnix 是写侧的统一取时点：heat_revision 由它派生（秒 × 1000）。
// 信封的 occurred_at 由 common/eventenvelope 自己盖章，outbox 列再从信封解析回来，
// 所以「列的 occurred_at」与「信封的 occurred_at」永远同源，可对账。
func nowUnix() int64 { return time.Now().Unix() }

// jsonUnmarshal 包装 json.Unmarshal，便于统一错误处理。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// jsonMustMarshal 包装 json.Marshal，错误时返回空切片。
// 用于缓存写入场景：序列化失败不应阻塞业务流程。
func jsonMustMarshal(v any) string {
	bs, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(bs)
}
