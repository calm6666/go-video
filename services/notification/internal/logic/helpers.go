package logic

import (
	"errors"
	"strings"

	"go-video/services/notification/model"
	"go-video/services/notification/rpc"
)

// logic 层共用的显式错误与助手。
// 约定（AGENTS.md §9）：参数不合法一律返回可判定的哨兵错误，
// 不允许“返回空结果 + nil 错误”这种看起来成功的写法。
var (
	// ErrOperatorRequired 模板与死信类操作必须留痕，操作人不能为空。
	ErrOperatorRequired = errors.New("notification/logic: operator is required")
	// ErrTemplateLanguageRequired 预览渲染必须显式指定语言，避免落到不确定的回落版本。
	ErrTemplateLanguageRequired = errors.New("notification/logic: language is required for preview")
	// ErrQuietTimePair quiet_start / quiet_end 必须成对出现（同空或同非空）。
	ErrQuietTimePair = errors.New("notification/logic: quiet_start and quiet_end must be provided together")
	// ErrDeadLetterHandled 死信已被处置，重复重投被拒绝。
	ErrDeadLetterHandled = errors.New("notification/logic: dead letter already handled")
	// ErrEventPayloadMissing 事件死信缺少可重放的原信封（payload_json 为空）。
	ErrEventPayloadMissing = errors.New("notification/logic: dead letter event payload is missing")
)

// normalizePage 统一分页参数：页码从 1 开始，每页上限 100（与 model 层一致）。
func normalizePage(pn, ps int32) (int32, int32) {
	if pn < 1 {
		pn = 1
	}
	if ps < 1 {
		ps = 20
	}
	if ps > 100 {
		ps = 100
	}
	return pn, ps
}

// channelsToInt32 把 rpc 通道枚举转成落库编码，遇到不支持的通道（含小程序通道）显式拒绝。
func channelsToInt32(in []rpc.Channel) ([]int32, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make([]int32, 0, len(in))
	for _, c := range in {
		code := int32(c)
		if !model.IsValidChannel(code) {
			return nil, model.ErrInvalidChannel
		}
		out = append(out, code)
	}
	return out, nil
}

// requireTemplateCode 校验模板码。
func requireTemplateCode(code string) (string, error) {
	v := strings.TrimSpace(code)
	if v == "" {
		return "", errors.New("notification/logic: template_code is required")
	}
	return v, nil
}
