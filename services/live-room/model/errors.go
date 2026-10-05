package model

import (
	"errors"
	"sort"
	"strings"
)

// ErrNotImplemented 表示本轮只落了契约、模型与迁移，该用例的业务实现尚未落地。
// logic 层必须把它原样返回给 gRPC，禁止用零值响应伪装成功（AGENTS.md §9）。
var ErrNotImplemented = errors.New("liveroom/model: not implemented")

// live-room 域哨兵错误。logic 层把它们映射为稳定的 gRPC status；
// 错误消息不得含 SQL 片段、下游原始响应、推流密钥或任何明文 IP/设备号（AGENTS.md §7/§9）。
var (
	// ErrInvalidMid 主播/操作者 mid 非法（<=0）。
	ErrInvalidMid = errors.New("liveroom: invalid mid")
	// ErrInvalidRoomID room_id 非法。
	ErrInvalidRoomID = errors.New("liveroom: invalid room_id")
	// ErrInvalidSessionID session_id 非法。
	ErrInvalidSessionID = errors.New("liveroom: invalid session_id")
	// ErrInvalidAreaID area_id 非法。
	ErrInvalidAreaID = errors.New("liveroom: invalid area_id")
	// ErrRequestIDRequired 写接口缺少幂等键 request_id。
	ErrRequestIDRequired = errors.New("liveroom: request_id required")
	// ErrEventIDRequired ReportStreamState / ApplyRoomModerationResult 缺少 event_id。
	ErrEventIDRequired = errors.New("liveroom: event_id required")
	// ErrTitleInvalid 房间标题为空或超过 80 字符（按 rune 计）。
	ErrTitleInvalid = errors.New("liveroom: invalid title, 1..80 characters required")
	// ErrCoverTooLong 封面引用超长（只允许 object key 或站内相对地址）。
	ErrCoverTooLong = errors.New("liveroom: cover reference too long")

	// ErrRoomNotFound 房间不存在。
	ErrRoomNotFound = errors.New("liveroom: room not found")
	// ErrSessionNotFound 场次不存在。
	ErrSessionNotFound = errors.New("liveroom: session not found")
	// ErrNoActiveSession 房间当前没有进行中场次。
	ErrNoActiveSession = errors.New("liveroom: no active session")
	// ErrRoomFinished 房间已处于 FINISHED 终态，不可再改资料或开播（AGENTS.md §8 保留审计）。
	ErrRoomFinished = errors.New("liveroom: room is finished")
	// ErrRoomStateNotEditable 当前房间状态不允许修改资料（仅 PENDING/READY 可改）。
	ErrRoomStateNotEditable = errors.New("liveroom: room state is not editable")
	// ErrRoomBanned 房间处于禁播中，操作被拒绝。
	ErrRoomBanned = errors.New("liveroom: room is banned")
	// ErrRoomNotBanned 房间没有生效禁播记录，LiftBan 无事可做。
	ErrRoomNotBanned = errors.New("liveroom: room has no active ban")
	// ErrRoomLimitExceeded 单主播生效房间数超过配置上限。
	ErrRoomLimitExceeded = errors.New("liveroom: owner room count limit exceeded")
	// ErrConcurrentUpdate 状态或版本号被并发修改，本次写入未生效，调用方可安全重试。
	ErrConcurrentUpdate = errors.New("liveroom: concurrent state update")

	// ErrInvalidRoomTransition 房间状态非法迁移（矩阵见 CanRoomTransition）。
	ErrInvalidRoomTransition = errors.New("liveroom: illegal room state transition")
	// ErrInvalidVerifyTransition 资料审核状态非法迁移。
	ErrInvalidVerifyTransition = errors.New("liveroom: illegal verify state transition")
	// ErrInvalidSessionTransition 场次状态非法迁移。
	ErrInvalidSessionTransition = errors.New("liveroom: illegal session state transition")
	// ErrInvalidReplayTransition 回放状态非法迁移。
	ErrInvalidReplayTransition = errors.New("liveroom: illegal replay state transition")
	// ErrInvalidBanTransition 禁播记录状态非法迁移。
	ErrInvalidBanTransition = errors.New("liveroom: illegal ban record state transition")
	// ErrNotVerified 资料审核未通过，不能进入 READY 或开播。
	ErrNotVerified = errors.New("liveroom: room profile not verified")

	// ErrAnchorNotFound 主播绑定记录不存在。
	ErrAnchorNotFound = errors.New("liveroom: anchor binding not found")
	// ErrAnchorForbidden 操作者不是房主/生效联合主播，无权执行该操作。
	ErrAnchorForbidden = errors.New("liveroom: operation forbidden for this mid")
	// ErrCannotUnbindOwner 房主不能通过 MutateAnchor 解绑（换房主是独立运营流程）。
	ErrCannotUnbindOwner = errors.New("liveroom: owner binding cannot be unbound here")
	// ErrDuplicateOwner 一个房间同一时刻只能有一个生效房主。
	ErrDuplicateOwner = errors.New("liveroom: room already has an active owner")
	// ErrAnchorRoleInvalid 绑定角色非法（ANCHOR_ROLE_UNSPECIFIED）。
	ErrAnchorRoleInvalid = errors.New("liveroom: invalid anchor role")
	// ErrAnchorActionInvalid 绑定动作非法（ANCHOR_ACTION_UNSPECIFIED）。
	ErrAnchorActionInvalid = errors.New("liveroom: invalid anchor action")
	// ErrAnchorLimitExceeded 单主播生效绑定房间数超过配置上限。
	ErrAnchorLimitExceeded = errors.New("liveroom: anchor binding count limit exceeded")

	// ErrAreaNotFound 分区不存在。
	ErrAreaNotFound = errors.New("liveroom: area not found")
	// ErrAreaDisabled 分区已停用，不能被新房间选用。
	ErrAreaDisabled = errors.New("liveroom: area is disabled")
	// ErrAreaNameConflict 分区名已存在（uniq_area_name）。
	ErrAreaNameConflict = errors.New("liveroom: area name already exists")
	// ErrAreaNameInvalid 分区名为空或超过 32 字符。
	ErrAreaNameInvalid = errors.New("liveroom: invalid area name, 1..32 characters required")
	// ErrAreaParentInvalid 上级分区不存在、为自身，或超过两级层级。
	ErrAreaParentInvalid = errors.New("liveroom: invalid parent area")
	// ErrAreaInUse 分区下仍有生效房间，不允许停用。
	ErrAreaInUse = errors.New("liveroom: area still in use by rooms")
	// ErrAreaStateInvalid 分区启停取值非法（只允许 0 停用 / 1 启用）。
	ErrAreaStateInvalid = errors.New("liveroom: invalid area state")

	// ErrStateTypeInvalid 状态流转日志的 state_type 未定义。
	ErrStateTypeInvalid = errors.New("liveroom: invalid state log type")
	// ErrQueryRangeRequired 审计类查询必须给时间范围（追加型大表禁止无界扫描）。
	ErrQueryRangeRequired = errors.New("liveroom: time range is required")
	// ErrIdempotencyKindInvalid 幂等记录类型未定义（既不是 request_id 也不是 event_id）。
	ErrIdempotencyKindInvalid = errors.New("liveroom: invalid idempotency record kind")
	// ErrIdempotencyResultMissing 幂等键已被抢占但结果尚未回填：上一次执行仍在进行中，
	// 调用方应按 retry_after 重试，绝不能重复执行副作用。
	ErrIdempotencyResultMissing = errors.New("liveroom: idempotency result not ready yet")

	// ErrOperatorRequired 运营/系统操作者 operator_mid 缺失或 <=0。
	ErrOperatorRequired = errors.New("liveroom: operator_mid required")
	// ErrBanTypeInvalid 禁播类型非法。
	ErrBanTypeInvalid = errors.New("liveroom: invalid ban type")
	// ErrBanDurationRequired 临时禁播必须给出正数 duration_seconds。
	ErrBanDurationRequired = errors.New("liveroom: ban duration_seconds must be positive")
	// ErrBanNotFound 指定 ban_id 的禁播记录不存在。
	ErrBanNotFound = errors.New("liveroom: ban record not found")
	// ErrBanNotActive 禁播记录已解除或已过期。
	ErrBanNotActive = errors.New("liveroom: ban record is not active")

	// ErrEndReasonInvalid 终止原因非法（RPC 入口只接受 ANCHOR_STOP/STREAM_REPLAY）。
	ErrEndReasonInvalid = errors.New("liveroom: invalid end reason for this entrypoint")
	// ErrSessionNotTerminal 场次未进入终态，不能关联回放。
	ErrSessionNotTerminal = errors.New("liveroom: session is not terminal")
	// ErrSessionRoomMismatch session_id 与 room_id 不属于同一房间（防跨房间串改）。
	ErrSessionRoomMismatch = errors.New("liveroom: session does not belong to the room")
	// ErrReplayStateInvalid AttachReplay 目标状态非法（只允许 PROCESSING/AVAILABLE/REMOVED）。
	ErrReplayStateInvalid = errors.New("liveroom: invalid replay state target")
	// ErrStreamStateInvalid stream_state 不在 live-ingest 约定取值内。
	ErrStreamStateInvalid = errors.New("liveroom: invalid stream state")
	// ErrStreamSeqStale 事件序号不大于已应用的最大序号，按乱序丢弃（不产生写入）。
	ErrStreamSeqStale = errors.New("liveroom: stale stream event sequence")
	// ErrStreamRefMismatch 事件的 stream_id / room_id 与进行中场次登记的不一致。
	ErrStreamRefMismatch = errors.New("liveroom: stream reference does not match the session")

	// ErrSettingInvalid 直播配置字段越界（live_type、min_client_version_code 等）。
	ErrSettingInvalid = errors.New("liveroom: invalid room setting")
	// ErrNoSettingRow 房间还没有配置行（RecordEnabledFor 用：区分「没配过」与「显式关闭」，
	// 不静默按关闭处理，避免录制开关被配置缺失吃掉）。
	ErrNoSettingRow = errors.New("liveroom: room setting row missing")
	// ErrVerdictInvalid 审核结论非法（VERDICT_UNSPECIFIED）。
	ErrVerdictInvalid = errors.New("liveroom: invalid moderation verdict")
	// ErrTaskMismatch 回写的 moderation task_id 与房间当前送审任务不一致（陈旧结论）。
	ErrTaskMismatch = errors.New("liveroom: moderation task mismatch")

	// ErrInvalidPage 分页参数非法（page/page_size 越界）。
	ErrInvalidPage = errors.New("liveroom: invalid pagination")
	// ErrPageSizeTooLarge 每页大小超过服务端上限。
	ErrPageSizeTooLarge = errors.New("liveroom: page_size exceeds limit")
	// ErrCursorInvalid 游标不可解析（ListSessions 的 next_cursor 必须原样回传）。
	ErrCursorInvalid = errors.New("liveroom: invalid cursor")

	// --- 逻辑轮补充的哨兵（契约轮未定义、但 logic 必须显式拒绝的分支）---

	// ErrSessionAlreadyActive 同一房间已存在非终态场次，禁止再开一场。
	ErrSessionAlreadyActive = errors.New("liveroom: room already has a non-terminal session")
	// ErrPlatformInvalid 平台取值不在 Android/iOS/HarmonyOS/桌面端之内（本项目不支持小程序）。
	ErrPlatformInvalid = errors.New("liveroom: invalid client platform")
	// ErrReasonTooLong 审计类文本（禁播原因/驳回原因）超过列宽，拒绝而不是截断，
	// 因为审计证据必须与库内值一致。
	ErrReasonTooLong = errors.New("liveroom: reason too long")
	// ErrDedupIDTooLong request_id / event_id 超过 live_room_idempotency.dedup_key 列宽。
	ErrDedupIDTooLong = errors.New("liveroom: dedup identifier too long")
	// ErrOffsetInvalid 偏移类入参为负（GetSession.offset 表示「从最近一场往前数」）。
	ErrOffsetInvalid = errors.New("liveroom: invalid offset")
	// ErrAnchorNotOwner 操作者不是该房间的生效房主（房主专属动作）。
	ErrAnchorNotOwner = errors.New("liveroom: operator is not the active owner")
	// ErrRequestIDReused 同一个去重标识被两个不同 RPC 用过：live_room_idempotency 的
	// uniq_dedup_key 是全局键、没有按方法分域，此时把别的方法的结果回放出去就是错的，
	// 只能显式拒绝（契约缺口见 services/live-room/README.md）。
	ErrRequestIDReused = errors.New("liveroom: dedup identifier reused by another rpc")
	// ErrRoomOrderInvalid 列表排序取值不在 rpc.RoomOrder 声明的取值内。
	// 排序必须有索引支撑，未知取值退化成「随便排一个」会让翻页不收敛。
	ErrRoomOrderInvalid = errors.New("liveroom: invalid list order")

	// ErrCreatorNotConfigured 未配置 creator RPC：开播资格无法评估，按未通过处理。
	ErrCreatorNotConfigured = errors.New("liveroom: creator rpc not configured")
	// ErrRiskControlNotConfigured 未配置 risk-control RPC：开播风控无法评估，按未通过处理。
	ErrRiskControlNotConfigured = errors.New("liveroom: risk-control rpc not configured")
	// ErrModerationNotConfigured 未配置 moderation RPC：不能伪造「已送审」，写入路径直接失败。
	ErrModerationNotConfigured = errors.New("liveroom: moderation rpc not configured")
	// ErrDownstreamUnavailable 下游 RPC 明确不可用（降级标记，PrepareLive 会把检查项记为 degraded）。
	ErrDownstreamUnavailable = errors.New("liveroom: downstream rpc unavailable")
)

