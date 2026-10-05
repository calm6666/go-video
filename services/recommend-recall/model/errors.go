package model

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

// ErrNotImplemented 表示本服务的业务能力尚未落地（契约轮占位）。
//
// logic 层的所有方法在本轮都返回该哨兵，禁止返回"看起来成功"的空响应；
// 调用方（gateway/app）据此走降级链路，而不是把空候选当成"该用户没有可推内容"。
// 参考 services/account/internal/repository/repository.go 的同名哨兵约定。
var ErrNotImplemented = errors.New("recommend-recall: not implemented")

// 入参与规模校验错误（在线面必须有上限，超限直接拒绝而不是静默裁剪）。
var (
	// ErrInvalidSource 召回路不在 Source 枚举内。
	ErrInvalidSource = errors.New("recommend-recall: invalid recall source")
	// ErrInvalidPoolKey pool_key 与 source 的语法不匹配（见 ValidatePoolKey）。
	ErrInvalidPoolKey = errors.New("recommend-recall: invalid pool key for this source")
	// ErrInvalidAid 稿件 ID 非正数（候选必须是 video 服务的合法主键）。
	ErrInvalidAid = errors.New("recommend-recall: invalid aid")
	// ErrPoolKeyTooLong pool_key 超长。
	ErrPoolKeyTooLong = errors.New("recommend-recall: pool key too long")
	// ErrLimitTooLarge 请求条数超过服务硬上限。
	ErrLimitTooLarge = errors.New("recommend-recall: limit exceeds max candidates")
	// ErrTooManySeeds 种子（aid/tag）数量超过上限。
	ErrTooManySeeds = errors.New("recommend-recall: too many seeds")
	// ErrTooManyItems 单次写入条数超过上限。
	ErrTooManyItems = errors.New("recommend-recall: too many items in one batch")
	// ErrPageTooDeep 分页偏移过深（日志与快照读都是运维路径，不允许任意深翻）。
	ErrPageTooDeep = errors.New("recommend-recall: page too deep")
	// ErrRawDeviceID 设备标识必须是 sha256 摘要，拒绝明文设备号。
	ErrRawDeviceID = errors.New("recommend-recall: device id must be a sha256 digest")
	// ErrIdempotencyKeyRequired 写接口缺少幂等键。
	ErrIdempotencyKeyRequired = errors.New("recommend-recall: idempotency key required")
	// ErrOperatorRequired 写接口缺少操作者。
	ErrOperatorRequired = errors.New("recommend-recall: operator required")
	// ErrReasonRequired 切换/回滚类操作缺少原因（审计要求）。
	ErrReasonRequired = errors.New("recommend-recall: reason required")
	// ErrInvalidLimit 查询/写入条数不是正数。模型层宁缺不空：
	// limit<=0 过去被静默当成"返回空集"，会让降级判定误读为"池是空的"。
	ErrInvalidLimit = errors.New("recommend-recall: limit must be positive")
	// ErrTooManyPools 一次查询请求的池数量超过上限（在线召回逐池取数，池数必须有界）。
	ErrTooManyPools = errors.New("recommend-recall: too many pools in one query")
	// ErrKeepVersionsTooSmall 清理保留窗口非正数（会把 CURRENT 一并纳入删除候选）。
	ErrKeepVersionsTooSmall = errors.New("recommend-recall: keep_versions must be positive")
	// ErrRequestRequired 请求体为空（gRPC 正常路径不会传 nil，出现即调用方缺陷，
	// 与"参数合法但没有数据"严格区分）。
	ErrRequestRequired = errors.New("recommend-recall: request is required")
	// ErrFieldTooLong 文本字段超出迁移文件列宽（scene/app_version/region/operator/batch_id 等）。
	// 这里的策略是拒绝而不是截断落库：审计字段被截断后按值分组会统计错（AGENTS.md §9）。
	ErrFieldTooLong = errors.New("recommend-recall: field exceeds column width")
)

