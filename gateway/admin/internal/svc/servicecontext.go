// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package svc

import (
	"go-video/gateway/admin/internal/config"
	"go-video/gateway/admin/internal/middleware"
	accountrpc "go-video/services/account/rpc"
	assetrpc "go-video/services/asset/rpc"
	auditrpc "go-video/services/audit/rpc"
	catalogrpc "go-video/services/catalog/rpc"
	coinrpc "go-video/services/coin/rpc"
	commentrpc "go-video/services/comment/rpc"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"
	creatorrpc "go-video/services/creator/rpc"
	cronrpc "go-video/services/cron/rpc"
	danmakurpc "go-video/services/danmaku/rpc"
	collectorrpc "go-video/services/event-collector/rpc"
	featurestorerpc "go-video/services/feature-store/rpc"
	inboxrpc "go-video/services/inbox/rpc"
	livegatewayrpc "go-video/services/live-gateway/rpc"
	liveingestrpc "go-video/services/live-ingest/rpc"
	livemediarpc "go-video/services/live-media/rpc"
	liveroomrpc "go-video/services/live-room/rpc"
	membershiprpc "go-video/services/membership/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	notificationrpc "go-video/services/notification/rpc"
	openplatformrpc "go-video/services/open-platform/rpc"
	operationrpc "go-video/services/operation/rpc"
	opsconfigrpc "go-video/services/ops-config/rpc"
	paymentrpc "go-video/services/payment/rpc"
	privatemessagerpc "go-video/services/private-message/rpc"
	rankrpc "go-video/services/recommend-rank/rpc"
	recallrpc "go-video/services/recommend-recall/rpc"
	rightsrpc "go-video/services/rights/rpc"
	riskcontrolrpc "go-video/services/risk-control/rpc"
	searchindexerrpc "go-video/services/search-indexer/rpc"
	spmrpc "go-video/services/spm/rpc"
	tradeorderrpc "go-video/services/trade-order/rpc"
	transcoderpc "go-video/services/transcode/rpc"
	userprofilerc "go-video/services/user-profile/rpc"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 gateway/admin 的运行时上下文，承载下游 RPC 客户端。
type ServiceContext struct {
	Config config.Config
	// Account account 服务 RPC 客户端。
	Account accountrpc.AccountClient
	// UserProfile user-profile 服务 RPC 客户端。
	UserProfile userprofilerc.UserProfileClient
	// Video video 服务 RPC 客户端（稿件审核与状态推进）。
	Video videorpc.VideoClient
	// Catalog catalog 服务 RPC 客户端（作品/季/集管理）。
	Catalog catalogrpc.CatalogClient
	// Rights rights 服务 RPC 客户端（版权合同与播放窗口）。
	Rights rightsrpc.RightsClient
	// Moderation moderation-orchestrator 服务 RPC 客户端（审核任务与申诉）。
	Moderation moderationrpc.ModerationOrchestratorClient
	// Transcode transcode 服务 RPC 客户端（转码任务与模板）。
	Transcode transcoderpc.TranscodeClient
	// Asset asset 服务 RPC 客户端（媒资元数据查询）。
	Asset assetrpc.AssetClient
	// Danmaku danmaku 服务 RPC 客户端（弹幕屏蔽词管理）。
	Danmaku danmakurpc.DanmakuClient
	// SearchIndexer search-indexer 服务 RPC 客户端（索引重建与别名切换）。
	SearchIndexer searchindexerrpc.SearchIndexerClient
	// RiskControl risk-control 服务 RPC 客户端（规则/名单/处罚/设备画像）。
	RiskControl riskcontrolrpc.RiskControlClient
	// Operation operation 服务 RPC 客户端（后台 RBAC/菜单/运营配置/任务/审计）。
	Operation operationrpc.OperationClient
	// Comment comment 服务 RPC 客户端（运营侧评论查询/删除/置顶/计数快照）。
	Comment commentrpc.CommentClient
	// Notification notification 服务 RPC 客户端（模板管理、投递记录、死信重投）。
	Notification notificationrpc.NotificationClient
	// Creator creator 服务 RPC 客户端（UP 主特殊分组与高能联盟签约）。
	Creator creatorrpc.CreatorClient
	// Inbox inbox 服务 RPC 客户端（运营下发系统站内信、未读快照修复）。
	Inbox inboxrpc.InboxClient
	// Audit audit 服务 RPC 客户端（存证检索、哈希链自证、导出与保留策略）。
	Audit auditrpc.AuditClient
	// OpsConfig ops-config 服务 RPC 客户端（配置项发布/回滚、灰度、专题、推荐位、开关）。
	OpsConfig opsconfigrpc.OpsConfigClient
	// Cron cron 服务 RPC 客户端（任务定义、执行记录、租约与游标、手动触发）。
	Cron cronrpc.CronClient
	// RecommendRecall recommend-recall 服务 RPC 客户端（召回池版本与请求日志）。
	RecommendRecall recallrpc.RecallClient
	// RecommendRank recommend-rank 服务 RPC 客户端（模型版本、特征配置、实验、决策回放）。
	RecommendRank rankrpc.RankClient
	// LiveIngest live-ingest 服务 RPC 客户端（推流密钥、流状态、接入节点、事件位点）。
	LiveIngest liveingestrpc.LiveIngestClient
	// LiveGateway live-gateway 服务 RPC 客户端（房间路由、广播台账、重连票据、配额）。
	LiveGateway livegatewayrpc.LiveGatewayClient
	// LiveMedia live-media 服务 RPC 客户端（直播转码任务、分发档位、录制与切片、
	// 回放拼接与资产引用、回收任务的运营面读写）。
	LiveMedia livemediarpc.LiveMediaClient
	// LiveRoom live-room 服务 RPC 客户端（房间检索、禁播、场次、分区字典）。
	LiveRoom liveroomrpc.LiveRoomClient
	// EventCollector event-collector 服务 RPC 客户端（行为事件接收与投递台账、投递推进、
	// 死信重放、采样/脱敏策略版本与采集健康度）。两个上报入口不接后台路由，见 config.Config。
	EventCollector collectorrpc.EventCollectorClient
	// PrivateMessage private-message 服务 RPC 客户端（私信举报台账与处置、留存到期清理）。
	// 审核结论回写不接后台路由，见 config.Config.PrivateMessageRPC 注释。
	PrivateMessage privatemessagerpc.PrivateMessageClient
	// Spm spm 服务 RPC 客户端（指标口径注册表、指标窗口读、热点榜、留存、聚合作业、
	// 消费链路可观测与死信）。窗口指标写回 WriteMetricWindow 不接后台路由，见 config.SpmRPC 注释。
	Spm spmrpc.SpmClient
	// FeatureStore feature-store 服务 RPC 客户端（特征定义与版本、版本切换审计、回填台账、
	// 按主体导出与擦除、TTL 清理）。WriteFeatures 与在线热路径读（GetFeature/BatchGetFeatures）
	// 不接后台路由，见 config.Config.FeatureStoreRPC 注释。
	FeatureStore featurestorerpc.FeatureStoreClient
	// Membership membership 服务 RPC 客户端（套餐目录、会员身份、授予台账与权益码目录）。
	// 权益判定与自动续费签约不接后台路由，见 config.Config.MembershipRPC 注释。
	Membership membershiprpc.MembershipClient
	// Payment payment 服务 RPC 客户端（钱包、四类资金台账、渠道自述；写面只有沙箱充值结算
	// 与余额差错更正）。支付/退款不接后台路由——唯一驱动方是 trade-order，见 PaymentRPC 注释。
	Payment paymentrpc.PaymentClient
	// TradeOrder trade-order 服务 RPC 客户端（订单检索、流转台账、卡单扫描与退款裁决）。
	// 建单/取消/申退/绑单/履约都不接后台路由，见 config.Config.TradeOrderRPC 注释。
	TradeOrder tradeorderrpc.TradeOrderClient
	// Coin coin 服务 RPC 客户端（硬币账户、流水台账、投币参数读取与运营发放）。
	// 硬币不是钱：与 Payment 是两套账；投币/撤币与内容侧汇总不接后台路由，见 CoinRPC 注释。
	Coin coinrpc.CoinClient
	// CreatorRevenue creator-revenue 服务 RPC 客户端（分成规则、参与名单、计量台账与结算单）。
	// 出金/提现/打款在本期范围外（payout_state 恒 NOT_PAYABLE），计量写入也不接后台路由，
	// 见 config.Config.CreatorRevenueRPC 注释。
	CreatorRevenue creatorrevenuerpc.CreatorRevenueClient
	// OpenPlatform open-platform 服务 RPC 客户端（第三方应用与密钥状态、scope 目录与授予审批、
	// 授权撤销、配额规则与用量投影、回调端点与投递台账）。注册应用/注册回调端点、OAuth 五法
	// （签发授权码、换发、刷新、introspect、AuthorizeRequest）与事件入队不接后台路由，
	// 见 config.Config.OpenPlatformRPC 注释。
	OpenPlatform openplatformrpc.OpenPlatformClient
	// AdminPermission /admin/operation 受保护路由的后台鉴权中间件，
	// 由 goctl 生成的 internal/handler/routes.go 按路由组挂载（serverCtx.AdminPermission）。
	AdminPermission rest.Middleware
}

// NewServiceContext 构造 ServiceContext。
// 对应 RPC 配置留空时客户端为 nil，logic 会返回明确的服务不可用错误。
func NewServiceContext(c config.Config) *ServiceContext {
	// AdminPermission 中间件需要 operation 客户端：沿用 gateway/app 的 AppkeyVerify 机制，
	// 由 goctl 生成的无参构造器 + ServiceContext 构造期的包级注入完成装配，
	// 这样 routes.go（生成产物）里的 serverCtx.AdminPermission 不需要任何手写改动。
	var operationCli operationrpc.OperationClient
	if c.OperationRPC.Target != "" || len(c.OperationRPC.Etcd.Hosts) > 0 {
		operationCli = operationrpc.NewOperationClient(zrpc.MustNewClient(c.OperationRPC).Conn())
	}
	middleware.SetOperationClient(operationCli)

	ctx := &ServiceContext{
		Config:          c,
		AdminPermission: middleware.NewAdminPermissionMiddleware().Handle,
	}
	if c.AccountRPC.Target != "" || len(c.AccountRPC.Etcd.Hosts) > 0 {
		ctx.Account = accountrpc.NewAccountClient(zrpc.MustNewClient(c.AccountRPC).Conn())
	}
	if c.UserProfileRPC.Target != "" || len(c.UserProfileRPC.Etcd.Hosts) > 0 {
		ctx.UserProfile = userprofilerc.NewUserProfileClient(zrpc.MustNewClient(c.UserProfileRPC).Conn())
	}
	if c.VideoRPC.Target != "" || len(c.VideoRPC.Etcd.Hosts) > 0 {
		ctx.Video = videorpc.NewVideoClient(zrpc.MustNewClient(c.VideoRPC).Conn())
	}
	if c.CatalogRPC.Target != "" || len(c.CatalogRPC.Etcd.Hosts) > 0 {
		ctx.Catalog = catalogrpc.NewCatalogClient(zrpc.MustNewClient(c.CatalogRPC).Conn())
	}
	if c.RightsRPC.Target != "" || len(c.RightsRPC.Etcd.Hosts) > 0 {
		ctx.Rights = rightsrpc.NewRightsClient(zrpc.MustNewClient(c.RightsRPC).Conn())
	}
	if c.ModerationRPC.Target != "" || len(c.ModerationRPC.Etcd.Hosts) > 0 {
		ctx.Moderation = moderationrpc.NewModerationOrchestratorClient(zrpc.MustNewClient(c.ModerationRPC).Conn())
	}
	if c.TranscodeRPC.Target != "" || len(c.TranscodeRPC.Etcd.Hosts) > 0 {
		ctx.Transcode = transcoderpc.NewTranscodeClient(zrpc.MustNewClient(c.TranscodeRPC).Conn())
	}
	if c.AssetRPC.Target != "" || len(c.AssetRPC.Etcd.Hosts) > 0 {
		ctx.Asset = assetrpc.NewAssetClient(zrpc.MustNewClient(c.AssetRPC).Conn())
	}
	if c.DanmakuRPC.Target != "" || len(c.DanmakuRPC.Etcd.Hosts) > 0 {
		ctx.Danmaku = danmakurpc.NewDanmakuClient(zrpc.MustNewClient(c.DanmakuRPC).Conn())
	}
	if c.SearchIndexerRPC.Target != "" || len(c.SearchIndexerRPC.Etcd.Hosts) > 0 {
		ctx.SearchIndexer = searchindexerrpc.NewSearchIndexerClient(zrpc.MustNewClient(c.SearchIndexerRPC).Conn())
	}
	if c.RiskControlRPC.Target != "" || len(c.RiskControlRPC.Etcd.Hosts) > 0 {
		ctx.RiskControl = riskcontrolrpc.NewRiskControlClient(zrpc.MustNewClient(c.RiskControlRPC).Conn())
	}
	if c.CommentRPC.Target != "" || len(c.CommentRPC.Etcd.Hosts) > 0 {
		ctx.Comment = commentrpc.NewCommentClient(zrpc.MustNewClient(c.CommentRPC).Conn())
	}
	if c.NotificationRPC.Target != "" || len(c.NotificationRPC.Etcd.Hosts) > 0 {
		ctx.Notification = notificationrpc.NewNotificationClient(zrpc.MustNewClient(c.NotificationRPC).Conn())
	}
	if c.CreatorRPC.Target != "" || len(c.CreatorRPC.Etcd.Hosts) > 0 {
		ctx.Creator = creatorrpc.NewCreatorClient(zrpc.MustNewClient(c.CreatorRPC).Conn())
	}
	if c.InboxRPC.Target != "" || len(c.InboxRPC.Etcd.Hosts) > 0 {
		ctx.Inbox = inboxrpc.NewInboxClient(zrpc.MustNewClient(c.InboxRPC).Conn())
	}
	if c.AuditRPC.Target != "" || len(c.AuditRPC.Etcd.Hosts) > 0 {
		ctx.Audit = auditrpc.NewAuditClient(zrpc.MustNewClient(c.AuditRPC).Conn())
	}
	if c.OpsConfigRPC.Target != "" || len(c.OpsConfigRPC.Etcd.Hosts) > 0 {
		ctx.OpsConfig = opsconfigrpc.NewOpsConfigClient(zrpc.MustNewClient(c.OpsConfigRPC).Conn())
	}
	if c.CronRPC.Target != "" || len(c.CronRPC.Etcd.Hosts) > 0 {
		ctx.Cron = cronrpc.NewCronClient(zrpc.MustNewClient(c.CronRPC).Conn())
	}
	if c.RecommendRecallRPC.Target != "" || len(c.RecommendRecallRPC.Etcd.Hosts) > 0 {
		ctx.RecommendRecall = recallrpc.NewRecallClient(zrpc.MustNewClient(c.RecommendRecallRPC).Conn())
	}
	if c.RecommendRankRPC.Target != "" || len(c.RecommendRankRPC.Etcd.Hosts) > 0 {
		ctx.RecommendRank = rankrpc.NewRankClient(zrpc.MustNewClient(c.RecommendRankRPC).Conn())
	}
	if c.LiveIngestRPC.Target != "" || len(c.LiveIngestRPC.Etcd.Hosts) > 0 {
		ctx.LiveIngest = liveingestrpc.NewLiveIngestClient(zrpc.MustNewClient(c.LiveIngestRPC).Conn())
	}
	if c.LiveGatewayRPC.Target != "" || len(c.LiveGatewayRPC.Etcd.Hosts) > 0 {
		ctx.LiveGateway = livegatewayrpc.NewLiveGatewayClient(zrpc.MustNewClient(c.LiveGatewayRPC).Conn())
	}
	if c.LiveMediaRPC.Target != "" || len(c.LiveMediaRPC.Etcd.Hosts) > 0 {
		ctx.LiveMedia = livemediarpc.NewLiveMediaClient(zrpc.MustNewClient(c.LiveMediaRPC).Conn())
	}
	if c.LiveRoomRPC.Target != "" || len(c.LiveRoomRPC.Etcd.Hosts) > 0 {
		ctx.LiveRoom = liveroomrpc.NewLiveRoomClient(zrpc.MustNewClient(c.LiveRoomRPC).Conn())
	}
	if c.EventCollectorRPC.Target != "" || len(c.EventCollectorRPC.Etcd.Hosts) > 0 {
		ctx.EventCollector = collectorrpc.NewEventCollectorClient(zrpc.MustNewClient(c.EventCollectorRPC).Conn())
	}
	if c.PrivateMessageRPC.Target != "" || len(c.PrivateMessageRPC.Etcd.Hosts) > 0 {
		ctx.PrivateMessage = privatemessagerpc.NewPrivateMessageClient(zrpc.MustNewClient(c.PrivateMessageRPC).Conn())
	}
	if c.SpmRPC.Target != "" || len(c.SpmRPC.Etcd.Hosts) > 0 {
		ctx.Spm = spmrpc.NewSpmClient(zrpc.MustNewClient(c.SpmRPC).Conn())
	}
	if c.FeatureStoreRPC.Target != "" || len(c.FeatureStoreRPC.Etcd.Hosts) > 0 {
		ctx.FeatureStore = featurestorerpc.NewFeatureStoreClient(zrpc.MustNewClient(c.FeatureStoreRPC).Conn())
	}
	if c.OpenPlatformRPC.Target != "" || len(c.OpenPlatformRPC.Etcd.Hosts) > 0 {
		ctx.OpenPlatform = openplatformrpc.NewOpenPlatformClient(zrpc.MustNewClient(c.OpenPlatformRPC).Conn())
	}
	// 商业化五域：客户端留空即 nil，logic 一律先判 nil 再调用，未配置回明确的 not configured，
	// 不返回空台账也不伪造成功（AGENTS.md §1 资金语义）。
	if c.MembershipRPC.Target != "" || len(c.MembershipRPC.Etcd.Hosts) > 0 {
		ctx.Membership = membershiprpc.NewMembershipClient(zrpc.MustNewClient(c.MembershipRPC).Conn())
	}
	if c.PaymentRPC.Target != "" || len(c.PaymentRPC.Etcd.Hosts) > 0 {
		ctx.Payment = paymentrpc.NewPaymentClient(zrpc.MustNewClient(c.PaymentRPC).Conn())
	}
	if c.TradeOrderRPC.Target != "" || len(c.TradeOrderRPC.Etcd.Hosts) > 0 {
		ctx.TradeOrder = tradeorderrpc.NewTradeOrderClient(zrpc.MustNewClient(c.TradeOrderRPC).Conn())
	}
	if c.CoinRPC.Target != "" || len(c.CoinRPC.Etcd.Hosts) > 0 {
		ctx.Coin = coinrpc.NewCoinClient(zrpc.MustNewClient(c.CoinRPC).Conn())
	}
	if c.CreatorRevenueRPC.Target != "" || len(c.CreatorRevenueRPC.Etcd.Hosts) > 0 {
		ctx.CreatorRevenue = creatorrevenuerpc.NewCreatorRevenueClient(zrpc.MustNewClient(c.CreatorRevenueRPC).Conn())
	}
	// operation 客户端在中间件装配前构造（见函数开头），未配置时保持 nil：
	// AdminPermission 与 operation logic 都会 fail-closed 返回明确错误，绝不放行。
	ctx.Operation = operationCli
	return ctx
}
