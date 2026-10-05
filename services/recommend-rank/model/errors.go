package model

import (
	"errors"
	"sort"
	"strings"
)

// ErrNotImplemented 表示本服务的业务能力尚未落地（契约轮占位）。
//
// logic 层的所有方法在本轮都返回该哨兵，禁止返回"看起来成功"的空排序；
// 调用方（gateway/app）据此回退 recommend-recall 的召回原序（README 降级矩阵）。
// 参考 services/account/internal/repository/repository.go 的同名哨兵约定。
var ErrNotImplemented = errors.New("recommend-rank: not implemented")

// 入参与规模校验错误。
var (
	// ErrInvalidAid 候选 aid 非正数。
	ErrInvalidAid = errors.New("recommend-rank: invalid aid")
	// ErrEmptyCandidates 入参候选为空：返回空列表 + 显式原因，不冒充"已排序"。
	ErrEmptyCandidates = errors.New("recommend-rank: candidates is empty")
	// ErrTooManyCandidates 入参候选条数超过硬上限（拒绝而不是静默裁剪）。
	ErrTooManyCandidates = errors.New("recommend-rank: too many candidates")
	// ErrLimitTooLarge 出参条数超过硬上限。
	ErrLimitTooLarge = errors.New("recommend-rank: limit exceeds max return")
	// ErrInvalidSource 候选来源不在 RankSource 枚举内。
	ErrInvalidSource = errors.New("recommend-rank: invalid rank source")
	// ErrRawDeviceID 设备标识必须是 sha256 摘要，拒绝明文设备号。
	ErrRawDeviceID = errors.New("recommend-rank: device id must be a sha256 digest")
	// ErrIdempotencyKeyRequired 写接口缺少幂等键。
	ErrIdempotencyKeyRequired = errors.New("recommend-rank: idempotency key required")
	// ErrOperatorRequired 写接口缺少操作者。
	ErrOperatorRequired = errors.New("recommend-rank: operator required")
	// ErrReasonRequired 状态切换缺少原因（模型激活/回滚是审计事件）。
	ErrReasonRequired = errors.New("recommend-rank: reason required")
	// ErrPageTooDeep 分页偏移过深。
	ErrPageTooDeep = errors.New("recommend-rank: page too deep")
	// ErrInvalidSubjectType 分桶主体类型不在枚举内。
	ErrInvalidSubjectType = errors.New("recommend-rank: invalid subject type")
	// ErrSubjectIDRequired 分桶查询缺少主体标识。
	ErrSubjectIDRequired = errors.New("recommend-rank: subject id required")
	// ErrInvalidObjective 优化目标 key 不在受控集合内（禁止广告/商业化目标）。
	ErrInvalidObjective = errors.New("recommend-rank: invalid objective")
	// ErrInvalidWeight 目标权重越界（负数或超过权重上限）。
	ErrInvalidWeight = errors.New("recommend-rank: invalid objective weight")
	// ErrInvalidMissingPolicy 特征缺失策略不在受控枚举内。
	ErrInvalidMissingPolicy = errors.New("recommend-rank: invalid feature missing policy")
	// ErrTooManyFeatures 特征清单条数超过上限。
	ErrTooManyFeatures = errors.New("recommend-rank: too many feature keys")
	// ErrInvalidBucketRange 分桶区间非法（要求 0 <= start < end <= bucketCount）。
	ErrInvalidBucketRange = errors.New("recommend-rank: invalid bucket range")
	// ErrInvalidTimeRange 实验时间窗非法（end_at 非 0 时必须 > start_at）。
	ErrInvalidTimeRange = errors.New("recommend-rank: invalid experiment time range")
	// ErrBucketOverlap 同一实验同一层内变体区间重叠（同层互斥必须不重叠）。
	ErrBucketOverlap = errors.New("recommend-rank: bucket range overlaps another variant")
	// ErrExperimentNotFound 实验变体不存在。
	ErrExperimentNotFound = errors.New("recommend-rank: experiment variant not found")
	// ErrInvalidExpState 实验状态不在枚举内。
	ErrInvalidExpState = errors.New("recommend-rank: invalid experiment state")
	// ErrExpStateTransition 非法的实验状态迁移。
	ErrExpStateTransition = errors.New("recommend-rank: illegal experiment state transition")
	// ErrModelNotFound 模型版本未登记。
	ErrModelNotFound = errors.New("recommend-rank: model version not found")
	// ErrModelVersionExists 同一 (model_key, version) 已登记（uniq_model_version 命中，幂等回放旧行）。
	ErrModelVersionExists = errors.New("recommend-rank: model version already exists")
	// ErrInvalidModelState 模型状态不在枚举内。
	ErrInvalidModelState = errors.New("recommend-rank: invalid model version state")
	// ErrModelStateTransition 非法的模型状态迁移。
	ErrModelStateTransition = errors.New("recommend-rank: illegal model version state transition")
	// ErrModelImmutable 版本已登记后禁止改语义字段（权重/特征绑定需新版本）。
	ErrModelImmutable = errors.New("recommend-rank: model version metadata is immutable once active")
	// ErrNoActiveModel 该 model_key 没有 ACTIVE 版本（在线必然降级）。
	ErrNoActiveModel = errors.New("recommend-rank: no active model version")
	// ErrFeatureConfigNotFound 特征配置版本未登记。
	ErrFeatureConfigNotFound = errors.New("recommend-rank: feature config version not found")
	// ErrFeatureConfigExists 同一 config_version 已登记（uniq_config_version 命中，幂等回放旧行）。
	ErrFeatureConfigExists = errors.New("recommend-rank: feature config version already exists")
	// ErrInvalidFeatureState 特征配置状态不在 FeatureState* 枚举内。
	ErrInvalidFeatureState = errors.New("recommend-rank: invalid feature config state")
	// ErrFeatureConfigInUse 仍被 ACTIVE 模型引用的特征配置禁止停用（停用会让在线排序无特征）。
	ErrFeatureConfigInUse = errors.New("recommend-rank: feature config is referenced by an active model")
	// ErrExperimentExists 同一 (exp_key, variant_key) 已登记（uniq_variant 命中）。
	ErrExperimentExists = errors.New("recommend-rank: experiment variant already exists")
	// ErrExperimentImmutable RUNNING 变体禁止改分桶区间与 hash_seed（会让已分桶主体跳组），
	// 必须先 SetExperimentState(PAUSED) 再改。
	ErrExperimentImmutable = errors.New("recommend-rank: running experiment bucket config is immutable")
	// ErrInvalidBucketCount 分桶空间取值非法（必须等于服务配置的 BucketCount 口径）。
	ErrInvalidBucketCount = errors.New("recommend-rank: invalid bucket count")
	// ErrHashSeedRequired 变体缺少分桶哈希盐（无盐无法保证跨进程一致的分桶）。
	ErrHashSeedRequired = errors.New("recommend-rank: hash seed required")
	// ErrAssignmentNotFound 主体在该实验当前哈希盐下尚无分桶记录。
	ErrAssignmentNotFound = errors.New("recommend-rank: experiment assignment not found")
	// ErrDecisionIDRequired 审计读既没有 decision_id 也没有 request_id，无法定位一次排序。
	ErrDecisionIDRequired = errors.New("recommend-rank: decision_id or request_id required")

	// ErrDecisionNotFound 排序决策摘要不存在。
	ErrDecisionNotFound = errors.New("recommend-rank: rank decision summary not found")
	// ErrDecisionExists 同一 request_id 的摘要已存在（幂等命中，调用方回放旧结果）。
	ErrDecisionExists = errors.New("recommend-rank: rank decision summary already exists")
	// ErrDegradationDisabled allow_degrade=false 且依赖故障时返回该错误而不是降级结果。
	ErrDegradationDisabled = errors.New("recommend-rank: degradation disabled by request")
	// ErrInvalidDegradeReason 降级原因不在受控 key 集合内（宁可报错也不落一条无法解释的摘要）。
	ErrInvalidDegradeReason = errors.New("recommend-rank: invalid degrade reason")
	// ErrInvalidFallbackStrategy 兜底策略不在受控 key 集合内。
	ErrInvalidFallbackStrategy = errors.New("recommend-rank: invalid fallback strategy")
	// ErrCandidateSubsetBroken 不变量被破坏：出参出现了入参候选之外的 aid。
	// 一旦触发必须放弃本次结果并降级回退召回原序，绝不把"编造的候选"下发给客户端。
	ErrCandidateSubsetBroken = errors.New("recommend-rank: result must be a subset of input candidates")
	// ErrInvalidOverrideKey 实验 overrides 出现了未登记的参数名
	// （详见 override.go：实验只允许改「怎么算分/怎么打散/怎么限频」）。
	ErrInvalidOverrideKey = errors.New("recommend-rank: invalid experiment override key")
	// ErrOverridesTooLarge 实验 overrides JSON 超过字节上限。
	ErrOverridesTooLarge = errors.New("recommend-rank: experiment overrides too large")
)

