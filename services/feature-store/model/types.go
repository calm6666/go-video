// Package model 是 feature-store 服务的数据库访问层，只操作 go_video_feature_store
// 库自身的表（AGENTS.md §5：服务只能写自己的 schema）。
//
// 表清单与 deploy/migrations/feature-store/*.sql 严格一致：
//
//	feature_definition       特征定义与版本（含隐私级别、TTL、窗口、默认值）
//	feature_active_version   每个 feature_key 对外生效的版本指针（读路径的唯一入口）
//	feature_value            特征值（含 TTL 与上游口径追溯），可从上游重算/回填
//	feature_version_switch   版本切换审计，只追加不改写
//	feature_backfill_job     回填任务与断点游标
//	feature_write_receipt    批量写入的幂等回执
//
// 隐私边界（AGENTS.md §7、docs/data-design.md §6）：
//   - entity_id 只允许主键十进制串（mid/aid/zone_id/item_id）或受控哈希摘要
//     （设备哈希、IP 摘要）；明文手机号、身份证、原始 IP、明文设备号一律拒绝入库，
//     校验入口是 ValidEntityID；
//   - 特征来源枚举结构上不存在广告/支付/会员语义，新增来源必须先评审契约；
//   - 任何读取都必须把降级原因（Degradation*）显式带出来，服务不把「读不到」伪装成「值为 0」。
package model

import (
	"fmt"
	"strconv"
	"strings"
)

// 特征主体类型，与 rpc.EntityScope 逐值对齐（proto 是契约源，这里是落库形态）。
// entity_id 是字符串列：DEVICE 与 IP_HASH 两个维度只有哈希摘要形态。
const (
	EntityScopeUnspecified int32 = 0
	EntityScopeMid         int32 = 1 // mid 十进制串
	EntityScopeAid         int32 = 2 // aid 十进制串
	EntityScopeZone        int32 = 3 // zone_id 十进制串
	EntityScopeDevice      int32 = 4 // 设备哈希摘要
	EntityScopeQuery       int32 = 5 // 归一化搜索词
	EntityScopeCatalogItem int32 = 6 // item_id 十进制串
	EntityScopeIPHash      int32 = 7 // IP 摘要
)

// ValidEntityScope 判断主体类型是否合法（拒绝 UNSPECIFIED）。
func ValidEntityScope(s int32) bool { return s >= EntityScopeMid && s <= EntityScopeIPHash }

// scopeIsNumericID 判断该主体的 entity_id 是否必须是十进制主键串。
func scopeIsNumericID(s int32) bool {
	return s == EntityScopeMid || s == EntityScopeAid || s == EntityScopeZone ||
		s == EntityScopeCatalogItem
}

// scopeIsHashID 判断该主体是否只接受哈希摘要（长度校验见 ValidEntityID）。
func scopeIsHashID(s int32) bool { return s == EntityScopeDevice || s == EntityScopeIPHash }

// 哈希摘要长度约束：SHA-256 十六进制全长 64，链路里也允许截断到 32
// （playback 的 mid_hash 就是 32 位）。允许 32/40/64 三档，别的长度一律拒绝——
// 「看起来像哈希」的短串最容易是误入库的明文标识符片段。
const (
	minHashHexLen = 32
	maxHashHexLen = 64
)