// 池与版本状态错误。
var (
	// ErrPoolNotFound 池没有任何版本登记。
	ErrPoolNotFound = errors.New("recommend-recall: pool not found")
	// ErrVersionNotFound 指定 (source, pool_key, version) 不存在。
	ErrVersionNotFound = errors.New("recommend-recall: pool version not found")
	// ErrInvalidVersion 版本号非正数。
	ErrInvalidVersion = errors.New("recommend-recall: invalid pool version")
	// ErrInvalidVersionState 版本状态不在枚举内。
	ErrInvalidVersionState = errors.New("recommend-recall: invalid pool version state")
	// ErrBatchIDRequired 写入池条目缺少生成批次号（可追溯性前提）。
	ErrBatchIDRequired = errors.New("recommend-recall: batch id required")
	// ErrVersionNotReady 只有 READY 版本可被发布为 CURRENT。
	ErrVersionNotReady = errors.New("recommend-recall: pool version is not ready")
	// ErrVersionReuseBlocked 同一 (source, pool_key, version) 已被另一个 batch_id 登记
	// （批次串写保护）。由 RecallPoolVersionModel.Register 返回。
	ErrVersionReuseBlocked = errors.New("recommend-recall: version already bound to another batch")
	// ErrBatchMismatch 写入批次与版本登记的批次不一致：调用方拿 (version, batch_id) 组合
	// 与登记表核对失败。由 repository 的批次核对路径返回（见 RecallPoolVersionModel.FindByBatch）。
	ErrBatchMismatch = errors.New("recommend-recall: batch id mismatch with version registration")
	// ErrSwitchConflict 版本指针的条件 UPDATE 未命中（RowsAffected=0）：
	// 指针已被并发切换，或 expect_version 与实际 CURRENT 不符。调用方必须重读后重试，
	// 绝不能把该错误当成"切换成功"。
	ErrSwitchConflict = errors.New("recommend-recall: pool current pointer changed concurrently")
	// ErrRollbackTargetInvalid 回滚目标版本不存在或从未完成写入。
	ErrRollbackTargetInvalid = errors.New("recommend-recall: rollback target version is not switchable")
	// ErrVersionImmutable 目标版本已封版（READY/CURRENT/RETIRED）或已失败，条目不可再改写。
	// 这是"已发布版本永不原地修改"这条核心不变量的落点：在线池的可见内容只能整批替换
	// （写新版本 + PublishPoolVersion 切指针），不允许原地增删条目。
	ErrVersionImmutable = errors.New("recommend-recall: pool version is immutable once sealed")
	// ErrVersionStateConflict 版本状态的条件更新未命中（RowsAffected=0）：
	// 状态已被并发推进（例如另一个请求把 BUILDING 置成了 READY/FAILED）。
	// 调用方必须重读后决策，不能当成"已经是我想要的状态"。
	ErrVersionStateConflict = errors.New("recommend-recall: pool version state changed concurrently")
	// ErrVersionNoItems 版本条目数为 0 却被发布/封版：
	// 空池上线会让在线召回把"池没内容"和"池没上线"混成一回事（降级原因会报错）。
	ErrVersionNoItems = errors.New("recommend-recall: pool version has no items")
	// ErrDuplicateItem 同一批写入里出现重复 aid。
	// 取最后一条或静默去重都会让"生成方声明的条数"与实到条数对不上，故整批拒绝。
	ErrDuplicateItem = errors.New("recommend-recall: duplicated aid in one batch")
	// ErrItemsRequired 写入请求没带任何条目。
	// 与 ErrVersionNoItems 区分：前者是调用方参数缺陷（空批次根本不该发），
	// 后者是"版本登记存在但一条都没写进去"的数据状态。
	ErrItemsRequired = errors.New("recommend-recall: at least one pool item is required")
	// ErrInvalidScore 候选分数不是有限实数（NaN/±Inf）。
	// MySQL 的 DOUBLE 列拒绝 NaN，驱动层报错文本不含定位信息；在入口拦掉才能给出可修的错。
	ErrInvalidScore = errors.New("recommend-recall: candidate score must be a finite number")
	// ErrSchemaVersionUnsupported 写入声明的条目结构版本与服务当前版本不一致。
	// 接受它等于允许写进在线读不懂的池：宁可在写入侧拒绝，也不让在线侧读到半可解的快照。
	ErrSchemaVersionUnsupported = errors.New("recommend-recall: pool item schema version is not supported")
	// ErrSourceDisabled 该召回路未在服务配置中启用。
	ErrSourceDisabled = errors.New("recommend-recall: recall source is disabled")
	// ErrAllSourcesEmpty 所有请求路都没有候选（调用方据此决定端到端降级）。
	ErrAllSourcesEmpty = errors.New("recommend-recall: all requested sources are empty")
	// ErrDegradationDisabled allow_degrade=false 且依赖故障时返回该错误而不是降级结果。
	ErrDegradationDisabled = errors.New("recommend-recall: degradation disabled by request")
	// ErrRequestNotFound 召回请求日志不存在。
	ErrRequestNotFound = errors.New("recommend-recall: recall request log not found")
	// ErrRequestLogExists 同一 request_id/snapshot_id 的审计行已存在（幂等命中，调用方应回放旧结果）。
	ErrRequestLogExists = errors.New("recommend-recall: recall request log already exists")
	// ErrRequestLogRequired 写审计行缺少 request_id 或 snapshot_id。
	// 与 ErrRequestNotFound 严格区分：前者是调用方参数缺陷（必须修代码），
	// 后者是查询未命中（正常业务分支，回复 entry=null）。
	ErrRequestLogRequired = errors.New("recommend-recall: request_id and snapshot_id are required")
	// ErrInvalidDegradeReason 降级原因 key 不在受控枚举内。
	ErrInvalidDegradeReason = errors.New("recommend-recall: invalid degrade reason")
	// ErrInvalidPlatform 客户端平台不在 Platform 枚举内（1..4，不支持小程序）。
	ErrInvalidPlatform = errors.New("recommend-recall: invalid client platform")
)

