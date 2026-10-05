// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）：运营侧入参的统一校验助手。
//
// 网关只做「有没有主体、有没有幂等键」这类门槛校验，不复述领域规则：
// 屏蔽词长度、处罚裁决取值、规则指标等一律交给拥有数据的服务判定（AGENTS.md §4/§5）。

package logic

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// requireOperator 校验审计主体存在。
// field 传下游契约里的字段名（risk-control/search 用 operator_id，
// danmaku 用 operator_mid），保证错误消息与请求体字段一致，便于后台表单定位。
func requireOperator(field string, id int64) error {
	if id <= 0 {
		return fmt.Errorf("gateway/admin: %s required", field)
	}
	return nil
}

// requireOperatorID 是 operator_id 字段的简写，错误消息固定为
// "gateway/admin: operator_id required"。
func requireOperatorID(id int64) error {
	if id <= 0 {
		return errors.New("gateway/admin: operator_id required")
	}
	return nil
}

// requireNonEmpty 校验必填字符串（幂等键、任务 ID、提交人等）。
// 只去空格判空，不改写原值：幂等键的任何改动都会让它失去幂等语义。
func requireNonEmpty(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("gateway/admin: %s required", field)
	}
	return nil
}

// requireNoWhitespace 拒绝含任何空白（含首尾）的引用单号：payment_no/biz_order_no 这类
// 跨服务引用是**精确匹配**的键，带空格落库就是一条永远对不上账的台账，而悄悄 trim
// 会改变运营实际给出的引用。长度上限与「这个单号存不存在」仍归拥有该数据的服务判定。
func requireNoWhitespace(field, value string) error {
	if strings.ContainsFunc(value, unicode.IsSpace) {
		return fmt.Errorf("gateway/admin: %s must not contain whitespace", field)
	}
	return nil
}
