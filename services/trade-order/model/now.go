package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳，供各 model 统一取时间。
//
// 订单的所有时间列（created_at/updated_at/paid_at/fulfilled_at/closed_at/expire_at）
// 都用 Unix 秒 BIGINT 存储：跨服务传递的是绝对时间点，不做时区换算，
// 展示层（gateway/运营页）负责本地化。
func nowUnix() int64 { return time.Now().Unix() }

// NowUnix 暴露服务端统一时钟，供 logic 计算履约重试间隔与关单时间，
// 避免 logic 自己 import time 造成两处时钟口径不同。
func NowUnix() int64 { return nowUnix() }
