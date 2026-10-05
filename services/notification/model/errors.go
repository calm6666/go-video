package model

import "errors"

// notification 数据层错误。
// 逻辑层依赖这些哨兵错误判定幂等与状态机结果，禁止用字符串匹配替换。
var (
	// ErrNotFound 记录不存在（统一由 model 层把 sql.ErrNoRows 转成 nil 或本错误）。
	ErrNotFound = errors.New("notification/model: record not found")
	// ErrDuplicateBizKey biz_key 唯一索引冲突，表示同一业务键已投递过（幂等命中）。
	ErrDuplicateBizKey = errors.New("notification/model: duplicate biz_key")
	// ErrDuplicateEventID event_id 唯一索引冲突，表示事件已登记。
	ErrDuplicateEventID = errors.New("notification/model: duplicate event_id")
	// ErrIllegalStateTransition 状态机非法迁移（更新未命中任何合法源状态）。
	ErrIllegalStateTransition = errors.New("notification/model: illegal state transition")
	// ErrTemplateNotFound 指定 template_code/channel/lang/version 无记录。
	ErrTemplateNotFound = errors.New("notification/model: template not found")
	// ErrInvalidChannel channel 取值不在允许集合内（不含小程序通道）。
	ErrInvalidChannel = errors.New("notification/model: invalid channel")
	// ErrInvalidLang lang 取值不合法。
	ErrInvalidLang = errors.New("notification/model: invalid lang")
	// ErrPsTooLarge 分页大小超限。
	ErrPsTooLarge = errors.New("notification/model: ps exceeds 100")
)