// 房间业务状态，与 live_room.state 列和 rpc.RoomState 取值严格一致。
// 取值已被 RPC 契约锁定，禁止重排或复用（AGENTS.md §8）。
const (
	// RoomStateUnspecified 未指定，视为非法入参。
	RoomStateUnspecified int32 = 0
	// RoomStatePending 待完善：创建后的初始态，资料未审核通过。
	RoomStatePending int32 = 1
	// RoomStateReady 可开播：资料审核通过 + 主播资格与风控检查通过。
	RoomStateReady int32 = 2
	// RoomStateLiving 直播中：存在一个 LIVING 场次。
	RoomStateLiving int32 = 3
	// RoomStateFinished 已关闭：终态，房间不再复用，历史与审计保留。
	RoomStateFinished int32 = 4
	// RoomStateBanned 违规禁播：BanRoom 进入，LiftBan 或到期回 READY/PENDING。
	RoomStateBanned int32 = 5
	// RoomStateDisabled 停用：主播自行停用或运营下架（非违规），可回 READY。
	RoomStateDisabled int32 = 6
)

// 资料审核状态，与 live_room.verify_state 列和 rpc.VerifyState 取值一致。
const (
	// VerifyStateUnspecified 未指定。
	VerifyStateUnspecified int32 = 0
	// VerifyStateNone 未提交审核。
	VerifyStateNone int32 = 1
	// VerifyStateReviewing 审核中（已提交 moderation）。
	VerifyStateReviewing int32 = 2
	// VerifyStatePassed 通过。
	VerifyStatePassed int32 = 3
	// VerifyStateRejected 驳回：需改资料后重新送审。
	VerifyStateRejected int32 = 4
)

