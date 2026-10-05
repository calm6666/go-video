// Package model 是 audit 服务的数据库模型与查询代码。
//
// 自有表（库名 go_video_audit，见 deploy/migrations/audit）：
//   - audit_entry            审计条目，append-only
//   - audit_chain_head       哈希链链头（按 chain_key 分行）
//   - audit_export_task      导出任务
//   - audit_retention_policy 保留期策略
//   - audit_archive_batch    归档批次清单
//
// 跨服务不得直连本库（AGENTS.md §5）：写入只能通过 audit.v1.rpc 的 Append 语义，
// 读取只能通过带时间范围与维度约束的查询/导出任务。
package model
