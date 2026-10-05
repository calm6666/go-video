// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package config

import (
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 live-room 服务的配置结构。
// 领域微服务只暴露 gRPC（AGENTS.md §3/§4），面向终端的 HTTP 由 gateway/app 聚合，
// 运营侧由 gateway/admin 聚合，本服务不提供 HTTP 面。
type Config struct {
	zrpc.RpcServerConf

	// CacheRedis 承载房间详情、分区树、PrepareLive 检查结果与开播令牌等易失缓存。
	// 不能命名为 Redis：zrpc.RpcServerConf 已内嵌同名 RedisKeyConf 字段（zrpc 鉴权/限流用），
	// 同名会让 conf.Load 报 "conflict key redis" —— 代码能编译但服务启动即失败。
	// 全仓统一用 CacheRedis（见 services/danmaku、services/live-gateway）。
	CacheRedis redis.RedisConf

	// DataSource 是本服务自有库 go_video_live_room 的 MySQL DSN。
	// 只允许访问 live_* 表，禁止直连 live-ingest / creator / moderation 的库表（AGENTS.md §5）。
	// 生产 DSN 与口令由配置中心/Secret 注入，示例值仅限本地开发。
	DataSource string

	// CreatorRPC 是 creator 服务的 zrpc client 配置（开播前置检查读主播身份，
	// UpAttr from=2 直播 UP / from=3 直播白名单）。未配置时 ServiceContext 不构造客户端，
	// PrepareLive 的 anchor_qualification 检查项记为 degraded=true 并按未通过处理，
	// 绝不静默放行（AGENTS.md §8）。
	CreatorRPC zrpc.RpcClientConf `json:",optional"`

	// RiskControlRPC 是 risk-control 的 zrpc client 配置
	//（CheckAction ACTION_LIVE_START）。语义同上：缺失即该检查项未通过。
	RiskControlRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC 是 moderation-orchestrator 的 zrpc client 配置
	//（SubmitForReview ContentType_CONTENT_TYPE_LIVE：房间资料送审）。
	// 缺失时 CreateRoom/UpdateRoomInfo 返回 ErrModerationNotConfigured，
	// 不把「未送审」写成「已送审」（AGENTS.md §9）。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// LiveRoom 是直播房间领域参数，全部来自 yaml 或配置中心，不在代码里写死。
	LiveRoom LiveRoomConf

	// Kafka 是事件消费链路配置位。本服务只消费一个 topic：
	// live.state.v1（生产者 live-ingest，唯一持有流生命周期真值的服务），
	// 翻译后进 ReportStreamState 入站入口，按 event_id 去重 + seq 守卫推进房间/场次投影。
	// 本服务不发布任何事件：这里刻意没有 PublishTopics 字段，写出来就会让人以为房间状态
	// 由两个服务往同一 topic 双写（收敛结论见 README「与 live-ingest 的边界」）。
	// Enabled 默认 false：默认构建不链接 kq，置 true 时启动即失败（见 internal/consumer）。
	Kafka KafkaConf `json:",optional"`
}

// KafkaConf 事件消费链路参数。
//
// 唯一被消费的 topic 是 live.state.v1 → logic.ReportStreamState；
// moderation.result.v1 的入站入口（ApplyRoomModerationResult）也已经是 RPC，
// 但本仓库里 moderation-orchestrator 还没有该 topic 的生产者（它只在
// internal/logic/submitworkerresultlogic.go:49 留了 TODO），因此这里不订阅它：
// 订阅一个没有映射的 topic 等于把事件读出来再丢掉，见 consumer.ValidateKafka。
type KafkaConf struct {
	// Enabled 决定进程是否随 RPC 启动 Kafka 消费者。默认 false。
	// 置 true 但二进制没链接 kq（-tags liveroom_kafka）、或下面的消费参数不完整时，
	// main 用 logx.Must 终止启动，绝不「安静地不消费」（投影落后于真实流状态且无人发现）。
	Enabled bool `json:",default=false"`
	// Brokers Kafka/Redpanda 地址列表，为空表示不启用事件链路。
	Brokers []string `json:",optional"`
	// Group 消费组名。同组多副本互为竞争，重复投递由
	// live_room_idempotency 的 uniq_dedup_key(dedup_key) 唯一键去重；
	// kind 只标记键来源（request_id / event_id），不参与唯一性。
	Group string `json:",default=live-room.v1"`
	// SubscribeTopics 订阅的事件 topic。只允许 live.state.v1：消费者只有这一条映射。
	SubscribeTopics []string `json:",optional"`
	// MaxRetries 同一 event_id 在本进程内的累计尝试上限（含首次）。
	// 达到上限就放弃并写错误日志 + 提交位点：本服务没有持久化消费位点表，
	// 不设上限会让一条依赖故障的事件在分区里无限热循环。
	// 注意 event_id 去重键在 logic 里先于任何读操作被占用，因此重投只对
	// 「抢键之前」的失败有意义，详见 consumer.Handler.Consume 的注释。
	MaxRetries int `json:",default=5"`
	// Offset 消费组没有位点时的起点。first 会重放整个保留窗口，
	// 重复事件由 live_room_idempotency 兜住，但默认取 last 更省。
	Offset string `json:",options=first|last,default=last"`
	// Conns 每个 topic 建立的 reader 连接数。
	Conns int `json:",default=1"`
	// Consumers 每条连接的拉取协程数。
	Consumers int `json:",default=2"`
	// Processors 每条连接的并发处理协程数。
	// 同一条流的事件要靠 stream_seq 串行化，乱序会被 logic 判成 result=3 丢弃，
	// 因此这里不需要按 key 保序（kq 的分区顺序已由生产侧的 aggregate_id 作分区键保证）。
	Processors int `json:",default=4"`
	// ForceCommit 处理失败时是否仍提交位点。默认 false：让 broker 重投，
	// 由 MaxRetries 在本进程内收敛尝试次数。
	ForceCommit bool `json:",default=false"`
	// Username SASL 用户名。生产只由环境变量/Secret 注入，示例配置留空。
	Username string `json:",optional"`
	// Password SASL 口令，必须与 Username 成对出现（见 consumer.ValidateKafka）。
	Password string `json:",optional"`
	// CaFile TLS 根证书路径；为空表示不启用 TLS。证书内容不进仓库。
	// 非空但文件不可读时必须在这里拒绝，否则 kq 内部直接 log.Fatal 打死进程。
	CaFile string `json:",optional"`
}

// LiveRoomConf 直播间业务参数（上限、宽限期、缓存 TTL、清理批量）。
type LiveRoomConf struct {
	// MaxRoomsPerOwner 单主播可持有的生效房间数上限（CreateRoom 校验，0 表示不限制）。
	MaxRoomsPerOwner int32 `json:",default=1"`
	// MaxOwnerBindingsPerMid 单主播作为生效绑定方（任一角色）可覆盖的房间数上限。
	MaxOwnerBindingsPerMid int32 `json:",default=20"`
	// MaxCohostPerRoom 单房间生效联合主播（连麦嘉宾）上限。
	MaxCohostPerRoom int32 `json:",default=8"`
	// MaxManagerPerRoom 单房间生效房管上限。
	MaxManagerPerRoom int32 `json:",default=30"`
	// TitleMaxLength 房间标题 rune 上限，与 live_room.title 列宽一致。
	TitleMaxLength int32 `json:",default=80"`
	// AreaNameMaxLength 分区名 rune 上限，与 live_area.area_name 列宽一致。
	AreaNameMaxLength int32 `json:",default=32"`
	// DefaultListPageSize 列表默认每页大小。
	DefaultListPageSize int32 `json:",default=20"`
	// MaxListPageSize 列表每页大小上限，超出直接拒绝（不静默截断到该值以外）。
	MaxListPageSize int32 `json:",default=100"`
	// MaxAreaPageSize ListAreas 每页上限（分区是运营维护的小表，可以比房间列表大）。
	MaxAreaPageSize int32 `json:",default=200"`
	// StreamInterruptGraceSeconds 断流宽限期（秒）：live-ingest 上报 INTERRUPTED 后，
	// 超过该时长仍未恢复才由 cron/事件把场次置为 TERMINATED(STREAM_TIMEOUT)。
	// 取值必须小于 live-gateway 的重连票据 TTL 才有意义（见其 LiveGateway.ReconnectGraceSeconds）。
	StreamInterruptGraceSeconds int32 `json:",default=120"`
	// PrepareCheckTTLSeconds PrepareLive 结果缓存秒数，0 表示每次实查下游。
	PrepareCheckTTLSeconds int `json:",default=60"`
	// RoomCacheTTLSeconds 房间详情缓存秒数，0 表示关闭。写路径必须同步失效该键。
	RoomCacheTTLSeconds int `json:",default=10"`
	// AreaListCacheTTLSeconds 分区列表缓存秒数，0 表示每次回源 DB。
	AreaListCacheTTLSeconds int `json:",default=300"`
	// ModerationBusiness 送审时登记的 business 标识（与 moderation 侧对齐，勿随意改）。
	ModerationBusiness string `json:",default=live"`
	// IdempotencyRetentionDays 幂等/事件去重记录保留天数（live_room_idempotency 清理窗口）。
	// 必须大于客户端最大重试间隔与 Kafka 最大重投间隔，否则幂等语义失效。
	IdempotencyRetentionDays int32 `json:",default=30"`
	// StateLogRetentionDays 状态流转日志保留天数，超期由 services/cron 归档。
	StateLogRetentionDays int32 `json:",default=365"`
	// SweepBatchLimit cron 单次扫描/清理的批量上限（禁播到期、幂等清理共用）。
	SweepBatchLimit int32 `json:",default=200"`
	// BanExpirySweepEnabled 是否允许 cron 推进「禁播到期」的房间状态。
	// 本轮 logic 未实现，保持 false，避免出现「配置开了但没人执行」的隐性期待。
	BanExpirySweepEnabled bool `json:",default=false"`
}
