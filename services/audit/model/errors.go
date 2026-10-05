package model

import "errors"

// ErrNotImplemented 表示本轮只落了契约与数据模型，该用例的业务实现尚未落地。
// logic 层直接把它原样返回给 gRPC（映射为 Unimplemented/Unknown 级别的明确错误），
// 禁止用零值响应伪装成功（AGENTS.md §9）。
var ErrNotImplemented = errors.New("audit/model: not implemented")

// audit 域错误。logic 层把它们映射为稳定的 gRPC status，
// 错误消息不包含口令、token、明文 IP 或 SQL 片段（AGENTS.md §9）。
var (
	// ErrEntryNotFound 审计条目不存在（entry_id 与 event_id 都查不到）。
	ErrEntryNotFound = errors.New("audit: entry not found")
	// ErrEventIDRequired event_id 是写入幂等键，缺失即拒绝。
	ErrEventIDRequired = errors.New("audit: event_id required")
	// ErrActorUnspecified actor_type 为 UNSPECIFIED 时禁止写入。
	ErrActorUnspecified = errors.New("audit: actor_type required")
	// ErrActionRequired action/action_domain 缺失即拒绝，无法定位哈希链。
	ErrActionRequired = errors.New("audit: action and action_domain required")
	// ErrDigestLooksPII 前后摘要命中疑似明文手机号/邮箱/证件号/口令形态，
	// 整条拒绝写入（AGENTS.md §7：日志脱敏）。这是硬约束，不提供配置开关。
	ErrDigestLooksPII = errors.New("audit: before/after digest looks like plaintext PII, only irreversible digests are allowed")
	// ErrDigestInvalid 前后摘要不符合白名单格式（见 model.ValidDigest）：
	// 只允许空串、64 位十六进制整对象摘要、或 `字段名=16 位十六进制` 的分号列表。
	ErrDigestInvalid = errors.New("audit: digest must be empty, a 64-hex object digest, or field=16hex pairs")
	// ErrFieldTooLong 自由文本字段超过入库长度上限（截断会破坏哈希链的可复算性，
	// 因此写入路径直接拒绝，由调用方改传摘要）。
	ErrFieldTooLong = errors.New("audit: field exceeds stored maximum length")
	// ErrBatchTooLarge BatchAppendAudit 超过单次上限。
	ErrBatchTooLarge = errors.New("audit: batch size exceeds limit")
	// ErrBatchEmpty 批量写入不接受空数组（空数组多半是上游 bug）。
	ErrBatchEmpty = errors.New("audit: entries required")
	// ErrChainKeyRequired 校验/归档必须指定 chain_key。
	ErrChainKeyRequired = errors.New("audit: chain_key required")
	// ErrChainHeadMissing 指定链尚无任何条目。
	ErrChainHeadMissing = errors.New("audit: chain head not found")
	// ErrChainConflict 并发追加时链头序号被他人抢先推进，调用方需重试。
	ErrChainConflict = errors.New("audit: chain sequence conflict, retry")
	// ErrQueryRangeRequired 查询/导出必须给时间范围（大表禁止全表扫描）。
	ErrQueryRangeRequired = errors.New("audit: start_at and end_at are required")
	// ErrQueryRangeTooWide 时间跨度超过服务端上限。
	ErrQueryRangeTooWide = errors.New("audit: query time range exceeds the allowed maximum")
	// ErrQueryTooBroad 未给出任何收窄维度（actor/action/target/trace 至少一个）。
	ErrQueryTooBroad = errors.New("audit: at least one narrowing dimension is required")
	// ErrInvalidPage 分页参数非法。
	ErrInvalidPage = errors.New("audit: invalid pagination")
	// ErrRequestIDRequired 写接口缺少幂等键。
	ErrRequestIDRequired = errors.New("audit: request_id required")
	// ErrCallerRequired 缺少调用方归因（caller_service 为空时无法判定写入者）。
	ErrCallerRequired = errors.New("audit: caller_service required")
	// ErrTaskNotFound 导出任务不存在。
	ErrTaskNotFound = errors.New("audit: export task not found")
	// ErrTaskExists request_id 命中已有任务（repository 回查后返回 reused=true）。
	ErrTaskExists = errors.New("audit: export task already exists")
	// ErrTaskBadTransition 导出任务状态非法迁移（model.CanExportTransition）。
	ErrTaskBadTransition = errors.New("audit: illegal export state transition")
	// ErrTaskNotReady 任务尚未完成，不能签发下载地址。
	ErrTaskNotReady = errors.New("audit: export task is not ready for download")
	// ErrExportFormatUnsupported 不支持的导出格式。
	ErrExportFormatUnsupported = errors.New("audit: unsupported export format")
	// ErrPolicyNotFound 保留期策略不存在。
	ErrPolicyNotFound = errors.New("audit: retention policy not found")
	// ErrPolicyExists action_domain 已存在（唯一索引冲突）。
	ErrPolicyExists = errors.New("audit: retention policy already exists")
	// ErrPolicyVersionConflict expect_version 与库中版本不一致。
	ErrPolicyVersionConflict = errors.New("audit: retention policy version conflict, reload and retry")
	// ErrPolicyDaysInvalid hot_days / archive_after_days / delete_after_days 顺序非法。
	ErrPolicyDaysInvalid = errors.New("audit: require 0 < archive_after_days <= hot_days and delete_after_days >= archive_after_days")
	// ErrBatchNotFound 归档批次不存在。
	ErrBatchNotFound = errors.New("audit: archive batch not found")
	// ErrBatchUnverified 归档清单尚未校验通过，禁止标记热表已归档。
	ErrBatchUnverified = errors.New("audit: archive manifest not verified")
	// ErrObjectStorageMissing 未配置对象存储：导出/归档不能只写库不出文件。
	ErrObjectStorageMissing = errors.New("audit: object storage is not configured")
	// ErrHashSaltMissing 未注入 IP/设备哈希盐：哈希会变成可被彩虹表反查的裸 SHA-256，
	// 因此缺失时直接拒绝写入，而不是静默降级（与 operation 的 token 密钥同语义）。
	ErrHashSaltMissing = errors.New("audit: hashing salt missing, set the env var named by Security.IpHashSaltRef")

	// --- 第二轮（logic 实现）补充的哨兵 ---

	// ErrRequestRequired 入参 message 为空：不猜调用方想写/查什么。
	ErrRequestRequired = errors.New("audit: request is required")
	// ErrDraftRequired AppendAudit 的 entry 为空。
	ErrDraftRequired = errors.New("audit: entry is required")
	// ErrEntryIDOrEventIDRequired GetAuditEntry 两个定位键都缺失。
	ErrEntryIDOrEventIDRequired = errors.New("audit: entry_id or event_id is required")
	// ErrTaskIDOrRequestIDRequired GetAuditExport 两个定位键都缺失。
	ErrTaskIDOrRequestIDRequired = errors.New("audit: task_id or request_id is required")
	// ErrSchemaVersionUnsupported schema_version 不是当前算法版本（0 视为 1）。
	// 允许就地兼容会让「同一链里混两种序列化」变成可能，因此只能拒绝。
	ErrSchemaVersionUnsupported = errors.New("audit: schema_version unsupported, current algorithm is v1")
	// ErrResultRequired result 为 UNSPECIFIED：迁移列注释明确「0 禁止入库」，
	// 一次没有结果的审计无法解释「到底成没成」。
	ErrResultRequired = errors.New("audit: result is required (ok/denied/error)")
	// ErrSourceAppRequired source_app 为 UNSPECIFIED：同上，来源端是审计维度而非可选项。
	ErrSourceAppRequired = errors.New("audit: source_app is required")
	// ErrActionDomainInvalid action_domain 字符集/长度非法（决定链名与策略唯一键）。
	ErrActionDomainInvalid = errors.New("audit: action_domain must be lowercase letters, digits or underscore, max 32")
	// ErrActionInvalid action 不是 <对象>.<动作> 形态。
	ErrActionInvalid = errors.New("audit: action must look like <object>.<action>")
	// ErrActorIDInvalid actor_id 为负数：无符号语义的引用键，负值只会让回查失败。
	ErrActorIDInvalid = errors.New("audit: actor_id must not be negative")
	// ErrOccurredAtFuture occurred_at 明显超前（超过容忍窗口），拒绝而不是新开一条未来链。
	// 未来链会永久躲开按「今天」扫描的归档与校验作业，等于把证据放进无人看守的抽屉。
	ErrOccurredAtFuture = errors.New("audit: occurred_at is too far in the future")
	// ErrDuplicateEventID 同一批次内 event_id 自撞：唯一键会让整批失败，
	// 报错必须点出是哪一条，否则调用方只能整批盲猜。
	ErrDuplicateEventID = errors.New("audit: duplicate event_id inside one batch")
	// ErrReasonRequired 导出/放宽清理窗口等高危动作必须留可追动机。
	ErrReasonRequired = errors.New("audit: reason is required")
	// ErrObjectStorageUnsupported 对象存储已配置但本服务未引入 S3/OSS 客户端：
	// 上传与签名地址都由外部实现，本期一律显式失败，绝不写「已生成文件」的假元数据。
	ErrObjectStorageUnsupported = errors.New("audit: object storage client is not implemented in this build")
	// ErrExportAlreadyClaimed 任务已被别的推进者持有（running），本期没有租约属主列可抢占。
	ErrExportAlreadyClaimed = errors.New("audit: export task is already running")
	// ErrExportFilterInvalid 任务里的 filter_json 快照无法还原成合法查询条件。
	ErrExportFilterInvalid = errors.New("audit: export filter snapshot is invalid")
	// ErrArchiveRangeInvalid 归档区间非法（from>to、负数、链上无该区间）。
	ErrArchiveRangeInvalid = errors.New("audit: archive seq range is invalid")
	// ErrArchiveRangeTooLarge 单批条数超过 Archive.MaxEntriesPerBatch，要求分片。
	ErrArchiveRangeTooLarge = errors.New("audit: archive range exceeds the configured entries per batch")
	// ErrArchiveNotDue 区间内最新条目还没超过保留策略的 archive_after_days：
	// 未到期就归档等于把热证据提前搬走，VerifyAuditChain 的在线覆盖范围会莫名缩小。
	ErrArchiveNotDue = errors.New("audit: archive range is not yet eligible by the retention policy")
	// ErrArchiveRangeGap 区间内条目数 != to_seq - from_seq + 1：链有洞，禁止归档。
	ErrArchiveRangeGap = errors.New("audit: archive range has seq gaps, refuse to move a truncated segment")
	// ErrExportRowLimit 单次导出累计行数超过 Export.MaxRowsPerTask：
	// 要求调用方按时间分片重新提交，而不是让一个任务无限跑下去。
	ErrExportRowLimit = errors.New("audit: export row count exceeds the per-task limit, split by time range")
	// ErrDownloadTrailUnwritten 取下载地址的留痕写不进去：实现按 fail-closed 处理，
	// 宁可不给链接，也不给出「存证被带走了而台账上查不到」的链接。
	ErrDownloadTrailUnwritten = errors.New("audit: download trail could not be written, url is not issued")
)

