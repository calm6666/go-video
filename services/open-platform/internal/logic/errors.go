package logic

import "errors"

// 本文件登记 logic 层的「参数形状」错误。
//
// model 包的 53 个哨兵表达领域不变量（凭证、审批、配额、状态机），跨服务可见；
// 而「某个必填字段没填」「区间太宽」这类纯粹是接口入参问题，不该污染领域错误集，
// 因此留在 logic 包内。两者文案一律带 `open-platform:` 前缀，
// 便于 gateway 按同一口径映射成响应信封 code（AGENTS.md §6）。
var (
	// errNameRequired 应用名缺失（注册与改资料都必填非空，Model 层不设默认名）。
	errNameRequired = errors.New("open-platform: application name required")
	// errDescriptionTooLong 简介超出列宽。
	errDescriptionTooLong = errors.New("open-platform: description too long")
	// errReasonRequired 破坏性/审批操作必须留原因（审计列不得为空串）。
	errReasonRequired = errors.New("open-platform: reason required")
	// errAPICodeRequired 配额与判定路径的接口标识缺失。
	errAPICodeRequired = errors.New("open-platform: api_code required")
	// errMethodPathRequired 网关前置检查缺少 method/path（流水无法解释这次调用）。
	errMethodPathRequired = errors.New("open-platform: method and path required")
	// errInvalidPolicyID 显式传的 policy_id 与唯一键算出的规则不是同一条（避免误改他人规则）。
	errInvalidPolicyID = errors.New("open-platform: policy_id does not match (app_id, api_code, window)")
	// errRecomputeRangeTooWide 重算区间超过配置上限，必须分段（防止把流水表扫成慢查询）。
	errRecomputeRangeTooWide = errors.New("open-platform: recompute range too wide, split it")
	// errRecomputeTooManyWindows 单次重算涉及的窗口数超上限（防止无界批量写）。
	errRecomputeTooManyWindows = errors.New("open-platform: too many quota windows in range")
	// errHighRiskScopeBatch 高风险 scope 不允许与其它 scope 同批授予。
	errHighRiskScopeBatch = errors.New("open-platform: high risk scope cannot be batch granted")
	// errTTLNotConfigured 令牌 TTL 未配置：禁止签发「立刻过期」或「永不过期」的凭证。
	errTTLNotConfigured = errors.New("open-platform: token ttl not configured")
	// errInvalidRevokeTarget 撤销目标缺失或未知（不做「按能推就推」的宽松解释）。
	errInvalidRevokeTarget = errors.New("open-platform: invalid revoke target")
	// errGrantNotFound 授权关系不存在（与「已撤销」区分：撤销是可重入的，不存在是参数错）。
	errGrantNotFound = errors.New("open-platform: grant not found")
	// errInvalidAppStatusFilter status 过滤器传了未定义值：当成「不过滤」会让运营看到意料之外的集合。
	errInvalidAppStatusFilter = errors.New("open-platform: invalid app status filter")
	// errInvalidDeliveryStateFilter 投递状态过滤器传了未定义值。
	errInvalidDeliveryStateFilter = errors.New("open-platform: invalid delivery state filter")
	// errPayloadNotJSON 事件正文必须是合法 JSON（下游按 JSON 解析，非 JSON 落库等于不可用数据）。
	errPayloadNotJSON = errors.New("open-platform: webhook payload must be valid JSON")
	// errPayloadCarriesCredential 事件正文疑似夹带凭证字段：投递正文会落库并按保留期清理，
	// 绝不能成为凭证的第二存储地。
	errPayloadCarriesCredential = errors.New("open-platform: webhook payload must not carry credentials")
	// errEventTooOld 事件发生时间过旧：不补投（防止历史批量回放打爆应用端点）。
	errEventTooOld = errors.New("open-platform: webhook event occurred too long ago")
	// errWebhookEndpointLimit 单应用端点数超上限。
	errWebhookEndpointLimit = errors.New("open-platform: webhook endpoint limit reached")
	// errScopeRequiredForIssue 签发授权码时 scope 为空：没有权限点的授权没有意义，
	// 且空 scope 会让「用户到底同意了什么」无法审计。
	errScopeRequiredForIssue = errors.New("open-platform: at least one scope required")
	// errNameTooLong 应用名超出列宽（64 rune）。
	errNameTooLong = errors.New("open-platform: application name too long")
	// errReasonTooLong 审计原因超出列宽：拒绝而不是截断入库，
	// 截断会让「为什么改了这条规则」的问责链在最后一环失真。
	errReasonTooLong = errors.New("open-platform: reason too long")
	// errStateTooLong 客户端 state 超出列宽。
	errStateTooLong = errors.New("open-platform: state too long")
	// errGraceTooLong 轮换宽限期超出允许范围（[0,86400]）。
	errGraceTooLong = errors.New("open-platform: grace_seconds out of range")
	// errOwnerMismatch 调用者 mid 与撤销目标不匹配（用户只能撤销自己的授权）。
	errOwnerMismatch = errors.New("open-platform: caller is not the authorization owner")
	// errNonceRequired 签名模式缺少 nonce（防重放屏障的唯一入参）。
	errNonceRequired = errors.New("open-platform: nonce required")
	// errTimestampRequired 签名模式缺少或非法时间戳。
	errTimestampRequired = errors.New("open-platform: timestamp required")
	// errTargetStatusRequired 运营通道必须显式给出目标状态：
	// 「is_operator=true 但 target_status=0」若被当成「只改资料」，等于把越权副作用合法化。
	errTargetStatusRequired = errors.New("open-platform: target_status required for operator update")
	// errInvalidTargetStatus 目标状态不在状态机枚举内（未知值一律拒绝，不映射为「不变」）。
	errInvalidTargetStatus = errors.New("open-platform: invalid target_status")
	// errSecretVersionRequired 吊销/轮换定位具体密钥版本时缺 target（避免误伤当前生效密钥）。
	errSecretVersionRequired = errors.New("open-platform: secret version required")
	// errClientMismatch 授权码不属于该 app_id：码与客户端绑定，换客户端即视为窃取。
	errClientMismatch = errors.New("open-platform: authorization code belongs to another client")
	// errGrantOrRevokeRequired 审批批次里 grant 与 revoke 同时为空：没有任何结论的审批
	// 不该产生写操作（也不该前移 app.version，那会让下游校验链路白白冷启动一次）。
	errGrantOrRevokeRequired = errors.New("open-platform: grant or revoke list required")
	// errSecretInvalid client_secret 校验失败（不区分「不存在」与「不匹配」，避免应用枚举）。
	errSecretInvalid = errors.New("open-platform: invalid client credential")
	// errTooManyQuotaAPIs 配额用量汇总视图涉及的接口数超上限：本方法不分页，
	// 因此要么按规则数收敛，要么要求调用方显式指定 api_code，绝不在这里做静默截断
	// （静默截断会让运营以为「这个应用就这些接口有用量」）。
	errTooManyQuotaAPIs = errors.New("open-platform: too many api_codes in quota usage view, specify api_code")
)
