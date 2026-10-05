package repository

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"go-video/services/search-query/model"
)

// 游标编解码。
//
// 契约依据 docs/api-and-events.md §2：分页 cursor 优先。两类游标：
//  1. offset 游标（Search）：引擎分页本质是 from/size，游标携带 offset + 查询指纹，
//     指纹不匹配说明翻页过程中改了筛选条件，直接报错而不是串页；
//  2. keyset 游标（搜索历史）：携带 (mtime, id) 做真游标，避免深分页。
//
// 编码格式：base64url(无填充) 的紧凑 JSON，版本号 v 必填，便于后续不兼容升级。
// 该格式只对客户端不透明，网关与客户端都不应解析它。

// cursorVersion 当前游标版本。
const cursorVersion = 1

// ErrUnsupportedCursorVersion 游标版本不受支持（滚动发布期间的旧/新版本）。
var ErrUnsupportedCursorVersion = errors.New("search-query: unsupported cursor version")

type offsetCursorPayload struct {
	V    int    `json:"v"`
	Off  int64  `json:"o"`
	Fing string `json:"f"`
}

type keysetCursorPayload struct {
	V int   `json:"v"`
	T int64 `json:"t"`
	I int64 `json:"i"`
}

// encodeCursorJSON 序列化游标（内部统一入口，避免调用方拼错编码）。
func encodeCursorJSON(v any) (string, error) {
	bs, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("search-query/cursor: marshal: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bs), nil
}

// decodeCursorJSON 反序列化游标；空串视为首页。
func decodeCursorJSON(s string, dst any) error {
	if s == "" {
		return nil
	}
	bs, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("%w: %v", model.ErrInvalidCursor, err)
	}
	if err := json.Unmarshal(bs, dst); err != nil {
		return fmt.Errorf("%w: %v", model.ErrInvalidCursor, err)
	}
	return nil
}

// EncodeOffsetCursor 生成下一页游标；offset 为下一页起始位置。
func EncodeOffsetCursor(offset int64, fingerprint string) (string, error) {
	return encodeCursorJSON(offsetCursorPayload{V: cursorVersion, Off: offset, Fing: fingerprint})
}

// DecodeOffsetCursor 解析 offset 游标并校验查询指纹一致。
// 返回 offset（游标为空表示首页，offset=0）；格式错误 -> ErrInvalidCursor，
// 条件变化 -> ErrCursorMismatch，版本不兼容 -> ErrUnsupportedCursorVersion。
func DecodeOffsetCursor(s, fingerprint string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	var p offsetCursorPayload
	if err := decodeCursorJSON(s, &p); err != nil {
		return 0, err
	}
	if p.V != cursorVersion {
		return 0, ErrUnsupportedCursorVersion
	}
	if p.Off < 0 {
		return 0, fmt.Errorf("%w: negative offset %d", model.ErrInvalidCursor, p.Off)
	}
	if p.Fing != fingerprint {
		return 0, model.ErrCursorMismatch
	}
	return p.Off, nil
}

// EncodeKeysetCursor 生成历史列表游标。
func EncodeKeysetCursor(mtime, id int64) (string, error) {
	return encodeCursorJSON(keysetCursorPayload{V: cursorVersion, T: mtime, I: id})
}

// DecodeKeysetCursor 解析历史列表游标；空串返回 (0, 0, nil) 表示首页。
func DecodeKeysetCursor(s string) (mtime, id int64, err error) {
	if s == "" {
		return 0, 0, nil
	}
	var p keysetCursorPayload
	if err := decodeCursorJSON(s, &p); err != nil {
		return 0, 0, err
	}
	if p.V != cursorVersion {
		return 0, 0, ErrUnsupportedCursorVersion
	}
	return p.T, p.I, nil
}

// PageToOffset 把 pn/ps（从 1 开始）折算为引擎 offset。
func PageToOffset(pn, ps int32) (int64, error) {
	if pn < 0 || ps <= 0 {
		return 0, fmt.Errorf("%w: pn=%d ps=%d", model.ErrInvalidPage, pn, ps)
	}
	if pn == 0 {
		pn = 1
	}
	return int64(pn-1) * int64(ps), nil
}