// 写接口幂等（recall_idempotency）相关错误。
var (
	// ErrIdempotencyExists 同一 (scope, idempotency_key) 已在执行中（PENDING）。
	// 调用方应等待或回放，不得启动第二次执行（AGENTS.md §5 写接口幂等）。
	ErrIdempotencyExists = errors.New("recommend-recall: idempotency claim already held")
	// ErrIdempotencyFingerprintMismatch 同一幂等键带来了不同的请求指纹：
	// 客户端复用键但改了参数，属于调用方缺陷，直接拒绝而不是执行第二份语义。
	ErrIdempotencyFingerprintMismatch = errors.New("recommend-recall: idempotency key reused with a different request")
	// ErrIdempotencyStateInvalid 幂等记录状态不在 pending/succeeded/failed 内。
	ErrIdempotencyStateInvalid = errors.New("recommend-recall: invalid idempotency state")
	// ErrIdempotencyKeyTooLong 幂等键超长（列宽 VARCHAR(128)）。
	ErrIdempotencyKeyTooLong = errors.New("recommend-recall: idempotency key too long")
)

// 事件与 Outbox 错误。
var (
	// ErrEventRequired 事件行缺少 event_id 或 event_type。
	ErrEventRequired = errors.New("recommend-recall: event id and type required")
	// ErrEventExists event_id 唯一索引冲突，表示事件已登记（生产者重放的幂等命中）。
	ErrEventExists = errors.New("recommend-recall: duplicate event id")
)

// 客户端平台（与 rpc Platform 编号一一对应；AGENTS.md §1 不支持小程序，故没有 5 及以上取值）。
// 一致性同样由 internal/logic/contract_consistency_test.go 断言。
const (
	// PlatformAndroid Android 端。
	PlatformAndroid int32 = 1
	// PlatformIOS iOS 端。
	PlatformIOS int32 = 2
	// PlatformHarmony HarmonyOS 端。
	PlatformHarmony int32 = 3
	// PlatformDesktop 电脑客户端。
	PlatformDesktop int32 = 4
)

// ValidPlatform 判定平台编号是否在枚举内（0 表示未指定，由各 logic 自行决定默认值）。
func ValidPlatform(platform int32) bool {
	return platform >= PlatformAndroid && platform <= PlatformDesktop
}

// 召回路（与 rpc/recommendrecall.proto 的 Source 编号一一对应，禁止漂移）。
// 一致性由 internal/logic/contract_consistency_test.go 断言。
const (
	// SourceHot 热门池。
	SourceHot int32 = 1
	// SourceFollow 关注池。
	SourceFollow int32 = 2
	// SourceTag 标签/分区池。
	SourceTag int32 = 3
	// SourceCollab 协同过滤候选（i2i）。
	SourceCollab int32 = 4
	// SourceVector 向量近邻候选。
	SourceVector int32 = 5
	// SourceCold 冷启动池。
	SourceCold int32 = 6
)

