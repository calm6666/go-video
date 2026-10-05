// Code scaffolded by goctl. Safe to edit.

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 live-gateway 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4）；WebSocket 接入层是独立适配器（见服务 README）。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 是长连接状态的主存储：租约、房间订阅集合、心跳计数、
	// 广播去重窗口、限流计数与重连票据有效位都存放在这里（易失、可重建）。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段，
	// 同名会让 conf.Load 报 "conflict key redis"，代码可编译但启动即失败。
	CacheRedis redis.RedisConf

	// DataSource 是 go_video_live_gateway 库的 MySQL DSN。
	// 该库只保存需要审计/重建的房间路由、配额配置、广播审计流水与票据审计；
	// 逐条连接状态一律不落业务主库（服务 README「数据分层」硬约束）。
	DataSource string

	// LiveRoomRPC 是 live-room 的 zrpc client 配置位：ANCHOR 角色校验与
	// 房间是否可广播（ROOM_CLOSED）都必须问房间所有者，本服务不自己判定。
	// 本轮 ServiceContext 不构造客户端，接线见 services/live-gateway/README.md「已知缺口」。
	LiveRoomRPC zrpc.RpcClientConf `json:",optional"`

	// LiveGateway 是长连接与广播领域参数（TTL、载荷上限、默认配额）。
	LiveGateway LiveGatewayConf

	// Security 是重连票据签名密钥的**配置位**：只写环境变量名，字面量不进仓库（AGENTS.md §4）。
	Security SecurityConf `json:",optional"`

	// Kafka 是事件消费配置位（live.state.v1 / danmaku.sent.v1 → 房间广播）。
	// 本轮未接线：internal/consumer 尚未创建，处理入口是 connection.Manager 的接口。
	Kafka KafkaConf
}