// 审计条目结果（audit_entry.result，取值与 audit.v1.AuditResult 一致）。
const (
	ResultUnspecified int32 = 0
	ResultOK          int32 = 1
	ResultDenied      int32 = 2
	ResultError       int32 = 3
)

// 发起者类型（audit_entry.actor_type，取值与 audit.v1.ActorType 一致）。
const (
	ActorUnspecified int32 = 0
	ActorAdmin       int32 = 1
	ActorUser        int32 = 2
	ActorSystem      int32 = 3
	ActorUnknown     int32 = 4
)

// 来源端（audit_entry.source_app，取值与 audit.v1.SourceApp 一致）。
const (
	SourceUnspecified int32 = 0
	SourceAndroid     int32 = 1
	SourceIOS         int32 = 2
	SourceHarmony     int32 = 3
	SourceDesktop     int32 = 4
	SourceAdminWeb    int32 = 5
	SourceInternalRPC int32 = 6
	SourceCron        int32 = 7
)

// 导出任务状态（audit_export_task.state）。
const (
	ExportStatePending   = "pending"
	ExportStateRunning   = "running"
	ExportStateSucceeded = "succeeded"
	ExportStateFailed    = "failed"
	ExportStateExpired   = "expired"
	ExportStateCanceled  = "canceled"
)

