package logic

// 本文件是 logic 包的手写共享代码（AGENTS.md §4：自定义代码只能放 internal/logic 等位置）。
// 职责：model 行 → rpc 消息的唯一投影口径 + 汇总缓存读写，
// 保证 10 个方法回显的账户/记录/流水结构完全一致，不在各 logic 里各写一份。

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"go-video/services/coin/internal/config"
	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// accountInfo 投影硬币账户。acc 为 nil 表示「从未建过账户」：
// 余额与历史投币一律 0（不是把缺失当 0 冒充存在，found 由调用方给），
// 但三项限额必须照抄生效配置，否则客户端会以为「没有账户就没有上限」。
func accountInfo(mid int64, acc *model.Account, todayTossed int64, coin config.CoinConf) *rpc.CoinAccountInfo {
	info := &rpc.CoinAccountInfo{
		Mid:                 mid,
		TodayTossed:         todayTossed,
		TodayLimit:          coin.DailyLimit,
		PerTargetLimit:      coin.PerTargetLimit,
		CancelWindowSeconds: coin.CancelWindowSeconds,
	}
	if acc == nil {
		return info
	}
	info.Balance = acc.Balance
	info.TotalTossed = acc.TotalTossed
	info.Version = acc.Version
	info.Ctime = acc.Ctime
	info.Mtime = acc.Mtime
	if acc.Mid != 0 {
		info.Mid = acc.Mid
	}
	return info
}

// tossInfo 投影投币记录。
func tossInfo(t *model.Toss) *rpc.TossInfo {
	if t == nil {
		return nil
	}
	return &rpc.TossInfo{
		TossId:        t.ID,
		Mid:           t.Mid,
		TargetAid:     t.TargetAid,
		Count:         t.Count,
		State:         rpcTossState(t.State),
		FirstTossedAt: t.FirstTossedAt,
		LastTossedAt:  t.LastTossedAt,
		CancelledAt:   t.CancelledAt,
		LastRequestId: t.LastRequestID,
	}
}

func tossInfos(rows []*model.Toss) []*rpc.TossInfo {
	out := make([]*rpc.TossInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, tossInfo(r))
	}
	return out
}

// flowInfo 投影流水台账行。
func flowInfo(f *model.Flow) *rpc.CoinFlowInfo {
	if f == nil {
		return nil
	}
	return &rpc.CoinFlowInfo{
		FlowId:       f.ID,
		Mid:          f.Mid,
		FlowType:     rpcFlowType(f.FlowType),
		Delta:        f.Delta,
		BalanceAfter: f.BalanceAfter,
		TargetAid:    f.TargetAid,
		BizNo:        f.BizNo,
		Operator:     f.Operator,
		RequestId:    f.RequestID,
		Remark:       f.Remark,
		Ctime:        f.Ctime,
	}
}

func flowInfos(rows []*model.Flow) []*rpc.CoinFlowInfo {
	out := make([]*rpc.CoinFlowInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, flowInfo(r))
	}
	return out
}

// rpcTossState / rpcFlowType 做显式映射而不是整数强转：
// 库里出现枚举外的值意味着数据被绕过本服务写过，必须塌成 UNSPECIFIED 让调用方看得见，
// 而不是把一个不存在的状态原样发出去。
func rpcTossState(v int32) rpc.TossState {
	switch v {
	case model.TossStateActive:
		return rpc.TossState_TOSS_STATE_ACTIVE
	case model.TossStateCancelled:
		return rpc.TossState_TOSS_STATE_CANCELLED
	default:
		return rpc.TossState_TOSS_STATE_UNSPECIFIED
	}
}

func rpcFlowType(v int32) rpc.CoinFlowType {
	switch v {
	case model.FlowTypeToss:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_TOSS
	case model.FlowTypeCancelToss:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_CANCEL_TOSS
	case model.FlowTypeOrderPack:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_ORDER_PACK
	case model.FlowTypeAdminGrant:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_ADMIN_GRANT
	case model.FlowTypeExpire:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_EXPIRE
	default:
		return rpc.CoinFlowType_COIN_FLOW_TYPE_UNSPECIFIED
	}
}

// validFlowType 判断入参给的流水类型是否是本服务认识的合法值。
// 注意 EXPIRE(5) 合法但本服务永不写入（未开启过期能力），只作为查询过滤条件存在。
func validFlowType(v int32) bool {
	return v >= model.FlowTypeToss && v <= model.FlowTypeExpire
}

// initialGrantFlow 构造建仓初始币流水：余额从 0 变到 InitialBalance 必须有凭证，
// 否则「余额 = 流水之和」的对账不变式在第一个用户身上就破了。
// request_id 由 mid 唯一决定，重复建仓在唯一索引上直接挡死。
func initialGrantFlow(mid, initial int64) *model.Flow {
	return &model.Flow{
		Mid:          mid,
		FlowType:     model.FlowTypeAdminGrant,
		Delta:        initial,
		BalanceAfter: initial,
		BizNo:        model.InitialGrantBizNo,
		Operator:     model.InitialGrantOperator,
		RequestID:    model.InitialGrantRequestID(mid),
		Remark:       fmt.Sprintf("账户建仓初始硬币 %d 枚（沙箱便利，非真实赠送规则）", initial),
	}
}

