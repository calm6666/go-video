package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳。
func nowUnix() int64 {
	return time.Now().Unix()
}