// ValidSource 判定召回路是否在枚举内。
func ValidSource(source int32) bool {
	return source >= SourceHot && source <= SourceCold
}

// SourcePriority 返回去重合并时的来源优先级（数值越小越优先）。
// 关注 > 协同 > 向量 > 标签 > 冷启动 > 热门：越"个性化"的路口越优先保留来源标记，
// 热门池作为最大公约数兜底放最后，避免候选来源被泛化路覆盖。
func SourcePriority(source int32) int {
	switch source {
	case SourceFollow:
		return 1
	case SourceCollab:
		return 2
	case SourceVector:
		return 3
	case SourceTag:
		return 4
	case SourceCold:
		return 5
	case SourceHot:
		return 6
	default:
		return 99
	}
}

// 池版本状态（与 rpc PoolVersionState 对应）。
const (
	// VersionStateBuilding 写入中，在线不可读。
	VersionStateBuilding int32 = 1
	// VersionStateReady 写入完成并通过校验，等待切换。
	VersionStateReady int32 = 2
	// VersionStateCurrent 当前生效版本（每个池至多一行）。
	VersionStateCurrent int32 = 3
	// VersionStateRetired 已退役，保留供回滚与回放。
	VersionStateRetired int32 = 4
	// VersionStateFailed 生成失败，永不在线出数。
	VersionStateFailed int32 = 5
)

// ValidVersionState 判定版本状态是否在枚举内。
func ValidVersionState(state int32) bool {
	return state >= VersionStateBuilding && state <= VersionStateFailed
}

// CanPublishState 判定版本是否可被发布为 CURRENT：
// READY（新批次正常上线）与 RETIRED（回滚到历史版本）允许，其余一律拒绝。
func CanPublishState(state int32) bool {
	return state == VersionStateReady || state == VersionStateRetired
}

// 降级原因（落库与日志使用稳定 key，rpc 侧对应 DegradeReason 枚举）。
const (
	// DegradeReasonNone 未降级。
	DegradeReasonNone = ""
	// DegradeReasonPoolNotReady 目标池没有可用的 CURRENT 版本。
	DegradeReasonPoolNotReady = "pool_not_ready"
	// DegradeReasonFeatureUnavailable 特征/行为下游不可读（spm、feature-store）。
	DegradeReasonFeatureUnavailable = "feature_unavailable"
	// DegradeReasonDownstreamTimeout 下游 RPC 超时或被熔断。
	DegradeReasonDownstreamTimeout = "downstream_timeout"
	// DegradeReasonStoreUnavailable Redis/MySQL 不可用。
	DegradeReasonStoreUnavailable = "store_unavailable"
	// DegradeReasonBudgetExhausted 内部时间预算耗尽，裁剪剩余召回路。
	DegradeReasonBudgetExhausted = "budget_exhausted"
	// DegradeReasonColdStart 冷启动走保底池（非故障，但必须显式声明）。
	DegradeReasonColdStart = "cold_start"
	// DegradeReasonAllSourcesEmpty 所有路都没有候选。
	DegradeReasonAllSourcesEmpty = "all_sources_empty"
)

// ValidDegradeReason 判定降级原因 key 是否受控。
func ValidDegradeReason(reason string) bool {
	switch reason {
	case DegradeReasonNone, DegradeReasonPoolNotReady, DegradeReasonFeatureUnavailable,
		DegradeReasonDownstreamTimeout, DegradeReasonStoreUnavailable,
		DegradeReasonBudgetExhausted, DegradeReasonColdStart, DegradeReasonAllSourcesEmpty:
		return true
	default:
		return false
	}
}

// 事件 Outbox 状态（与 search-query 等服务的 outbox 语义一致）。
const (
	// OutboxStatePending 待发布。
	OutboxStatePending int32 = 0
	// OutboxStateSent 已发布。
	OutboxStateSent int32 = 1
	// OutboxStateFailed 超过重试上限，进入死信等待人工处理。
	OutboxStateFailed int32 = 2
)

// TopicPoolPublished 池版本切换事件 topic（AGENTS.md §7：只描述池快照变化，无商业化语义）。
const TopicPoolPublished = "recall.pool.published.v1"

