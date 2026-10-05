package model

import "time"

// NowUnix 返回当前 Unix 秒时间戳。
func NowUnix() int64 {
	return time.Now().Unix()
}
