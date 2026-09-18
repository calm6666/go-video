// Package flagvar 提供自定义 flag.Value 实现，把命令行参数解析为切片等结构化类型。
package flagvar

import (
	"strings"
)

// StringVars 是 []string 类型的 flag.Value 实现，支持多次出现的同名 flag 累积为切片。
type StringVars []string

// String 返回以逗号分隔的扁平字符串，用于 flag 帮助与默认值显示。
func (s StringVars) String() string {
	return strings.Join(s, ",")
}

// Set 把单个值追加到切片。flag 包对每个出现的 flag 调用一次 Set。
func (s *StringVars) Set(val string) error {
	*s = append(*s, val)
	return nil
}
