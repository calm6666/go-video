// Code scaffolded by goctl. Safe to edit.

package config

import (
	"errors"

	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 spm（用户行为分析与推荐特征计算）服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），对外 HTTP 与响应信封由 gateway 聚合。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载热榜分页短缓存、口径 ACTIVE 指针缓存与聚合作业的租约续期锁。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段（限流用），
	// 同名会让 conf.Load 报 "conflict key redis"，全仓统一用 CacheRedis。
	//
	// 这里缓存的必须是**可从 go_video_spm 投影重建的东西**：缓存丢失只影响延迟，
	// 不影响任何指标口径（AGENTS.md §5 数据所有权）。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_spm 库的 MySQL DSN。本服务只写自己的 schema，
	// 跨服务数据一律走 RPC 或本地只读投影（spm_content_projection）。
	DataSource string

	// Spm 是行为分析领域参数：窗口粒度、批量上限、留存期与迟到容忍。
	// 全部来自 etc yaml 或配置中心，不允许写死在 logic 里（否则同一口径在不同
	// 进程里有两套边界，重算结果就无法解释）。
	Spm SpmConf
}

// SpmConf 行为分析参数。
//
// 每个字段的取值上界都对应 model 层的一处硬约束（注释里点了名）：配置可以收紧，
// 不能放宽到 model 会静默 clamp 的程度——那会让「调用方以为取了 500 行」
// 变成「实际只取了 100 行」，而响应里 total 看着完全正常。
// Validate 就是把这条边界变成启动期错误。
type SpmConf struct {
	// PageSize 请求未指定 ps 时的默认每页条数。
	PageSize int32 `json:",default=20"`
	// MaxPageSize 服务端允许的每页上限，对应契约的「ps 上限 100」与 model.maxListLimit。
	MaxPageSize int32 `json:",default=100"`
	// MaxMetricKeysPerRequest BatchGetMetrics 单次口径数上限（契约 1..50，
	// 对应 model.maxMetricKeysPerRequest：再大就是 50 个 key × 30 窗口的行集，
	// 单请求响应体会超过 gRPC 默认消息上限）。
	MaxMetricKeysPerRequest int32 `json:",default=50"`
	// MaxWindowCount BatchGetMetrics 单次连续窗口数上限（契约 1..30）。
	MaxWindowCount int32 `json:",default=30"`
	// MaxWindowsPerJob 单个聚合作业的计划窗口数上限（windows_total）。
	// 超限直接拒绝而不是「拆一半先跑」：拆半会让 SubmitAggregationJob 的幂等语义变成
	// 「同一个 request_id 有时覆盖整段、有时只覆盖一段」，重算进度就没法解释了。
	// 取 2880 = 5 分钟窗口 × 24 小时 × 10 天，是一次回填作业的合理最大跨度。
	MaxWindowsPerJob int32 `json:",default=2880"`
	// MaxWritePoints WriteMetricWindow 单次写入点数上限
	// （对应 model.maxMetricPointsPerBatch：超过就把一次事务的持锁时间线性拉长）。
	MaxWritePoints int32 `json:",default=500"`
	// MaxRetentionDay GetRetention 允许的最大 N（契约 1..90，对应 model.maxRetentionDay）。
	// 收紧它是口径决策：D90 之后的留存曲线只能依赖保留更久的行为事实，见 BehaviorRetentionDays。
	MaxRetentionDay int32 `json:",default=90"`
	// InterestTopN GetUserInterest 未指定 top_n 时返回的兴趣数。
	InterestTopN int32 `json:",default=20"`
	// MaxInterestTopN GetUserInterest 的 top_n 上限（契约 100，对应 model.maxInterestRowsPerMid）。
	// 画像行本身按此上限裁剪，返回条数不可能超过它。
	MaxInterestTopN int32 `json:",default=100"`
	// RealtimeWindowType 实时聚合器推进的窗口粒度：1=5 分钟、2=1 小时。
	// 必须是 model.WindowType 里的实时档（day/week/total 属离线回填）。
	RealtimeWindowType int32 `json:",default=1,options=1|2"`
	// LateToleranceSeconds 窗口闭合后仍允许写入的迟到事件秒数。
	// 必须小于实时窗口长度（Validate 强制）：5 分钟窗口配 2 分钟容忍，
	// 意味着最多改写「刚刚闭合」的那一个窗口。超过这条线的迟到数据只能走
	// WriteMetricWindowReq.allow_late_write=true 的显式回填，
	// 否则「同一窗口的值被更晚到达的旧事件改掉」会在热榜上表现为无法复现的抖动。
	LateToleranceSeconds int64 `json:",default=120"`
	// InterestStaleAfterSeconds 画像最近一次更新超过该秒数时 GetUserInterest 回 stale=true，
	// 调用方（feature-store/recommend）据此走冷启动策略，而不是拿一份半年前的权重继续排序。
	InterestStaleAfterSeconds int64 `json:",default=86400"`
	// WatermarkLagSeconds 水位落后当前时间超过该秒数即视为「聚合器停摆」，
	// 热榜继续返回最近闭合窗口，同时在响应里保留真实 window_start 供调用方判断。
	WatermarkLagSeconds int64 `json:",default=900"`
	// BehaviorRetentionDays spm_behavior_event 事实留存天数。
	// 到期由 DeleteExpired 分批清理；<=0 等于永不清理，只允许一次性调试实例（见 Validate）。
	BehaviorRetentionDays int `json:",default=180"`
	// ProjectionRetentionDays 投影表（metric_window/user_interest/retention_cohort）的清理水位天数。
	// 必须长于事实留存期：投影比事实活得久才有「口径改了但事实已删」的可解释历史。
	ProjectionRetentionDays int `json:",default=400"`
	// DeleteBatchSize 清理类写操作（事实过期删除、投影删除）的单批行数上限，
	// 对应 model.maxBatch：无 LIMIT 的 DELETE 会在一个事务里锁住整段区间。
	DeleteBatchSize int32 `json:",default=2000"`
	// JobLeaseSeconds 聚合作业租约时长，ClaimPending/RenewLease 以此为抢占边界。
	// 必须大于单窗口最坏耗时，否则两个进程会同时写同一窗口。
	JobLeaseSeconds int64 `json:",default=300"`
	// JobMaxRetry 作业失败后的最大重试次数，超过即置 FAILED 终态（不无限重放）。
	JobMaxRetry int32 `json:",default=3"`
	// MaxRetryIntervalSeconds 消费失败的最大重试间隔（指数退避的上限）。
	MaxRetryIntervalSeconds int64 `json:",default=600"`
	// DeadLetterPreviewBytes 死信留档的脱敏预览字节上限。
	// 只用于「让运维认出是哪条消息」，原文一律不入库。
	DeadLetterPreviewBytes int32 `json:",default=512"`
	// ReadQps 进程级读接口 QPS（保护 MySQL 投影表，真值判定仍以口径为准）。
	ReadQps int32 `json:",default=2000"`
	// WriteQps 进程级写接口 QPS：WriteMetricWindow/SubmitAggregationJob 等计算链路入口。
	WriteQps int32 `json:",default=200"`
	// WriteBurst 写侧令牌桶突发容量（离线回填引擎会成批提交）。
	WriteBurst int32 `json:",default=100"`
}