// LiveGatewayConf 长连接与广播业务参数，全部来自 etc yaml 或配置中心。
type LiveGatewayConf struct {
	// WsListenOn 是 WebSocket 接入层监听地址（约定值）。
	// 已知缺口：仓库无 websocket 依赖且禁止新增依赖，真实 WS 服务端未实装，
	// 该字段当前只是接线约定，ServiceContext 不会监听它。
	WsListenOn string `json:",default=0.0.0.0:8122"`
	// DefaultLeaseTTLSeconds 连接租约默认 TTL（客户端需在此周期内续租）。
	DefaultLeaseTTLSeconds int32 `json:",default=30"`
	// MaxLeaseTTLSeconds 租约 TTL 上限；配额配置里的值也会被夹到这里。
	MaxLeaseTTLSeconds int32 `json:",default=300"`
	// MinLeaseTTLSeconds 租约 TTL 下限，防止客户端把 TTL 配得过小打爆 Redis。
	MinLeaseTTLSeconds int32 `json:",default=10"`
	// DefaultTicketTTLSeconds 断线重连票据默认有效期。
	DefaultTicketTTLSeconds int32 `json:",default=120"`
	// MaxTicketTTLSeconds 票据 TTL 上限。
	MaxTicketTTLSeconds int32 `json:",default=600"`
	// HeartbeatMaxSkewSeconds 客户端心跳时间与服务端可容忍偏差（秒），超出只观测不落库。
	HeartbeatMaxSkewSeconds int32 `json:",default=300"`
	// ReconnectGraceSeconds 断线宽限期：期内重连可复用订阅视图与漏消息估算。
	ReconnectGraceSeconds int32 `json:",default=60"`
	// MaxPayloadBytes 单条广播/单播载荷上限（字节）。
	MaxPayloadBytes int32 `json:",default=32768"`
	// DefaultRoomBroadcastQps 房间广播默认 QPS（无配额行时使用）。
	DefaultRoomBroadcastQps int32 `json:",default=200"`
	// DefaultUserBroadcastQps 单用户发送默认 QPS。
	DefaultUserBroadcastQps int32 `json:",default=5"`
	// DefaultMaxRoomConnections 单房间默认最大连接数。
	DefaultMaxRoomConnections int32 `json:",default=50000"`
	// AllowGuestByDefault 无配额行时是否允许游客（mid=0）接入。
	AllowGuestByDefault bool `json:",default=true"`
	// PayloadDigestBytes 审计流水保存的载荷摘要长度（sha256 hex 前缀，正文不入库）。
	PayloadDigestBytes int `json:",default=32"`
	// BroadcastLogRetentionDays 广播审计流水保留天数，超期由回收/归档任务清理。
	BroadcastLogRetentionDays int32 `json:",default=30"`
	// RoomRouteCacheTTLSeconds 房间路由读缓存秒数，0 表示关闭。
	RoomRouteCacheTTLSeconds int `json:",default=10"`
	// QuotaCacheTTLSeconds 配额配置读缓存秒数，0 表示每次回源 DB。
	QuotaCacheTTLSeconds int `json:",default=60"`
	// ConnectionScanLimit ListRoomConnections 单次从 Redis 扫描的连接数上限。
	ConnectionScanLimit int32 `json:",default=500"`
	// MaxPageSize 分页响应每页大小上限（model.clampPage 的 maxPS），越界夹到默认 20。
	MaxPageSize int32 `json:",default=50"`
	// DedupWindowSeconds 广播/事件去重窗口与写接口 request_id 幂等窗口的 Redis TTL（秒）。
	// 必须显著大于上游重试间隔，否则重试会在 Redis 放行、只在 DB 唯一键上被拦下。
	DedupWindowSeconds int32 `json:",default=600"`
	// RateWindowSeconds 限流固定窗口长度（秒）。QPS 口径 = 单窗口内允许的条数，
	// 放大窗口只降低抖动敏感度，不改变「超限就丢弃」的语义。
	RateWindowSeconds int32 `json:",default=1"`
	// MaxTicketsPerSubject 单个 (room_id, mid) 允许同时持有的未使用票据数上限：
	// 超过即拒绝再签发，防「票据风暴」（囤票后可批量重连，也能把 Redis 有效位键撑爆）。
	MaxTicketsPerSubject int32 `json:",default=5"`
	// DefaultRoomShardCount 房间首次登记路由时的默认广播分片数（>=1）。
	DefaultRoomShardCount int32 `json:",default=1"`
	// TrustedSourceServices ForwardSystemEvent 允许的 source_service 白名单。
	// 空列表 = 不放行任何自报来源（fail-closed）：系统事件能驱动「开播/禁言/封停」，
	// 让任意内网调用方自报来源等于给它一条伪造房间状态的通道。
	TrustedSourceServices []string `json:",optional"`
	// RequireAttestedOperator 为 true 时，运营/内部面读方法（ListRoomConnections、ListRoomRoutes、
	// ListBroadcastLogs）必须由 gRPC metadata 声明 OPERATOR/SERVICE 角色才放行。
	// 默认 false 的原因：本仓库的 gateway/admin 目前不在 zrpc 调用上附加 caller metadata
	// （见服务 README「已知缺口」），置 true 会让后台的运营视图整体 403。
	// 一旦接入层补上 x-gw-caller-* metadata，这里必须改 true 才算权限闭环。
	RequireAttestedOperator bool `json:",default=false"`
	// AllowUnattestedOperatorWrites 为 true 时才允许「未经 metadata 归因」的调用方执行处置类写操作
	// （KickConnection、DrainRoomRoute、UpsertAccessQuota、RevokeReconnectTicket）。
	// 默认 false：处置接口是「主动改变他人连接状态」的能力，拿不到可信主体就不执行是 fail-closed 的方向。
	// 读接口用 RequireAttestedOperator（默认放开）、写接口用本开关（默认收紧）是刻意不对称的：
	// 读漏了是隐私问题、写漏了是处置被冒名执行，二者风险等级不同。
	AllowUnattestedOperatorWrites bool `json:",default=false"`
	// MaxTargetRoles target_roles 列表长度上限（越界直接拒，防把广播写成按角色枚举的放大器）。
	MaxTargetRoles int32 `json:",default=6"`
	// MaxTopicsPerSubscription 单连接订阅子通道数上限（默认全集只有 3 个）。
	MaxTopicsPerSubscription int32 `json:",default=8"`
	// MaxIdLenBytes request_id / message_id / event_id / conn_id 等标识列的长度上限，
	// 与迁移 SQL 的 VARCHAR(64) 对齐：超长会静默截断（MySQL 非严格模式）或被拒，两种都比
	// 在这里显式拒绝更难排查。
	MaxIdLenBytes int `json:",default=64"`
}

// SecurityConf 凭据类配置。约定与 services/audit 的 Security 段一致：
// 配置里只放**环境变量名**，密钥字面量由 Secret/Vault 注入（AGENTS.md §4）。
type SecurityConf struct {
	// TicketSignKeyRef 存放重连票据 HMAC-SHA256 签名密钥的环境变量名。
	// 未注入时签发/兑换一律显式失败（repository.ErrSignerMissing），
	// 绝不退化成「用固定 key」或「不签名」——那等于任何人都能自造重连凭据。
	TicketSignKeyRef string `json:",default=LIVEGW_TICKET_SIGN_KEY"`
}

// KafkaConf 事件消费参数（待接线，见服务 README「已知缺口」）。
type KafkaConf struct {
	// Brokers Kafka/Redpanda 地址列表，为空表示不启用事件链路。
	Brokers []string `json:",optional"`
	// Group 消费组名。
	Group string `json:",default=live-gateway.v1"`
	// SubscribeTopics 订阅的领域事件 topic（房间状态、弹幕）。
	SubscribeTopics []string `json:",optional"`
	// MaxRetries 单事件累计尝试上限。
	MaxRetries int `json:",default=5"`
	// RetryBackoffSec 退避基数（指数退避）。
	RetryBackoffSec int `json:",default=5"`
}