// 直播场次状态，与 live_session.state 列和 rpc.SessionState 取值一致。
const (
	// SessionStateUnspecified 未指定。
	SessionStateUnspecified int32 = 0
	// SessionStatePending 已建档，等待推流到达。
	SessionStatePending int32 = 1
	// SessionStateLiving 直播中。
	SessionStateLiving int32 = 2
	// SessionStateEnded 正常下播（终态）。
	SessionStateEnded int32 = 3
	// SessionStateTerminated 异常终止：禁播/关闭房间/断流超时（终态）。
	SessionStateTerminated int32 = 4
)

// 场次终止原因，与 live_session.end_reason 列和 rpc.EndReason 取值一致。
const (
	// EndReasonUnspecified 未指定。
	EndReasonUnspecified int32 = 0
	// EndReasonAnchorStop 主播主动下播（EndLive 可由客户端触发）。
	EndReasonAnchorStop int32 = 1
	// EndReasonBanned 风控/运营禁播（只由 BanRoom 内部写入）。
	EndReasonBanned int32 = 2
	// EndReasonRoomClosed 房间被关闭（只由 CloseRoom 内部写入）。
	EndReasonRoomClosed int32 = 3
	// EndReasonStreamTimeout 断流超过宽限期未重连（只由 ReportStreamState 写入）。
	EndReasonStreamTimeout int32 = 4
	// EndReasonStreamReplay 收到更晚序号的停止事件补偿（EndLive 可幂等补偿）。
	EndReasonStreamReplay int32 = 5
)