// 依赖未接线（fail closed）：配置声明启用某条下游、但 client 缺失或仍是契约轮 stub 时，
// 返回这里的具体哨兵而不是退化成「读不到就当没有」。
// 参考 internal/repository/downstream.go：stub 返回 ErrNotImplemented，
// logic 把它翻译成下面的一条，运维从错误名就能看出是「没配」还是「配了但线上没接」。
var (
	// ErrRepositoryNotConfigured ServiceContext 未装配 Repository（启动顺序错误）：
	// 本服务所有事实都在 MySQL，没有仓库就无法工作，直接显式失败而不是 panic。
	ErrRepositoryNotConfigured = errors.New("recommend-rank: repository is not configured")
	// ErrFeatureSourceNotConfigured FeatureFetchEnabled=true 但没有 feature-store client。
	ErrFeatureSourceNotConfigured = errors.New("recommend-rank: feature source is not configured")
	// ErrBehaviorSourceNotConfigured BehaviorFetchEnabled=true 但没有 spm client。
	ErrBehaviorSourceNotConfigured = errors.New("recommend-rank: behavior source is not configured")
	// ErrSafetyGateNotConfigured SafetyCheckEnabled=true 但没有内容安全可见性查询能力：
	// 「读不到可见性结论」不能默认放行，只能显式报错。
	ErrSafetyGateNotConfigured = errors.New("recommend-rank: safety gate is not configured")
	// ErrOpsConfigNotConfigured OpsConfigEnabled=true 但没有 ops-config 读取能力。
	ErrOpsConfigNotConfigured = errors.New("recommend-rank: ops config reader is not configured")
)

