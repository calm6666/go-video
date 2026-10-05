package model

import "errors"

// ErrNotImplemented 表示本轮只落了契约与数据模型，该用例的业务实现尚未落地。
// logic 层把它原样返回给 gRPC，禁止用零值响应伪装成功（AGENTS.md §9）。
var ErrNotImplemented = errors.New("ops-config/model: not implemented")

// 本服务的错误都是「可预期的业务结果」，消息里只出现键名、版本号这类运维需要的标识，
// 绝不带 DSN、SQL 片段或凭据（AGENTS.md §9）。
var (
	// --- 通用入参 ---
	// ErrRequestIDRequired 写接口缺少幂等键。
	ErrRequestIDRequired = errors.New("ops-config: request_id required")
	// ErrOperatorRequired 写接口缺少操作人（admin_id 为 0 时无法归因，直接拒绝）。
	ErrOperatorRequired = errors.New("ops-config: operator_id required")
	// ErrReasonRequired 发布/回滚/启停/刷新缓存必须给原因：这些动作会影响线上展示。
	ErrReasonRequired = errors.New("ops-config: reason required")
	// ErrInvalidPage 分页参数非法。
	ErrInvalidPage = errors.New("ops-config: invalid pagination")
	// ErrBatchTooLarge 批量解析/批量条目超过单次上限。
	ErrBatchTooLarge = errors.New("ops-config: batch size exceeds limit")
	// ErrBatchEmpty 批量入参为空数组（多半是上游 bug）。
	ErrBatchEmpty = errors.New("ops-config: items required")

	// --- 配置项与版本 ---
	// ErrConfigKeyRequired cfg_key 为空。
	ErrConfigKeyRequired = errors.New("ops-config: cfg_key required")
	// ErrConfigNotFound 配置项不存在（或该 scope 下没有）。
	ErrConfigNotFound = errors.New("ops-config: config not found")
	// ErrConfigDisabled 配置项已停用，运行时读取按「未命中」处理而不是报错。
	ErrConfigDisabled = errors.New("ops-config: config is off")
	// ErrConfigExists (cfg_key, scope) 已存在（uniq_key_scope 冲突）。
	// 判定不依赖驱动专有错误码：INSERT ... ON DUPLICATE KEY UPDATE 空更新后
	// RowsAffected == 0 即为冲突（AGENTS.md §5 幂等写入要求）。
	ErrConfigExists = errors.New("ops-config: config key already exists in this scope")
	// ErrConfigKeyInvalid 键名不符合 ^[a-z0-9_.]{1,64}$。
	ErrConfigKeyInvalid = errors.New("ops-config: cfg_key must match ^[a-z0-9_.]{1,64}$")
	// ErrScopeUnknown scope 不在允许集合内（global + 四个端标识）。
	ErrScopeUnknown = errors.New("ops-config: unknown scope")
	// ErrValueTypeUnsupported 值类型不在 string/int/bool/json 之内。
	ErrValueTypeUnsupported = errors.New("ops-config: unsupported value type")
	// ErrValueInvalid 值与声明的类型不符（例如 int 键写了 "abc"）。
	ErrValueInvalid = errors.New("ops-config: value does not match value_type")
	// ErrValueTooLong 值超过 OpsValue.MaxBytes。
	ErrValueTooLong = errors.New("ops-config: value exceeds maximum size")
	// ErrVersionConflict 乐观锁未命中：expect_version 与库里当前正式版本不一致。
	// 语义是「你基于的版本已被别人抢先发布」，调用方必须重读后重试，不能盲重试覆盖。
	ErrVersionConflict = errors.New("ops-config: config version conflict, reload and retry")
	// ErrUnpublished 配置项从未发布过（latest_version = 0），运行时没有可读版本。
	ErrUnpublished = errors.New("ops-config: config has no published version")
	// ErrVersionNotFound 指定版本号不存在（回滚目标或灰度挂载目标非法）。
	ErrVersionNotFound = errors.New("ops-config: config version not found")
	// ErrVersionExists (config_id, version) 已存在：同一次发布被并发重放。
	ErrVersionExists = errors.New("ops-config: config version already exists")
	// ErrChangeTypeInvalid change_type 不在 create/publish/rollback 之内。
	// 版本行是发布历史的唯一事实源，出现第四个取值说明写入路径绕过了状态机，
	// 因此拒写而不是按 create 兜底（AGENTS.md §8：状态只能按合法路径推进）。
	ErrChangeTypeInvalid = errors.New("ops-config: change_type must be create, publish or rollback")
	// ErrRollbackToLatest 回滚目标已经是当前正式版本，属于无效操作。
	ErrRollbackToLatest = errors.New("ops-config: rollback target is already the current version")
	// ErrAuditRefAlreadySet 版本行已记过审计条目，拒绝二次覆盖（补偿任务据此收敛）。
	ErrAuditRefAlreadySet = errors.New("ops-config: audit entry reference already set")
	// ErrReasonTooLong 变更原因超过 OpsValue.MaxReasonLen。
	// 与「截断」相对：理由会进审计摘要并被事后追责引用，截断等于篡改证据。
	ErrReasonTooLong = errors.New("ops-config: reason exceeds configured maximum length")
	// ErrOperatorNameTooLong 操作人展示名快照超长（列宽 VARCHAR(64)，见迁移 000001）。
	ErrOperatorNameTooLong = errors.New("ops-config: operator_name exceeds configured maximum length")
	// ErrValueTypeImmutable 已存在的配置项不允许在发布时改值类型：
	// 类型是「怎么解释这段值」的契约，改类型等于把所有读取方按错的方式解析同一把键。
	// 需要换类型请新建键并迁移读取方（契约里没有独立的改类型入口，见 README 缺口）。
	ErrValueTypeImmutable = errors.New("ops-config: value_type of an existing config key cannot change")
	// ErrReleaseIncomplete request_id 命中的版本行存在、但当前生效指针没有推进，
	// 且该版本也没有挂灰度规则 —— 说明上一次发布在「插版本行」与「推指针」之间被打断。
	// 幂等回放不能据此报成功（那等于把半发布状态藏起来），必须显式失败让运维介入。
	ErrReleaseIncomplete = errors.New("ops-config: previous release wrote a version but never advanced the pointer, manual check required")

	// --- 灰度规则 ---
	// ErrRuleNotFound 规则不存在。
	ErrRuleNotFound = errors.New("ops-config: rollout rule not found")
	// ErrRuleNameRequired 规则名是 upsert 幂等句柄，缺失即无法判定新建还是更新。
	ErrRuleNameRequired = errors.New("ops-config: rollout rule name required")
	// ErrRuleModeRequired mode 为 UNSPECIFIED。
	ErrRuleModeRequired = errors.New("ops-config: rollout mode required")
	// ErrRuleNoCondition 规则没有任何有效条件（百分比 0、无版本区间、无端、无尾号、无白名单），
	// 这类规则要么永真（等于绕过灰度）要么永假，都属配置事故。
	ErrRuleNoCondition = errors.New("ops-config: rollout rule has no effective condition")
	// ErrPercentageOutOfRange 放量百分比不在 0-100。
	ErrPercentageOutOfRange = errors.New("ops-config: percentage must be within 0..100")
	// ErrAppVersionRangeInvalid 版本区间倒挂（min > max）或格式非法。
	ErrAppVersionRangeInvalid = errors.New("ops-config: invalid app version range")
	// ErrWhitelistTooLarge 白名单 mid 数量超过上限（白名单是排障工具，不是放量手段）。
	ErrWhitelistTooLarge = errors.New("ops-config: whitelist too large")
	// ErrMidSuffixInvalid 尾号列表必须是 1..9 个十进制单个数字，如 "0,3,7"。
	ErrMidSuffixInvalid = errors.New("ops-config: mid suffixes must be comma separated digits")
	// ErrRuleTimeRangeInvalid end_at 非 0 但不晚于 start_at。
	ErrRuleTimeRangeInvalid = errors.New("ops-config: rule end_at must be later than start_at")
	// ErrRuleModeMismatch mode 与其对应的必填维度没填（或 FULL 又填了别的维度）：
	// 这种规则的实际生效范围与运营在控制台看到的标签不一致，必须拒写。
	ErrRuleModeMismatch = errors.New("ops-config: rollout mode does not match the filled conditions")
	// ErrRuleStateInvalid state 不在 1/2。
	ErrRuleStateInvalid = errors.New("ops-config: invalid rule state")
	// ErrRuleStateUnchanged 目标状态与当前一致，幂等空操作。
	ErrRuleStateUnchanged = errors.New("ops-config: rule state unchanged")

	// --- 列宽（与 deploy/migrations/ops-config 的 VARCHAR 宽度严格对应）---
	// 这一组的共同点：字段参与唯一键或批量写入，超长必须**拒**，不能交给 MySQL 报 1406
	// ——那会把整批操作变成一条无法定位的「保存失败」。上限数字见 model/limits.go。
	// ErrRuleNameTooLong 规则名超过 MaxRuleNameChars（VARCHAR(64)）。
	ErrRuleNameTooLong = errors.New("ops-config: rollout rule name exceeds column width")
	// ErrAppVersionTooLong 版本边界超过 MaxAppVersionChars（VARCHAR(32)）：
	// 格式合法但长度失控的串不是版本号。
	ErrAppVersionTooLong = errors.New("ops-config: app version exceeds column width")
	// ErrItemIDTooLong 引用的内容主键超过 MaxItemIDChars（VARCHAR(32)）。
	ErrItemIDTooLong = errors.New("ops-config: referenced item_id exceeds column width")
	// ErrRequestIDTooLong 幂等键超过 MaxRequestIDChars（VARCHAR(64)）：
	// 它是 uniq_request_id 的列，超长会让整次写入在库里失败而不是在这里被拒。
	ErrRequestIDTooLong = errors.New("ops-config: request_id exceeds column width")

	// --- 专题 ---
	// ErrTopicNotFound 专题不存在。
	ErrTopicNotFound = errors.New("ops-config: topic not found")
	// ErrTopicSlugRequired 新建专题必须有 slug（端上按此寻址）。
	ErrTopicSlugRequired = errors.New("ops-config: topic slug required")
	// ErrTopicSlugInvalid slug 不符合 ^[a-z0-9][a-z0-9_-]{1,63}$。
	ErrTopicSlugInvalid = errors.New("ops-config: topic slug must match ^[a-z0-9][a-z0-9_-]{1,63}$")
	// ErrTopicSlugConflict slug 已被占用（uniq_slug）。
	ErrTopicSlugConflict = errors.New("ops-config: topic slug already exists")
	// ErrTopicTitleRequired 标题为空。
	ErrTopicTitleRequired = errors.New("ops-config: topic title required")
	// ErrTopicTimeRangeInvalid end_at 非 0 但不晚于 start_at。
	ErrTopicTimeRangeInvalid = errors.New("ops-config: topic end_at must be later than start_at")
	// ErrTopicIDListTooLong 引用列表（zone_ids/tag_ids）超过入库长度上限。
	ErrTopicIDListTooLong = errors.New("ops-config: referenced id list too long")
	// ErrTopicItemLimit 单专题条目数超过上限。
	ErrTopicItemLimit = errors.New("ops-config: topic items exceed limit")
	// ErrTopicPositionNotSequential position 必须从 1 连续：有空洞说明调用方拼接有误，
	// 直接入库会让专题顺序出现无法解释的跳跃。
	ErrTopicPositionNotSequential = errors.New("ops-config: topic item position must be 1..n without gaps")
	// ErrItemRefRequired item_type/item_id 缺失。
	ErrItemRefRequired = errors.New("ops-config: item_type and item_id required")
	// ErrItemTypeUnsupported item_type 不在允许集合内。
	ErrItemTypeUnsupported = errors.New("ops-config: unsupported item type")

	// --- 推荐位 ---
	// ErrSlotNotFound 坑位不存在。
	ErrSlotNotFound = errors.New("ops-config: slot not found")
	// ErrSlotCodeRequired code 为空。
	ErrSlotCodeRequired = errors.New("ops-config: slot code required")
	// ErrSlotCodeInvalid code 不符合 ^[a-z0-9][a-z0-9_.]{1,63}$。
	ErrSlotCodeInvalid = errors.New("ops-config: slot code must match ^[a-z0-9][a-z0-9_.]{1,63}$")
	// ErrSlotCodeConflict code 已被占用（uniq_code）。
	ErrSlotCodeConflict = errors.New("ops-config: slot code already exists")
	// ErrSlotCapacityInvalid capacity 不在 [1, OpsSlot.MaxCapacity]：坑位没有容量上限
	// 等于给网关一个无界结果集。
	ErrSlotCapacityInvalid = errors.New("ops-config: slot capacity out of range")
	// ErrSlotItemLimit 单坑位条目数超过上限。
	ErrSlotItemLimit = errors.New("ops-config: slot items exceed limit")
	// ErrSlotPositionOutOfRange position 必须落在 1..capacity：越界条目永远无法展示，
	// 入库只会成为「看起来配了但永远看不到」的隐性问题。
	ErrSlotPositionOutOfRange = errors.New("ops-config: slot item position must be within 1..capacity")
	// ErrSlotPositionDuplicated 同一坑位内 position 重复。
	ErrSlotPositionDuplicated = errors.New("ops-config: duplicate slot item position")
	// ErrSlotItemDuplicated 同一坑位内重复挂同一内容。
	ErrSlotItemDuplicated = errors.New("ops-config: duplicate slot item")

	// --- 客户端开关 ---
	// ErrSwitchNotFound 开关不存在。
	ErrSwitchNotFound = errors.New("ops-config: client switch not found")
	// ErrSwitchKeyRequired switch_key 为空。
	ErrSwitchKeyRequired = errors.New("ops-config: switch_key required")
	// ErrPlatformRequired platform 为 UNSPECIFIED：开关必须按端定义，
	// 不分端的「能力开关」会让四端行为耦合在一次发布里。
	ErrPlatformRequired = errors.New("ops-config: platform required")
	// ErrPlatformUnknown platform 取值不在四端之内。
	ErrPlatformUnknown = errors.New("ops-config: unknown platform")
	// ErrSwitchConflict (switch_key, platform) 已存在。
	ErrSwitchConflict = errors.New("ops-config: client switch already exists for this platform")

	// --- 缓存刷新 ---
	// ErrRefreshTargetRequired target 为空。
	ErrRefreshTargetRequired = errors.New("ops-config: refresh target required")
	// ErrRefreshTargetUnsupported target 不在 config/topic/slot/all。
	ErrRefreshTargetUnsupported = errors.New("ops-config: unknown refresh target")
	// ErrRefreshReasonRequired target=all 时必须写明全量失效原因：这是故障兜底手段，
	// 无理由的全量刷新等同于对下游的一次自伤式压测。
	ErrRefreshReasonRequired = errors.New("ops-config: reason is required when refreshing all caches")
	// ErrInvalidTTL 建议 TTL 非法（超过上限或为负）。
	ErrInvalidTTL = errors.New("ops-config: invalid ttl")
)

