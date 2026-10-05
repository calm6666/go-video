package model

import (
	"errors"
	"strings"
)

// ErrNotImplemented 表示该用例的 logic 尚未落地（契约轮占位）。
// 风格与 services/account/internal/repository/repository.go:20 一致：显式哨兵错误。
// live-gateway 尤其重要：任何"下发/租约"接口都不得返回假成功来掩盖越权或通道未接线。
var ErrNotImplemented = errors.New("live-gateway: not implemented")

// live-gateway 域错误。gRPC 直接返回这些哨兵，由调用方映射为 HTTP 信封的非零 code。
// 错误文本脱敏：不含票据原文、签名密钥、IP 明文或载荷正文（AGENTS.md §6）。
var (
	// ErrInvalidRoomID room_id 非正数。
	ErrInvalidRoomID = errors.New("live-gateway: invalid room_id")
	// ErrInvalidMid mid 为负数（0 是合法值：游客，是否允许由配额 allow_guest 决定）。
	ErrInvalidMid = errors.New("live-gateway: invalid mid")
	// ErrEmptyConnID conn_id 为空：租约幂等键需要 (node_id, conn_id)。
	ErrEmptyConnID = errors.New("live-gateway: conn_id is required")
	// ErrEmptyNodeID node_id 为空：路由与租约都必须由 WS 接入层标明承载节点。
	ErrEmptyNodeID = errors.New("live-gateway: node_id is required")
	// ErrEmptyRequestID 写接口缺少幂等键。
	ErrEmptyRequestID = errors.New("live-gateway: request_id is required")
	// ErrRequestIdDuplicated 幂等键唯一索引冲突，调用方应回读既有结果（幂等重放）。
	ErrRequestIdDuplicated = errors.New("live-gateway: request_id already registered")
	// ErrEmptyLeaseID lease_id 为空。
	ErrEmptyLeaseID = errors.New("live-gateway: lease_id is required")
	// ErrEmptyTicket 票据为空。
	ErrEmptyTicket = errors.New("live-gateway: ticket is required")
	// ErrEmptyMessageID 广播缺少 message_id：去重与审计都依赖它，必须由调用方或服务端生成。
	ErrEmptyMessageID = errors.New("live-gateway: message_id is required")
	// ErrEmptyEventID 系统事件缺少 event_id（无法幂等）。
	ErrEmptyEventID = errors.New("live-gateway: event_id is required")
	// ErrEmptyReason 撤销/踢下线/排空必须给出审计原因。
	ErrEmptyReason = errors.New("live-gateway: reason is required")
	// ErrEmptyOperator 运营面写接口必须记录操作者。
	ErrEmptyOperator = errors.New("live-gateway: operator is required")

	// ErrLeaseNotFound 租约不存在（Redis 已过期或从未签发）。
	ErrLeaseNotFound = errors.New("live-gateway: lease not found")
	// ErrLeaseExpired 租约已过期：客户端必须重连，服务端不得静默续期。
	ErrLeaseExpired = errors.New("live-gateway: lease expired")
	// ErrLeaseKicked 租约被强制下线（风控/审核/主播踢人），禁止续租。
	ErrLeaseKicked = errors.New("live-gateway: lease kicked")
	// ErrTripletMismatch (mid, room_id, 有效期) 三元组与凭据不一致：这是越权信号，
	// 一律拒绝并落审计，绝不"成功下发 0 个连接"。
	ErrTripletMismatch = errors.New("live-gateway: credential does not match (mid, room_id)")
	// ErrTicketNotFound 票据不存在。
	ErrTicketNotFound = errors.New("live-gateway: ticket not found")
	// ErrTicketUsed 票据已被使用（一次性凭据，禁止重放）。
	ErrTicketUsed = errors.New("live-gateway: ticket already used")
	// ErrTicketRevoked 票据已被撤销。
	ErrTicketRevoked = errors.New("live-gateway: ticket revoked")
	// ErrTicketExpired 票据已过期。
	ErrTicketExpired = errors.New("live-gateway: ticket expired")
	// ErrReconnectBanned 处于禁止重连窗口内（KickConnection 写入的 ban_until）。
	ErrReconnectBanned = errors.New("live-gateway: reconnect forbidden until ban window ends")

	// ErrRouteNotFound 房间路由不存在。
	ErrRouteNotFound = errors.New("live-gateway: room route not found")
	// ErrNoRoute 房间当前无在线节点路由（广播可丢弃，原因必须是 NO_ROUTE）。
	ErrNoRoute = errors.New("live-gateway: no serving route for room")
	// ErrRouteDraining 路由处于排空中：不接受新连接，只允许退出。
	ErrRouteDraining = errors.New("live-gateway: room route is draining")
	// ErrInvalidTransition 状态机不允许该迁移。
	ErrInvalidTransition = errors.New("live-gateway: invalid state transition")
	// ErrVersionConflict expected_version 与服务端不一致（乐观并发）。
	ErrVersionConflict = errors.New("live-gateway: version conflict, reload and retry")

	// ErrQuotaNotFound 该作用域没有配额配置（调用方需回退到上一层或全局默认）。
	ErrQuotaNotFound = errors.New("live-gateway: access quota not found")
	// ErrInvalidQuotaScope scope 取值非法。
	ErrInvalidQuotaScope = errors.New("live-gateway: invalid quota scope")
	// ErrQuotaExceeded 超过配额（房间/用户维度）：丢弃原因 RATE_LIMITED 的落库形态。
	ErrQuotaExceeded = errors.New("live-gateway: quota exceeded")
	// ErrGuestDenied 该作用域不允许游客（mid=0）接入。
	ErrGuestDenied = errors.New("live-gateway: guest connection not allowed")
	// ErrPayloadTooLarge 载荷超过配额/配置的 Broadcast.MaxPayloadBytes。
	ErrPayloadTooLarge = errors.New("live-gateway: payload too large")
	// ErrMessageDuplicated 同一 (room_id, message_id) 已下发过：幂等丢弃。
	ErrMessageDuplicated = errors.New("live-gateway: duplicated message dropped")
	// ErrInvalidBroadcastKind kind 不在取值范围内（不合法一律拒发，不能当普通消息放过）。
	ErrInvalidBroadcastKind = errors.New("live-gateway: invalid broadcast kind")
	// ErrPermissionDenied 权限矩阵判定为越权（例如 VIEWER 发审核处置消息）。
	ErrPermissionDenied = errors.New("live-gateway: permission denied")
	// ErrTransportUnavailable 跨节点下发通道未接线（本轮 stub），必须显式失败而非静默丢弃。
	ErrTransportUnavailable = errors.New("live-gateway: broadcast transport not wired")
	// ErrSubscriptionNotFound 订阅关系不存在（LeaveRoom 幂等返回 0 行的语义）。
	ErrSubscriptionNotFound = errors.New("live-gateway: subscription not found")
	// ErrPsTooLarge 每页大小超过服务端上限。
	ErrPsTooLarge = errors.New("live-gateway: ps exceeds server limit")
	// ErrScanLimitTooLarge 连接扫描条数超过配置上限（Redis SCAN/SSCAN 保护）。
	ErrScanLimitTooLarge = errors.New("live-gateway: scan limit exceeds server config")
	// ErrInvalidClientInfo 客户端信息不合法：device_id_hash 超长/含空白等，说明客户端误传了设备号明文。
	// 本服务永不接受 IP 与设备号原值（AGENTS.md §6），必须在这里挡住而不是落库后再脱敏。
	ErrInvalidClientInfo = errors.New("live-gateway: invalid client info, digest fields expected instead of raw values")
	// ErrUnknownEventType 系统事件的 event_type 不在白名单内（未知事件放过等于给「任意状态变更」开了通道）。
	ErrUnknownEventType = errors.New("live-gateway: unknown event_type")
	// ErrEmptyEventType 系统事件缺少 event_type。
	ErrEmptyEventType = errors.New("live-gateway: event_type is required")
)

