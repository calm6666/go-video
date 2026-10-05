// 本文件是 logic 包的手写读路径公共件（翻页参数收敛、时间窗校验、Outbox 真值覆盖），
// 不是 goctl 生成产物。
//
// 五个读方法（GetIngestBatch / ListIngestBatches / GetEventRecord / ListEventRecords /
// ListDeadLetters）必须共用同一口径：同一个库里「A 接口把非法游标当成第一页、B 接口报错」
// 会让运营在两个页面看到互相矛盾的翻页结果，翻页重复计数比直接报错更危险（AGENTS.md §5）。
//
// 读路径的隐私底线（AGENTS.md §7）由 conv.go 的投影负责，本文件只保证「查得到、查得可控」：
// 任何读接口都不开放无界扫描，未知 ID 明确报 model.Err*NotFound 而不是回空对象 + nil。

package logic

import (
	"fmt"
	"strconv"
	"strings"

	"go-video/services/event-collector/internal/config"
	"go-video/services/event-collector/model"
	"go-video/services/event-collector/rpc"
)

// maxListWindowSeconds 时间过滤跨度的硬上限（90 天，与死信保留期同量级）。
//
// 台账都按 (ctime, id) 游标翻页，跨度大只是慢；但「从 Unix 0 开始查」几乎一定是
// 调用方把「不传」和「传 0」搞混了 —— 早报错比返回一个看着像「没有数据」的空页有用。
const maxListWindowSeconds = 90 * 86400

// pageWindow 归一化后的翻页与时间窗参数。
type pageWindow struct {
	pageSize int32
	cursor   model.Cursor
	from     int64
	to       int64
}

// newPageWindow 统一收敛 page_size / cursor / ctime 区间。
//
// page_size 越界报 ErrInvalidPage（不静默截断成上限，那会让调用方误以为拿到了整页）；
// cursor 非法报 ErrInvalidCursor（不能当成第一页）；区间倒置或跨度越界报 ErrInvalidPage。
func newPageWindow(c config.Config, ps int32, cursor string, from, to int64) (pageWindow, error) {
	size, err := clampPage(c, ps)
	if err != nil {
		return pageWindow{}, err
	}
	cur, err := model.ParseCursor(cursor)
	if err != nil {
		return pageWindow{}, err
	}
	if from < 0 || to < 0 {
		return pageWindow{}, model.ErrInvalidPage
	}
	if from > 0 && to > 0 {
		if from > to {
			return pageWindow{}, fmt.Errorf("%w: ctime_from 不能晚于 ctime_to", model.ErrInvalidPage)
		}
		if to-from > maxListWindowSeconds {
			return pageWindow{}, fmt.Errorf("%w: 时间跨度 %d 秒超过上限 %d 秒", model.ErrInvalidPage,
				to-from, maxListWindowSeconds)
		}
	}
	return pageWindow{pageSize: size, cursor: cur, from: from, to: to}, nil
}

// fetchLimit 返回探测 has_more 用的 LIMIT（多取一条）。
func (w pageWindow) fetchLimit() int32 { return fetchMoreLimit(w.pageSize) }

// nextCursor 生成 next_cursor。
//
// 已到底时返回空串：回带最后一行的游标会让调用方以为「还能翻」，
// 于是再要一个空页，并把 has_more=false 的信号抹掉。
func nextCursor(hasMore bool, ctime, id int64) string {
	if !hasMore {
		return ""
	}
	return model.EncodeCursor(ctime, id)
}

// requireAnyFilter 拒绝「零条件」的台账扫描。
//
// ec_event_record / ec_ingest_batch 随采集流量线性增长，没有条件的 COUNT + 翻页
// 等于把排障接口变成打垮自身 MySQL 的攻击面；hint 用来告诉调用方哪些条件可用。
func requireAnyFilter(hit bool, hint string) error {
	if hit {
		return nil
	}
	return fmt.Errorf("%w: 至少需要一个过滤条件（%s），接收台账不开放无界扫描", model.ErrInvalidPage, hint)
}

// requireWindowOrFilter 组合口径：既没有任何等值条件、也没有任何时间边界时，
// 视为整表扫描（ListDeadLetters / ListIngestBatches 用）。
func requireWindowOrFilter(equalFilters bool, w pageWindow, hint string) error {
	if equalFilters || w.from > 0 || w.to > 0 {
		return nil
	}
	return requireAnyFilter(false, hint)
}