// 事件字面量。三者必须自洽：TopicPoolPublished == eventenvelope.Topic(EventPoolPublished, PoolVersionSchemaVersion)，
// 因为投递 topic 是由 event_type + schema_version 拼出来的，事件行只存 event_type。
const (
	// ProducerName 事件生产者标识（与 docs/api-and-events.md 的服务名一致）。
	ProducerName = "recommend-recall"
	// EventPoolPublished 池版本切换（含回滚）事件的 event_type 列值。
	EventPoolPublished = "recall.pool.published"
	// AggregateTypePool 事件聚合根类型：一个召回池。
	AggregateTypePool = "recall_pool"
)

// PoolVersionSchemaVersion 当前池条目结构版本，写入 recall_pool_version.schema_version。
const PoolVersionSchemaVersion = 1

// MaxPoolKeyLen pool_key 最大长度（与迁移 recall_pool.pool_key 列宽 VARCHAR(128) 对齐）。
const MaxPoolKeyLen = 128

// 模型层硬上限：etc yaml 里的业务上限（config.Recall.*）决定"正常请求给多少"，
// 这里的常量决定"数据库这一层最多允许什么"。两者是两道独立闸门，
// 目的是即使配置写错或被绕过，也不出现无 LIMIT 的全表扫描/无界 DELETE。
const (
	// MaxPoolItemBatch 单次 BatchUpsert 写入的条目条数上限（对应 rpc MaxBatchItems）。
	MaxPoolItemBatch = 2000
	// MaxPoolQueryLimit recall_pool 单次读取条数上限（在线取数与快照分页共用）。
	MaxPoolQueryLimit = 5000
	// MaxVersionListLimit recall_pool_version 单次列取上限（对应 rpc MaxVersionList）。
	MaxVersionListLimit = 500
	// MaxRequestLogPageSize recall_request_log 单页上限（对应 rpc MaxRequestLogPage）。
	MaxRequestLogPageSize = 500
	// MaxRequestLogOffset 审计日志允许的最深偏移（超过就该改用时间窗口过滤）。
	MaxRequestLogOffset = 10000
	// MaxPoolSnapshotOffset 池快照允许的最深偏移（运维路径，同上）。
	MaxPoolSnapshotOffset = 100000
	// MaxPoolRefsPerQuery 一次批量查询涉及的池数量上限（在线召回最多 6 路 x 少量分区）。
	MaxPoolRefsPerQuery = 200
	// MaxOutboxBatch 单次 Outbox 拉取/标记行数上限。
	MaxOutboxBatch = 1000
	// MaxDeleteRows 单条 DELETE 语句的行数上限（锁与主从延迟保护，对应 rpc max_rows）。
	MaxDeleteRows = 5000
	// MaxIdempotencyKeyLen 幂等键最大长度（与迁移 recall_idempotency.idempotency_key 列宽一致）。
	MaxIdempotencyKeyLen = 128
	// MaxIdempotencyPayloadLen 幂等回放载荷最大字节数（超过则不缓存结果，重放时重新执行）。
	MaxIdempotencyPayloadLen = 4096
	// RequestHashLen 请求指纹长度：sha256 的小写 hex（64 字符）。
	RequestHashLen = 64
)

// CheckLimit 校验读取条数是否落在 (0, max] 内。
// 所有列表查询都必须经它，避免出现无 LIMIT 的语句或对 0 的隐式解释。
func CheckLimit(limit, max int) error {
	if limit <= 0 {
		return ErrInvalidLimit
	}
	if limit > max {
		return ErrLimitTooLarge
	}
	return nil
}

// CheckInt64Limit 校验以 int64 表达的条数上限（DELETE ... LIMIT ?）。
func CheckInt64Limit(limit, max int64) error {
	if limit <= 0 {
		return ErrInvalidLimit
	}
	if limit > max {
		return ErrLimitTooLarge
	}
	return nil
}