// --- 客户端平台（rpc.Platform）：编号与 playback 一致 ---

const (
	PlatformAndroid int32 = 1
	PlatformIOS     int32 = 2
	PlatformHarmony int32 = 3
	PlatformDesktop int32 = 4
)

// --- 连接角色（rpc.ConnRole）：live-gateway 下发权限的唯一依据 ---

const (
	RoleUnspecified int32 = 0 // 客户端自报/未指定：一律按 RoleViewer 处理并记录告警
	RoleViewer      int32 = 1
	RoleAnchor      int32 = 2
	RoleRoomAdmin   int32 = 3
	RoleOperator    int32 = 4
	RoleService     int32 = 5
)

// ValidRole 判断角色取值是否合法。
func ValidRole(role int32) bool { return role >= RoleUnspecified && role <= RoleService }

// NormalizeRole 把未指定/越界的角色收敛为 VIEWER：
// README 的安全约束——客户端自报角色不可信，本服务不承认任何未识别角色。
func NormalizeRole(role int32) int32 {
	if role < RoleViewer || role > RoleService {
		return RoleViewer
	}
	return role
}

// --- 租约状态（rpc.LeaseState）：事实源是 Redis，DB 只在审计视图里出现同样取值 ---

const (
	LeaseStateUnspecified int32 = 0
	LeaseStateActive      int32 = 1
	LeaseStateExpired     int32 = 2
	LeaseStateReleased    int32 = 3
	LeaseStateKicked      int32 = 4
)

