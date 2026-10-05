package model

import (
	"strconv"
	"strings"
	"time"
)

// nowUnix 返回当前 Unix 秒时间戳。
// 契约层时间一律 Unix 秒（与 rpc 里的 occurred_at/received_at/ctime 一致），
// 不在 model 里保存时区，也不使用 DATETIME 列以免跨机房漂移。
func nowUnix() int64 { return time.Now().Unix() }

// Cursor 台账分页游标：(ctime, id) 倒序，二者都能唯一定位一行。
type Cursor struct {
	Ctime int64
	ID    int64
}

// EncodeCursor 生成游标字符串；零值返回空串表示「从最新开始」。
func EncodeCursor(ctime, id int64) string {
	if ctime <= 0 && id <= 0 {
		return ""
	}
	return strconv.FormatInt(ctime, 10) + "_" + strconv.FormatInt(id, 10)
}

// ParseCursor 解析游标；空串返回 (零值, nil) 表示第一页。
// 格式非法返回 ErrInvalidCursor，禁止把它当成「从头开始」以免运营翻页时拿到重复页。
func ParseCursor(s string) (Cursor, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Cursor{}, nil
	}
	parts := strings.SplitN(s, "_", 2)
	if len(parts) != 2 {
		return Cursor{}, ErrInvalidCursor
	}
	ctime, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || ctime < 0 {
		return Cursor{}, ErrInvalidCursor
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id < 0 {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{Ctime: ctime, ID: id}, nil
}

// ClampPageSize 把请求的 page_size 收敛到 [1, max]，0 表示用默认值。
// 返回 0 表示入参非法（负数或超过服务端上限），由 logic 层转成 ErrInvalidPage，
// 不允许「悄悄截断」成上限（那会让调用方误以为拿到了整页）。
func ClampPageSize(ps, def, max int32) int32 {
	if max <= 0 || def <= 0 || def > max {
		return 0
	}
	if ps == 0 {
		return def
	}
	if ps < 0 || ps > max {
		return 0
	}
	return ps
}