// maxRemarkLen / maxIDLen 不在这里重复列宽数字，而是引用 model 的列宽常量：
// 宽度只有一处真值，且由 model/migration_parity_test.go 拿它与建表语句的 VARCHAR(n) 对照。
const (
	// maxRemarkLen 与 cn_flow.remark 的 VARCHAR(255) 对齐（按字符数，避免截断多字节中文时留半字）。
	maxRemarkLen = model.MaxRemarkChars
	// maxIDLen 与 cn_flow.{request_id,biz_no,operator,trace_id} / cn_toss.{last_request_id,trace_id}
	// 的 VARCHAR(64) 对齐（MySQL 的长度单位是字符，不是字节）。
	maxIDLen = model.MaxIDChars
)

// clipRemark 把运营填写的原因等长文本收敛到列宽内，超长部分丢弃而不是让整笔交易失败：
// 流水的凭证价值在 delta/balance_after/request_id，remark 只是摘要。
func clipRemark(s string) string {
	r := []rune(s)
	if len(r) <= maxRemarkLen {
		return s
	}
	return string(r[:maxRemarkLen])
}

// clipID 截断客户端可控的字符串列宽（trace_id / operator / biz_no，VARCHAR(64)），
// 避免超长入参把整笔已判定成功的交易打成 MySQL 错误。
// 按字符而不是按字节：MySQL 的 VARCHAR(64) 是 64 个**字符**，按字节切会把多字节
// 操作者名/订单号腰斩成非法 UTF-8，那种串写库直接报 1366，正是本函数要挡的故障。
func clipID(s string) string {
	r := []rune(s)
	if len(r) <= maxIDLen {
		return s
	}
	return string(r[:maxIDLen])
}

// --- 单内容投币汇总缓存 ---
//
// 只缓存展示口径（详情页/列表页的 coin_count、投币人数），TTL 内的秒级偏差可接受；
// 余额、日额度、幂等判定永不走缓存（见 config.CoinConf 注释）。
// 缓存任何一步失败都按 miss 处理并回源 MySQL：缓存不能影响正确性。

const summaryCachePrefix = "coin:sum:"

// cachedSummary 是缓存值结构，字段与 rpc.TargetCoinSummary 的聚合部分一一对应。
// like_count 不入缓存：它恒为 0（点赞归 engagement），缓存里没必要再存一个 0。
type cachedSummary struct {
	CoinCount     int64 `json:"c"`
	CoinUserCount int64 `json:"u"`
}

func summaryCacheKey(aid int64) string {
	return summaryCachePrefix + strconv.FormatInt(aid, 10)
}

// readSummaryCache 返回命中的 aid → 聚合值；未启用缓存或整体 miss 时返回 nil。
func readSummaryCache(ctx context.Context, s *svc.ServiceContext, aids []int64) map[int64]cachedSummary {
	if s.Cache == nil || s.Coin().TargetSummaryCacheTTLSeconds <= 0 || len(aids) == 0 {
		return nil
	}
	keys := make([]string, 0, len(aids))
	for _, aid := range aids {
		keys = append(keys, summaryCacheKey(aid))
	}
	vals, err := s.Cache.MgetCtx(ctx, keys...)
	if err != nil {
		// miss 与故障不可区分，一律回源；这里不打日志由调用方按回源结果继续。
		return nil
	}
	out := make(map[int64]cachedSummary, len(aids))
	for i, aid := range aids {
		if i >= len(vals) || vals[i] == "" {
			continue
		}
		var v cachedSummary
		if err := json.Unmarshal([]byte(vals[i]), &v); err != nil {
			continue
		}
		out[aid] = v
	}
	return out
}

// writeSummaryCache 回填缓存；aid 即使聚合结果为 0 也要写（空值同样要缓存，
// 否则无投币的内容每次列表页都回源，等于没有缓存）。
func writeSummaryCache(ctx context.Context, s *svc.ServiceContext, rows map[int64]cachedSummary) {
	if s.Cache == nil || len(rows) == 0 {
		return
	}
	ttl := int(s.Coin().TargetSummaryCacheTTLSeconds)
	if ttl <= 0 {
		return
	}
	for aid, v := range rows {
		bs, err := json.Marshal(v)
		if err != nil {
			continue
		}
		if err := s.Cache.SetexCtx(ctx, summaryCacheKey(aid), string(bs), ttl); err != nil {
			return
		}
	}
}

// invalidateSummaryCache 在投币/取消提交后清掉受影响 aid 的缓存。
// 失败只记日志：TTL 已经保证了最终一致，清不掉只是让详情页多展示几秒旧计数，
// 不值得为此把已经提交的交易改成失败。
func invalidateSummaryCache(ctx context.Context, s *svc.ServiceContext, aid int64) {
	if s.Cache == nil || aid <= 0 {
		return
	}
	if _, err := s.Cache.DelCtx(ctx, summaryCacheKey(aid)); err != nil {
		logx.Errorf("coin: invalidate summary cache aid=%d: %v", aid, err)
	}
}