// 字段形态与必填校验：列宽取自 deploy/migrations/recommend-rank/*.sql，
// 由 logic 在进入事务前把守（model 只保证「不合法就别写」）。
var (
	// ErrFieldTooLong 文本字段超出列宽。超长一律拒绝而不是静默截断：
	// 截断后的标识会在审计里指向另一个对象（model_key/version/exp_key/hash_seed 都是主键片段）。
	ErrFieldTooLong = errors.New("recommend-rank: text field exceeds column width")
	// ErrKeyRequired 业务主键类文本缺失（model_key / version / config_version / exp_key / variant_key）。
	ErrKeyRequired = errors.New("recommend-rank: business key required")
	// ErrInvalidFeatureKey 特征 key 形态非法：空、含逗号（会破坏 csv 清单的往返一致性）、
	// 含控制字符或超长。
	ErrInvalidFeatureKey = errors.New("recommend-rank: invalid feature key")
	// ErrDuplicateObjective 同一模型版本登记了重复优化目标：权重语义随之不确定，
	// 禁止「后者覆盖前者」。
	ErrDuplicateObjective = errors.New("recommend-rank: duplicated objective weight")
	// ErrArtifactRefInvalid 模型工件引用非法（超出列宽、带 scheme 的完整 URL 或内联凭据）。
	ErrArtifactRefInvalid = errors.New("recommend-rank: invalid model artifact ref")
	// ErrOfflineMetricsInvalid 离线指标不是合法 JSON 对象或超出列宽。
	ErrOfflineMetricsInvalid = errors.New("recommend-rank: invalid offline metrics")
	// ErrFeatureConfigDisabled 特征配置版本处于停用态：模型不得绑定/激活到没有特征的清单上，
	// 否则上线即无特征可取（AGENTS.md §9 的「配置层自伤」）。
	ErrFeatureConfigDisabled = errors.New("recommend-rank: feature config version is disabled")
	// ErrExperimentNotRunning 实验下没有当前生效的 RUNNING 变体：
	// 此时登记分桶会把主体冻进一个并不存在的分组，宁可报错。
	ErrExperimentNotRunning = errors.New("recommend-rank: experiment has no running variant")
	// ErrExperimentSeedConflict 同一实验的变体登记了不同的 hash_seed：
	// 单一主体在该实验下无法有确定桶号，必须先修配置再分桶。
	ErrExperimentSeedConflict = errors.New("recommend-rank: experiment variants disagree on hash_seed")
	// ErrOverrideValueInvalid 实验 overrides 的取值非法（目标权重区间、召回路配额之和、
	// 打散/频控上界）。key 白名单由 ValidateOverrideKeys 把守，这里是值侧校验。
	ErrOverrideValueInvalid = errors.New("recommend-rank: invalid experiment override value")
	// ErrRequestIDReused 同一 request_id 被用于不同的候选集：幂等锚点被误用，
	// 既不能回放别人的结果，也不能静默重算第二条决策。
	ErrRequestIDReused = errors.New("recommend-rank: request_id reused with different candidates")
	// ErrInvalidPage 分页参数非法（pn/ps 非正）。与 ErrPageTooDeep 区分：
	// 前者是「这一页无法解释」，后者是「页码/页大小超出允许的扫描窗口」。
	ErrInvalidPage = errors.New("recommend-rank: invalid page arguments")
)

