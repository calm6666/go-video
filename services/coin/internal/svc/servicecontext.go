// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"fmt"

	"go-video/services/coin/internal/config"
	"go-video/services/coin/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// ServiceContext 是 coin 服务的运行时上下文，承载跨请求共享的依赖。
//
// 装配原则（AGENTS.md §5）：
//   - 只拥有 go_video_coin 库的 4 张 cn_* 表；硬币余额的唯一写入口就是本服务，
//     payment 的现金余额是另一套账，两者不互换、不共表、不互查；
//   - 所有余额判定与扣减都必须落在这份数据上，禁止用 Redis 的计数结果做门禁
//     （CacheRedis 只放「单内容投币汇总」这类展示口径，见 config.CoinConf 注释）；
//   - 本轮不接 MQ、不写 outbox、不接风控：投币是社区行为，其计数投影由
//     engagement/video 侧自行按 RPC 读取（见 README「已知缺口」）；
//   - MySQL 是硬依赖（条件扣减与幂等都要落库），连不上就启动失败；
//     CacheRedis 可选，Host 为空时退化为直接回源。
type ServiceContext struct {
	Config config.Config

	// DB 本服务自有库（go_video_coin）的连接，logic 用它起事务。
	DB sqlx.SqlConn

	// Cache 汇总缓存；nil 表示未配置或已关闭，调用方必须回源 MySQL 而不是返回空。
	Cache *redis.Redis

	// --- 本服务自有表的数据访问（库 go_video_coin，一表一 model，见 services/coin/model）---

	// Accounts cn_account：硬币余额唯一写入口。
	Accounts model.AccountModel
	// Daily cn_daily_toss：(mid, date) 一行一天，日额度判定唯一真值。
	Daily model.DailyTossModel
	// Tosses cn_toss：(mid, target_aid) 投币事实，coin_count 由它聚合得出。
	Tosses model.TossModel
	// Flows cn_flow：append-only 台账，兼作 request_id 幂等锚点。
	Flows model.FlowModel
}

// NewServiceContext 构造 ServiceContext。
//
// 配置里的限额若被写成非正数，Sanitize 会退回与 etc 默认值一致的安全值并打日志：
// 「无日限」等于限额判定失效，是比「配错一个数字」严重得多的事故面。
func NewServiceContext(c config.Config) *ServiceContext {
	coin := c.Coin
	if notes := coin.Sanitize(); len(notes) > 0 {
		for _, n := range notes {
			logx.Errorf("coin/svc: %s", n)
		}
	}

	var cache *redis.Redis
	if coin.TargetSummaryCacheTTLSeconds > 0 && c.CacheRedis.Host != "" {
		cache = redis.MustNewRedis(c.CacheRedis)
	} else {
		logx.Infof("coin/svc: 汇总缓存未启用（TTL=%d, CacheRedis.Host=%q），GetTargetSummary 直接回源 MySQL",
			coin.TargetSummaryCacheTTLSeconds, c.CacheRedis.Host)
	}

	conn := sqlx.NewMysql(c.DataSource)
	sc := &ServiceContext{
		Config:   c,
		DB:       conn,
		Cache:    cache,
		Accounts: model.NewAccountModel(conn),
		Daily:    model.NewDailyTossModel(conn),
		Tosses:   model.NewTossModel(conn),
		Flows:    model.NewFlowModel(conn),
	}
	for _, note := range sc.Notes() {
		logx.Infof("coin/svc: %s", note)
	}
	return sc
}

// Coin 返回已收敛的生效领域参数，logic 只读这里，不再碰原始配置。
func (s *ServiceContext) Coin() config.CoinConf { return s.Config.Coin }

// PageSize 把请求的每页大小收敛到允许区间：
// <=0 用默认值；超上限返回错误而不是静默截断 —— 截断会让调用方误判「没有下一页」。
func (s *ServiceContext) PageSize(requested int64) (int, error) {
	coin := s.Config.Coin
	if requested <= 0 {
		return int(coin.DefaultPageSize), nil
	}
	if requested > coin.MaxPageSize {
		return 0, fmt.Errorf("%w: size=%d, max=%d", model.ErrPageSizeTooLarge, requested, coin.MaxPageSize)
	}
	return int(requested), nil
}

// Offset 把 1 起始的页码换算成 OFFSET，非法页码按第 1 页处理（不报错）：
// 翻过头是列表页的常态，返回空列表即可，没必要把它做成错误。
func (s *ServiceContext) Offset(page int64, size int) int64 {
	if page <= 1 {
		return 0
	}
	return (page - 1) * int64(size)
}

// Notes 输出装配期诊断，便于运维确认本环境的生效限额。
func (s *ServiceContext) Notes() []string {
	coin := s.Config.Coin
	return []string{
		fmt.Sprintf("生效限额：日投 %d 枚、单片累计 %d 枚、取消窗口 %d 秒、单次发放绝对值上限 %d 枚",
			coin.DailyLimit, coin.PerTargetLimit, coin.CancelWindowSeconds, coin.MaxGrantDelta),
		fmt.Sprintf("新建账户初始硬币 %d 枚（随建仓写 biz_no=INITIAL_BALANCE 的 ADMIN_GRANT 流水）",
			coin.InitialBalance),
	}
}