// 回放状态，与 live_session.replay_state 列和 rpc.ReplayState 取值一致。
const (
	// ReplayStateUnspecified 未指定。
	ReplayStateUnspecified int32 = 0
	// ReplayStateNone 无回放。
	ReplayStateNone int32 = 1
	// ReplayStateProcessing 录制/转码中（live-media 推进）。
	ReplayStateProcessing int32 = 2
	// ReplayStateAvailable 可回放。
	ReplayStateAvailable int32 = 3
	// ReplayStateRemoved 回放已下架（版权撤回或违规）。
	ReplayStateRemoved int32 = 4
)

// 主播绑定角色，与 live_room_anchor.role 列和 rpc.AnchorRole 取值一致。
const (
	// AnchorRoleUnspecified 未指定。
	AnchorRoleUnspecified int32 = 0
	// AnchorRoleOwner 房主：一个房间同一时刻只有一个生效房主。
	AnchorRoleOwner int32 = 1
	// AnchorRoleCohost 联合主播（连麦嘉宾，不含商业化分成语义）。
	AnchorRoleCohost int32 = 2
	// AnchorRoleManager 房间管理员（房管）。
	AnchorRoleManager int32 = 3
)

// 主播绑定生效位（live_room_anchor.state / live_room_ban 之外的通用启停取值）。
const (
	// BindStateDisabled 已解绑（软状态，保留行作为审计证据）。
	BindStateDisabled int32 = 0
	// BindStateEnabled 生效。
	BindStateEnabled int32 = 1
)