// 归档批次状态（audit_archive_batch.state）。
// pending → writing → verified → purged；任一阶段失败进入 failed。
const (
	BatchStatePending  = "pending"
	BatchStateWriting  = "writing"
	BatchStateVerified = "verified"
	BatchStatePurged   = "purged"
	BatchStateFailed   = "failed"
)

// 通用状态（audit_retention_policy.state）。
const (
	StateEnable  int32 = 1
	StateDisable int32 = 2
)

// 当前条目契约版本；哈希链算法版本与之绑定（见 rpc/audit.proto 文件头）。
const SchemaVersion int32 = 1

// default 是未匹配 action_domain 时使用的保留期策略键（迁移里以种子行写入）。
const DefaultPolicyDomain = "default"

// exportTransitions 是导出任务状态机的合法转换表（AGENTS.md §8：状态只能通过
// 合法迁移推进，终态无出边）。
var exportTransitions = map[string][]string{
	ExportStatePending:   {ExportStateRunning, ExportStateCanceled, ExportStateFailed},
	ExportStateRunning:   {ExportStateSucceeded, ExportStateFailed, ExportStateCanceled},
	ExportStateSucceeded: {ExportStateExpired},
	ExportStateFailed:    {},
	ExportStateExpired:   {},
	ExportStateCanceled:  {},
}