// 通用启停状态（ops_config_item.state / ops_topic.state /
// ops_recommend_slot.state / ops_rollout_rule.state / 各类 item 的 state）。
// 取值与 opsconfig.v1.ItemState 一致。
const (
	StateUnspecified int32 = 0
	StateOn          int32 = 1
	StateOff         int32 = 2
)

// 终端标识（ops_rollout_rule.platforms / ops_recommend_slot.platforms /
// ops_client_switch.platform），取值与 opsconfig.v1.ClientPlatform 一致。
// 只有四端：项目不含小程序（AGENTS.md §1、§6）。
const (
	PlatformAndroid int32 = 1
	PlatformIOS     int32 = 2
	PlatformHarmony int32 = 3
	PlatformDesktop int32 = 4
)

// 灰度判定方式（ops_rollout_rule.mode），取值与 opsconfig.v1.RolloutMode 一致。
const (
	ModeUnspecified int32 = 0
	ModeFull        int32 = 1
	ModePercentage  int32 = 2
	ModeAppVersion  int32 = 3
	ModePlatform    int32 = 4
	ModeMidSuffix   int32 = 5
	ModeWhitelist   int32 = 6
)

// 配置值类型（ops_config_item.value_type / ops_config_version.value_type），
// 取值与 opsconfig.v1.ConfigValueType 一致。
const (
	ValueTypeUnspecified int32 = 0
	ValueTypeString      int32 = 1
	ValueTypeInt         int32 = 2
	ValueTypeBool        int32 = 3
	ValueTypeJSON        int32 = 4
)

