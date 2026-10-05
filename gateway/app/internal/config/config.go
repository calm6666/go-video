// Code scaffolded by goctl. Safe to edit.
// goctl 1.9.2

package config

import (
	"github.com/zeromicro/go-zero/rest"
	"github.com/zeromicro/go-zero/zrpc"
)

// Config 是 gateway/app 的配置结构。
// 网关只做入口与聚合（AGENTS.md §3）：HTTP 收口在此，领域数据经下游 RPC 获取。
type Config struct {
	rest.RestConf

	// AccountRPC 是 account 服务的 zrpc client 配置（Info/Card/Profile/Vip/DelCache 聚合）。
	AccountRPC zrpc.RpcClientConf

	// UserProfileRPC 是 user-profile 服务的 zrpc client 配置
	// （资料查询/编辑、节操、经验、实名认证聚合）。
	UserProfileRPC zrpc.RpcClientConf

	// VideoRPC 是 video 服务的 zrpc client 配置（投稿创建/查询/列表/状态推进聚合）。
	VideoRPC zrpc.RpcClientConf `json:",optional"`

	// SocialGraphRPC 是 social-graph 服务的 zrpc client 配置
	// （关注/取关/查询/列表/统计聚合）。
	SocialGraphRPC zrpc.RpcClientConf `json:",optional"`

	// FeedRPC 是 feed 服务的 zrpc client 配置（关注流/用户主页/未读聚合）。
	FeedRPC zrpc.RpcClientConf `json:",optional"`

	// UploadRPC 是 upload 服务的 zrpc client 配置
	// （分片上传会话初始化/预签名 URL/完成/取消/状态聚合）。
	UploadRPC zrpc.RpcClientConf `json:",optional"`

	// CatalogRPC 是 catalog 服务的 zrpc client 配置
	// （版权内容目录作品/季/集/分区聚合）。
	CatalogRPC zrpc.RpcClientConf `json:",optional"`

	// EngagementRPC 是 engagement 服务的 zrpc client 配置
	// （点赞/收藏/分享等社区互动聚合）。
	EngagementRPC zrpc.RpcClientConf `json:",optional"`

	// PlaybackRPC 是 playback 服务的 zrpc client 配置
	// （短期防盗链播放地址签发、播放心跳、播放会话）。
	PlaybackRPC zrpc.RpcClientConf `json:",optional"`

	// DanmakuRPC 是 danmaku 服务的 zrpc client 配置
	// （弹幕发送/时间轴拉取/举报/用户级屏蔽聚合）。
	DanmakuRPC zrpc.RpcClientConf `json:",optional"`

	// SearchQueryRPC 是 search-query 服务的 zrpc client 配置
	// （搜索、联想、热词、搜索历史、查询行为上报）。
	SearchQueryRPC zrpc.RpcClientConf `json:",optional"`

	// InboxRPC 是 inbox 服务的 zrpc client 配置
	// （站内信收件箱列表、已读、未读数、软删除）。
	InboxRPC zrpc.RpcClientConf `json:",optional"`

	// TranscodeRPC 是 transcode 服务的 zrpc client 配置。
	// 仅用于播放链路把 asset_id 解析成可播放版次的 object_key；
	// 网关不写转码数据（AGENTS.md §5）。
	TranscodeRPC zrpc.RpcClientConf `json:",optional"`

	// CommentRPC 是 comment 服务的 zrpc client 配置
	// （评论/楼中楼发布、列表、删除、置顶、举报、计数快照）。
	CommentRPC zrpc.RpcClientConf `json:",optional"`

	// NotificationRPC 是 notification 服务的 zrpc client 配置。
	// 终端只读取/更新本人通道偏好与免打扰设置，投递与模板管理属运营面。
	NotificationRPC zrpc.RpcClientConf `json:",optional"`

	// CreatorRPC 是 creator 服务的 zrpc client 配置
	// （UP 主特殊属性、身份属性、关注弹窗开关）。
	CreatorRPC zrpc.RpcClientConf `json:",optional"`

	// ModerationRPC 是 moderation-orchestrator 服务的 zrpc client 配置。
	// 终端只提交申诉，审核结论与申诉处理由运营面走该服务（AGENTS.md §5）。
	ModerationRPC zrpc.RpcClientConf `json:",optional"`

	// PrivateMessageRPC 是 private-message 服务的 zrpc client 配置
	// （单聊会话/消息分页/发送幂等/撤回/已读游标/未读汇总/举报/反骚扰偏好）。
	// 网关不实现门禁与密文，正文与游标语义以 privatemessage.v1 契约为准（AGENTS.md §5）。
	PrivateMessageRPC zrpc.RpcClientConf `json:",optional"`

	// LiveRoomRPC 是 live-room 服务的 zrpc client 配置
	// （房间读写/分区列表/开播前置检查/开播下播/房间配置/主播绑定/场次历史）。
	// 主播资格与风控判定在 live-room 内部经 creator/risk-control RPC 完成，网关不代判。
	LiveRoomRPC zrpc.RpcClientConf `json:",optional"`

	// MembershipRPC 是 membership 服务的 zrpc client 配置
	// （套餐列表/我的会员/权益判定/自动续费签约/我的开通记录）。
	// 网关不判定权益：判定口径唯一出口是 membership.CheckEntitlement(s)。
	MembershipRPC zrpc.RpcClientConf `json:",optional"`

	// PaymentRPC 是 payment 服务的 zrpc client 配置（余额/沙箱充值单/资金流水/渠道自述）。
	// 只有 SANDBOX 渠道：网关不接任何真实支付渠道，也不实现回调验签。
	PaymentRPC zrpc.RpcClientConf `json:",optional"`

	// TradeOrderRPC 是 trade-order 服务的 zrpc client 配置（下单/我的订单/取消/退款申请/流转台账）。
	// 金额由服务侧重算，网关只透传客户端上报值供一致性校验（AGENTS.md §8 状态机也在服务侧）。
	TradeOrderRPC zrpc.RpcClientConf `json:",optional"`

	// CoinRPC 是 coin 服务的 zrpc client 配置（硬币账户/投币/取消投币/内容投币汇总）。
	CoinRPC zrpc.RpcClientConf `json:",optional"`

	// CreatorRevenueRPC 是 creator-revenue 服务的 zrpc client 配置
	// （收益概览/生效规则/参与状态/计量明细/结算单）。
	// 只读到「应计金额」与结算单：本项目无出金通道，网关不得渲染成已到账。
	CreatorRevenueRPC zrpc.RpcClientConf `json:",optional"`

	// PrivacyAppKeys 是 /account/privacy 接口的调用方白名单。
	// 对应参考仓库 conf.AppkeyFilter.Privacy，仅列表内的 appkey 可以查询隐私信息。
	// 为空列表表示不启用 appkey 校验（仅限开发环境）。
	PrivacyAppKeys []string `json:",optional"`

	// PassportRSAPublicKey 登录密码加密的 RSA 公钥（PEM 格式）。
	// /x/passport-login/key 返回给客户端用于加密登录/注册密码；
	// 必须与 account 服务配置的 PassportRSA.PrivateKey 配对。
	// 留空表示未配置（仅限开发环境，/key 返回明确错误）。
	PassportRSAPublicKey string `json:",optional"`
}
