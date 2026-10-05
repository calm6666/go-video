package model

import (
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// pick 在 session 非空时用事务句柄，否则用连接池。
// 采集路径要求「批次行 + 事件行 + 投递行」同事务写入，logic 传入 session；
// 只读查询与游标翻页不传 session，避免长事务占连接。
func pick(session sqlx.Session, conn sqlx.SqlConn) sqlx.SqlConn {
	if session != nil {
		return sqlx.NewSqlConnFromSession(session)
	}
	return conn
}

// placeholders 生成 "?,?,?"（长度至少 1，调用方需自行保证非空集合）。
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// maxIDList 批量读写单次 ID 上限：调用方（logic）必须先按配置切分，
// 这里再兜一层，防止把 10 万个 event_id 拼进一条 IN 打爆 MySQL 包大小。
const maxIDList = 500

// maxStringIDList 字符串主键（event_id）批量上限，与 maxIDList 同源约束。
const maxStringIDList = 500