// applyPendingTruth 用 Outbox（ec_pending_delivery）真值覆盖事件台账的投递投影。
//
// ec_event_record.delivery_state 是 dispatcher 回写的投影，可能滞后（README 已知缺口）。
// 单条读接口既然能读到 Outbox，就必须以它为准：否则排障时拿一个旧投影得出
// 「事件已发出去」的结论，比报错更糟。
//
// 没有投递行时保持投影不动 —— 被采样丢弃、重复计数、校验拒绝的事件本来就不入 Outbox，
// 「没有投递行」不是异常。多行（一条事件投多个 topic）时状态取「最不利」的那个，
// 只要还有一行在途就不能说「已投递」；topic/envelope 列在多行时语义不唯一，
// 只有单行才覆盖。
func applyPendingTruth(rec *rpc.EventRecord, rows []*model.PendingDelivery) {
	if rec == nil || len(rows) == 0 {
		return
	}
	var (
		open      *model.PendingDelivery
		maxTry    int32
		lastRetry int64
		sawDead   bool
		sawSent   bool
	)
	for _, r := range rows {
		if r == nil {
			continue
		}
		if r.Attempts > maxTry {
			maxTry = r.Attempts
		}
		if r.NextRetryAt > lastRetry {
			lastRetry = r.NextRetryAt
		}
		switch {
		case model.DeliveryStateOpen(r.State):
			// RETRYING 比 PENDING 信息量大（真的试过且失败），优先作为对外结论。
			if open == nil || (r.State == model.DeliveryStateRetrying &&
				open.State != model.DeliveryStateRetrying) {
				open = r
			}
		case r.State == model.DeliveryStateDead:
			sawDead = true
		case r.State == model.DeliveryStateSent:
			sawSent = true
		}
	}
	switch {
	case open != nil:
		rec.DeliveryState = rpc.DeliveryState(open.State)
		rec.NextRetryAt = lastRetry
	case sawDead:
		rec.DeliveryState = rpc.DeliveryState_DELIVERY_STATE_DEAD
		rec.NextRetryAt = 0
	case sawSent:
		rec.DeliveryState = rpc.DeliveryState_DELIVERY_STATE_SENT
		rec.NextRetryAt = 0
	}
	if maxTry > rec.DeliveryAttempts {
		rec.DeliveryAttempts = maxTry
	}
	if len(rows) == 1 && rows[0] != nil {
		rec.Topic = rows[0].Topic
		rec.EnvelopeEventId = rows[0].EnvelopeEventID
	}
}

// --- 过滤条件的形状校验（读路径的隐私底线）---
//
// 过滤条件会作为 SQL 参数进 MySQL：明文设备号/完整 IP 即使不落库，也会以参数形态
// 留在 general log 与慢查询日志里。所以只接受「已脱敏形态」的值 ——
// 台账没存明文不代表整条链路没有漏口（AGENTS.md §7）。

// deviceHashPrefix 与 model.SaltedHash 的输出前缀同源（model 侧未导出，按形状校验）。
const deviceHashPrefix = "h1:"

// validDeviceHashFilter 只接受「h1: + 64 位小写 hex」的加盐哈希形态。
func validDeviceHashFilter(s string) bool {
	if len(s) != len(deviceHashPrefix)+64 || !strings.HasPrefix(s, deviceHashPrefix) {
		return false
	}
	for _, r := range s[len(deviceHashPrefix):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// validIPSegmentFilter 接受脱敏段形态（10.20.30.0/24、2001:db8:1::/48），
// 拒绝带主机位的完整 IPv4（/32）：那是明文 IP。
func validIPSegmentFilter(s string) bool {
	if s == "" || len(s) > maxIPSegmentBytes {
		return false
	}
	i := strings.LastIndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return false
	}
	bits, err := strconv.Atoi(s[i+1:])
	if err != nil || bits <= 0 {
		return false
	}
	if strings.Contains(s, ".") && !strings.Contains(s, ":") && bits >= 32 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F',
			r == '.', r == ':', r == '/':
		default:
			return false
		}
	}
	return true
}

// validEventTypeFilter 校验 event_type 过滤条件（与采集侧同一份形状规则）。
func validEventTypeFilter(s string) bool {
	return s != "" && len(s) <= maxEventTypeBytes && validEventTypeString(s)
}