// 版本变更类型（ops_config_version.change_type）。
// create 首次发布、publish 常规发布、rollback 回滚（值取自历史版本但仍写入新版本号）。
const (
	ChangeTypeCreate   = "create"
	ChangeTypePublish  = "publish"
	ChangeTypeRollback = "rollback"
)

// 生效范围（ops_config_item.scope）。端标识与平台枚举同名，便于排障时对齐；
// 读取方仍必须靠 platform 过滤，这里只是配置的组织维度而不是 UI 语义。
const (
	ScopeGlobal  = "global"
	ScopeAndroid = "android"
	ScopeIOS     = "ios"
	ScopeHarmony = "harmony"
	ScopeDesktop = "desktop"
)

// 内容条目类型（ops_topic_item.item_type / ops_recommend_slot_item.item_type）。
// 值域刻意收窄：新增类型需要评审（它意味着要引用一个新域的主键形态）。
const (
	ItemTypeUGCVideo   = "ugc_video"   // 引用 video.aid
	ItemTypePGCSeason  = "pgc_season"  // 引用 catalog_season.season_id
	ItemTypePGCEpisode = "pgc_episode" // 引用 catalog_episode.epid
	ItemTypeTopic      = "topic"       // 只允许出现在坑位里，引用 ops_topic.topic_id
)

