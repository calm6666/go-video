// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 gateway/admin 的配置结构。
// 管理后台入口聚合（AGENTS.md §3）：HTTP 收口在此，领域数据经下游 RPC 获取，
// 管理业务逻辑归属 services/operation，本网关只做路由与聚合。
type Config struct {
	rest.RestConf

	// AccountRPC 是 account 服务的 zrpc client 配置（缓存失效运营路由）。
	AccountRPC zrpc.RpcClientConf

	// UserProfileRPC 是 user-profile 服务的 zrpc client 配置
	// （节操/经验/属性审核/实名脱敏运营路由）。
	UserProfileRPC zrpc.RpcClientConf

	// VideoRPC 是 video 服务的 zrpc client 配置（稿件审核与状态推进运营路由）。
	VideoRPC zrpc.RpcClientConf `json:",optional"`

	// CatalogRPC 是 catalog 服务的 zrpc client 配置（作品/季/集管理运营路由）。
	CatalogRPC zrpc.RpcClientConf `json:",optional"`

	// RightsRPC 是 rights 服务的 zrpc client 配置（版权合同与播放窗口运营路由）。
	RightsRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC 是 moderation-orchestrator 服务的 zrpc client 配置
	// （审核任务查询与申诉处理运营路由）。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// TranscodeRPC 是 transcode 服务的 zrpc client 配置（转码任务与模板运营路由）。
	TranscodeRPC zrpc.RpcClientConf `json:",optional"`

	// AssetRPC 是 asset 服务的 zrpc client 配置（媒资元数据查询运营路由）。
	AssetRPC zrpc.RpcClientConf `json:",optional"`

	// DanmakuRPC 是 danmaku 服务的 zrpc client 配置
	// （弹幕屏蔽词管理运营路由；用户级屏蔽只在 gateway/app 暴露）。
	DanmakuRPC zrpc.RpcClientConf `json:",optional"`

	// SearchIndexerRPC 是 search-indexer 服务的 zrpc client 配置
	// （索引重建任务、别名切换与索引健康运营路由）。
	SearchIndexerRPC zrpc.RpcClientConf `json:",optional"`

	// RiskControlRPC 是 risk-control 服务的 zrpc client 配置
	// （规则/名单/处罚/设备画像管理与裁决调试运营路由）。
	RiskControlRPC zrpc.RpcClientConf `json:",optional"`

	// OperationRPC 是 operation 服务的 zrpc client 配置：管理后台自身的
	// RBAC/菜单/运营配置/管理任务/审计索引，以及 AdminPermission 中间件的
	// VerifyAdminPermission 判定都走它（AGENTS.md §3：后台业务归 services/operation）。
	OperationRPC zrpc.RpcClientConf `json:",optional"`

	// CommentRPC 是 comment 服务的 zrpc client 配置
	// （运营侧评论查询/删除/置顶/计数快照）。
	CommentRPC zrpc.RpcClientConf `json:",optional"`

	// NotificationRPC 是 notification 服务的 zrpc client 配置
	// （模板管理、投递记录、死信重投）。不提供群发入口（AGENTS.md §1）。
	NotificationRPC zrpc.RpcClientConf `json:",optional"`

	// CreatorRPC 是 creator 服务的 zrpc client 配置
	// （UP 主特殊分组字典、分组名册、高能联盟签约）。
	CreatorRPC zrpc.RpcClientConf `json:",optional"`

	// InboxRPC 是 inbox 服务的 zrpc client 配置
	// （运营下发系统站内信、未读快照修复）。收件箱正文属用户隐私，后台不开放代读/代删。
	InboxRPC zrpc.RpcClientConf `json:",optional"`

	// AuditRPC 是 audit 服务的 zrpc client 配置（追加式不可抵赖存证的检索、
	// 哈希链自证、导出任务与保留期策略）。审计条目由领域服务写入，后台只读与申请导出。
	AuditRPC zrpc.RpcClientConf `json:",optional"`

	// OpsConfigRPC 是 ops-config 服务的 zrpc client 配置（配置项发布/回滚、灰度规则、
	// 专题与推荐位、客户端开关与缓存刷新）。运行时 Resolve* 属终端面，不在后台开放。
	OpsConfigRPC zrpc.RpcClientConf `json:",optional"`

	// CronRPC 是 cron 服务的 zrpc client 配置（任务定义注册与启停、执行记录、
	// 租约与游标查询、手动触发）。Acquire/Renew/Release 租约属 worker 内部协议。
	CronRPC zrpc.RpcClientConf `json:",optional"`

	// RecommendRecallRPC 是 recommend-recall 服务的 zrpc client 配置
	// （召回池版本发布/回滚/清理、池条目写入、召回请求日志回放）。
	RecommendRecallRPC zrpc.RpcClientConf `json:",optional"`

	// RecommendRankRPC 是 recommend-rank 服务的 zrpc client 配置
	// （特征配置与模型版本登记、实验变体与状态、排序决策回放）。
	// 二者都不提供广告位参数（AGENTS.md §7）。
	RecommendRankRPC zrpc.RpcClientConf `json:",optional"`

	// LiveIngestRPC 是 live-ingest 服务的 zrpc client 配置
	// （推流密钥元数据与吊销、流状态巡检、接入节点与事件位点观测）。
	// 密钥明文只在签发响应出现一次，网关不落日志也不缓存。
	LiveIngestRPC zrpc.RpcClientConf `json:",optional"`

	// LiveGatewayRPC 是 live-gateway 服务的 zrpc client 配置
	// （房间路由与排空、连接与广播台账、重连票据撤销、接入配额）。
	LiveGatewayRPC zrpc.RpcClientConf `json:",optional"`

	// LiveMediaRPC 是 live-media 服务的 zrpc client 配置
	// （直播转码任务的登记/停止/重试/取消与台账、分发档位的上下线、录制任务与切片、
	// 回放拼接任务与 asset/稿件引用回填、回收任务提交与台账，共 23 条 /admin/live 路由）。
	// 五个 Report* 回报口属 Worker→服务链路，刻意不开后台路由，因此本网关不会代为推进任务状态机。
	LiveMediaRPC zrpc.RpcClientConf `json:",optional"`

	// LiveRoomRPC 是 live-room 服务的 zrpc client 配置
	// （房间检索、禁播与解除、场次与分区字典）。开播/下播是主播侧动作，归 gateway/app。
	LiveRoomRPC zrpc.RpcClientConf `json:",optional"`

	// EventCollectorRPC 是 event-collector 服务的 zrpc client 配置（行为事件接收与投递台账的
	// 读取、投递推进与死信重放、采样/脱敏策略草稿与切换、采集健康度，共 13 条 /admin/collector 路由）。
	// 两个上报入口（CollectEvents / IngestServerEvents）刻意不接后台路由：前者是终端面 SDK 入口
	// （归 gateway/app），后者要求调用方服务身份，网关代为上报会让信封 producer 失真。
	EventCollectorRPC zrpc.RpcClientConf `json:",optional"`

	// PrivateMessageRPC 是 private-message 服务的 zrpc client 配置（私信举报台账、举报处置、
	// 留存到期清理，共 3 条 /admin/private-message 路由）。
	// ApplyModerationVerdict 刻意不接后台路由：人审结论的唯一写入口是审核侧（服务身份），
	// 从后台点出来等于多一条「不过审核就能改结论」的路径。
	PrivateMessageRPC zrpc.RpcClientConf `json:",optional"`

	// SpmRPC 是 spm 服务的 zrpc client 配置：/admin/spm 的 15 条路由
	// （读 10：GetMetric/BatchGetMetrics/ListHotSubjects/GetRetention/GetMetricDefinition/
	// ListMetricDefinitions/GetAggregationJob/ListAggregationJobs/ListConsumerState/ListDeadLetters；
	// 受判定保护 1：GetUserInterest（定向的个人画像读）；
	// 写 4：UpsertMetricDefinition/UpdateMetricDefinitionState/SubmitAggregationJob/RecomputeMetrics）。
	// 刻意不接：WriteMetricWindow（proto 头注释即「写回通道只接受计算链路来源」，
	// 后台代写窗口指标等于让「运营觉得该火」变成一个指标源，违反 AGENTS.md §7 第 3 条）。
	// 未配置时 15 条路由一律返回明确错误，不返回空榜单——空榜单会被读成「这台内容没人看」。
	SpmRPC zrpc.RpcClientConf `json:",optional"`

	// FeatureStoreRPC 是 feature-store 服务的 zrpc client 配置：/admin/feature-store 的 13 条路由
	// （读 5：GetFeatureDefinition/ListFeatureDefinitions/ListVersionSwitches/GetBackfillJob/
	// ListBackfillJobs；受判定保护 1：ListEntityFeatures（定向的个人特征导出）；
	// 写 7：RegisterFeature/UpdateFeatureState/UpdateFeaturePrivacy/SwitchFeatureVersion/
	// SubmitBackfillJob/EraseEntityFeatures/PurgeExpired）。
	// 刻意不接：WriteFeatures（特征值只能由计算链路写入——spm 指标投影、离线模型回填、风控滑窗，
	// 后台代写等于让运营主观判断变成在线特征，AGENTS.md §7）、
	// GetFeature/BatchGetFeatures（recommend-* 每次排序都打的在线热路径读，不开后台面：
	// 排障页刷新不该吃排序的特征读配额，看单主体走 /entity-feature/list、看口径走 /definition/get）。
	// 未配置时 13 条路由一律返回明确错误，不退化成空目录——空列表会被读成「没有注册过特征」。
	FeatureStoreRPC zrpc.RpcClientConf `json:",optional"`

	// MembershipRPC 是 membership 服务的 zrpc client 配置：/admin/membership 的 10 条路由
	// （读 5：ListPlansAdmin/GetMembership/ListGrants/ListExpiringMemberships/ListEntitlements；
	// 写 5：UpsertPlan/SetPlanState/GrantMembership/RevokeMembership/UpsertEntitlement）。
	// 刻意不接的方法：ExpireMembership（到期终结归 cron，后台点它等于手工改状态）、
	// SetAutoRenew（用户自身签约动作，归 gateway/app）、CheckEntitlement/CheckEntitlements
	// （权益判定是服务间读，每次播放都要问，做成后台接口只会多一条绕过后端判定的公开口径）。
	// 未配置时 10 条路由一律返回明确错误，不回空台账、也不伪造授予成功（AGENTS.md §1 资金语义）。
	// 链接前置条件（契约轮已修）：membership 的 descriptor 路径必须是
	// services/membership/rpc/membership.proto 而不是裸名 "membership.proto"——后者与
	// go.etcd.io/etcd/api/v3/membershippb 注册的同名文件在全局 protoregistry 冲突，
	// 任何同时链接 clientv3（zrpc 服务发现）与该 rpc 包的进程会在 init 阶段 panic。
	// 该路径由 scripts/gen.ps1 的 $descriptorPrefixedProtos 保证，网关侧无需改动。
	MembershipRPC zrpc.RpcClientConf `json:",optional"`

	// PaymentRPC 是 payment 服务的 zrpc client 配置：/admin/payment 的 8 条路由
	// （读 6：GetWallet/ListRecharges/ListPayments/ListRefunds/ListFlows/DescribeChannels；
	// 写 2：SettleSandboxRecharge/AdjustBalance）。
	// 本域只走沙箱台账：不请求任何真实支付渠道、没有渠道回调，DescribeChannels 的 real_money 恒 false，
	// 后台读到的是「这里没有真钱」这一事实本身。
	// 刻意不接：OpenRecharge/CancelRecharge（用户自己发起与取消，归 gateway/app）、
	// CreatePayment/ClosePayment/RefundPayment（唯一驱动方是 trade-order 订单状态机，
	// 从后台直连资金写口会造出第二个写主，两边幂等键不共享时对不上）、GetPayment（列表已覆盖）。
	// AdjustBalance 是唯一能直接改资金台账的后台口，只用于沙箱差错更正，服务侧留审计。
	PaymentRPC zrpc.RpcClientConf `json:",optional"`

	// TradeOrderRPC 是 trade-order 服务的 zrpc client 配置：/admin/order 的 6 条路由
	// （读 4：ListOrders/GetOrder/ListOrderEvents/ListStuckOrders；
	// 写 2：ApproveRefund/RejectRefund）。
	// 刻意不接：CreateOrder/ListMyOrders/CancelOrder/RequestRefund（买家的单只能买家自己发起、
	// 取消、申退，归 gateway/app；后台代下单等于伪造用户意愿）、BindPayment（支付侧回调/补偿链路，
	// 调用方是 cron 与对账，服务身份）、FulfillOrder（履约推进由服务/cron 驱动，
	// 后台触发会出现「按一次就多送几天」的重复发放）。
	// 退款审批的「退余额 + 回收权益」两步都在服务侧串起来，网关不自行编排资金动作（§5）。
	TradeOrderRPC zrpc.RpcClientConf `json:",optional"`

	// CoinRPC 是 coin 服务的 zrpc client 配置：/admin/coin 的 4 条路由
	// （读 3：GetCoinAccount/ListCoinFlows/GetTossConfig；写 1：GrantCoin）。
	// 硬币是社区虚拟币、不是钱：本客户端不接任何资金路由，与 PaymentRPC 是两套账、不互换。
	// 刻意不接：TossCoin/CancelToss/ListMyTosses（投币与撤币是终端用户动作，归 gateway/app，
	// 后台代投会伪造互动信号并污染 spm 特征，§7）、
	// GetTargetSummary/BatchGetTargetSummary/ListTargetTossers（内容侧聚合读，归 gateway/app 与审核面）、
	// 投币参数（日限/单片上限/取消窗口）也没有写口：那套规则属 ops-config 域，不属 coin。
	CoinRPC zrpc.RpcClientConf `json:",optional"`

	// CreatorRevenueRPC 是 creator-revenue 服务的 zrpc client 配置：/admin/creator-revenue 的
	// 11 条路由（读 6：ListRevenueRules/GetRevenueRule/ListEnrollments/ListRevenueMetrics/
	// ListSettlements/GetSettlement；写 5：UpsertRevenueRule/SetRevenueRuleState/
	// SetEnrollmentState/GenerateSettlement/ConfirmSettlement）。
	// 刻意不接：EnrollCreator/LeavePlan（加入/退出是创作者本人动作且必须带本人确认的规则版本，
	// 归 gateway/app）、RecordRevenueMetric（计量事实的写入方是 spm/coin/cron，后台代写会污染计量源）、
	// GetEnrollment/GetRevenueSummary（创作者本人视角的读，归 gateway/app）。
	// **出金不在本期范围**：契约里没有提现/打款/退款到卡/发票/对账方法，本客户端也不会去凑
	// 一条「钱已付出」的结论——结算单 payout_state 恒 NOT_PAYABLE、payout_available 恒 false。
	CreatorRevenueRPC zrpc.RpcClientConf `json:",optional"`

	// OpenPlatformRPC 是 open-platform 服务的 zrpc client 配置：/admin/open-platform 的 16 条路由
	// （读 7：ListScopes/GetApplication/ListApplications/ListQuotaPolicies/ListQuotaUsage/
	// ListWebhooks/ListWebhookDeliveries——scope 目录一条免判定，其余六条要求 operator_mid；
	// 写 9：UpdateApplication(仅状态)/RotateApplicationSecret/RevokeApplicationSecret/
	// GrantApplicationScopes/RevokeAuthorization/UpsertQuotaPolicy/RecomputeQuota/
	// DeleteWebhook/RetryWebhookDelivery）。
	// 刻意不接的 8 个方法（三条口径，详见 admin.api 段头）：
	//   1. 后台不代替开发者表达意愿：RegisterApplication（替某个 owner_mid 建应用并首发一次性
	//      明文密钥）、RegisterWebhook（回调地址决定第三方数据去向，代设有 SSRF 与越权读取后果）；
	//      UpdateApplication 只接状态通道，资料通道在服务侧就被运营位拒（ErrOwnerRequired）；
	//   2. 后台不签发也不换发用户凭证：IssueAuthorizationCode（须用户在授权页显式同意）、
	//      ExchangeAuthorizationCode、RefreshAccessToken、IntrospectToken、
	//      AuthorizeRequest（每次开放调用的热路径：凭证 + scope + 配额扣减 + 流水）；
	//   3. 后台不注入事实：EnqueueWebhookEvent（手工入队一条等于伪造「已经发生过的事件」）。
	// 未配置时 16 条路由一律返回明确错误，不回空目录——空应用列表会被读成「没有第三方接入」。
	OpenPlatformRPC zrpc.RpcClientConf `json:",optional"`
}
