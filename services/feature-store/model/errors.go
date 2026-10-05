package model

import "errors"

// 本服务的错误哨兵。logic 层据此向调用方返回可枚举的失败原因，
// 错误信息里不得出现 SQL 片段、特征值原文或标识符明文（AGENTS.md §6）。
var (
	// ErrNotImplemented 表示契约已定、业务实现留给逻辑轮。
	// 本轮所有 logic 方法都返回它：绝不返回伪造的成功响应（AGENTS.md §9）。
	ErrNotImplemented = errors.New("feature-store: not implemented")

	// ErrEntityScopeRequired 主体类型未声明或为 UNSPECIFIED。
	ErrEntityScopeRequired = errors.New("feature-store: entity_scope is required")
	// ErrEntityIDInvalid entity_id 不符合该主体类型的形态约束（主键串 / 哈希摘要）。
	// 这道校验同时拦住「误把明文设备号或原始 IP 写进摘要列」。
	ErrEntityIDInvalid = errors.New("feature-store: entity_id is invalid for this entity_scope")
	// ErrEntityScopeMismatch 写入/读取的主体类型与特征定义不一致（跨 scope 读写一律拒绝）。
	ErrEntityScopeMismatch = errors.New("feature-store: entity_scope mismatches the feature definition")

	// ErrFeatureKeyRequired feature_key 为空。
	ErrFeatureKeyRequired = errors.New("feature-store: feature_key is required")
	// ErrDefinitionIncomplete 注册请求缺少解释性必备字段（口径说明为空、展示名/变更说明超长）。
	// 这些字段不进摘要、不改值语义，但没有它们这条定义在一年后就是不可读的。
	ErrDefinitionIncomplete = errors.New("feature-store: feature definition is incomplete")
	// ErrFeatureVersionRequired 写入与切换必须显式带版本，避免「悄悄写到 ACTIVE 版本」。
	ErrFeatureVersionRequired = errors.New("feature-store: feature version must be explicit")
	// ErrFeatureNotFound 特征版本未登记。读接口按降级语义返回而不是伪造值。
	ErrFeatureNotFound = errors.New("feature-store: feature definition not found")
	// ErrFeatureVersionExists (feature_key, version) 已登记。RegisterFeature 不把它当失败：
	// 它触发「比对摘要 → 幂等复用或拒绝原地改写」这条分支。
	ErrFeatureVersionExists = errors.New("feature-store: feature version already registered")
	// ErrFeatureMetadataImmutable 不可变五字段相同、但 TTL/默认值/口径说明等版本元数据不同：
	// 已有版本永不原地改写，必须注册新版本（否则「复用」会悄悄换掉 TTL 语义）。
	ErrFeatureMetadataImmutable = errors.New(
		"feature-store: version metadata differs, register a new version instead of reusing")
	// ErrFeatureKeyForbidden feature_key 命中禁用前缀（广告/支付/会员/分成等范围外语义，
	// AGENTS.md §1/§7）。真正的闸门是 FeatureSource 枚举里根本没有这些来源，
	// 这里是第二道防线：防止有人用「合法来源 + 越界命名」把范围外特征塞进来。
	ErrFeatureKeyForbidden = errors.New("feature-store: feature key is out of the allowed scope")
	// ErrWindowRequired 行为类来源（spm 指标/兴趣/留存）必须声明时间窗口：
	// 无窗口的行为特征等于「全历史累计」，既无法解释也不可回填。
	ErrWindowRequired = errors.New("feature-store: window_seconds is required for behaviour sources")
	// ErrDimensionInvalid dimension 与 value_type 不匹配（标量必须为 0，列表必须 1..上限）。
	ErrDimensionInvalid = errors.New("feature-store: dimension does not match value_type")
	// ErrPrivacyScopeMismatch 隐私级别与主体类型不自洽：
	// 个体维度（mid/设备/IP）不允许声明为聚合级，内容维度不允许声明为用户画像级。
	ErrPrivacyScopeMismatch = errors.New("feature-store: privacy_level is inconsistent with entity_scope")
	// ErrFeatureRetired 特征已下线：不接受写入，读侧返回默认值并标 DEGRADATION_FEATURE_RETIRED。
	ErrFeatureRetired = errors.New("feature-store: feature is retired")
	// ErrFeatureNotActive 特征处于 DRAFT/RETIRED，不参与对外读。
	ErrFeatureNotActive = errors.New("feature-store: feature is not active")
	// ErrFeatureDefinitionImmutable 已登记版本的不可变字段被改动（只能注册新版本）。
	ErrFeatureDefinitionImmutable = errors.New(
		"feature-store: feature definition fields are immutable, register a new version instead")
	// ErrFeatureStateTransition 状态迁移非法（含 RETIRED 复活）。
	ErrFeatureStateTransition = errors.New("feature-store: invalid feature state transition")
	// ErrMultipleActiveVersions 同一 key 出现多个 ACTIVE 指针，读语义二义。
	ErrMultipleActiveVersions = errors.New(
		"feature-store: more than one active version for the same feature_key")
	// ErrNoActiveVersion 该 key 没有对外生效的版本（读侧按 DEFAULT_VALUE 降级）。
	ErrNoActiveVersion = errors.New("feature-store: no active version for the feature_key")
	// ErrActiveVersionMissing feature_active_version 指针缺失（注册流程未走完）。
	ErrActiveVersionMissing = errors.New("feature-store: active version pointer is missing")
	// ErrTransactionRequired 该操作必须在事务内执行（指针切换 + 审计行必须原子落库）。
	// 脱离事务的 FOR UPDATE 会立刻释放锁，那是假的串行化保证，因此直接拒绝而不是降级执行。
	ErrTransactionRequired = errors.New("feature-store: operation must run inside a transaction")

	// ErrPrivacyLevelRequired 隐私级别未声明：注册与写入一律拒绝（AGENTS.md §7）。
	ErrPrivacyLevelRequired = errors.New("feature-store: privacy_level is required")
	// ErrSourceRequired 特征来源未声明（来源枚举里本就没有广告/支付语义，见 types.go）。
	ErrSourceRequired = errors.New("feature-store: feature source must be a computation pipeline")
	// ErrSourceMismatch 写入方身份与特征定义的 source 不一致（防跨链路串写）。
	ErrSourceMismatch = errors.New("feature-store: writer source mismatches the definition")
	// ErrValueTypeInvalid 值类型未声明。
	ErrValueTypeInvalid = errors.New("feature-store: value_type is invalid")
	// ErrTTLRequired TTL 未声明：值会永不过期，注册时直接拒绝。
	ErrTTLRequired = errors.New("feature-store: ttl_seconds must be positive")
	// ErrDimensionExceeded 列表/向量长度超过定义的 dimension（无界向量会被结构挡住）。
	ErrDimensionExceeded = errors.New("feature-store: value length exceeds the declared dimension")
	// ErrMalformedValue 值形态与 value_type 不符（多余填充或字段缺失）。
	ErrMalformedValue = errors.New("feature-store: malformed feature value")
	// ErrMalformedListValue list_values 列内容不是纯数字列表（脏数据或跨版本误读）。
	ErrMalformedListValue = errors.New("feature-store: malformed list value in storage")
	// ErrDefaultValueInvalid 默认值无法按 value_type 解析：缺值降级会返回不可用数据。
	ErrDefaultValueInvalid = errors.New("feature-store: default_value does not match value_type")
	// ErrDegradationRequired 读结果缺少降级原因（服务内部不变量，绝不静默返回 UNSPECIFIED）。
	ErrDegradationRequired = errors.New("feature-store: degradation reason is required")
	// ErrStaleEventTime 写入的 event_time 早于库中已有值：默认不覆盖（计入 rejected），
	// 只有回填/离线重算路径显式带 AllowStaleOverwrite 才放行。
	// 没有这道闸，一次迟到的旧批次会把线上正在生效的特征值退回历史。
	ErrStaleEventTime = errors.New("feature-store: incoming event_time is older than the stored value")
	// ErrRowDisappeared 写入的目标行在「读到存在」与「条件更新」之间被并发清理/隐私删除抹掉：
	// 本次没写成，但入参本身没错，调用方换 request_id 重试即可（与 ErrStaleEventTime 的
	// 「值更新到更高版本了，重试也是白重试」是两种处置，混在一起会让上游的重试策略失效）。
	ErrRowDisappeared = errors.New("feature-store: target row disappeared concurrently")
	// ErrDuplicateRowInBatch 同一批 WriteFeatures 里对同一个 (feature_key, version, entity)
	// 写了两次：后一条生效，前一条标成本批重复。不报出来的话，上游的重复计算会一直静默存在。
	ErrDuplicateRowInBatch = errors.New("feature-store: duplicate key inside the same write batch")

	// ErrTooManyEntries 批量读条目数超过硬上限（报错而不是截断，见契约「批量上限」）。
	ErrTooManyEntries = errors.New("feature-store: requested entries exceed the batch limit")
	// ErrTooManyRows 批量写行数超过硬上限。
	ErrTooManyRows = errors.New("feature-store: writes exceed the batch limit")
	// ErrTooManyEntities 回填显式主体列表超过上限。
	ErrTooManyEntities = errors.New("feature-store: entity_ids exceed the batch limit")
	// ErrLimitTooLarge 单次清理/分页数量超过服务端上限。
	ErrLimitTooLarge = errors.New("feature-store: limit exceeds server limit")

	// ErrRequestIdRequired 写接口缺少幂等键。
	ErrRequestIdRequired = errors.New("feature-store: request_id is required")
	// ErrOperatorRequired 变更接口缺少操作人（审计要求）。
	ErrOperatorRequired = errors.New("feature-store: operator is required")
	// ErrReasonRequired 状态/隐私/切换/回填变更必须给理由。
	ErrReasonRequired = errors.New("feature-store: reason is required")
	// ErrRequestIdReused 同一 request_id 携带了不同内容：幂等回放会给出错误结果。
	ErrRequestIdReused = errors.New("feature-store: request_id was already used with a different body")

	// ErrVersionConflict 版本切换的乐观校验失败（expected_from_version 与服务端不符）。
	ErrVersionConflict = errors.New("feature-store: active version changed concurrently")
	// ErrImmutableFieldMismatch 切换时新旧版本不可变字段不一致（切出去的是另一种特征）。
	ErrImmutableFieldMismatch = errors.New(
		"feature-store: switching to a version with different immutable fields")
	// ErrSwitchNotFound 审计行不存在。回滚必须引用一条真实存在的 switch_id：
	// 允许引用「凭空的切换」会让审计链断裂，事后无法解释当前版本是怎么来的。
	ErrSwitchNotFound = errors.New("feature-store: version switch record not found")

	// ErrJobNotFound 回填作业不存在（按 job_id / request_id 单查时 found=false，不报错）。
	ErrJobNotFound = errors.New("feature-store: backfill job not found")
	// ErrJobExists 同一 request_id 的作业已提交过：SubmitBackfillJob 不把它当失败，
	// 而是回查首次作业并返回 reused=true（重复提交同一批补数是常态，报错只会让上游重试风暴）。
	ErrJobExists = errors.New("feature-store: backfill job already submitted for this request_id")
	// ErrJobStateInvalid 传入的回填状态不是已声明的枚举值（含 UNSPECIFIED）。
	ErrJobStateInvalid = errors.New("feature-store: backfill state is invalid")
	// ErrJobBadTransition 回填作业状态迁移非法（终态回退、未认领直接收尾等）。
	// 与 ErrJobAlreadyTerminal 的分工：后者是「重复推进幂等无害」，前者是「这个迁移不该发生」，
	// 混用会让 worker 无法判断该重试还是该停下。
	ErrJobBadTransition = errors.New("feature-store: invalid backfill job state transition")
	// ErrJobAlreadyTerminal 作业已终态，重复推进不改变状态（幂等）。
	ErrJobAlreadyTerminal = errors.New("feature-store: job already in terminal state")
	// ErrBackfillTargetNotDraft 只允许给 DRAFT 版本回填：给 ACTIVE 版本补历史值会让线上
	// 读到的值在无人复核的情况下改变。
	ErrBackfillTargetNotDraft = errors.New("feature-store: backfill target version must be DRAFT")
	// ErrBackfillSourceMismatch 回填取数来源与特征定义不一致。
	ErrBackfillSourceMismatch = errors.New("feature-store: backfill source mismatches the definition")
	// ErrBackfillEntityIDsTooLarge 显式主体列表编码后超过单作业承载上限。
	// 上限按「列宽」而不是「条数」卡：1000 个 64 字符摘要本身就 64KB，
	// 只数条数会让全量与显式两条路径的内存上界完全不同。
	ErrBackfillEntityIDsTooLarge = errors.New("feature-store: entity_ids payload exceeds the job size limit")
	// ErrBackfillWindowInvalid 回填时间窗非法（from<=0、to<from）。
	// 无窗口的「全历史回填」会锁住一整张源表的扫描，必须显式给区间。
	ErrBackfillWindowInvalid = errors.New("feature-store: backfill window is invalid")
	// ErrCutoffRequired 清理类操作必须显式给截止时间（保留期 = now-N 天由调用方算好传入）。
	// 与 ErrBackfillWindowInvalid 分开：后者是「区间不合法」，这里是「根本没有上界」，
	// 把 0 当「删干净」会让一次漏传参数变成不可逆的全表清理。
	ErrCutoffRequired = errors.New("feature-store: cutoff timestamp is required")
	// ErrJobNotRunning 作业不在 RUNNING（推进/收尾前的条件更新未命中）：
	// 说明认领已失效或被别的 worker 接管，当前 worker 必须停下而不是继续写值。
	ErrJobNotRunning = errors.New("feature-store: job is not running")
	// ErrJobLeaseExpired 作业租约过期（被超时接管），原推进者的进度写入被拒绝。
	ErrJobLeaseExpired = errors.New("feature-store: job lease expired")

	// ErrReceiptNotFound 幂等回执不存在（首轮请求尚未落库）。
	ErrReceiptNotFound = errors.New("feature-store: idempotency receipt not found")
	// ErrReceiptOpRequired 落回执必须声明操作类型，否则同一 request_id 在不同接口间串味。
	ErrReceiptOpRequired = errors.New("feature-store: receipt operation type is required")
	// ErrReceiptInProgress 同一 request_id 的另一条请求仍在处理中（回执 state=in_progress）。
	// 必须报错而不是「顺手再执行一遍」：并发改成两次执行，WriteFeatures 会写双份行、
	// SwitchFeatureVersion 会留两条互相矛盾的审计。调用方应退避重试而不是换 request_id。
	ErrReceiptInProgress = errors.New("feature-store: the same request_id is still in progress")
	// ErrReceiptStateInvalid 库里的回执状态不是已声明的枚举值（被外部改表或历史脏数据）。
	// 这里不猜它相当于哪个状态：猜错就是在一条来历不明的回执上执行写操作。
	ErrReceiptStateInvalid = errors.New("feature-store: receipt state is invalid")
	// ErrReceiptResultTooLarge 回执快照超过 result_json 列宽：
	// 说明被回执的操作返回了不该返回的体量（幂等回放要重放它），必须在写入前拒绝。
	ErrReceiptResultTooLarge = errors.New("feature-store: receipt result snapshot exceeds the column width")

	// ErrPrivacyOperatorForbidden 隐私类写操作（EraseEntityFeatures）只允许隐私工单链路
	// 触发（operator 前缀在白名单内）。放开给任意调用方等于把「抹数据」变成 DoS 面。
	ErrPrivacyOperatorForbidden = errors.New("feature-store: privacy operation requires an authorized operator")
	// ErrPrivacyUnsetNotAllowed 隐私级别调整不允许出现 UNSPECIFIED：
	// 与注册同口径，「不知道多敏感」的特征一律不允许存在，双向调整都要显式定级。
	ErrPrivacyUnsetNotAllowed = errors.New("feature-store: privacy_level must be explicitly set")
)
