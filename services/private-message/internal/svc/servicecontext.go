// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"go-video/common/ratelimit"
	"go-video/services/private-message/internal/config"
	"go-video/services/private-message/model"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"
)

// ServiceContext 是 private-message 的运行时上下文：MySQL 连接、缓存、各表 model
// 与下游 RPC 客户端。logic 只通过这里取依赖，不在方法内建连接。
type ServiceContext struct {
	Config config.Config

	// DB 是 go_video_private_message 库连接（事务由 logic 侧 conn.TransactCtx 发起）。
	DB sqlx.SqlConn

	// Cache 是未读计数与频控窗口的加速层；真值始终在 MySQL，缓存缺失只回源不报错。
	Cache *redis.Redis

	// Conversations 会话主体（pair_key 幂等 + seq 锚点）。
	Conversations model.ConversationModel
	// Members 会话成员（已读游标 + 未读/展示投影）。
	Members model.ConversationMemberModel
	// Messages 私信消息（密文正文 + 会话内 seq）。
	Messages model.MessageModel
	// Settings 用户反骚扰偏好。
	Settings model.UserSettingModel
	// Reports 举报事实。
	Reports model.ReportModel
	// WithdrawLogs 撤回审计流水（append-only）。
	WithdrawLogs model.WithdrawLogModel

	// SocialGraph 黑名单/关注关系真值；未配置时为 nil，门禁方法返回明确错误。
	SocialGraph zrpc.Client
	// RiskControl 名单与频控判定真值；未配置时为 nil。
	RiskControl zrpc.Client
	// Moderation 送审与结论回写通道；未配置时为 nil。
	Moderation zrpc.Client

	// WriteLimiter 进程级令牌桶（复用 common/ratelimit），保护 MySQL 与审核下游。
	WriteLimiter ratelimit.Limiter
}

// NewServiceContext 构造 ServiceContext。
//
// 依赖缺失策略：配置自检失败直接 Severe 终止启动（宁可不启动，也不带着
// 「正文永不清理」这类危险配置上线）；下游 RPC 未配置只记日志、不 panic，
// 让服务可独立启动，但相关判定方法会返回 model.Err*NotConfigured 而不是放行。
func NewServiceContext(c config.Config) *ServiceContext {
	if err := c.Validate(); err != nil {
		logx.Severe("private-message/svc: 配置自检失败: ", err)
	}

	conn := sqlx.NewMysql(c.DataSource)
	qps := int(c.PrivateMessage.PostQps)
	burst := int(c.PrivateMessage.PostBurst)
	if burst < qps {
		burst = qps
	}

	return &ServiceContext{
		Config:        c,
		DB:            conn,
		Cache:         redis.MustNewRedis(c.CacheRedis),
		Conversations: model.NewConversationModel(conn),
		Members:       model.NewConversationMemberModel(conn),
		Messages:      model.NewMessageModel(conn),
		Settings:      model.NewUserSettingModel(conn),
		Reports:       model.NewReportModel(conn),
		WithdrawLogs:  model.NewWithdrawLogModel(conn),
		SocialGraph:   newClientIfConfigured(c.SocialGraphRPC, "SocialGraphRPC"),
		RiskControl:   newClientIfConfigured(c.RiskControlRPC, "RiskControlRPC"),
		Moderation:    newClientIfConfigured(c.ModerationRPC, "ModerationRPC"),
		WriteLimiter:  ratelimit.NewTokenBucket(qps, burst),
	}
}

// newClientIfConfigured 仅在给出可用发现配置时构造客户端；构造失败只记日志返回 nil，
// 由调用方在请求路径上返回明确的 Err*NotConfigured（禁止把「没接上」当成「已通过」）。
func newClientIfConfigured(conf zrpc.RpcClientConf, name string) zrpc.Client {
	if len(conf.Endpoints) == 0 && conf.Target == "" && len(conf.Etcd.Hosts) == 0 {
		logx.Infof("private-message/svc: %s 未配置，依赖它的门禁将返回 not configured", name)
		return nil
	}
	cli, err := zrpc.NewClient(conf)
	if err != nil {
		logx.Errorf("private-message/svc: 初始化 %s 客户端失败: %v", name, err)
		return nil
	}
	return cli
}

// SocialGraphClient 返回 social-graph 客户端，未配置时给出明确错误（门禁不可绕过）。
func (s *ServiceContext) SocialGraphClient() (zrpc.Client, error) {
	if s.SocialGraph == nil {
		return nil, model.ErrSocialGraphNotConfigured
	}
	return s.SocialGraph, nil
}

// RiskControlClient 返回 risk-control 客户端，未配置时给出明确错误。
func (s *ServiceContext) RiskControlClient() (zrpc.Client, error) {
	if s.RiskControl == nil {
		return nil, model.ErrRiskControlNotConfigured
	}
	return s.RiskControl, nil
}

// ModerationClient 返回 moderation-orchestrator 客户端，未配置时给出明确错误。
func (s *ServiceContext) ModerationClient() (zrpc.Client, error) {
	if s.Moderation == nil {
		return nil, model.ErrModerationNotConfigured
	}
	return s.Moderation, nil
}
