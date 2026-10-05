package model

import "errors"

// 本服务的错误哨兵。logic 层据此向调用方返回可枚举的失败原因，
// 不得把 SQL 片段或敏感字段拼进错误信息（AGENTS.md §6：内部错误不外泄）。
var (
	// ErrRuleNotFound 规则不存在。
	ErrRuleNotFound = errors.New("risk-control: rule not found")
	// ErrRuleNameDuplicated 规则名唯一约束冲突。
	ErrRuleNameDuplicated = errors.New("risk-control: rule name duplicated")
	// ErrInvalidRule 规则字段非法（指标未实现、阈值或窗口不合法、裁决为 ALLOW）。
	ErrInvalidRule = errors.New("risk-control: invalid rule")
	// ErrPunishmentNotFound 处罚记录不存在。
	ErrPunishmentNotFound = errors.New("risk-control: punishment not found")
	// ErrPunishmentAlreadyFinished 处罚已是终态（已解除/已过期），幂等返回不报错覆盖。
	ErrPunishmentAlreadyFinished = errors.New("risk-control: punishment already finished")
	// ErrPunishmentAlreadyActive 同一 (mid, scope) 已存在生效中的处罚。
	// 重叠处罚会让「当前到底在罚什么」无法解释，必须先解除再下发。
	ErrPunishmentAlreadyActive = errors.New("risk-control: an active punishment already exists for the same mid and scope")
	// ErrAmbiguousPunishment 按 (mid, scope) 解除时命中多条生效处罚，需显式给 punishment_id。
	ErrAmbiguousPunishment = errors.New("risk-control: ambiguous punishment, punishment_id required")
	// ErrListEntryNotFound 名单条目不存在。
	ErrListEntryNotFound = errors.New("risk-control: list entry not found")
	// ErrInvalidListEntry 名单条目字段非法。
	ErrInvalidListEntry = errors.New("risk-control: invalid list entry")
	// ErrOperatorRequired 规则/处罚/名单变更必须由 admin operator 留痕（AGENTS.md §5 审计）。
	ErrOperatorRequired = errors.New("risk-control: operator required")
	// ErrIdempotencyKeyRequired 写接口缺少幂等键。
	ErrIdempotencyKeyRequired = errors.New("risk-control: idempotency key required")
	// ErrInvalidTarget 动作或目标类型不在枚举范围内。
	ErrInvalidTarget = errors.New("risk-control: invalid action or target")
	// ErrDeviceNotFound 设备画像不存在。
	ErrDeviceNotFound = errors.New("risk-control: device profile not found")
	// ErrEmptyDeviceID 设备标识为空，无法生成受控 ID。
	ErrEmptyDeviceID = errors.New("risk-control: device id required")
	// ErrRawIPForbidden ip_hash 疑似明文 IP，禁止入库（敏感信息不得明文落库）。
	ErrRawIPForbidden = errors.New("risk-control: raw ip is not accepted, use pre-hashed ip_hash")
)
