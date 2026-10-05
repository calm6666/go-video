package repository

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// 本文件是 Redis 滑窗计数的纯计算部分：档位选择、桶定位与 key 构造。
// 与 common/counter 的 rolling 计数语义一致（窗口 = buckets 个等长桶），
// 区别是这里把桶落到 Redis key 上，从而支持多实例共享同一个窗口。
// 单独成文件是为了让桶数学可脱离 Redis 单测。

// Redis key 前缀。本服务只读写 rc: 前缀下的 key（AGENTS.md §5 禁止跨服务共用业务 key）。
const (
	keyPrefixCounter = "rc:c"   // rc:c:<metric>:<subject>:<action>:<tier>:<bucket>
	keyPrefixEvent   = "rc:evt" // rc:evt:<event_id> 行为上报幂等标记
	keyPrefixCheck   = "rc:ck"  // rc:ck:<request_id> 裁决幂等回放
	keyPrefixRules   = "rc:rl"  // rc:rl:<action> 启用规则集合缓存
)

// DefaultCounterTiers 是未配置档位时使用的窗口档位（秒）。
var DefaultCounterTiers = []int64{60, 600, 3600}

// normalizeTiers 过滤非法档位并升序去重；结果为空时回落到默认档位。
func normalizeTiers(tiers []int64) []int64 {
	seen := make(map[int64]struct{}, len(tiers))
	out := make([]int64, 0, len(tiers))
	for _, t := range tiers {
		if t <= 0 {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) == 0 {
		out = append(out, DefaultCounterTiers...)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// pickTier 为期望窗口 want 选择不小于它的最小档位。
// ok=false 表示 want 超过已配置的最大档位，该规则不可观测（不得用近似窗口冒充）。
func pickTier(tiers []int64, want int64) (tier int64, ok bool) {
	for _, t := range tiers {
		if t >= want {
			return t, true
		}
	}
	return 0, false
}

// bucketSize 返回档位 tier 下每个桶的秒数，至少 1 秒。
func bucketSize(tier int64, buckets int) int64 {
	if buckets <= 0 {
		buckets = 1
	}
	bs := tier / int64(buckets)
	if bs < 1 {
		bs = 1
	}
	return bs
}

// bucketSpan 返回覆盖 [now-window, now] 需要读取的桶个数。
// 由于窗口按桶边界对齐，实际统计跨度可能比 window 最多大 bucket-1 秒，
// 这是滑窗计数在分布式实现下的已知近似，README 已说明。
func bucketSpan(now, window, bucket int64) int {
	if bucket <= 0 || window <= 0 {
		return 1
	}
	hi := now / bucket
	lo := (now - window) / bucket
	span := hi - lo + 1
	if span < 1 {
		return 1
	}
	// 保护：窗口远超档位时不做跨档位扫描，交由 pickTier 提前判定。
	if span > 4096 {
		span = 4096
	}
	return int(span)
}

// bucketIndex 返回时间戳 ts 在给定桶长下的桶号。
func bucketIndex(ts, bucket int64) int64 {
	if bucket <= 0 {
		return 0
	}
	return ts / bucket
}

// counterKey 构造计数器 key。subject 必须是已带命名空间的受控标识
// （如 mid:123、dev:<sha256hex>、ip:<sha256hex>），不得传入明文 IP 或设备号。
func counterKey(metric, subject string, action int32, tier, bucket int64) string {
	return keyPrefixCounter + ":" + metric + ":" + subject + ":" +
		strconv.FormatInt(int64(action), 10) + ":" +
		strconv.FormatInt(tier, 10) + ":" +
		strconv.FormatInt(bucket, 10)
}

// counterKeys 返回某指标在窗口内需要读取的全部桶 key（按时间从早到晚）。
// tiers/buckets 非法或窗口超过最大档位时返回 ok=false。
func counterKeys(metric, subject string, action int32, tiers []int64, buckets int, window, now int64) (keys []string, tier, bucket int64, ok bool) {
	tier, ok = pickTier(tiers, window)
	if !ok {
		return nil, 0, 0, false
	}
	bucket = bucketSize(tier, buckets)
	start := bucketIndex(now-window, bucket)
	end := bucketIndex(now, bucket)
	keys = make([]string, 0, end-start+1)
	for i := start; i <= end; i++ {
		keys = append(keys, counterKey(metric, subject, action, tier, i))
	}
	return keys, tier, bucket, true
}

// subjectMid / subjectDevice / subjectIP 生成计数器主体标识。
func subjectMid(mid int64) string      { return "mid:" + strconv.FormatInt(mid, 10) }
func subjectDevice(hash string) string { return "dev:" + hash }
func subjectIP(hash string) string     { return "ip:" + hash }

// eventKey 返回行为上报幂等标记 key；eventID 为空时返回空串（不去重）。
func eventKey(eventID string) string {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return ""
	}
	return keyPrefixEvent + ":" + eventID
}

// checkKey 返回裁决回放缓存 key。
func checkKey(requestID string) string {
	if strings.TrimSpace(requestID) == "" {
		return ""
	}
	return keyPrefixCheck + ":" + requestID
}

// rulesKey 返回某动作启用规则集合的缓存 key。
func rulesKey(action int32) string {
	return fmt.Sprintf("%s:%d", keyPrefixRules, action)
}

// parseCounts 把 MGET 结果累加为计数；非法值按 0 处理而不是中断评估，
// 因为个别脏 key 不应放大成整条风控链路失败。
func parseCounts(values []string) int64 {
	var total int64
	for _, v := range values {
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		total += n
	}
	return total
}

// dedupTTLSeconds 计算上报去重标记的存活时间：至少 2 个默认窗口，上限 2 小时。
func dedupTTLSeconds(defaultWindow int64) int {
	ttl := defaultWindow * 2
	if ttl < 120 {
		ttl = 120
	}
	if ttl > 7200 {
		ttl = 7200
	}
	return int(ttl)
}

// clampInt 把可能溢出的 int64 收敛到 int，避免把异常配置变成 panic。
func clampInt(v int64) int {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < 0 {
		return 0
	}
	return int(v)
}
