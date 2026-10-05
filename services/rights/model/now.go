package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳（model 包内使用）。
func nowUnix() int64 {
	return time.Now().Unix()
}

// NowUnix 返回当前 Unix 秒时间戳，供 repository/logic 跨包使用。
func NowUnix() int64 {
	return time.Now().Unix()
}
