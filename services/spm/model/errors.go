package model

import "errors"

// 本服务的错误哨兵。logic 层据此向调用方返回可枚举的失败原因，
// 不得把 SQL 片段、事件原文或标识符明文拼进错误信息（AGENTS.md §6）。
var (
	// ErrNotImplemented 表示契约已定、业务实现留给逻辑轮。
	// 本轮所有 logic 方法都返回它：绝不返回伪造的成功响应（AGENTS.md §9）。
	ErrNotImplemented = errors.New("spm: not implemented")

	// ErrInvalidSubject 主体类型或主体主键非法。
	ErrInvalidSubject = errors.New("spm: invalid subject_type or subject_id")
	// ErrInvalidWindow 窗口粒度或窗口边界非法。
	ErrInvalidWindow = errors.New("spm: invalid window_type or window_start")
	// ErrEventTimeRequired 投影写入必须给出推进时刻（口径版本/乱序守卫的基准），
	// 缺省让迟到判定与画像过期观测都失去依据。
	ErrEventTimeRequired = errors.New("spm: event_time is required")
	// ErrMetricKeyEmpty 指标键为空。
	ErrMetricKeyEmpty = errors.New("spm: metric_key is required")
	// ErrMetricVersionRequired 修复/重算链路必须显式指定口径版本，
	// 否则会拿当前 ACTIVE 口径去改写历史窗口，事后无人能解释数据。
	ErrMetricVersionRequired = errors.New("spm: metric_version must be explicit")
	// ErrMetricVersionImmutable 已登记的口径版本不允许原地改写（只能新增版本）。
	ErrMetricVersionImmutable = errors.New("spm: metric_version is immutable, register a new version instead")
	// ErrMetricDefinitionNotFound 口径未登记，读接口按 found=false 返回而不是伪造口径。
	ErrMetricDefinitionNotFound = errors.New("spm: metric definition not found")
	// ErrMetricNotActive 口径处于 DRAFT/RETIRED，不接受写入也不对外可读。
	ErrMetricNotActive = errors.New("spm: metric definition is not active")
	// ErrMultipleActiveDefinition 同一 metric_key 存在多个 ACTIVE 版本，
	// 会让 metric_version=0 的读请求语义二义，必须先退役多余版本。
	ErrMultipleActiveDefinition = errors.New("spm: more than one active definition for the same metric_key")
	// ErrInvalidDefinitionState 口径状态迁移非法（含 RETIRED 复活）。
	ErrInvalidDefinitionState = errors.New("spm: invalid metric definition state transition")
	// ErrInvalidDefinitionSpec 口径说明本身不合法：unit 不在受控集合、name/formula 缺失、
	// source_event_types 落在事件白名单之外。这类错误必须在登记前拒绝——
	// 口径一旦登记就不可改写，写进去的坏说明会永久污染「这个数是怎么算出来的」。
	ErrInvalidDefinitionSpec = errors.New("spm: invalid metric definition spec")
	// ErrInvalidMetricSource 指标写入来源不在计算链路白名单内（拒绝人工覆盖）。
	ErrInvalidMetricSource = errors.New("spm: metric source must be a computation pipeline")
	// ErrTooManyPoints 单次写入的指标行数超过上限。
	ErrTooManyPoints = errors.New("spm: points count exceeds the batch limit")
	// ErrNegativeMetricPoint 指标行的计数列为负：计数/分子/分母/样本数都不可能是负数，
	// 负值只能来自上游算错或重复回写，写进去会永久污染榜与留存曲线。
	ErrNegativeMetricPoint = errors.New("spm: metric counters must not be negative")
	// ErrDuplicatePointInBatch 同一批里出现两条同自然键的指标行：
	// 交给 MySQL 只会「后写覆盖先写」并返回一个骗人的行数，必须让调用方先去重。
	ErrDuplicatePointInBatch = errors.New("spm: duplicate metric point in the same batch")
	// ErrTooManyKeys 单次批量读取的口径数超过上限。
	ErrTooManyKeys = errors.New("spm: metric keys exceed the batch limit")
	// ErrWindowRangeTooLarge 请求的窗口跨度超过单次上限（避免无界扫描）。
	ErrWindowRangeTooLarge = errors.New("spm: window range exceeds the per-request limit")
	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("spm: ps exceeds server limit")
	// ErrOperatorRequired 口径与作业变更必须留痕。
	ErrOperatorRequired = errors.New("spm: operator is required")
	// ErrReasonRequired 状态变更与作业提交必须给出理由（审计与复盘需要）。
	ErrReasonRequired = errors.New("spm: reason is required")
	// ErrRequestIdRequired 写接口缺少幂等键。
	ErrRequestIdRequired = errors.New("spm: request_id is required")
	// ErrRequestIdConflict 同一个 request_id 被复用到内容不同的请求上。
	// 幂等键的语义是「同一份请求的重复投递」，一旦允许同键不同内容，
	// 重放返回的首次结果就和调用方这次真正想做的事无关。
	ErrRequestIdConflict = errors.New("spm: request_id already used for a different request")
	// ErrRateLimited 进程级令牌桶无剩余令牌（读侧保护投影表，写侧保护计算链路）。
	// 必须是显式错误而不是「静默变慢」：调用方（聚合器/cron）依据它决定退避重试，
	// 返回空数据会被读成「这段时间没有指标」。
	ErrRateLimited = errors.New("spm: rate limit exceeded")

	// ErrJobNotFound 作业不存在（按 job_id / request_id 单查时 found=false，不报错）。
	ErrJobNotFound = errors.New("spm: aggregation job not found")
	// ErrInvalidJobType 作业类型非法。
	ErrInvalidJobType = errors.New("spm: invalid job type")
	// ErrJobAlreadyTerminal 作业已是终态，重复推进不改变状态（幂等）。
	ErrJobAlreadyTerminal = errors.New("spm: job already in terminal state")
	// ErrInvalidJobState 作业目标状态不是终态（MarkFinished 只接受 SUCCEEDED/FAILED/CANCELLED）。
	ErrInvalidJobState = errors.New("spm: invalid target job state")
	// ErrNegativeProgress 进度增量必须非负：负增量会让「已完成窗口数」倒退，
	// 使重算进度无法判定，也是重复累加被掩盖的信号。
	ErrNegativeProgress = errors.New("spm: progress deltas must be non-negative")

	// ErrInvalidMid mid 非法。
	ErrInvalidMid = errors.New("spm: invalid mid")
	// ErrInvalidInterestKey 兴趣键不在受控词表内（空键、未知前缀、非主键 ID、自由文本）：
	// 兴趣键必须能被解释成「某个跨服务主键」，否则画像既无法复核也无法删除。
	ErrInvalidInterestKey = errors.New("spm: interest_key is not a controlled key")
	// ErrTopNTooLarge 兴趣画像 top_n 超过上限。
	ErrTopNTooLarge = errors.New("spm: top_n exceeds server limit")
	// ErrInterestWeightOutOfRange 兴趣权重越界：单条必须在 [0,1]，整组和不得大于 1。
	// 归一化没做就落库，画像读取方按权重排序时得到的是一串没有上界的数字，
	// 「有多喜欢」就再也解释不了了。
	ErrInterestWeightOutOfRange = errors.New("spm: interest weight out of range")
	// ErrInvalidCohort 留存分桶参数非法。
	ErrInvalidCohort = errors.New("spm: invalid cohort_type or cohort_date")
	// ErrMaxDayTooLarge 留存天数超过支持区间。
	ErrMaxDayTooLarge = errors.New("spm: max_day exceeds 90")

	// ErrEventIDEmpty 事件 ID 为空，无法领取处理权。
	ErrEventIDEmpty = errors.New("spm: event_id is empty")
	// ErrConflictProcessing 同一事件正被其它处理器执行（processing 未过期）。
	ErrConflictProcessing = errors.New("spm: event is being processed by another consumer")
	// ErrUnsupportedEventType 事件类型不在白名单内（消费者按契约跳过，不算故障）。
	ErrUnsupportedEventType = errors.New("spm: unsupported event_type")
	// ErrInvalidActionKey 归一化动作键为空：事实行必须带受控动作键，否则口径无法重算。
	ErrInvalidActionKey = errors.New("spm: action_key is required")
	// ErrInvalidSourceChannel 事件来源通道非法（未知通道的事件不允许进事实表）。
	ErrInvalidSourceChannel = errors.New("spm: invalid event source_channel")
	// ErrEmptyPayload 投递内容为空。
	ErrEmptyPayload = errors.New("spm: empty payload")

	// ErrInvalidDeadLetterState 死信状态非法（含把已处理项改回 open 之外的未知值）。
	ErrInvalidDeadLetterState = errors.New("spm: invalid dead letter state")
	// ErrInvalidConsumerState 消费状态机取值非法。
	ErrInvalidConsumerState = errors.New("spm: invalid consumer state")
)