// 禁播类型，与 live_room_ban.ban_type 列和 rpc.BanType 取值一致。
const (
	// BanTypeUnspecified 未指定。
	BanTypeUnspecified int32 = 0
	// BanTypeTemporary 临时禁播，end_at 必须 > start_at。
	BanTypeTemporary int32 = 1
	// BanTypePermanent 永久禁播，end_at = 0，必须 LiftBan 解除。
	BanTypePermanent int32 = 2
)

// 禁播记录状态，与 live_room_ban.state 列一致。
const (
	// BanStateActive 生效中。
	BanStateActive int32 = 1
	// BanStateLifted 已人工解除。
	BanStateLifted int32 = 2
	// BanStateExpired 到期自动失效。
	BanStateExpired int32 = 3
)

// 客户端平台，与 live_room.platform 列和 rpc.Platform 取值一致（AGENTS.md §6 不写死单端）。
const (
	// PlatformUnspecified 未指定，服务端按 PlatformAndroid 记录。
	PlatformUnspecified int32 = 0
	// PlatformAndroid Android。
	PlatformAndroid int32 = 1
	// PlatformIOS iOS。
	PlatformIOS int32 = 2
	// PlatformHarmony HarmonyOS。
	PlatformHarmony int32 = 3
	// PlatformDesktop 电脑客户端（含 OBS 类直播助手）。
	PlatformDesktop int32 = 4
)