// Validate 启动期自检：把「配置能加载但语义危险」的组合在启动时暴露，
// 而不是等到某次热榜查询或重算作业才失败。返回 error 时 NewServiceContext 直接终止启动。
//
// 这里刻意不 import model：config 只依赖 go-zero，保持「配置层 → 领域层」单向依赖；
// 但边界值必须与 model 的硬约束一致（改动任一侧都要同步另一侧，注释已点名对应常量）。
func (c Config) Validate() error {
	if c.DataSource == "" {
		return errors.New("DataSource 不能为空：spm 是唯一事实表的持有者，连库都起不来时" +
			"所有指标读接口都会返回空值，那会被调用方读成「这段时间没有数据」")
	}
	s := c.Spm
	if s.MaxPageSize < s.PageSize || s.MaxPageSize > hardListLimit {
		return errors.New("Spm.MaxPageSize 必须落在 [PageSize, 100]：契约声明 ps 上限 100，" +
			"配到 100 以上时 model 会静默 clamp，调用方拿到的 total 与实际行数就会分叉")
	}
	if s.MaxMetricKeysPerRequest <= 0 || s.MaxMetricKeysPerRequest > hardMetricKeysPerRequest {
		return errors.New("Spm.MaxMetricKeysPerRequest 必须落在 (0, 50]（BatchGetMetrics 契约上限）")
	}
	if s.MaxWindowCount <= 0 || s.MaxWindowCount > hardWindowCount {
		return errors.New("Spm.MaxWindowCount 必须落在 (0, 30]（契约 window_count 上限）")
	}
	if s.MaxWindowsPerJob <= 0 || s.MaxWindowsPerJob > hardWindowsPerJob {
		return errors.New("Spm.MaxWindowsPerJob 必须落在 (0, 100000]：" +
			"windows_total 是 INT 列，且作业按窗口逐个推进，超过这个跨度就该由调用方拆分提交")
	}
	if s.MaxWritePoints <= 0 || s.MaxWritePoints > hardWritePoints {
		return errors.New("Spm.MaxWritePoints 必须落在 (0, 500]（model.maxMetricPointsPerBatch），" +
			"再大的单批写入会把一次窗口重算变成长事务")
	}
	if s.MaxRetentionDay <= 0 || s.MaxRetentionDay > hardRetentionDay {
		return errors.New("Spm.MaxRetentionDay 必须落在 (0, 90]（GetRetention 契约 max_day 上限）")
	}
	if int64(s.MaxRetentionDay) > int64(s.BehaviorRetentionDays) {
		return errors.New("Spm.MaxRetentionDay 不能大于 Spm.BehaviorRetentionDays：" +
			"D90 留存曲线要能重算，就必须保留 90 天前的活跃事实，" +
			"否则新 cohort 走到后半程就会因事实被删而静默少样本")
	}
	if s.InterestTopN <= 0 || s.MaxInterestTopN <= 0 || s.MaxInterestTopN > hardInterestRowsPerMid ||
		s.InterestTopN > s.MaxInterestTopN {
		return errors.New("Spm.InterestTopN 必须 <= MaxInterestTopN 且 MaxInterestTopN <= 100" +
			"（每个 mid 的画像行数上限，超出部分在写侧就被裁掉，读侧不可能返回更多）")
	}
	if s.RealtimeWindowType != realtimeWindow5Min && s.RealtimeWindowType != realtimeWindowHour {
		return errors.New("Spm.RealtimeWindowType 只能是 1（5 分钟）或 2（1 小时）：" +
			"day/week/total 由离线回填作业推进，实时聚合器没有闭合它们的时机")
	}
	if s.LateToleranceSeconds < 0 || s.LateToleranceSeconds >= windowSecondsOf(s.RealtimeWindowType) {
		return errors.New("Spm.LateToleranceSeconds 必须 >= 0 且小于实时窗口长度：" +
			"容忍窗比窗口本身还长时，每个已闭合窗口都会被反复改写，热榜就成了随机数")
	}
	if s.InterestStaleAfterSeconds <= 0 || s.WatermarkLagSeconds <= 0 {
		return errors.New("Spm.InterestStaleAfterSeconds 与 WatermarkLagSeconds 必须 > 0：" +
			"取 0 等于「任何画像都不算过期、任何停摆都不算落后」，可观测面就废了")
	}
	if s.BehaviorRetentionDays <= 0 {
		return errors.New("Spm.BehaviorRetentionDays 必须 > 0：行为事实必须有留存上限，" +
			"0 表示永不清理，只允许在一次性调试实例上手工绕过")
	}
	if s.ProjectionRetentionDays < s.BehaviorRetentionDays {
		return errors.New("Spm.ProjectionRetentionDays 不能小于 BehaviorRetentionDays：" +
			"投影必须比事实活得久，否则口径变更后想解释历史，连旧投影都已一起被删")
	}
	if s.DeleteBatchSize <= 0 || s.DeleteBatchSize > hardDeleteBatch {
		return errors.New("Spm.DeleteBatchSize 必须落在 (0, 10000]（model.maxBatch）：" +
			"清理必须分批，单批过大会长时间持锁并放大 binlog")
	}
	if s.JobLeaseSeconds <= 0 || s.JobMaxRetry < 0 {
		return errors.New("Spm.JobLeaseSeconds 必须 > 0 且 JobMaxRetry 不能为负：" +
			"没有租约就无法判断作业是否被抢占，重试无上限则会无限重放")
	}
	if s.MaxRetryIntervalSeconds <= 0 {
		return errors.New("Spm.MaxRetryIntervalSeconds 必须 > 0，否则退化成密集重试打死下游")
	}
	if s.DeadLetterPreviewBytes <= 0 {
		return errors.New("Spm.DeadLetterPreviewBytes 必须 > 0：预览为 0 时死信只剩摘要，无法定位消息")
	}
	if s.ReadQps <= 0 || s.WriteQps <= 0 || s.WriteBurst <= 0 {
		return errors.New("Spm.ReadQps/WriteQps/WriteBurst 必须 > 0，否则启动即自锁（桶容量为 0）")
	}
	return nil
}