// 缓存刷新目标（RefreshCacheReq.target）。
const (
	RefreshTargetConfig = "config"
	RefreshTargetTopic  = "topic"
	RefreshTargetSlot   = "slot"
	RefreshTargetAll    = "all"
)

// ValidState 判定通用启停状态（0 由调用方按「默认停用」归一后再传入）。
func ValidState(v int32) bool { return v == StateOn || v == StateOff }

// ValidPlatform 判定端标识。
func ValidPlatform(v int32) bool { return v >= PlatformAndroid && v <= PlatformDesktop }

// ValidRolloutMode 判定灰度方式。
func ValidRolloutMode(v int32) bool { return v >= ModeFull && v <= ModeWhitelist }

// ValidValueType 判定值类型。
func ValidValueType(v int32) bool { return v >= ValueTypeString && v <= ValueTypeJSON }

// ValidItemType 判定内容引用类型；withTopic=false 时禁止 topic
// （topic 只能出现在推荐位，专题里挂专题会形成自引用环）。
func ValidItemType(s string, withTopic bool) bool {
	switch s {
	case ItemTypeUGCVideo, ItemTypePGCSeason, ItemTypePGCEpisode:
		return true
	case ItemTypeTopic:
		return withTopic
	default:
		return false
	}
}

// ValidScope 判定生效范围；空串由调用方归一为 global 后再判定。
func ValidScope(s string) bool {
	switch s {
	case ScopeGlobal, ScopeAndroid, ScopeIOS, ScopeHarmony, ScopeDesktop:
		return true
	default:
		return false
	}
}

// ScopeOfPlatform 把端标识映射成默认生效范围标识，用于「按端配置」的调用方偷懒传空。
// 未知端返回空串，由调用方报 ErrPlatformUnknown。
func ScopeOfPlatform(platform int32) string {
	switch platform {
	case PlatformAndroid:
		return ScopeAndroid
	case PlatformIOS:
		return ScopeIOS
	case PlatformHarmony:
		return ScopeHarmony
	case PlatformDesktop:
		return ScopeDesktop
	default:
		return ""
	}
}