// 候选来源（与 rpc/rank.proto 的 RankSource 编号一一对应，
// 并与 rpc/recall.proto 的 Source 同名同值；一致性由
// internal/logic/contract_consistency_test.go 断言，禁止漂移）。
const (
	// SourceHot 热门池候选。
	SourceHot int32 = 1
	// SourceFollow 关注池候选。
	SourceFollow int32 = 2
	// SourceTag 标签池候选。
	SourceTag int32 = 3
	// SourceCollab 协同候选。
	SourceCollab int32 = 4
	// SourceVector 向量候选。
	SourceVector int32 = 5
	// SourceCold 冷启动池候选。
	SourceCold int32 = 6
)

// ValidSource 判定来源是否在枚举内。
func ValidSource(source int32) bool { return source >= SourceHot && source <= SourceCold }

// SourcePriority 返回兜底排序（回退召回原序）时的路间优先级，数值越小越优先。
// 与 recommend-recall 的 SourcePriority 同序：个性化路优先于泛化路。
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

// 模型版本状态（与 rpc ModelVersionState 对应）。
const (
	// ModelStateDraft 登记中，不可上线。
	ModelStateDraft int32 = 1
	// ModelStateReady 校验通过，可激活。
	ModelStateReady int32 = 2
	// ModelStateActive 当前生效（每个 model_key 至多一个）。
	ModelStateActive int32 = 3
	// ModelStateRetired 已下线（保留供审计与回滚）。
	ModelStateRetired int32 = 4
)

// ValidModelState 判定模型状态是否在枚举内。
func ValidModelState(state int32) bool { return state >= ModelStateDraft && state <= ModelStateRetired }

// CanTransitionModelState 判定迁移是否合法：
// DRAFT -> READY -> ACTIVE -> RETIRED，READY -> RETIRED，ACTIVE -> RETIRED（下线）。
// 禁止 RETIRED -> ACTIVE 的"复活"：回滚请用另一个仍在 ACTIVE/READY 的版本，
// 保证审计时间线上每次激活都有唯一先后。
func CanTransitionModelState(from, to int32) bool {
	if !ValidModelState(from) || !ValidModelState(to) || from == to {
		return false
	}
	switch from {
	case ModelStateDraft:
		return to == ModelStateReady || to == ModelStateRetired
	case ModelStateReady:
		return to == ModelStateActive || to == ModelStateRetired
	case ModelStateActive:
		return to == ModelStateRetired
	default:
		return false
	}
}

// 实验状态（与 rpc ExperimentState 对应）。
const (
	// ExpStateDraft 草稿。
	ExpStateDraft int32 = 1
	// ExpStateRunning 分流中。
	ExpStateRunning int32 = 2
	// ExpStatePaused 暂停（已分桶主体保持不变）。
	ExpStatePaused int32 = 3
	// ExpStateStopped 结束（终态）。
	ExpStateStopped int32 = 4
)

// ValidExpState 判定实验状态是否在枚举内。
func ValidExpState(state int32) bool { return state >= ExpStateDraft && state <= ExpStateStopped }