// ValidEntityID 校验 entity_id 是否符合主体类型的形态约束。
//
// 这是隐私的第一道闸：DEVICE/IP_HASH 必须是十六进制摘要（拒绝任何含非 hex 字符的
// 明文设备号），MID/AID/ZONE/CATALOG_ITEM 必须是正十进制主键，QUERY 只限制长度
// （词本身已由 search-query 侧规范化）。
func ValidEntityID(scope int32, entityID string) bool {
	id := strings.TrimSpace(entityID)
	if id == "" || len(id) > maxEntityIDLen {
		return false
	}
	switch {
	case scopeIsNumericID(scope):
		v, err := strconv.ParseInt(id, 10, 64)
		return err == nil && v > 0
	case scopeIsHashID(scope):
		if len(id) != minHashHexLen && len(id) != 40 && len(id) != maxHashHexLen {
			return false
		}
		for i := 0; i < len(id); i++ {
			c := id[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
		return true
	case scope == EntityScopeQuery:
		// 搜索词是最容易夹带隐私的维度：只允许短、无空白、无分隔符的规范化结果。
		return !strings.ContainsAny(id, " \t\r\n/\\@:;,")
	default:
		return false
	}
}

const maxEntityIDLen = 64

// 特征值类型，与 rpc.FeatureValueType 对齐。注册后不可变更：类型漂移会让历史值无法解释。
const (
	ValueTypeUnspecified int32 = 0
	ValueTypeInt64       int32 = 1
	ValueTypeDouble      int32 = 2
	ValueTypeBool        int32 = 3
	ValueTypeString      int32 = 4
	ValueTypeInt64List   int32 = 5
	ValueTypeDoubleList  int32 = 6
)

// ValidValueType 判断值类型是否合法。
func ValidValueType(t int32) bool { return t >= ValueTypeInt64 && t <= ValueTypeDoubleList }

// ValueTypeIsList 判断是否为列表/向量类型（这类值走 list_values 列，受 dimension 约束）。
func ValueTypeIsList(t int32) bool { return t == ValueTypeInt64List || t == ValueTypeDoubleList }

// 特征来源，与 rpc.FeatureSource 对齐。
// 注意这里「没有」的语义：不存在 AD/PAYMENT/MEMBERSHIP/ORDER 等来源（AGENTS.md §7），
// 新增任何来源都必须先评审契约，不能靠加枚举绕过范围控制。
const (
	SourceUnspecified  int32 = 0
	SourceSpmMetric    int32 = 1 // spm 指标投影
	SourceSpmInterest  int32 = 2 // spm 用户兴趣画像
	SourceSpmRetention int32 = 3 // spm 留存口径
	SourceOfflineModel int32 = 4 // 离线模型产出（经回填作业导入）
	SourceRealtimeRule int32 = 5 // 上游实时规则滑窗
	SourceStaticConfig int32 = 6 // 运营静态配置（非行为数据）
)

// ValidSource 判断特征来源是否属于允许的计算链路。
func ValidSource(s int32) bool { return s >= SourceSpmMetric && s <= SourceStaticConfig }

// SourceRequiresWindow 判断该来源是否必须声明非零时间窗口。
// spm 三类指标与离线模型都是「一段窗口内的统计量」，window_seconds=0 意味着
// 全历史累计：口径不可解释、回填无边界、特征值随时间单调漂移，注册时直接拒绝。
// 只有实时规则滑窗（自带 TTL 语义）与静态配置允许 window_seconds=0。
func SourceRequiresWindow(s int32) bool {
	switch s {
	case SourceSpmMetric, SourceSpmInterest, SourceSpmRetention, SourceOfflineModel:
		return true
	default:
		return false
	}
}

// SourceIsBehaviour 判断来源是否基于用户行为数据（决定脱敏与隐私下限）。
func SourceIsBehaviour(s int32) bool { return ValidSource(s) && s != SourceStaticConfig }

// 隐私级别，与 rpc.PrivacyLevel 对齐。数值越大越敏感；0 一律拒绝。
const (
	PrivacyUnspecified      int32 = 0
	PrivacyPublicAggregate  int32 = 1
	PrivacyContentAttribute int32 = 2
	PrivacyPseudonymous     int32 = 3
	PrivacyUserProfile      int32 = 4
)

// ValidPrivacyLevel 判断隐私级别是否已声明（未声明一律拒绝注册与写入）。
func ValidPrivacyLevel(p int32) bool { return p >= PrivacyPublicAggregate && p <= PrivacyUserProfile }

// PrivacyIsIndividual 判断该级别是否与个体身份关联（隐私删除与导出只处理这些级别）。
func PrivacyIsIndividual(p int32) bool { return p >= PrivacyPseudonymous }

// ScopeMinPrivacy / ScopeMaxPrivacy 给出「主体维度 ↔ 隐私级别」的自洽区间。
//
// 为什么必须有这张矩阵：privacy_level 决定谁能读、是否进入隐私删除范围，
// 如果允许把一个 MID 维度的特征标成 PUBLIC_AGGREGATE，调用方就会绕过个体授权
// 直接读到画像；反过来把 AID 维度标成 USER_PROFILE 会让内容热度被无谓限流。
// 因此注册与隐私调整两处都按这张表校验（ErrPrivacyScopeMismatch）。
func ScopeMinPrivacy(scope int32) int32 {
	switch scope {
	case EntityScopeMid, EntityScopeDevice, EntityScopeIPHash:
		return PrivacyPseudonymous
	default:
		return PrivacyPublicAggregate
	}
}

// ScopeMaxPrivacy 返回该主体维度允许的最高隐私级别。
func ScopeMaxPrivacy(scope int32) int32 {
	switch scope {
	case EntityScopeMid, EntityScopeDevice, EntityScopeIPHash:
		return PrivacyUserProfile
	default:
		// 内容与搜索词维度是「聚合/属性」，个体身份不在行上：
		// 声明为 USER_PROFILE 会让删除接口误判它是个体特征（值与个体无关却按个体清）。
		return PrivacyPseudonymous
	}
}

// PrivacyMatchesScope 校验隐私级别与主体类型是否自洽（两端都含）。
func PrivacyMatchesScope(scope, privacy int32) bool {
	return ValidPrivacyLevel(privacy) && privacy >= ScopeMinPrivacy(scope) && privacy <= ScopeMaxPrivacy(scope)
}

// OperatorInPrefixes 判断 operator 是否落在白名单前缀内，供隐私类写操作
// （EraseEntityFeatures）判定调用方身份是否被授权。
//
// 三条刻意的取舍：
//   - 空白名单一律拒绝（fail closed）：漏配结果应该是「删不掉并报警」，
//     而不是「谁都能删」；
//   - 前缀大小写敏感：operator 由各服务按 <kind>:<name> 约定生成，
//     忽略大小写会让 "Admin:" 与 "admin:" 被当成同一个身份；
//   - 名单来自配置而不是写死在 model：跑隐私工单的服务名在各环境不同，
//     写死就等于要么误拒要么改代码。
func OperatorInPrefixes(operator string, prefixes []string) bool {
	op := strings.TrimSpace(operator)
	if op == "" {
		return false
	}
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p != "" && strings.HasPrefix(op, p) {
			return true
		}
	}
	return false
}

// featureKeyPattern 约束 feature_key：小写字母开头，仅小写字母/数字/下划线，长度 2..64。
// 长度上限与 DDL 的 VARCHAR(64) 一致；命名统一走 snake_case 是为了让
// 「前缀过滤 + 按 key 排序」在 ListFeatureDefinitions 里可用同一个索引完成。
const (
	minFeatureKeyLen = 2
	maxFeatureKeyLen = 64
)

// ValidFeatureKey 判断 feature_key 是否符合命名形态（不看语义，语义见 ForbiddenFeatureKey）。
func ValidFeatureKey(key string) bool {
	if len(key) < minFeatureKeyLen || len(key) > maxFeatureKeyLen {
		return false
	}
	if key[0] < 'a' || key[0] > 'z' {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// forbiddenFeatureKeySegments 是范围外语义的「按下划线切分后的整段」名单
// （AGENTS.md §1：会员、订单、支付、投币、广告、创作者分成不在本期范围）。
//
// 这不是隐私策略，也不是靠关键词支撑的安全边界 —— 真正的闸门是 FeatureSource 枚举里
// 根本不存在广告/支付来源（结构上无法注册）。这里只是第二道防线：拦住
// 「合法来源 + 越界命名」把范围外特征塞进特征库；命名一旦进了库，
// 下游 recommend-* 就会把它当输入，范围不能靠口头约定。
//
// 匹配按整段而不是子串：否则 "broad_band"（含 "ad_"）、"encode_v2"（含 "coin"）
// 这类合法命名会被误杀，白名单就会在实践中被绕过。
var forbiddenFeatureKeySegments = map[string]struct{}{
	"ad": {}, "ads": {}, "advert": {}, "advertisement": {}, "advertising": {}, "commercial": {},
	"pay": {}, "payment": {}, "payments": {}, "order": {}, "orders": {}, "checkout": {},
	"vip": {}, "member": {}, "members": {}, "membership": {}, "subscription": {}, "subs": {},
	"coin": {}, "coins": {}, "donate": {}, "tip": {}, "tips": {},
	"revenue": {}, "income": {}, "payout": {}, "commission": {}, "monetize": {},
}

// ForbiddenFeatureKey 判断 feature_key 是否命中范围外语义（整段匹配，见上面的名单注释）。
func ForbiddenFeatureKey(key string) bool {
	for _, seg := range strings.Split(strings.ToLower(strings.TrimSpace(key)), "_") {
		if _, hit := forbiddenFeatureKeySegments[seg]; hit {
			return true
		}
	}
	return false
}

// 降级原因，与 rpc.FeatureDegradation 对齐。
// NONE 之外的每一个值都要求调用方按冷启动/降权策略处理——降级必须被表达出来。
const (
	DegradationUnspecified       int32 = 0
	DegradationNone              int32 = 1
	DegradationDefaultValue      int32 = 2
	DegradationPreviousVersion   int32 = 3
	DegradationExpired           int32 = 4
	DegradationSourceUnavailable int32 = 5
	DegradationFeatureRetired    int32 = 6
)

// ValidDegradation 判断降级原因是否为已声明的枚举值。
func ValidDegradation(d int32) bool { return d >= DegradationNone && d <= DegradationFeatureRetired }

// ReadOutcome 是一次特征读「已经观测到的事实」，由 repository 填充后交给
// ClassifyDegradation 出结论。把事实与判定分开，是为了让 16 个方法里所有读路径
// （GetFeature / BatchGetFeatures / ListEntityFeatures / 版本切换前的预检）
// 共用同一张降级矩阵，而不是各自 if-else 出一套略有差别的降级口径。
type ReadOutcome struct {
	// DefinitionFound feature_definition 里有没有这个 (key, version)。
	DefinitionFound bool
	// State 定义状态（FeatureState*）。
	State int32
	// ResolvedVersion 实际取值的版本（解析过 ACTIVE 指针之后的值）。
	ResolvedVersion int32
	// ValueFound feature_value 里有没有这一行（Redis 命中也算 found）。
	ValueFound bool
	// Expired 命中值但 expire_at 已过（或窗口口径判定为不新鲜）。
	Expired bool
	// AllowStale 调用方是否接受过期旧值。
	AllowStale bool
	// PreviousVersionAvailable 上一个 ACTIVE 版本对同一主体有可用（未过期）值。
	PreviousVersionAvailable bool
	// SourceAvailable Redis 与 DB 至少一条路径可用；false = 上游整体不可用。
	SourceAvailable bool
}

// ClassifyDegradation 按固定优先级给出降级原因与 found。
//
// 优先级（越靠前越优先，不可调换）：
//  1. 定义不存在 → ErrFeatureNotFound：没有定义就没有默认值，返回任何值都是伪造；
//  2. 特征已 RETIRED → FEATURE_RETIRED（默认值 + found=false）；
//  3. 上游整体不可用 → SOURCE_UNAVAILABLE（默认值 + found=false）：
//     这与「没有值」必须区分开，调用方只有据此才能决定是重试还是冷启动；
//  4. 有值且新鲜 → NONE + found=true；
//  5. 有值但过期且 allow_stale → EXPIRED + found=true（可兜底，不可用于训练）；
//  6. 无可用值且上一 ACTIVE 版本有值 → PREVIOUS_VERSION + found=true；
//  7. 其余 → DEFAULT_VALUE + found=false（默认值来自定义，注册时已强制可解析）。
//
// 任何分支都必须返回 ValidDegradation 的值，否则返回 ErrDegradationRequired：
// 「降级原因没填」会让调用方把降级值当真实值用（AGENTS.md §7 的冷启动风险）。
func ClassifyDegradation(o ReadOutcome) (degradation int32, found bool, err error) {
	if !o.DefinitionFound {
		return DegradationUnspecified, false, ErrFeatureNotFound
	}
	switch {
	case o.State == FeatureStateRetired:
		return DegradationFeatureRetired, false, nil
	case !o.SourceAvailable:
		return DegradationSourceUnavailable, false, nil
	case o.ValueFound && !o.Expired:
		return DegradationNone, true, nil
	case o.ValueFound && o.Expired && o.AllowStale:
		return DegradationExpired, true, nil
	case o.PreviousVersionAvailable:
		return DegradationPreviousVersion, true, nil
	default:
		return DegradationDefaultValue, false, nil
	}
}

// CacheTTL 计算特征值 Redis 主读键的存活秒数。
//
// 规则：不超过定义 TTL，且按 jitterRatio 向下随机抖动（randFactor ∈ [0,1)）。
// 抖动的必要性：上游按分钟批量写入，一批键的 expire_at 完全相同；
// 缓存 TTL 若也取整值，就会在同一秒集体失效并让 DB 兜底路径承受惊群。
// 只向下抖（不向上）是因为缓存比库里 expire_at 更久存活等于延长了旧值的可见时间。
func CacheTTL(ttlSeconds int64, jitterRatio, randFactor float64) int64 {
	if ttlSeconds <= 0 {
		return 0
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	if jitterRatio > 0.5 {
		// 抖动上限 50%：再大就有半数键的可用窗口短于半个 TTL，命中率会莫名下滑。
		jitterRatio = 0.5
	}
	if randFactor < 0 {
		randFactor = 0
	}
	if randFactor >= 1 {
		randFactor = 0.999999
	}
	ttl := float64(ttlSeconds) * (1 - jitterRatio*randFactor)
	if ttl < 1 {
		return 1
	}
	if int64(ttl) > ttlSeconds {
		return ttlSeconds
	}
	return int64(ttl)
}

// ValidateBatchReadLimits 校验批量读的条数上限（feature × entity 笛卡尔积）。
//
// 超上限返回 ErrTooManyEntries 而不是静默截断：调用方截断后拿到的是「半个候选集」，
// 排序会以为这些就是全部输入，比报错危险得多。
// 返回 entries 供上层与响应大小上限（Read.MaxBatchResponseBytes）一起记账。
func ValidateBatchReadLimits(nFeatures, nEntities int) (entries int, err error) {
	if nFeatures <= 0 || nEntities <= 0 {
		return 0, ErrFeatureKeyRequired
	}
	if nFeatures > MaxBatchFeatures {
		return 0, fmt.Errorf("%w: %d features > %d", ErrTooManyEntries, nFeatures, MaxBatchFeatures)
	}
	if nEntities > MaxBatchEntities {
		return 0, fmt.Errorf("%w: %d entities > %d", ErrTooManyEntries, nEntities, MaxBatchEntities)
	}
	entries = nFeatures * nEntities
	if entries > MaxBatchReadEntries {
		return 0, fmt.Errorf("%w: %d entries > %d", ErrTooManyEntries, entries, MaxBatchReadEntries)
	}
	return entries, nil
}

// ValidateBatchWriteRows 校验批量写的行数上限（超限报错，不截断，理由同上）。
func ValidateBatchWriteRows(nRows int) error {
	if nRows <= 0 {
		return ErrMalformedValue
	}
	if nRows > MaxBatchWriteRows {
		return fmt.Errorf("%w: %d rows > %d", ErrTooManyRows, nRows, MaxBatchWriteRows)
	}
	return nil
}

// ValidatePageSize 校验列表分页参数：pn 从 1 起、ps 落在 1..MaxListPageSize。
// 与 Filter.Normalize 的夹取不同，这里是「对外契约的入参校验」：
// 契约里写了上限，就要在越界时明确报错，让调用方知道是自己传大了。
func ValidatePageSize(pn, ps int32) error {
	if pn < 1 {
		return fmt.Errorf("%w: pn must start from 1", ErrLimitTooLarge)
	}
	if ps < 1 || ps > MaxListPageSize {
		return fmt.Errorf("%w: ps %d out of 1..%d", ErrLimitTooLarge, ps, MaxListPageSize)
	}
	return nil
}

// 特征状态机，与 rpc.FeatureState 对齐。RETIRED 不接受写入，读侧返回默认值并标降级。
const (
	FeatureStateUnspecified int32 = 0
	FeatureStateDraft       int32 = 1
	FeatureStateActive      int32 = 2
	FeatureStateRetired     int32 = 3
)

// ValidFeatureState 判断状态是否合法。
func ValidFeatureState(s int32) bool { return s >= FeatureStateDraft && s <= FeatureStateRetired }

// ValidFeatureStateTransition 校验状态迁移。
// 规则：DRAFT→ACTIVE→RETIRED 单向；RETIRED 不能复活（复活会让历史值在无人复核的情况下
// 重新参与排序），DRAFT 也不能直接跳 RETIRED 以外的组合。
func ValidFeatureStateTransition(from, to int32) bool {
	switch from {
	case FeatureStateDraft:
		return to == FeatureStateActive || to == FeatureStateRetired
	case FeatureStateActive:
		return to == FeatureStateRetired
	case FeatureStateRetired:
		return false
	default:
		return false
	}
}

// 回填作业状态机，与 rpc.BackfillState 对齐。
const (
	BackfillStateUnspecified int32 = 0
	BackfillStatePending     int32 = 1
	BackfillStateRunning     int32 = 2
	BackfillStateSucceeded   int32 = 3
	BackfillStateFailed      int32 = 4
	BackfillStateCancelled   int32 = 5
)

// ValidBackfillState 判断回填状态是否合法。
func ValidBackfillState(s int32) bool {
	return s >= BackfillStatePending && s <= BackfillStateCancelled
}

// IsBackfillTerminal 判断回填状态是否为终态（终态不可回退）。
func IsBackfillTerminal(s int32) bool {
	return s == BackfillStateSucceeded || s == BackfillStateFailed || s == BackfillStateCancelled
}

// ValidBackfillTransition 校验作业状态迁移。
//
//	PENDING  → RUNNING（Claim）| CANCELLED（提交方中止，尚未开跑）
//	RUNNING  → RUNNING（租约续期/重复认领）| SUCCEEDED | FAILED | CANCELLED
//	终态     → 任何值都非法
//
// 两个刻意不提供的迁移：
//   - PENDING → FAILED：worker 必须先 Claim 才能观测到失败，跳过 RUNNING 会让
//     「谁在什么时间开始跑这个作业」在审计里消失；
//   - RUNNING → PENDING（退回重跑）：接管由「租约过期的 RUNNING 可被再次 Claim」表达，
//     退回 PENDING 会丢 entities_done 的归属信息，进度会被重复累加。
//
// from == to 的 RUNNING 自迁移是允许的（AddProgress 心跳即在此状态），
// 其余自迁移一律非法：终态「再写一次同样的终态」应该由 RowsAffected = 0 表达为幂等空操作，
// 而不是把 finished_at 刷成新的时间。
func ValidBackfillTransition(from, to int32) bool {
	switch from {
	case BackfillStatePending:
		return to == BackfillStateRunning || to == BackfillStateCancelled
	case BackfillStateRunning:
		return to == BackfillStateRunning || to == BackfillStateSucceeded ||
			to == BackfillStateFailed || to == BackfillStateCancelled
	default:
		// UNSPECIFIED 与三种终态都没有出路。
		return false
	}
}

// 批量读写的硬上限：超上限直接报错而不是静默截断，
// 否则调用方会以为拿到了全集（契约里明确写入的语义）。
const (
	// MaxBatchFeatures 单次 BatchGetFeatures 的特征数上限。
	MaxBatchFeatures = 50
	// MaxBatchEntities 单次 BatchGetFeatures 的主体数上限。
	MaxBatchEntities = 20
	// MaxBatchReadEntries 单次批量读的笛卡尔积条目上限（50 × 20）。
	MaxBatchReadEntries = MaxBatchFeatures * MaxBatchEntities
	// MaxBatchWriteRows 单次 WriteFeatures 的行数上限。
	MaxBatchWriteRows = 500
	// MaxListPageSize 列表接口 ps 上限。
	MaxListPageSize = 100
	// MaxPurgeRows 单次 PurgeExpired 的清理行数上限。
	MaxPurgeRows = 5000
	// MaxBackfillEntityIDs 回填显式主体列表的条数上限。
	MaxBackfillEntityIDs = 1000
	// MaxBackfillEntityIDsBytes 回填显式主体列表编码后的字节上限。
	// 单靠条数上限挡不住：1000 个 64 字符摘要加 999 个分隔符就是 65999 字节，
	// 已经超过 MySQL TEXT 列宽（65535），插入时才报错会变成作业提交环节的意外失败。
	// 因此按列宽再卡一道（取 65000，留余量给字符集与转义）：
	// 超限的正确做法是改走「全量扫描」或按窗口拆多个作业，而不是把一列撑成 BLOB。
	MaxBackfillEntityIDsBytes = 65000
	// MaxDimension 列表/向量特征的元素数上限（不引入外部向量库，维度必须受控）。
	MaxDimension = 512
	// MaxStringValueLen 标量字符串值的字节上限（特征值不是自由文本存储）。
	MaxStringValueLen = 512
	// MaxListValueBytes 列表值序列化后的字节上限（feature_value.list_values 是 TEXT，
	// 但「能放下」不等于「允许放」：无界向量会让一次 MGET 回源的响应体不可控，
	// 因此按 MaxDimension 个 double 的最坏宽度再收一档）。
	MaxListValueBytes = 12288
	// MaxEntityIDLen entity_id 列宽（与 DDL VARCHAR(64) 一致）。
	MaxEntityIDLen = maxEntityIDLen
	// MaxSourceMetricKeyLen 上游口径追溯键的列宽。
	MaxSourceMetricKeyLen = 128
	// MaxBatchResponseBytes 单次批量读的响应体硬上限（1 MiB）。
	// 条数上限挡不住「50 个 512 维向量」这种组合：条目数合法但响应能长到几十 MB，
	// 会把 gRPC 消息上限和调用方内存一起打穿，因此响应字节数是独立的第二道闸。
	// 配置项 Read.MaxBatchResponseBytes 只能下调、不能超过本常量。
	MaxBatchResponseBytes = 1 << 20
)

// Redis 主读缓存的键方案（DB 是兜底与真相，缓存 miss 必须能回源）。
//
//	值：      <prefix>:val:<feature_key>:<version>:<entity_scope>:<entity_id>
//	ACTIVE 指针：<prefix>:active:<feature_key>
//
// 键里带 version 是刻意的：切换 ACTIVE 版本时不需要清理历史版本的值键，
// 也让 PREVIOUS_VERSION 降级能直接命中旧版本缓存。TTL 由定义里的 ttl_seconds 决定。
const (
	RedisKeyPrefix       = "fs"
	RedisValueKeyFormat  = RedisKeyPrefix + ":val:%s:%d:%d:%s"
	RedisActiveKeyFormat = RedisKeyPrefix + ":active:%s"
)

// ValueCacheKey 返回特征值的 Redis 主读键。version 必须是已解析的具体版本：
// 键里带 version 是刻意的，切换 ACTIVE 版本时不必清理历史值键，
// PREVIOUS_VERSION 降级也能直接命中旧版本缓存。
func ValueCacheKey(featureKey string, version, entityScope int32, entityID string) string {
	return fmt.Sprintf(RedisValueKeyFormat, featureKey, version, entityScope, entityID)
}

// ActiveVersionCacheKey 返回 ACTIVE 版本指针的缓存键。
func ActiveVersionCacheKey(featureKey string) string {
	return fmt.Sprintf(RedisActiveKeyFormat, featureKey)
}

// 数值列表（int64/double）的落库编解码：list_values 列只允许逗号分隔的纯数字，
// 不放 JSON、不放自由文本，因此 dimension 上限可以逐元素校验。
const listSeparator = ","

// FormatInt64List 编码 int64 列表；超过 dimension 返回 false（不静默截断）。
func FormatInt64List(values []int64, dimension int32) (string, bool) {
	if !withinDimension(len(values), dimension) {
		return "", false
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, strconv.FormatInt(v, 10))
	}
	return strings.Join(parts, listSeparator), true
}

// FormatFloat64List 编码 double 列表（定点表示，避免 1e-9 之类的科学计数法歧义）。
func FormatFloat64List(values []float64, dimension int32) (string, bool) {
	if !withinDimension(len(values), dimension) {
		return "", false
	}
	parts := make([]string, 0, len(values))
	for _, v := range values {
		// 'f' 定点 + 最短表示：科学计数法（1e-09）会被 MySQL 的 DOUBLE 解析成另一个值，
		// 而 DECIMAL/字符串比对会不等，回填校验就会假报警。
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64))
	}
	return strings.Join(parts, listSeparator), true
}

// ParseInt64List 解码 int64 列表。任何元素非法就整体失败：
// 半截向量比报错危险得多（模型会把它当成合法输入）。
func ParseInt64List(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, listSeparator)
	out := make([]int64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil {
			return nil, ErrMalformedListValue
		}
		out = append(out, v)
	}
	return out, nil
}

// ParseFloat64List 解码 double 列表。
func ParseFloat64List(raw string) ([]float64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, listSeparator)
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return nil, ErrMalformedListValue
		}
		out = append(out, v)
	}
	return out, nil
}

func withinDimension(n int, dimension int32) bool {
	if dimension <= 0 {
		// 定义没写 dimension 的列表特征本身就是契约违规，但上限仍要守住。
		return n <= MaxDimension
	}
	if dimension > MaxDimension {
		dimension = MaxDimension
	}
	return int32(n) <= dimension
}
