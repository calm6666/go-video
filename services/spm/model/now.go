package model

import (
	"strings"
	"time"
)

// nowUnix 统一由 model 层取时间，避免调用方各自读时钟导致状态时间不一致。
// 所有落库时间列都是 Unix 秒（BIGINT），与契约里的时间口径一致。
func nowUnix() int64 { return time.Now().Unix() }

// truncate 把字符串折叠成单行并按字节上限截断，同时保证不切断 UTF-8 序列，
// 否则 utf8mb4 列会因非法字节拒绝写入（错误信息里带中文时尤其容易踩到）。
// 用于 last_error / reason 这类「必须留痕但不能撑爆列」的字段。
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	replaced := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' || c == '\r' || c == '\t' {
			c = ' '
		}
		replaced = append(replaced, c)
	}
	if len(replaced) <= max {
		return string(replaced)
	}
	cut := max
	// 回退到合法字符边界（续字节形如 0b10xxxxxx）。
	for cut > 0 && replaced[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(replaced[:cut])
}

// dayStartUnix 返回 ts 所在 UTC 日的零点。留存分桶必须按同一基准对齐，
// 否则跨时区部署会让同一个 cohort_date 落到不同的桶里。
func dayStartUnix(ts int64) int64 { return ts - ts%86400 }

// DayStartUnix 导出天级左边界规整，供 logic 在响应里回显「实际使用的分桶日」。
// 读接口的 WHERE 条件本身已在 model 内对齐，这里只解决「回显什么给用户」：
// logic 不给同一个值，调用方下次传回来的 cohort_date 就会落到另一个桶。
func DayStartUnix(ts int64) int64 { return dayStartUnix(ts) }

// rowPlaceholders 生成批量 INSERT 的 VALUES 段，形如 "(?,?,?),(?,?,?)"。
// 只用于长度已校验过的批量写入路径（单批上限见 logic 层常量）。
func rowPlaceholders(rowLen, rows int) string {
	if rowLen <= 0 || rows <= 0 {
		return ""
	}
	one := strings.TrimSuffix(strings.Repeat("?,", rowLen), ",")
	return strings.TrimSuffix(strings.Repeat("("+one+"),", rows), ",")
}

// 列表查询的分页上限。model 层自己兜底而不是信任调用方：
// 「忘了传 limit」在这类拼接 SQL 里的表现是无界全表扫描，
// 而 SQL 语法完全合法、编译与单测都发现不了，只有线上慢查询会暴露它。
const (
	defaultListLimit int32 = 50
	maxListLimit     int32 = 100
)

// clampLimit 把任意 limit 收敛到 [1, maxListLimit]；<=0 用默认页大小。
// 语义与契约的 ps 上限一致（ListHotSubjects/ListMetricDefinitions/... 均为 100）。
func clampLimit(limit int32) int32 {
	if limit <= 0 {
		return defaultListLimit
	}
	if limit > maxListLimit {
		return maxListLimit
	}
	return limit
}

// clampOffset 兜住负数偏移：`LIMIT ? OFFSET -1` 在 MySQL 里是语法错误，
// 而上层传入的 offset 来自 (p-1)*ps 这类算术，一个非法 p 就能把它变成负数。
func clampOffset(offset int32) int32 {
	if offset < 0 {
		return 0
	}
	return offset
}

// 清理/重算路径的单批行数上限。列表分页与批量作业是两套约束：
// 分页面向用户（上限 100 行响应体就够），批量面向吞吐，
// 但都必须有上限——无 LIMIT 的 DELETE 会在一个事务里锁住整段区间，
// 无 LIMIT 的游标扫描则会把一次回填变成一次全表聚合。
const (
	defaultBatch int32 = 2000
	maxBatch     int32 = 10000
)

// clampBatch 把批量作业的行数收敛到 [1, maxBatch]；<=0 用默认批。
func clampBatch(limit int32) int32 {
	if limit <= 0 {
		return defaultBatch
	}
	if limit > maxBatch {
		return maxBatch
	}
	return limit
}