// 直播类型（live_room_setting.live_type）。
const (
	// LiveTypeVideo 视频直播。
	LiveTypeVideo int32 = 1
	// LiveTypeAudio 语音直播。
	LiveTypeAudio int32 = 2
	// LiveTypeScreen 屏幕分享。
	LiveTypeScreen int32 = 3
)

// 房间列表排序（rpc.RoomOrder）。
const (
	// RoomOrderIDDesc room_id 倒序（默认）。
	RoomOrderIDDesc int32 = 0
	// RoomOrderLivingFirst 直播中优先，其次 room_id 倒序。
	RoomOrderLivingFirst int32 = 1
	// RoomOrderCtimeDesc 创建时间倒序。
	RoomOrderCtimeDesc int32 = 2
)

// 状态流转日志的状态类别（live_room_state_log.state_type）。
const (
	// LogTypeRoomState 房间业务状态迁移。
	LogTypeRoomState int32 = 1
	// LogTypeVerifyState 资料审核状态迁移。
	LogTypeVerifyState int32 = 2
	// LogTypeSessionState 场次状态迁移。
	LogTypeSessionState int32 = 3
	// LogTypeReplayState 回放状态迁移。
	LogTypeReplayState int32 = 4
)

// 幂等记录类型（live_room_idempotency.kind）。
const (
	// IdempotencyKindRequest 客户端 request_id（写 RPC 重放）。
	IdempotencyKindRequest int32 = 1
	// IdempotencyKindEvent 上游 event_id（live.state.v1 / moderation.result.v1 投递去重）。
	IdempotencyKindEvent int32 = 2
)

// live-ingest 推流状态（ReportStreamStateReq.stream_state 的约定取值）。
// 本服务不拥有流状态，只按此映射推进房间/场次状态；取值变更属跨服务契约变更。
const (
	// StreamStateIdle 流空闲（未推流）。
	StreamStateIdle int32 = 1
	// StreamStatePublishing 推流中。
	StreamStatePublishing int32 = 2
	// StreamStateInterrupted 中断（宽限期内可重连）。
	StreamStateInterrupted int32 = 3
	// StreamStateStopped 已停止。
	StreamStateStopped int32 = 4
)

// ReportStreamState 的处理结果（ReportStreamStateReply.result）。
const (
	// StreamResultApplied 事件已应用。
	StreamResultApplied int32 = 1
	// StreamResultDuplicate 同一 event_id 重复投递。
	StreamResultDuplicate int32 = 2
	// StreamResultStale seq 乱序，丢弃。
	StreamResultStale int32 = 3
	// StreamResultIllegalTransition 非法迁移，未产生写入。
	StreamResultIllegalTransition int32 = 4
	// StreamResultMismatch 房间/场次/流引用不匹配。
	StreamResultMismatch int32 = 5
)

// roomTransitions 是房间状态机的合法迁移矩阵：key 为当前状态，value 为允许的目标状态集合。
//
// 说明（与 rpc.RoomState 注释、AGENTS.md §8 一致）：
//   - PENDING ↔ READY 由资料审核结论与 PrepareLive 驱动；
//   - READY → LIVING 只能由 StartLive 驱动，LIVING → READY 由 EndLive / 流停止事件驱动；
//   - FINISHED 是终态，任何路径都不得离开（CloseRoom 之后只读）；
//   - BANNED / DISABLED 解禁后的落点取决于 verify_state：
//     审核通过回 READY，否则回 PENDING，因此两条边都保留。
var roomTransitions = map[int32]map[int32]struct{}{
	RoomStatePending:  {RoomStateReady: {}, RoomStateBanned: {}, RoomStateDisabled: {}, RoomStateFinished: {}},
	RoomStateReady:    {RoomStatePending: {}, RoomStateLiving: {}, RoomStateBanned: {}, RoomStateDisabled: {}, RoomStateFinished: {}},
	RoomStateLiving:   {RoomStateReady: {}, RoomStateBanned: {}, RoomStateFinished: {}},
	RoomStateFinished: {},
	RoomStateBanned:   {RoomStatePending: {}, RoomStateReady: {}, RoomStateFinished: {}},
	RoomStateDisabled: {RoomStatePending: {}, RoomStateReady: {}, RoomStateFinished: {}, RoomStateBanned: {}},
}