// IsLeaseTerminal 租约终态：RELEASED/KICKED 不可再续租（EXPIRED 可通过重新申请恢复）。
func IsLeaseTerminal(state int32) bool {
	return state == LeaseStateReleased || state == LeaseStateKicked
}

// leaseTransitions 租约状态机（Redis 里的 state 字段与审计视图共用）。
var leaseTransitions = map[int32][]int32{
	LeaseStateActive:   {LeaseStateActive, LeaseStateExpired, LeaseStateReleased, LeaseStateKicked},
	LeaseStateExpired:  {LeaseStateActive}, // 客户端带新凭据重连/续租成功
	LeaseStateKicked:   {},
	LeaseStateReleased: {},
}

// IsValidLeaseTransition 判断租约状态机是否允许 old→new。
func IsValidLeaseTransition(oldState, newState int32) bool {
	for _, allow := range leaseTransitions[oldState] {
		if allow == newState {
			return true
		}
	}
	return false
}

// --- 重连票据状态（rpc.TicketState，落库 live_gw_reconnect_ticket.state） ---

const (
	TicketStateUnspecified int32 = 0
	TicketStateIssued      int32 = 1
	TicketStateUsed        int32 = 2
	TicketStateRevoked     int32 = 3
	TicketStateExpired     int32 = 4
)

// IsTicketTerminal 票据终态：一次性凭据，USED/REVOKED/EXPIRED 之后不再变化。
func IsTicketTerminal(state int32) bool {
	return state >= TicketStateUsed && state <= TicketStateExpired
}

// ticketTransitions 票据状态机。
// USED 与 REVOKED 互斥且都不可逆：撤销已消费的票据没有意义，
// 已撤销的票据也绝不能被 Redeem 重新置为 USED。
var ticketTransitions = map[int32][]int32{
	TicketStateIssued: {TicketStateUsed, TicketStateRevoked, TicketStateExpired},
}

// IsValidTicketTransition 判断票据状态机是否允许 old→new。
func IsValidTicketTransition(oldState, newState int32) bool {
	for _, allow := range ticketTransitions[oldState] {
		if allow == newState {
			return true
		}
	}
	return false
}

// --- 房间路由状态（rpc.RouteState，落库 live_gw_room_route.state） ---

const (
	RouteStateUnspecified int32 = 0
	RouteStateServing     int32 = 1
	RouteStateDraining    int32 = 2
	RouteStateOffline     int32 = 3
)

// routeTransitions 路由状态机（唯一事实来源，logic 不得自行放宽）：
// SERVING ⇄ DRAINING（排空可撤销）；DRAINING/OFFLINE → SERVING（节点重新承接，重启后重建路由）；
// SERVING → OFFLINE 一律拒绝：必须先经 DRAINING 只出不进地排空，否则在线连接被硬切，
// 观众看到的是「莫名掉线」。房间关闭也要走 SERVING→DRAINING→OFFLINE 两步条件更新，
// 这里不给任何绕过口子——真要为 room.close 开例外，属契约变更，需先评审再改本表。
var routeTransitions = map[int32][]int32{
	RouteStateServing:  {RouteStateDraining},
	RouteStateDraining: {RouteStateServing, RouteStateOffline},
	RouteStateOffline:  {RouteStateServing},
}

// IsValidRouteTransition 判断路由状态机是否允许 old→new。
func IsValidRouteTransition(oldState, newState int32) bool {
	for _, allow := range routeTransitions[oldState] {
		if allow == newState {
			return true
		}
	}
	return false
}

// ValidRouteState 判断路由状态取值是否合法。
func ValidRouteState(state int32) bool {
	return state >= RouteStateServing && state <= RouteStateOffline
}

// --- 广播类别（rpc.BroadcastKind）：只含社区与运行事件，无商业化消息 ---

const (
	KindUnspecified int32 = 0
	KindDanmaku     int32 = 1
	KindRoomState   int32 = 2
	KindSystem      int32 = 3
	KindInteraction int32 = 4
	KindModeration  int32 = 5
	KindAnchorTip   int32 = 6
)

// ValidBroadcastKind 判断广播类别是否合法。
func ValidBroadcastKind(kind int32) bool { return kind >= KindDanmaku && kind <= KindAnchorTip }

// KindRequiresTrustedSender 判定该类消息是否必须由"可信来源"发出：
// ROOM_STATE / MODERATION / SYSTEM / ANCHOR_TIP 不能被观众态连接触发，
// 只允许带 Service/Operator 角色或内部服务凭据的调用方发起（README 权限矩阵）。
func KindRequiresTrustedSender(kind int32) bool {
	switch kind {
	case KindRoomState, KindSystem, KindModeration, KindAnchorTip:
		return true
	default:
		return false
	}
}

