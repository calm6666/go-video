// Package repository 是 live-gateway 的数据访问层，把「易失在线态」和「持久审计/配置」分开承载：
//
//	LeaseStore  —— Redis（go-zero redis.Redis）：连接租约、房间订阅集合、心跳簿记、
//	               广播去重窗口、限流计数、禁止重连窗口、重连票据有效位、路由与配额读缓存。
//	               全部易失、可重建，Redis 丢失只造成「客户端重连」，不造成业务事实丢失。
//	Store       —— MySQL go_video_live_gateway 的四张表（直接组合 model，不重复包一层）：
//	               live_gw_room_route / live_gw_access_quota / live_gw_broadcast_log /
//	               live_gw_reconnect_ticket。
//	RoomGate    —— 跨服务读 live-room 的房间归属与可广播状态（本服务不复判房间事实，AGENTS.md §5）。
//	Transport   —— WS 接入层的扇出通道。**本期未接线**（仓库无 websocket 依赖，禁止新增），
//	               由 UnwiredTransport 显式返回 ErrTransportUnavailable。
//	Signer      —— 重连票据的 HMAC-SHA256 签名/验签；密钥只从环境变量注入（AGENTS.md §4）。
//
// 分工（AGENTS.md §4/§5）：SQL、条件 UPDATE 与 CAS 语义全在 model；这里只放
// 「Redis 键空间 + 跨服务客户端 + 凭据签名」。logic 不接触任何查询语句，也不自己拼 Redis key。
//
// 键空间独占 `govideo:livegw:` 前缀：不与他服务共用 key（AGENTS.md §5 禁止直连别人的 Redis 业务键）。
package repository

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"

	"go-video/services/live-gateway/model"
)

// 本包哨兵错误。文本一律脱敏：不含票据原文、签名密钥、IP 明文与载荷正文（AGENTS.md §6）。
var (
	// ErrLiveRoomNotConfigured 未配置 live-room 客户端：ANCHOR 归属与 ROOM_CLOSED 判定都问不到真值。
	// fail-closed：调用方必须据此拒绝放行高权限角色，而不是「当作检查通过」（README 已知缺口）。
	ErrLiveRoomNotConfigured = errors.New("live-gateway: live-room rpc client not configured")
	// ErrSignerMissing 未注入票据签名密钥：签发/验签一律显式失败，绝不退化成固定 key 或不签名。
	ErrSignerMissing = errors.New("live-gateway: ticket signing key missing, set the env var named by Security.TicketSignKeyRef")
	// ErrStoreUnavailable Redis/MySQL 等硬依赖不可用。调用方不得降级为「无连接」「无路由」这类假结论。
	ErrStoreUnavailable = errors.New("live-gateway: state store unavailable")
	// ErrUnknownTopic 订阅/投递指定了本服务不认识的子通道。拼错 topic 会永久收不到消息，
	// 属可解释失败，必须拒绝而不是静默忽略。
	ErrUnknownTopic = errors.New("live-gateway: unknown subscribe topic")
	// ErrTooManyTopics 子通道数量越界（防把 topics 列表当成广播放大器参数）。
	ErrTooManyTopics = errors.New("live-gateway: too many topics in one request")
	// ErrTooManyTargetRoles 角色过滤器长度越界：与 ErrTooManyTopics 同性质，
	// target_roles 同样是调用方可控的扇出放大器参数（上限来自 LiveGateway.MaxTargetRoles）。
	ErrTooManyTargetRoles = errors.New("live-gateway: too many target roles in one request")
)

