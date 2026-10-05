// Package model 是 open-platform 服务的数据库访问层，只操作 go_video_open_platform 库自身的表
// （AGENTS.md §5：服务只能写自己的 schema；用户主数据归 account/user-profile）。
//
// 表清单与 deploy/migrations/open-platform/*.sql 严格一致：
//
//	op_app               应用主体（状态机 + 乐观锁版本）
//	op_app_secret        client_secret 哈希（salt + HMAC-SHA256，明文只签发一次）
//	op_scope             权限点目录（逐个 scope 声明读/写与风险级别，由迁移 seed 维护）
//	op_app_scope         应用与 scope 的审批关系
//	op_auth_code         OAuth 授权码（只存哈希，短期 + 一次性消费）
//	op_grant             用户对应用的授权关系，兼作“撤销位点”
//	op_token             access/refresh token 哈希与轮换链
//	op_quota_policy      配额规则（应用 × 接口 × 时间窗，含全局默认）
//	op_quota_usage       配额用量投影（可由 op_api_call_log 重算）
//	op_api_call_log      接口调用流水（配额与审计的事实来源）
//	op_webhook_endpoint  回调端点（签名密钥版本由服务端派生，不落任何密钥材料）
//	op_webhook_delivery  回调投递任务（重试退避 + 死信）
//
// 商业化红线（AGENTS.md §1）：本包不存在会员/订单/支付/投币/分成/广告相关的表或列，
// 新能力必须先扩 scope 目录并过范围评审，再落地 model。
package model

import "time"

// nowUnix 返回当前 Unix 秒时间戳。
// 由 model 层统一取时钟，避免各表写入时间不一致；契约层时间一律 Unix 秒。
func nowUnix() int64 { return time.Now().Unix() }
