// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package svc

import (
	"go-video/gateway/app/internal/config"
	"go-video/gateway/app/internal/middleware"
	accountrpc "go-video/services/account/rpc"
	catalogrpc "go-video/services/catalog/rpc"
	coinrpc "go-video/services/coin/rpc"
	commentrpc "go-video/services/comment/rpc"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"
	creatorrpc "go-video/services/creator/rpc"
	danmakurpc "go-video/services/danmaku/rpc"
	engagementrpc "go-video/services/engagement/rpc"
	feedrpc "go-video/services/feed/rpc"
	inboxrpc "go-video/services/inbox/rpc"
	liveroomrpc "go-video/services/live-room/rpc"
	membershiprpc "go-video/services/membership/rpc"
	moderationrpc "go-video/services/moderation-orchestrator/rpc"
	notificationrpc "go-video/services/notification/rpc"
	paymentrpc "go-video/services/payment/rpc"
	playbackrpc "go-video/services/playback/rpc"
	privatemessagerpc "go-video/services/private-message/rpc"
	searchqueryrpc "go-video/services/search-query/rpc"
	socialgraphrpc "go-video/services/social-graph/rpc"
	tradeorderrpc "go-video/services/trade-order/rpc"
	transcoderpc "go-video/services/transcode/rpc"
	uploadrpc "go-video/services/upload/rpc"
	userprofilerc "go-video/services/user-profile/rpc"
	videorpc "go-video/services/video/rpc"

	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 gateway/app 的运行时上下文，承载下游 RPC 客户端。
type ServiceContext struct {
	Config config.Config
	// Account account 服务 RPC 客户端。
	Account accountrpc.AccountClient
	// UserProfile user-profile 服务 RPC 客户端。
	UserProfile userprofilerc.UserProfileClient
	// Video video 服务 RPC 客户端（投稿聚合）。
	Video videorpc.VideoClient
	// SocialGraph social-graph 服务 RPC 客户端（关系链聚合）。
	SocialGraph socialgraphrpc.SocialGraphClient
	// Feed feed 服务 RPC 客户端（动态流聚合）。
	Feed feedrpc.FeedClient
	// Upload upload 服务 RPC 客户端（分片上传聚合）。
	Upload uploadrpc.UploadClient
	// Catalog catalog 服务 RPC 客户端（版权内容目录聚合）。
	Catalog catalogrpc.CatalogClient
	// Engagement engagement 服务 RPC 客户端（点赞/收藏/分享聚合）。
	Engagement engagementrpc.EngagementClient
	// Playback playback 服务 RPC 客户端（播放授权签发/心跳/会话）。
	Playback playbackrpc.PlaybackClient
	// Danmaku danmaku 服务 RPC 客户端（时间轴弹幕聚合）。
	Danmaku danmakurpc.DanmakuClient
	// SearchQuery search-query 服务 RPC 客户端（搜索/联想/热词/历史）。
	SearchQuery searchqueryrpc.SearchQueryClient
	// Inbox inbox 服务 RPC 客户端（站内信收件箱聚合）。
	Inbox inboxrpc.InboxClient
	// Transcode transcode 服务 RPC 客户端（播放版次解析，只读）。
	Transcode transcoderpc.TranscodeClient
	// Comment comment 服务 RPC 客户端（评论/楼中楼聚合）。
	Comment commentrpc.CommentClient
	// Notification notification 服务 RPC 客户端（本人通道偏好与免打扰设置）。
	Notification notificationrpc.NotificationClient
	// Creator creator 服务 RPC 客户端（UP 主身份与开关）。
	Creator creatorrpc.CreatorClient
	// Moderation moderation-orchestrator 服务 RPC 客户端（作者申诉提交）。
	Moderation moderationrpc.ModerationOrchestratorClient
	// PrivateMessage private-message 服务 RPC 客户端（单聊私信聚合）。
	PrivateMessage privatemessagerpc.PrivateMessageClient
	// LiveRoom live-room 服务 RPC 客户端（直播间观众面与主播面聚合）。
	LiveRoom liveroomrpc.LiveRoomClient
	// Membership membership 服务 RPC 客户端（套餐/我的会员/权益判定/开通记录）。
	// 权益判定不在网关实现，这里只是转发通道。
	Membership membershiprpc.MembershipClient
	// Payment payment 服务 RPC 客户端（余额/沙箱充值单/资金流水/渠道自述）。
	Payment paymentrpc.PaymentClient
	// TradeOrder trade-order 服务 RPC 客户端（下单/我的订单/取消/退款申请）。
	TradeOrder tradeorderrpc.TradeOrderClient
	// Coin coin 服务 RPC 客户端（硬币账户/投币/取消投币/内容汇总）。
	Coin coinrpc.CoinClient
	// CreatorRevenue creator-revenue 服务 RPC 客户端（收益概览/规则/参与/结算单）。
	CreatorRevenue creatorrevenuerpc.CreatorRevenueClient
	// AppkeyVerify /account/privacy 的白名单 appkey 校验中间件。
	AppkeyVerify rest.Middleware
}

// NewServiceContext 构造 ServiceContext。
// 对应 RPC 配置留空时客户端为 nil，logic 会返回明确的服务不可用错误。
func NewServiceContext(c config.Config) *ServiceContext {
	middleware.SetPrivacyAppKeys(c.PrivacyAppKeys)

	ctx := &ServiceContext{
		Config:       c,
		AppkeyVerify: middleware.NewAppkeyVerifyMiddleware().Handle,
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
	if c.SocialGraphRPC.Target != "" || len(c.SocialGraphRPC.Etcd.Hosts) > 0 {
		ctx.SocialGraph = socialgraphrpc.NewSocialGraphClient(zrpc.MustNewClient(c.SocialGraphRPC).Conn())
	}
	if c.FeedRPC.Target != "" || len(c.FeedRPC.Etcd.Hosts) > 0 {
		ctx.Feed = feedrpc.NewFeedClient(zrpc.MustNewClient(c.FeedRPC).Conn())
	}
	if c.UploadRPC.Target != "" || len(c.UploadRPC.Etcd.Hosts) > 0 {
		ctx.Upload = uploadrpc.NewUploadClient(zrpc.MustNewClient(c.UploadRPC).Conn())
	}
	if c.CatalogRPC.Target != "" || len(c.CatalogRPC.Etcd.Hosts) > 0 {
		ctx.Catalog = catalogrpc.NewCatalogClient(zrpc.MustNewClient(c.CatalogRPC).Conn())
	}
	if c.EngagementRPC.Target != "" || len(c.EngagementRPC.Etcd.Hosts) > 0 {
		ctx.Engagement = engagementrpc.NewEngagementClient(zrpc.MustNewClient(c.EngagementRPC).Conn())
	}
	if c.PlaybackRPC.Target != "" || len(c.PlaybackRPC.Etcd.Hosts) > 0 {
		ctx.Playback = playbackrpc.NewPlaybackClient(zrpc.MustNewClient(c.PlaybackRPC).Conn())
	}
	if c.DanmakuRPC.Target != "" || len(c.DanmakuRPC.Etcd.Hosts) > 0 {
		ctx.Danmaku = danmakurpc.NewDanmakuClient(zrpc.MustNewClient(c.DanmakuRPC).Conn())
	}
	if c.SearchQueryRPC.Target != "" || len(c.SearchQueryRPC.Etcd.Hosts) > 0 {
		ctx.SearchQuery = searchqueryrpc.NewSearchQueryClient(zrpc.MustNewClient(c.SearchQueryRPC).Conn())
	}
	if c.InboxRPC.Target != "" || len(c.InboxRPC.Etcd.Hosts) > 0 {
		ctx.Inbox = inboxrpc.NewInboxClient(zrpc.MustNewClient(c.InboxRPC).Conn())
	}
	if c.TranscodeRPC.Target != "" || len(c.TranscodeRPC.Etcd.Hosts) > 0 {
		ctx.Transcode = transcoderpc.NewTranscodeClient(zrpc.MustNewClient(c.TranscodeRPC).Conn())
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
	if c.ModerationRPC.Target != "" || len(c.ModerationRPC.Etcd.Hosts) > 0 {
		ctx.Moderation = moderationrpc.NewModerationOrchestratorClient(zrpc.MustNewClient(c.ModerationRPC).Conn())
	}
	if c.PrivateMessageRPC.Target != "" || len(c.PrivateMessageRPC.Etcd.Hosts) > 0 {
		ctx.PrivateMessage = privatemessagerpc.NewPrivateMessageClient(zrpc.MustNewClient(c.PrivateMessageRPC).Conn())
	}
	if c.LiveRoomRPC.Target != "" || len(c.LiveRoomRPC.Etcd.Hosts) > 0 {
		ctx.LiveRoom = liveroomrpc.NewLiveRoomClient(zrpc.MustNewClient(c.LiveRoomRPC).Conn())
	}
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
	return ctx
}
