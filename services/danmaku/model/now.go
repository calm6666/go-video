package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳，供各 model 统一取时间。
func nowUnix() int64 { return time.Now().Unix() }