// verifyTransitions 是资料审核状态机。
// PASSED → REVIEWING 表示改资料后重新送审（UpdateRoomInfo），
// REJECTED 只能经重新送审离开，禁止直接写 PASSED（AGENTS.md §8）。
var verifyTransitions = map[int32]map[int32]struct{}{
	VerifyStateNone:      {VerifyStateReviewing: {}},
	VerifyStateReviewing: {VerifyStatePassed: {}, VerifyStateRejected: {}},
	VerifyStatePassed:    {VerifyStateReviewing: {}},
	VerifyStateRejected:  {VerifyStateReviewing: {}},
}

// sessionTransitions 是场次状态机；ENDED / TERMINATED 为终态。
var sessionTransitions = map[int32]map[int32]struct{}{
	SessionStatePending:    {SessionStateLiving: {}, SessionStateEnded: {}, SessionStateTerminated: {}},
	SessionStateLiving:     {SessionStateEnded: {}, SessionStateTerminated: {}},
	SessionStateEnded:      {},
	SessionStateTerminated: {},
}

// replayTransitions 是回放状态机。PROCESSING → NONE 表示录制失败且确认无回放；
// AVAILABLE → REMOVED 表示版权撤回或违规下架（引用保留，可审计）。
var replayTransitions = map[int32]map[int32]struct{}{
	ReplayStateNone:       {ReplayStateProcessing: {}, ReplayStateAvailable: {}},
	ReplayStateProcessing: {ReplayStateAvailable: {}, ReplayStateNone: {}, ReplayStateRemoved: {}},
	ReplayStateAvailable:  {ReplayStateRemoved: {}},
	ReplayStateRemoved:    {},
}

// banRecordTransitions 是禁播记录自身的状态机（生效 → 解除/过期，均为终态）。
var banRecordTransitions = map[int32]map[int32]struct{}{
	BanStateActive:  {BanStateLifted: {}, BanStateExpired: {}},
	BanStateLifted:  {},
	BanStateExpired: {},
}

// CanRoomTransition 校验房间状态 from → to 是否合法。
// to 为 RoomStateUnspecified 或未知取值一律 false（禁止「未知即放行」）。
func CanRoomTransition(from, to int32) bool {
	return canTransition(roomTransitions, from, to)
}

// CanVerifyTransition 校验资料审核状态 from → to 是否合法。
func CanVerifyTransition(from, to int32) bool {
	return canTransition(verifyTransitions, from, to)
}

// CanSessionTransition 校验场次状态 from → to 是否合法。
func CanSessionTransition(from, to int32) bool {
	return canTransition(sessionTransitions, from, to)
}

// CanReplayTransition 校验回放状态 from → to 是否合法。
func CanReplayTransition(from, to int32) bool {
	return canTransition(replayTransitions, from, to)
}

// CanBanRecordTransition 校验禁播记录状态 from → to 是否合法。
func CanBanRecordTransition(from, to int32) bool {
	return canTransition(banRecordTransitions, from, to)
}

// canTransition 是矩阵查表实现：不在 key 集合内（含 0 与未知值）直接拒绝。
func canTransition(matrix map[int32]map[int32]struct{}, from, to int32) bool {
	targets, ok := matrix[from]
	if !ok {
		return false
	}
	_, ok = targets[to]
	return ok
}

// RoomTransitionTargets 返回 from 的合法目标状态集合（升序），
// 供 README 矩阵一致性测试与运营侧「可执行操作」展示使用。
func RoomTransitionTargets(from int32) []int32 {
	return transitionTargets(roomTransitions, from)
}

