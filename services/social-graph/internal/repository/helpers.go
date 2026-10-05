package repository

import "strconv"

// parseInt64 解析 Redis 字符串为 int64。
func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}
