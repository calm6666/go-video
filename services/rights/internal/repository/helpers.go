package repository

import (
	"strconv"
	"strings"
)

// parseCheckCache 解析 CheckPlayable 缓存值。
// 缓存格式："playable:window_id:end_time"，例如 "1:42:1700000000" 或 "0:0:0"。
// 返回 (playable, windowID, endTime, ok, err)：ok=false 表示缓存 miss 或格式不合法。
func parseCheckCache(raw string) (playable bool, windowID, endTime int64, ok bool, err error) {
	if raw == "" {
		return false, 0, 0, false, nil
	}
	parts := strings.SplitN(raw, ":", 3)
	if len(parts) != 3 {
		return false, 0, 0, false, nil
	}
	playableByte := parts[0]
	wid, err1 := strconv.ParseInt(parts[1], 10, 64)
	et, err2 := strconv.ParseInt(parts[2], 10, 64)
	if err1 != nil || err2 != nil {
		return false, 0, 0, false, nil
	}
	return playableByte == "1", wid, et, true, nil
}

// formatCheckCache 构造 CheckPlayable 缓存值。
func formatCheckCache(playable bool, windowID, endTime int64) string {
	v := "0"
	if playable {
		v = "1"
	}
	return v + ":" + strconv.FormatInt(windowID, 10) + ":" + strconv.FormatInt(endTime, 10)
}
