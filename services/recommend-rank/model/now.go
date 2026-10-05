package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳（本服务所有时间列均为 BIGINT 秒，AGENTS.md §4 契约约定）。
func nowUnix() int64 { return time.Now().Unix() }

// NowUnix 是仓库层与 logic 层使用的统一时钟入口：
// 决策摘要的 ctime、分桶的 assigned_at、状态切换的 mtime 必须同源，
// 否则「同一次排序」在两张表里的时间线会对不上，审计就失去意义。
func NowUnix() int64 { return nowUnix() }