// CanExportTransition 校验导出任务 from → to 是否为合法迁移。未知 from 一律 false。
func CanExportTransition(from, to string) bool {
	targets, ok := exportTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// IsExportFinalState 判断导出任务是否已不可推进。
// succeeded 不是终态：对象到期后还要流转到 expired。
func IsExportFinalState(state string) bool {
	switch state {
	case ExportStateFailed, ExportStateExpired, ExportStateCanceled:
		return true
	default:
		return false
	}
}

// batchTransitions 是归档批次状态机的合法转换表。
// purged 意味着热表行已标记 archived_at，物理删除不在本契约内（见 README 缺口）。
var batchTransitions = map[string][]string{
	BatchStatePending:  {BatchStateWriting, BatchStateFailed},
	BatchStateWriting:  {BatchStateVerified, BatchStateFailed},
	BatchStateVerified: {BatchStatePurged, BatchStateFailed},
	BatchStatePurged:   {},
	BatchStateFailed:   {},
}

// CanBatchTransition 校验归档批次状态迁移是否合法。
func CanBatchTransition(from, to string) bool {
	targets, ok := batchTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// ValidResult / ValidActorType / ValidSourceApp 判定入参枚举是否落在已定义取值。
// 0（UNSPECIFIED）对 result 与 source_app 是非法写入值：拒绝而不是记一条无法解释的行。
func ValidResult(v int32) bool { return v >= ResultOK && v <= ResultError }

// ValidActorType 判定 actor_type 合法（UNSPECIFIED 除外）。
func ValidActorType(v int32) bool { return v >= ActorAdmin && v <= ActorUnknown }

// ValidSourceApp 判定 source_app 合法。
func ValidSourceApp(v int32) bool { return v >= SourceAndroid && v <= SourceCron }
