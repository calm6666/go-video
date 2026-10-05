package model

import "time"

// NowUnix 返回当前 Unix 秒时间戳。
func NowUnix() int64 {
	return time.Now().Unix()
}

// NowMilli 返回当前 Unix 毫秒时间戳（doc_revision 使用毫秒，保证同秒内可比较）。
func NowMilli() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}