// 与 model 硬约束一一对应的上界，写在这里是为了让 Validate 的错误文案自解释，
// 也避免 config 反向依赖 model 包。改动任何一侧都必须同步另一侧。
const (
	hardListLimit             int32 = 100    // model.maxListLimit
	hardMetricKeysPerRequest  int32 = 50     // model.maxMetricKeysPerRequest
	hardWindowCount           int32 = 30     // BatchGetMetrics.window_count 契约上限
	hardWindowsPerJob         int32 = 100000 // spm_aggregation_job.windows_total 的跨度上限
	hardWritePoints           int32 = 500    // model.maxMetricPointsPerBatch
	hardRetentionDay          int32 = 90     // model.maxRetentionDay
	hardInterestRowsPerMid    int32 = 100    // model.maxInterestRowsPerMid
	hardDeleteBatch           int32 = 10000  // model.maxBatch
	realtimeWindow5Min        int32 = 1      // model.WindowType5Min
	realtimeWindowHour        int32 = 2      // model.WindowTypeHour
	realtimeWindow5MinSeconds int64 = 300    // model.WindowSeconds(WindowType5Min)
	realtimeWindowHourSeconds int64 = 3600   // model.WindowSeconds(WindowTypeHour)
)

// windowSecondsOf 返回实时窗口的秒数。只支持两个实时档位，
// 其余粒度由 Validate 在更早的位置拒绝。
func windowSecondsOf(windowType int32) int64 {
	switch windowType {
	case realtimeWindow5Min:
		return realtimeWindow5MinSeconds
	case realtimeWindowHour:
		return realtimeWindowHourSeconds
	default:
		return 0
	}
}
