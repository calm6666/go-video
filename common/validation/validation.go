// Package validation 提供可复用的输入校验工具，
// 覆盖分页、字符串长度、字节长度、枚举成员、切片/map 大小等场景。
// 实现 AGENTS.md §6 中关于外部输入必须限制 size、format、分页、timeout 的要求。
//
// 所有函数均返回普通 error，错误信息中包含出错字段名和实际值，
// 调用方可用 common/ecode 包装后通过统一响应信封返回，
// 无需为每个字段单独分配错误码。
package validation

import (
	"fmt"
	"reflect"
	"strings"
)

// Page 保存归一化后的偏移分页参数。
// 推荐通过 NormalizePage 构造，不建议直接赋值。
type Page struct {
	Page     int
	PageSize int
}

// NormalizePage 将 page/page_size 约束到合法范围。
// page <= 0 归一化为 1；pageSize <= 0 归一化为 20（项目默认值）；
// pageSize > maxPageSize 时截断到 maxPageSize。
// maxPageSize 必须 > 0，若 <= 0 则使用 50。
func NormalizePage(page, pageSize, maxPageSize int) Page {
	if maxPageSize <= 0 {
		maxPageSize = 50
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	return Page{Page: page, PageSize: pageSize}
}

// Offset 返回该分页对应的 SQL OFFSET 值。
func (p Page) Offset() int { return (p.Page - 1) * p.PageSize }

// Cursor 保存游标分页参数，用于避免深度分页的列表接口。
// Value 对本包是不透明的，由服务方定义其语义（如 ULID、last_modified_at）。
type Cursor struct {
	Value string
	Limit int
}

// NormalizeCursor 将 cursor/limit 约束到合法范围。
// limit <= 0 归一化为 20；limit > maxLimit 时截断到 maxLimit。
// maxLimit <= 0 时回退到 100。空 value 保留原样（由服务方视为首页）。
func NormalizeCursor(value string, limit, maxLimit int) Cursor {
	if maxLimit <= 0 {
		maxLimit = 100
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	return Cursor{Value: value, Limit: limit}
}

// ValidateStringLength 校验 s 的 rune 数是否在 [minLen, maxLen] 范围内。
// maxLen <= 0 表示不设上限。适用于面向用户可见的文本，
// 多字节字符（如中文）应计为 1 个单位。
func ValidateStringLength(s string, minLen, maxLen int) error {
	n := len([]rune(s))
	if minLen > 0 && n < minLen {
		return fmt.Errorf("validation: string length %d below minimum %d", n, minLen)
	}
	if maxLen > 0 && n > maxLen {
		return fmt.Errorf("validation: string length %d exceeds maximum %d", n, maxLen)
	}
	return nil
}

// ValidateByteLength 校验 s 的字节数是否在 [minLen, maxLen] 范围内。
// 适用于受存储约束的字段（如 utf8mb4 表上的 VARCHAR(255)）。
func ValidateByteLength(s string, minLen, maxLen int) error {
	n := len(s)
	if minLen > 0 && n < minLen {
		return fmt.Errorf("validation: byte length %d below minimum %d", n, minLen)
	}
	if maxLen > 0 && n > maxLen {
		return fmt.Errorf("validation: byte length %d exceeds maximum %d", n, maxLen)
	}
	return nil
}

// ValidateEnum 校验 value 是否出现在 allowed 集合中。
// 比较为精确匹配（区分大小写）；如需大小写不敏感，
// 调用方应先对 value 归一化。
func ValidateEnum(value string, allowed []string) error {
	for _, a := range allowed {
		if value == a {
			return nil
		}
	}
	return fmt.Errorf("validation: value %q not in allowed set [%s]", value, strings.Join(allowed, ", "))
}

// ValidateSliceLength 校验切片长度是否在 [minLen, maxLen] 范围内。
// s 必须是 slice、array、string 或 channel，否则返回错误。
// maxLen <= 0 表示不设上限。
func ValidateSliceLength(s any, minLen, maxLen int) error {
	v := reflect.ValueOf(s)
	if !v.IsValid() {
		return fmt.Errorf("validation: value is nil")
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array, reflect.String, reflect.Chan:
		n := v.Len()
		if minLen > 0 && n < minLen {
			return fmt.Errorf("validation: length %d below minimum %d", n, minLen)
		}
		if maxLen > 0 && n > maxLen {
			return fmt.Errorf("validation: length %d exceeds maximum %d", n, maxLen)
		}
		return nil
	default:
		return fmt.Errorf("validation: value of kind %s is not sliceable", v.Kind())
	}
}

// ValidateMapSize 校验 map 大小是否在 [minSize, maxSize] 范围内。
// maxSize <= 0 表示不设上限。
func ValidateMapSize(m map[string]any, minSize, maxSize int) error {
	n := len(m)
	if minSize > 0 && n < minSize {
		return fmt.Errorf("validation: map size %d below minimum %d", n, minSize)
	}
	if maxSize > 0 && n > maxSize {
		return fmt.Errorf("validation: map size %d exceeds maximum %d", n, maxSize)
	}
	return nil
}

// ValidateNonEmptyString 校验 value 在去除首尾空白后是否非空。
// field 参数会包含在错误信息中。
func ValidateNonEmptyString(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("validation: field %q must not be empty", field)
	}
	return nil
}

// ValidatePositiveInt 校验 value 是否 > 0。错误信息中包含 field。
func ValidatePositiveInt(field string, value int64) error {
	if value <= 0 {
		return fmt.Errorf("validation: field %q must be positive, got %d", field, value)
	}
	return nil
}

// ValidateRange 校验 value 是否在 [min, max] 范围内（闭区间）。
// 错误信息中包含 field。min > max 视为永远非法的范围。
func ValidateRange(field string, value, min, max int64) error {
	if min > max {
		return fmt.Errorf("validation: field %q range [%d, %d] is invalid", field, min, max)
	}
	if value < min || value > max {
		return fmt.Errorf("validation: field %q value %d out of range [%d, %d]", field, value, min, max)
	}
	return nil
}
