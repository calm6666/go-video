package model

import (
	"strings"
	"time"
)

// nowUnix 返回当前 Unix 秒时间戳（model 包内使用）。
func nowUnix() int64 {
	return time.Now().Unix()
}

// NowUnix 返回当前 Unix 秒时间戳，供 repository/logic 跨包使用。
func NowUnix() int64 {
	return time.Now().Unix()
}

// placeholders 生成 n 个 "?, " 连接符，用于 IN (...) 批量查询。
// 调用方需保证 values 非空，并把元素按顺序作为参数传入。
func placeholders(n int) string {
	if n <= 0 {
		return "?"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// DateKey 把 Unix 秒转成 UTC 自然日键（YYYY-MM-DD）。
// 用 UTC 而非本地时区：哈希链分链边界必须与部署机时区无关，
// 否则同一份数据在不同时区实例上会落进不同链，校验结果不可复现。
func DateKey(unixSec int64) string {
	return time.Unix(unixSec, 0).UTC().Format("2006-01-02")
}