// 表的事实源 / 投影定位（AGENTS.md §5：只有事实源需要唯一约束，投影允许从事实重算）：
//
//	recall_pool            投影。可从生成批次（batch_id + generator）整批重算，
//	                       删除任何版本都不丢业务事实；回滚只切指针。
//	recall_pool_version    半事实：登记"某批次产出了哪个版本、多少条、状态如何"，
//	                       是候选可追溯性的证据链，不随条目重算而丢失，不可删。
//	recall_pool_current    控制位，不是业务事实：在线读的唯一权威指针（每池一行）。
//	                       它与 recall_pool_version.state 在同一事务内一起写，二者互为镜像——
//	                       读路径只认指针，state=CURRENT 那一行是供人排查的冗余视图，
//	                       必要时可由指针重建，因此两边都不需要事务外备份。
//	recall_request_log     事实（审计证据）。一次在线召回读了哪些版本不可再生，保留期内不得改写。
//	recall_outbox          事实（事件产生证据）。投递状态可变，事件本体不可改写。
//	recall_idempotency     控制位。仅用于短期防重放，可按 expire_at 清理，
//	                       丢失只会让重复请求再执行一次（条目写入本身仍是幂等 upsert）。

// ValidatePoolKey 校验 (source, pool_key) 组合是否落在受控语法内。
//
// 语法与 rpc/recommendrecall.proto 的 PoolRef 注释同源，不接受调用方自由拼接的字符串键，
// 否则同一个池会出现两个互不可见的键名（本仓 search-query 字段词汇漂移的教训）：
//
//	SOURCE_HOT    -> "global" | "zone:<typeid>"
//	SOURCE_FOLLOW -> "mid:<mid>"
//	SOURCE_TAG    -> "tag:<tag_id>"
//	SOURCE_COLLAB -> "aid:<seed_aid>"
//	SOURCE_VECTOR -> "mid:<mid>"
//	SOURCE_COLD   -> "global" | "platform:<Platform 编号>"
func ValidatePoolKey(source int32, key string) error {
	if len(key) == 0 {
		return ErrInvalidPoolKey
	}
	if len(key) > MaxPoolKeyLen {
		return ErrPoolKeyTooLong
	}
	if !ValidSource(source) {
		return ErrInvalidSource
	}
	switch source {
	case SourceHot, SourceCold:
		if key == "global" {
			return nil
		}
		prefix := "zone:"
		if source == SourceCold {
			prefix = "platform:"
		}
		if len(key) <= len(prefix) {
			return ErrInvalidPoolKey
		}
		if !strings.HasPrefix(key, prefix) || !isPositiveDecimal(key[len(prefix):]) {
			return ErrInvalidPoolKey
		}
		if source == SourceCold {
			// 平台编号必须落在 Platform 枚举 1..4（Android/iOS/HarmonyOS/桌面端）。
			n, err := strconv.Atoi(key[len(prefix):])
			if err != nil || !ValidPlatform(int32(n)) {
				return ErrInvalidPoolKey
			}
		}
		return nil
	case SourceFollow, SourceVector:
		return validatePrefixedInt(key, "mid:")
	case SourceTag:
		return validatePrefixedInt(key, "tag:")
	case SourceCollab:
		return validatePrefixedInt(key, "aid:")
	default:
		return ErrInvalidSource
	}
}

// validatePrefixedInt 校验 "<prefix><正十进制整数>" 形式。
func validatePrefixedInt(key, prefix string) error {
	if len(key) <= len(prefix) || !strings.HasPrefix(key, prefix) {
		return ErrInvalidPoolKey
	}
	if !isPositiveDecimal(key[len(prefix):]) {
		return ErrInvalidPoolKey
	}
	return nil
}

// isPositiveDecimal 判定是否为不带符号、无前导零的正整数字符串（"0" 之外）。
func isPositiveDecimal(s string) bool {
	if s == "" || s == "0" {
		return false
	}
	if len(s) > 1 && s[0] == '0' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// SourceListString 把召回路列表渲染成稳定文本（csv，升序去重），用于日志与缓存 key。
func SourceListString(sources []int32) string {
	if len(sources) == 0 {
		return ""
	}
	ordered := sortedUnique(sources)
	parts := make([]string, 0, len(ordered))
	for _, s := range ordered {
		parts = append(parts, strconv.FormatInt(int64(s), 10))
	}
	return strings.Join(parts, ",")
}

// sortedUnique 返回升序去重后的切片（保证指纹稳定，与入参顺序无关）。
func sortedUnique(in []int32) []int32 {
	out := make([]int32, 0, len(in))
	seen := make(map[int32]struct{}, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
