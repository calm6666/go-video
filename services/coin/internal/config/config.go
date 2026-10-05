// Code scaffolded by goctl. Safe to edit.

package config

import (
	"fmt"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 coin（投币域）服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向端的 HTTP 由 gateway/app 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 只承载「单内容投币汇总」这类读多写少的易失副本；
	// 余额与投币记录的真值恒在 MySQL，缓存缺失一律回源，不参与任何判定。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_coin 库的 MySQL DSN（本服务唯一可写的库，AGENTS.md §5）。
	DataSource string

	// Coin 是投币领域参数：日限、单片上限、取消窗口、发放上限等全部来自
	// etc yaml / 配置中心，客户端不得写死（AGENTS.md §6，GetTossConfig 负责回显）。
	Coin CoinConf
}

// CoinConf 是投币业务参数。零值/负值不被信任，svc 侧统一收敛到 CoinConf.Sanitize 的兜底值，
// 因为「无日限」和「无初始余额」都会让限额判定形同虚设。
type CoinConf struct {
	// InitialBalance 新建硬币账户的初始硬币数（沙箱便利，非真实赠送规则）。
	// 建仓时同步写一条 ADMIN_GRANT 流水（biz_no=INITIAL_BALANCE），
	// 保证「余额恒等于流水之和」这一对账不变式成立；0 表示不发初始币。
	InitialBalance int64 `json:",default=5"`

	// DailyLimit 单用户每自然日可投出的硬币上限。
	DailyLimit int64 `json:",default=10"`

	// PerTargetLimit 单用户对同一条内容累计可投的硬币上限（取消后重新投币重新计数）。
	PerTargetLimit int64 `json:",default=2"`

	// CancelWindowSeconds 取消投币的时间窗（秒），自最后一次投币时刻起算。
	CancelWindowSeconds int64 `json:",default=86400"`

	// MinBalanceToToss 投币时要求「扣减后余额仍不低于 0 且扣减额不小于本值」的下限门槛，
	// 即条件扣减用 max(count, MinBalanceToToss) 作为余额门槛。默认 1 等于「有一枚就能投一枚」。
	MinBalanceToToss int64 `json:",default=1"`

	// MaxGrantDelta GrantCoin 单次发放/扣回的绝对值上限，超限直接拒绝，
	// 用来挡住运营误操作与订单履约侧的异常金额。
	MaxGrantDelta int64 `json:",default=1000"`

	// DefaultPageSize 列表查询未显式给 size 时的每页条数。
	DefaultPageSize int64 `json:",default=20"`

	// MaxPageSize 列表查询每页条数硬上限（台账/投币记录/投币人列表共用）。
	MaxPageSize int64 `json:",default=100"`

	// MaxBatchAids BatchGetTargetSummary 单次接受的 aid 数量上限，超出部分裁剪，
	// 挡住列表页把整屏 aid 一次性塞进来打爆 MySQL。
	MaxBatchAids int64 `json:",default=50"`

	// TargetSummaryCacheTTLSeconds 单内容投币汇总的 Redis 缓存秒数，0 表示关闭缓存直接回源。
	// 汇总值是展示口径，允许该 TTL 内的秒级偏差；余额与额度判定永不走缓存。
	TargetSummaryCacheTTLSeconds int64 `json:",default=10"`
}

// 兜底值：与 etc/coin.v1.yaml 的显式取值保持一致，改一处必须改两处，
// 由 internal/config/config_load_test.go 断言两边不漂移。
const (
	fallbackInitialBalance      = int64(5)
	fallbackDailyLimit          = int64(10)
	fallbackPerTargetLimit      = int64(2)
	fallbackCancelWindowSeconds = int64(86400)
	fallbackMinBalanceToToss    = int64(1)
	fallbackMaxGrantDelta       = int64(1000)
	fallbackDefaultPageSize     = int64(20)
	fallbackMaxPageSize         = int64(100)
	fallbackMaxBatchAids        = int64(50)
)

// Sanitize 就地收敛不可信的限额取值，返回被纠正项的说明（供启动期打日志）。
//
// 只纠正「会让门禁失效」的取值，不改运营有意写小的值：
//   - InitialBalance 允许 0（表示不发初始币），负数按 0 处理；
//   - DailyLimit / PerTargetLimit 为负会让「tossed + delta <= limit」恒假、
//     「count + delta <= limit」恒假，投币全量失败；这里回到兜底值而不是 0，
//     因为 0 意味着「谁都不能投币」，那是配置事故的另一种表现，不该由代码猜；
//   - 分页/批量类上限为 0 或负数会让每条列表查询都报错，同样回到兜底值，
//     并保证 DefaultPageSize <= MaxPageSize。
func (c *CoinConf) Sanitize() []string {
	var notes []string
	note := func(field string, got, want int64) {
		notes = append(notes, fmt.Sprintf("Coin.%s=%d 不可用，按 %d 兜底（etc/coin.v1.yaml 同名键为期望值）", field, got, want))
	}

	if c.InitialBalance < 0 {
		note("InitialBalance", c.InitialBalance, 0)
		c.InitialBalance = 0
	}
	if c.DailyLimit <= 0 {
		note("DailyLimit", c.DailyLimit, fallbackDailyLimit)
		c.DailyLimit = fallbackDailyLimit
	}
	if c.PerTargetLimit <= 0 {
		note("PerTargetLimit", c.PerTargetLimit, fallbackPerTargetLimit)
		c.PerTargetLimit = fallbackPerTargetLimit
	}
	if c.CancelWindowSeconds <= 0 {
		note("CancelWindowSeconds", c.CancelWindowSeconds, fallbackCancelWindowSeconds)
		c.CancelWindowSeconds = fallbackCancelWindowSeconds
	}
	if c.MinBalanceToToss <= 0 {
		note("MinBalanceToToss", c.MinBalanceToToss, fallbackMinBalanceToToss)
		c.MinBalanceToToss = fallbackMinBalanceToToss
	}
	if c.MaxGrantDelta <= 0 {
		note("MaxGrantDelta", c.MaxGrantDelta, fallbackMaxGrantDelta)
		c.MaxGrantDelta = fallbackMaxGrantDelta
	}
	if c.MaxPageSize <= 0 {
		note("MaxPageSize", c.MaxPageSize, fallbackMaxPageSize)
		c.MaxPageSize = fallbackMaxPageSize
	}
	if c.DefaultPageSize <= 0 {
		note("DefaultPageSize", c.DefaultPageSize, fallbackDefaultPageSize)
		c.DefaultPageSize = fallbackDefaultPageSize
	}
	if c.DefaultPageSize > c.MaxPageSize {
		note("DefaultPageSize>MaxPageSize", c.DefaultPageSize, c.MaxPageSize)
		c.DefaultPageSize = c.MaxPageSize
	}
	if c.MaxBatchAids <= 0 {
		note("MaxBatchAids", c.MaxBatchAids, fallbackMaxBatchAids)
		c.MaxBatchAids = fallbackMaxBatchAids
	}
	if c.TargetSummaryCacheTTLSeconds < 0 {
		note("TargetSummaryCacheTTLSeconds", c.TargetSummaryCacheTTLSeconds, 0)
		c.TargetSummaryCacheTTLSeconds = 0
	}
	return notes
}
