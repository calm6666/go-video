package repository

import (
	"errors"
	"strings"
)

// errStateCasMiss 是事务内 CAS 未命中的内部控制流错误：
// 表示弹幕状态已被并发读者推进，本次迁移整体回滚，不视为业务失败。
var errStateCasMiss = errors.New("danmaku: state cas miss")

// isDuplicateErr 判断底层错误是否为唯一索引冲突。
//
// 这里不导入 go-sql-driver/mysql（该依赖在本仓库是 indirect，直接引用会改变
// go.mod 的依赖分类），因此按 MySQL 错误报文做字符串判定：
// 驱动错误格式固定为 `Error <number>: ...`，重复键为 1062 / "Duplicate entry"。
// 判定只影响“重复投递”是否降级为幂等成功，不会掩盖其它错误。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "Duplicate entry") {
		return true
	}
	return strings.Contains(msg, "Error 1062")
}
