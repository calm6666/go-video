package model

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// pickSession 返回事务会话；tx 为空时退化为连接自身（自动提交）。
// 本服务所有「状态迁移 + 事件 + Outbox」的写路径都必须在同一事务里，
// 因此各 model 的写方法都接受 sqlx.Session（AGENTS.md §5）。
func pickSession(conn sqlx.SqlConn, tx sqlx.Session) sqlx.Session {
	if tx != nil {
		return tx
	}
	return conn
}

// rowsAffected 把 UPDATE 结果转成「本次是否真的改到了行」。
// 状态机迁移一律用条件 UPDATE + RowsAffected 判定，禁止先读后写（丢更新）。
func rowsAffected(res sql.Result, op string) (bool, error) {
	affected, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("%s RowsAffected: %w", op, err)
	}
	return affected > 0, nil
}

// clampLimit 夹取 LIMIT：非正值取默认，上限 max。
// 所有列表查询都必须经过它，保证 SQL 里恒有 LIMIT（AGENTS.md §9 全表扫描红线）。
func clampLimit(limit, max int32) int32 {
	if limit <= 0 {
		return max / 5 // 未指定时取上限的五分之一作为默认页大小
	}
	if limit > max {
		return max
	}
	return limit
}

// truncate 按字节上限截断自由文本，避免超出列宽导致整条写入失败。
// 截断在 rune 边界处停止，不会切出半个 UTF-8 序列。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

// pageArgs 组装分页尾参「LIMIT ? OFFSET ?」：limit 经 clampLimit 夹取，offset 归负为零。
//
// 只拼 LIMIT 的列表查询会让第二页原样重复第一页——调用方看到 pn 被回显就以为翻页成功。
// 所有 ListByFilter 都必须走这里，负 offset 也不能透传给 MySQL（会直接报错）。
func pageArgs(limit, max, offset int32) (string, []interface{}) {
	if offset < 0 {
		offset = 0
	}
	return " LIMIT ? OFFSET ?", []interface{}{clampLimit(limit, max), offset}
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// statePlaceholders 返回「非终态流」的 IN 片段与参数（state IN (?,?,?)）。
func statePlaceholders() (string, []interface{}) {
	states := ActiveStreamStates()
	args := make([]interface{}, 0, len(states))
	for _, s := range states {
		args = append(args, s)
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(states)), ","), args
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062）。
// go-sql-driver 在本仓库是间接依赖，这里按错误文案判定，避免新增直接依赖。
// 幂等写入路径（密钥签发、状态上报、回调留证）都靠它区分「重放」与「真失败」。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}

// IsDuplicate 对外暴露唯一索引冲突判定，供 logic 把「并发重复写入」转成幂等重放语义。
func IsDuplicate(err error) bool { return isDuplicateErr(err) }

// isNoRows 判断查询是否命中「无记录」。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// stringArgs 把 []string 转成 SQL 变参。
func stringArgs(vals []string) []interface{} {
	args := make([]interface{}, 0, len(vals))
	for _, v := range vals {
		args = append(args, v)
	}
	return args
}