// Redis 键空间（独占前缀，见包注释）。
const (
	keyLease       = "govideo:livegw:lease:%s"       // lease_id → 租约 JSON
	keyConnLease   = "govideo:livegw:conn:%s|%s"     // node_id|conn_id → lease_id（租约幂等键的一半）
	keyRequest     = "govideo:livegw:req:%s|%s"      // rpc|request_id → 结果标识（写接口幂等）
	keyRoomLeases  = "govideo:livegw:room:%d:leases" // room_id → set(lease_id)
	keyRoomUser    = "govideo:livegw:room:%d:mid:%d" // room_id|mid → set(lease_id)（多端同时在线）
	keyRoomSeq     = "govideo:livegw:room:%d:seq"    // room_id → 已受理广播序号（漏消息估算）
	keyOfflineView = "govideo:livegw:offline:%d|%d"  // room_id|mid → "断开秒|当时序号"（重连宽限期视图）
	keyMsgDedup    = "govideo:livegw:msg:%d|%s"      // room_id|message_id → 首次结论（广播去重窗口）
	keyEventDedup  = "govideo:livegw:evt:%d|%s"      // room_id|event_id → 首次结论
	keyRate        = "govideo:livegw:rate:%s:%d"     // 维度标签|窗口桶 → 计数（固定窗口限流）
	keyBanRoomUser = "govideo:livegw:ban:room:%d|%d" // room_id|mid → ban_until
	keyBanUser     = "govideo:livegw:ban:mid:%d"     // mid → ban_until（用户维度封禁）
	keyTicket      = "govideo:livegw:ticket:%s"      // ticket_hash → ticket_id（一次性有效位）
	keyRouteCache  = "govideo:livegw:route:%d"       // room_id → 路由 JSON 读缓存
	keyQuotaCache  = "govideo:livegw:quota:%d:%d:%d" // epoch|scope|scope_id → 生效配额 JSON（epoch 递增即整体失效）
	keyQuotaEpoch  = "govideo:livegw:quota:epoch"    // 配额代次计数器：UpsertAccessQuota 后 INCR
	keyRenewDedup  = "govideo:livegw:renew:%s:%d"    // lease_id|秒 → 续租去重
)

// 默认窗口与上限（配置缺省/非法时的兜底，与 etc 示例值同口径）。
const (
	// defaultDedupWindowSeconds 去重窗口兜底。
	defaultDedupWindowSeconds = 600
	// defaultRateWindowSeconds 限流窗口兜底。
	defaultRateWindowSeconds = 1
	// maxKeyPartBytes node_id / conn_id / request_id 进入 Redis key 前的长度上限。
	// 不限制会出现「一个 10KB 的 conn_id 撑爆 Redis」这类客户端可触发的资源事故。
	maxKeyPartBytes = 128
)

// Store 聚合本服务自有的四张 MySQL 表。字段名与 logic 里的用法一致，不再包一层转发方法：
// model 已经把「条件 UPDATE + RowsAffected」的语义表达清楚了（AGENTS.md §5）。
type Store struct {
	RoomRoutes    model.LiveGwRoomRouteModel
	Quotas        model.LiveGwAccessQuotaModel
	BroadcastLogs model.LiveGwBroadcastLogModel
	Tickets       model.LiveGwReconnectTicketModel
	DB            sqlx.SqlConn
}

// NewStore 用 DSN 构造四表 model。DSN 为空时返回错误而不是一个 nil-safe 的空 Store：
// 半残装配会让每个写请求在运行期以难以归因的方式失败。
func NewStore(dsn string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("%w: DataSource is empty", ErrStoreUnavailable)
	}
	conn := sqlx.NewMysql(dsn)
	return &Store{
		DB:            conn,
		RoomRoutes:    model.NewLiveGwRoomRouteModel(conn),
		Quotas:        model.NewLiveGwAccessQuotaModel(conn),
		BroadcastLogs: model.NewLiveGwBroadcastLogModel(conn),
		Tickets:       model.NewLiveGwReconnectTicketModel(conn),
	}, nil
}

// checkKeyPart 校验进入 Redis key 的客户端可控片段：空、超长、含空格或换行的都拒绝
// （换行会直接污染 RESP 协议的 key 文本）。
func checkKeyPart(name, v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return fmt.Errorf("%w: %s is empty", ErrStoreUnavailable, name)
	}
	if len(v) > maxKeyPartBytes || v != strings.TrimSpace(v) {
		return fmt.Errorf("%w: %s too long (max %d bytes)", ErrStoreUnavailable, name, maxKeyPartBytes)
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return fmt.Errorf("%w: %s must not contain whitespace", ErrStoreUnavailable, name)
	}
	return nil
}

// RouteCacheKey 房间路由读缓存键（DrainRoomRoute/Register 写路径必须主动失效）。
func RouteCacheKey(roomID int64) string { return fmt.Sprintf(keyRouteCache, roomID) }

// QuotaCacheKey 配额解析结果的读缓存键，**含配额代次 epoch**：
// 任何一次 UpsertAccessQuota 都 INCR epoch，于是所有旧键在同一瞬间逻辑失效（无需 SCAN 清扫），
// 也就不用为「GLOBAL 降配如何影响数千个 ROOM 解析结果」写一份键清单。
// 不这么做的话，「降配封禁」最多要等 QuotaCacheTTLSeconds（默认 60s）才生效，
// 而运营改完配额立刻去验证却发现还是旧行为——这类延迟是配置类事故里最难复盘的一种。
func QuotaCacheKey(epoch, scope int32, scopeID int64) string {
	return fmt.Sprintf(keyQuotaCache, epoch, scope, scopeID)
}
