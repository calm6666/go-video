package repository

import (
	"encoding/base64"
	"strconv"
	"strings"

	"go-video/services/inbox/model"
)

// Cursor 是收件箱列表的不透明游标载体。
// 采用 (ctime, id) 双列：同一秒内投递的多条消息也能稳定翻页，不重不漏。
type Cursor struct {
	Time int64 // inbox_user_message.ctime
	ID   int64 // inbox_user_message.id
}

// EncodeCursor 生成 URL 安全的游标字符串；零值游标返回空串（表示第一页）。
func EncodeCursor(c Cursor) string {
	if c.Time == 0 && c.ID == 0 {
		return ""
	}
	raw := strconv.FormatInt(c.Time, 10) + ":" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor 解析游标；空串表示第一页。格式非法返回 model.ErrInvalidCursor。
func DecodeCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, model.ErrInvalidCursor
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) != 2 {
		return Cursor{}, model.ErrInvalidCursor
	}
	t, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || t < 0 {
		return Cursor{}, model.ErrInvalidCursor
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 0 {
		return Cursor{}, model.ErrInvalidCursor
	}
	return Cursor{Time: t, ID: id}, nil
}