// CanTransitionExpState 判定实验状态迁移是否合法。STOPPED 是终态，不可回退。
func CanTransitionExpState(from, to int32) bool {
	if !ValidExpState(from) || !ValidExpState(to) || from == to {
		return false
	}
	switch from {
	case ExpStateDraft:
		return to == ExpStateRunning || to == ExpStateStopped
	case ExpStateRunning:
		return to == ExpStatePaused || to == ExpStateStopped
	case ExpStatePaused:
		return to == ExpStateRunning || to == ExpStateStopped
	default:
		return false
	}
}

// 分桶主体类型（与 rpc SubjectType 对应）。
const (
	// SubjectMid 主体是登录用户（subject_id 为 mid 十进制字符串）。
	SubjectMid int32 = 1
	// SubjectDevice 主体是设备受控摘要（sha256 hex）。
	SubjectDevice int32 = 2
)

// ValidSubjectType 判定主体类型是否在枚举内。
func ValidSubjectType(t int32) bool { return t == SubjectMid || t == SubjectDevice }

// 客户端平台（与 rpc Platform 对应，编号与 recommend-recall 一致；
// 本服务只用它做决策摘要的维度切分，不写死任何端 UI 行为）。
const (
	// PlatformUnspecified 未指定平台。
	PlatformUnspecified int32 = 0
	// PlatformAndroid Android 客户端。
	PlatformAndroid int32 = 1
	// PlatformIOS iOS 客户端。
	PlatformIOS int32 = 2
	// PlatformHarmony HarmonyOS 客户端。
	PlatformHarmony int32 = 3
	// PlatformDesktop 电脑客户端。
	PlatformDesktop int32 = 4
)

// ValidPlatform 判定平台是否在枚举内。
func ValidPlatform(p int32) bool { return p >= PlatformUnspecified && p <= PlatformDesktop }

// DefaultBucketCount 是默认分桶空间大小（千分位），与 rpc GetExperimentAssignmentReq.bucket_count=0 的取值一致。
const DefaultBucketCount int32 = 1000

// ValidBucketRange 判定变体分桶区间是否合法：左闭右开且落在 [0, bucketCount) 内。
func ValidBucketRange(start, end, bucketCount int32) bool {
	if bucketCount <= 0 {
		bucketCount = DefaultBucketCount
	}
	return start >= 0 && end > start && end <= bucketCount
}

// ValidateSubjectID 校验主体标识：MID 必须是正十进制，DEVICE 必须是 64 位 sha256 hex。
// 明文设备号（长度不是 64 或含非 hex 字符）直接被拒，避免本服务成为可反查的设备表。
func ValidateSubjectID(subjectType int32, subjectID string) error {
	switch subjectType {
	case SubjectMid:
		if subjectID == "" || subjectID == "0" {
			return ErrSubjectIDRequired
		}
		for i := 0; i < len(subjectID); i++ {
			if subjectID[i] < '0' || subjectID[i] > '9' {
				return ErrSubjectIDRequired
			}
		}
		return nil
	case SubjectDevice:
		if !IsSha256Hex(subjectID) {
			return ErrRawDeviceID
		}
		return nil
	default:
		return ErrInvalidSubjectType
	}
}

// IsSha256Hex 判定是否为 64 位十六进制摘要（大小写均可）。
func IsSha256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

// 受控优化目标（AGENTS.md §7：只有内容与行为质量目标，
// 不接受广告、付费转化、会员等商业化目标；新增必须先在此登记）。
const (
	// ObjectiveClick 预估点击率。
	ObjectiveClick = "pred_click"
	// ObjectiveFinish 预估完播率。
	ObjectiveFinish = "pred_finish"
	// ObjectiveInteract 预估互动率（点赞/评论/弹幕/关注）。
	ObjectiveInteract = "pred_interact"
	// ObjectiveNegative 预估负反馈率（不喜欢/举报），参与扣分而非加分。
	ObjectiveNegative = "pred_negative"
)

// SupportedObjectives 返回已登记的优化目标集合。
func SupportedObjectives() map[string]struct{} {
	return map[string]struct{}{
		ObjectiveClick:    {},
		ObjectiveFinish:   {},
		ObjectiveInteract: {},
		ObjectiveNegative: {},
	}
}

// ValidObjective 判定目标 key 是否受控。
func ValidObjective(name string) bool {
	_, ok := SupportedObjectives()[name]
	return ok
}

