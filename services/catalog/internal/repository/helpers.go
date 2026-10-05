package repository

import "encoding/json"

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