// SessionTransitionTargets 返回场次状态的合法目标集合（升序）。
func SessionTransitionTargets(from int32) []int32 {
	return transitionTargets(sessionTransitions, from)
}

// transitionTargets 复制矩阵行并升序排序，避免调用方改到矩阵本身。
func transitionTargets(matrix map[int32]map[int32]struct{}, from int32) []int32 {
	targets := matrix[from]
	out := make([]int32, 0, len(targets))
	for to := range targets {
		out = append(out, to)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidRoomState 判断取值是否是已定义的房间状态（含 UNSPECIFIED 之外的全部）。
func ValidRoomState(state int32) bool {
	_, ok := roomTransitions[state]
	return ok
}

// ValidVerifyState 判断取值是否是已定义的资料审核状态。
func ValidVerifyState(state int32) bool {
	_, ok := verifyTransitions[state]
	return ok
}

// ValidSessionState 判断取值是否是已定义的场次状态。
func ValidSessionState(state int32) bool {
	_, ok := sessionTransitions[state]
	return ok
}

// ValidReplayState 判断取值是否是已定义的回放状态。
func ValidReplayState(state int32) bool {
	_, ok := replayTransitions[state]
	return ok
}

// ValidBanState 判断取值是否是已定义的禁播记录状态。
func ValidBanState(state int32) bool {
	_, ok := banRecordTransitions[state]
	return ok
}

// ValidAnchorRole 判断绑定角色是否合法（房主/联合主播/房管）。
func ValidAnchorRole(role int32) bool {
	switch role {
	case AnchorRoleOwner, AnchorRoleCohost, AnchorRoleManager:
		return true
	default:
		return false
	}
}

// ValidPlatform 判断客户端平台是否合法。
func ValidPlatform(platform int32) bool {
	switch platform {
	case PlatformAndroid, PlatformIOS, PlatformHarmony, PlatformDesktop:
		return true
	default:
		return false
	}
}

// ValidLiveType 判断直播类型是否合法（视频/语音/屏幕分享）。
func ValidLiveType(liveType int32) bool {
	switch liveType {
	case LiveTypeVideo, LiveTypeAudio, LiveTypeScreen:
		return true
	default:
		return false
	}
}

// ValidStreamState 判断 live-ingest 推流状态取值是否在约定范围内。
func ValidStreamState(state int32) bool {
	switch state {
	case StreamStateIdle, StreamStatePublishing, StreamStateInterrupted, StreamStateStopped:
		return true
	default:
		return false
	}
}

// RoomStateIsTerminal 判断房间状态是否为终态（FINISHED）。
// 终态房间禁止改资料、禁止开播，只允许读与审计查询（AGENTS.md §8）。
func RoomStateIsTerminal(state int32) bool {
	return state == RoomStateFinished
}

// SessionStateIsTerminal 判断场次状态是否为终态（ENDED / TERMINATED）。
// AttachReplay 要求场次已终态，避免给进行中的场次挂回放。
func SessionStateIsTerminal(state int32) bool {
	return state == SessionStateEnded || state == SessionStateTerminated
}

// errOwnerNotHeld 是事务内部的控制流错误：换房主时发现 fromMid 已经不是生效房主，
// 返回它触发回滚，再由 TransferOwner 翻译成对外的 ErrConcurrentUpdate。
var errOwnerNotHeld = errors.New("liveroom: owner placeholder not held by from_mid")

// isDuplicateErr 判断底层错误是否为唯一索引冲突（1062 / "Duplicate entry"）。
//
// 这里刻意不导入 go-sql-driver/mysql：该依赖在本仓库 go.mod 里标记为 indirect，
// 直接引用会改变依赖分类并需要改 go.mod（本期禁止，见 services/danmaku 同名 helper）。
// 判定只影响「幂等键/占位列冲突」是否翻译成可重放的语义错误，不吞掉其它错误。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "Error 1062")
}