// 特征缺失策略（rpc UpsertFeatureConfigReq.missing_policy 的受控取值）。
const (
	// MissingPolicyDefault 用登记的默认值补齐后继续打分。
	MissingPolicyDefault = "default"
	// MissingPolicyDropSource 该路候选整体不进入本次排序（保守）。
	MissingPolicyDropSource = "drop_source"
	// MissingPolicyReject 特征缺失即视为不可排序，返回降级结果。
	MissingPolicyReject = "reject"
)

// ValidMissingPolicy 判定缺失策略是否受控。
func ValidMissingPolicy(policy string) bool {
	switch policy {
	case MissingPolicyDefault, MissingPolicyDropSource, MissingPolicyReject:
		return true
	default:
		return false
	}
}

// 降级原因 key（与 rpc RankDegradeReason 对应，落库存字符串便于人工审计）。
const (
	// DegradeReasonNone 未降级。
	DegradeReasonNone = ""
	// DegradeReasonModelUnavailable 无 ACTIVE 模型或模型加载失败。
	DegradeReasonModelUnavailable = "model_unavailable"
	// DegradeReasonFeatureUnavailable 特征读取下游不可用。
	DegradeReasonFeatureUnavailable = "feature_unavailable"
	// DegradeReasonStoreUnavailable Redis/MySQL 不可用。
	DegradeReasonStoreUnavailable = "store_unavailable"
	// DegradeReasonBudgetExhausted 打分预算耗尽。
	DegradeReasonBudgetExhausted = "budget_exhausted"
	// DegradeReasonSafetyUnavailable 内容安全结论不可读。
	DegradeReasonSafetyUnavailable = "safety_unavailable"
	// DegradeReasonExperimentUnavailable 实验配置不可读，退回默认变体。
	DegradeReasonExperimentUnavailable = "experiment_unavailable"
	// DegradeReasonEmptyCandidates 入参候选为空。
	DegradeReasonEmptyCandidates = "empty_candidates"
)

// ValidDegradeReason 判定降级原因 key 是否受控。
func ValidDegradeReason(reason string) bool {
	switch reason {
	case DegradeReasonNone, DegradeReasonModelUnavailable, DegradeReasonFeatureUnavailable,
		DegradeReasonStoreUnavailable, DegradeReasonBudgetExhausted, DegradeReasonSafetyUnavailable,
		DegradeReasonExperimentUnavailable, DegradeReasonEmptyCandidates:
		return true
	default:
		return false
	}
}

// 兜底策略 key（与 rpc FallbackStrategy 对应）。
const (
	// FallbackNone 未降级。
	FallbackNone = ""
	// FallbackRecallOrder 回退召回原序（默认兜底）。
	FallbackRecallOrder = "recall_order"
	// FallbackPreviousModel 回退上一个 ACTIVE 模型。
	FallbackPreviousModel = "previous_model"
	// FallbackSafetyOnly 只做安全过滤与去重。
	FallbackSafetyOnly = "safety_only"
)

// ValidFallback 判定兜底策略 key 是否受控。
func ValidFallback(fallback string) bool {
	switch fallback {
	case FallbackNone, FallbackRecallOrder, FallbackPreviousModel, FallbackSafetyOnly:
		return true
	default:
		return false
	}
}

// ControlVariant 默认变体名：未命中任何 RUNNING 实验时使用的对照组。
const ControlVariant = "control"

// ExperimentAggregateID 生成实验变体的稳定标识（日志与审计聚合根）。
func ExperimentAggregateID(expKey, variantKey string) string {
	return expKey + "/" + variantKey
}

// JoinFeatureKeys 以稳定顺序（升序去重）渲染特征清单，用于配置指纹。
func JoinFeatureKeys(keys []string) string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// inPlaceholders 生成 n 个逗号分隔的 "?"，用于显式展开 IN 列表。
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（错误号 1062 / Duplicate entry 文本）。
// 与 notification 服务保持同一口径：不引入驱动私有错误类型，避免把幂等命中漏成 500。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}

// joinWhere 用 AND 连接已构造好的条件片段（片段文本只来自本包字面量，值一律走占位符）。
func joinWhere(conditions []string) string {
	if len(conditions) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conditions, " AND ")
}