// RoleAllowedToSend 权限矩阵：角色 role 是否有权发送 kind 类消息。
// 唯一事实来源是本函数（logic 层不得自行放宽），越权返回 DROP_REASON_PERMISSION_DENIED。
func RoleAllowedToSend(role, kind int32) bool {
	if !ValidBroadcastKind(kind) {
		return false
	}
	switch role {
	case RoleService, RoleOperator:
		// 内部服务与运营通道：可发系统/状态/处置类，也可以代发弹幕与互动提示。
		return true
	case RoleRoomAdmin:
		// 房管：系统公告与互动提示，不能发房间状态（开播/下播归主播与 live-room）。
		return kind == KindSystem || kind == KindInteraction || kind == KindDanmaku
	case RoleAnchor:
		// 主播：弹幕、互动提示、主播提词；房间状态由 live-room 事件驱动，不由客户端发。
		return kind == KindDanmaku || kind == KindInteraction || kind == KindAnchorTip
	case RoleViewer:
		// 观众：只有弹幕与互动提示（点赞计数等）。
		return kind == KindDanmaku || kind == KindInteraction
	default:
		return false
	}
}

// --- 丢弃/拒绝原因（rpc.DropReason）：丢弃必须可解释 ---

const (
	DropUnspecified          int32 = 0
	DropOK                   int32 = 1 // 未丢弃
	DropNoRoute              int32 = 2
	DropNoSubscriber         int32 = 3
	DropPermissionDenied     int32 = 4
	DropBadTicket            int32 = 5
	DropRateLimited          int32 = 6
	DropDuplicated           int32 = 7
	DropPayloadTooLarge      int32 = 8
	DropRoomClosed           int32 = 9
	DropTransportUnavailable int32 = 10
)

// ValidDropReason 判断原因取值是否合法（0 表示"未指定"，落库时视为脏数据）。
func ValidDropReason(reason int32) bool {
	return reason >= DropOK && reason <= DropTransportUnavailable
}

// --- 单播投递结果（rpc.DeliveryResult） ---

const (
	DeliveryUnspecified          int32 = 0
	DeliverySent                 int32 = 1
	DeliveryNoLease              int32 = 2
	DeliveryDenied               int32 = 3
	DeliveryTransportUnavailable int32 = 4
)

// --- 广播审计流水状态（live_gw_broadcast_log.state） ---

const (
	BroadcastLogSent       int32 = 1 // 已下发
	BroadcastLogDropped    int32 = 2 // 已丢弃（无路由/无订阅者/超限/载荷过大/通道未接线）
	BroadcastLogDenied     int32 = 3 // 越权拒绝（权限矩阵或三元组不匹配）
	BroadcastLogDuplicated int32 = 4 // 重复投递丢弃
)

// ValidBroadcastLogState 判断审计状态取值是否合法。
func ValidBroadcastLogState(state int32) bool {
	return state >= BroadcastLogSent && state <= BroadcastLogDuplicated
}

// --- 配额作用域（rpc.QuotaScope，落库 live_gw_access_quota.scope） ---

const (
	QuotaScopeUnspecified int32 = 0
	QuotaScopeGlobal      int32 = 1
	QuotaScopeNode        int32 = 2
	QuotaScopeRoom        int32 = 3
	QuotaScopeUser        int32 = 4
)

// ValidQuotaScope 判断作用域取值是否合法。
func ValidQuotaScope(scope int32) bool { return scope >= QuotaScopeGlobal && scope <= QuotaScopeUser }

// QuotaScopeChain 配额继承链（从具体到通用）：读取时按此顺序回退。
// USER 只看用户层与全局：把 ROOM/NODE 夹在中间会让「某房间某用户」的降配被节点配置覆盖，
// 语义与运营直觉相反，也让排障时无法解释"为什么这条连接被拒"。
func QuotaScopeChain(scope int32) []int32 {
	switch scope {
	case QuotaScopeUser:
		return []int32{QuotaScopeUser, QuotaScopeGlobal}
	case QuotaScopeRoom:
		return []int32{QuotaScopeRoom, QuotaScopeGlobal}
	case QuotaScopeNode:
		return []int32{QuotaScopeNode, QuotaScopeGlobal}
	default:
		return []int32{QuotaScopeGlobal}
	}
}

// isDuplicateErr 识别 MySQL 唯一索引冲突（1062 / Duplicate entry），
// 与 services/notification/model/common.go 同样按错误文本判定（不新增依赖）。
func isDuplicateErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Error 1062") || strings.Contains(msg, "Duplicate entry")
}
