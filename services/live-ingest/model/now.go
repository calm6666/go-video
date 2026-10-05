package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳，供各 model 统一取时间。
// 本服务所有时间列都是 Unix 秒（BIGINT），不用 DATETIME，避免跨机房时区歧义。
func nowUnix() int64 { return time.Now().Unix() }
