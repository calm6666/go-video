package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳（本服务所有时间列均为 BIGINT 秒，AGENTS.md §4 契约约定）。
func nowUnix() int64 { return time.Now().Unix() }
