package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳。
// 由 model 层统一取时钟，避免各表写入时间不一致；契约层时间一律 Unix 秒。
func nowUnix() int64 { return time.Now().Unix() }
