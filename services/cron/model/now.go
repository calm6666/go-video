package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳。
// cron 的所有时间列（planned_at / lease_expire_at / next_retry_at / ctime）统一使用
// Unix 秒，与 RPC 契约一致；cron_task_run.duration_ms 是唯一的毫秒字段，
// 由 worker 用 time.Since 测量后回传，不在模型层取时钟。
func nowUnix() int64 { return time.Now().Unix() }
