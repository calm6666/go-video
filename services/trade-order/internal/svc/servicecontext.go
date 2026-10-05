// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"context"
	"errors"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
	"github.com/zeromicro/go-zero/zrpc"

	coinrpc "go-video/services/coin/rpc"
	memberrpc "go-video/services/membership/rpc"
	paymentrpc "go-video/services/payment/rpc"
	"go-video/services/trade-order/internal/config"
	"go-video/services/trade-order/model"
)

// ServiceContext 是 trade-order 的运行时上下文。
//
// 装配原则（AGENTS.md §5）：
//   - 本服务只拥有 go_video_trade_order 库；资金台账在 payment、会员身份在 membership、
//     硬币余额在 coin，三者只能通过下面的 gRPC 客户端驱动，绝不 import 它们的
//     internal/model，绝不直连它们的库表/Redis key，也不允许它们回写订单状态；
//   - 下游客户端全部可选：Endpoints / Target / Etcd.Hosts 皆空时字段保持 nil，
//     调用方拿到显式的 Err*NotConfigured 错误而不是假成功——本服务最危险的失效模式
//     就是「建出一张看起来已支付/已发放的订单」；
//   - CacheRedis 只作为读侧缓存预留位，本轮不写任何订单缓存：
//     金额与状态这类资金事实必须回源 MySQL。
type ServiceContext struct {
	Config config.Config

	// Orders 是 to_order 的数据访问入口（订单事实表，本服务独占写权）。
	Orders model.OrderModel
	// OrderEvents 是 to_order_event 的状态流转台账；与主表更新同事务写入。
	OrderEvents model.OrderEventModel

	// Conn 本服务库（go_video_trade_order）的连接。logic 只用它开事务
	// （见 Transact），SQL 一律留在 model 里（AGENTS.md §4）。
	Conn sqlx.SqlConn

	// Cache 读侧缓存客户端；nil 表示未配置。本轮读路径不使用（见类型注释）。
	Cache *redis.Redis

	// --- 下游领域服务客户端（nil 表示该环境未接入，调用方必须显式失败）---

	// Membership 会员域：取套餐价（唯一价格来源）、发放/回收会员权益。
	Membership memberrpc.MembershipClient
	// Payment 资金域：受理支付、退款到余额、关单。
	Payment paymentrpc.PaymentClient
	// Coin 硬币域：硬币包履约与回收。
	Coin coinrpc.CoinClient
}

// NewServiceContext 构造上下文。
//
// DataSource 由 sqlx.NewMysql 惰性建连，配置缺项由
// internal/config/config_load_test.go 在 CI 阶段拦住，这里不做「连不上就退出」。
func NewServiceContext(c config.Config) *ServiceContext {
	conn := sqlx.NewMysql(c.DataSource)

	ctx := &ServiceContext{
		Config:      c,
		Conn:        conn,
		Orders:      model.NewOrderModel(conn),
		OrderEvents: model.NewOrderEventModel(conn),
	}
	if c.CacheRedis.Host != "" {
		ctx.Cache = redis.MustNewRedis(c.CacheRedis)
	}

	ctx.Membership = newMembershipClient(c.MembershipRPC)
	ctx.Payment = newPaymentClient(c.PaymentRPC)
	ctx.Coin = newCoinClient(c.CoinRPC)

	for _, note := range ctx.DownstreamNotes() {
		logx.Infof("trade-order/svc: %s", note)
	}
	return ctx
}

// rpcConfigured 判断下游 zrpc 客户端配置是否给出了可用的发现方式。
// 判定写法与 danmaku 的 moderationConfigured 一致：Endpoints / Target / Etcd.Hosts
// 三者全空即视为「本环境未接入该下游」，不构造客户端。
func rpcConfigured(c zrpc.RpcClientConf) bool {
	return len(c.Endpoints) > 0 || c.Target != "" || len(c.Etcd.Hosts) > 0
}

func newMembershipClient(c zrpc.RpcClientConf) memberrpc.MembershipClient {
	if !rpcConfigured(c) {
		return nil
	}
	return memberrpc.NewMembershipClient(zrpc.MustNewClient(c).Conn())
}

func newPaymentClient(c zrpc.RpcClientConf) paymentrpc.PaymentClient {
	if !rpcConfigured(c) {
		return nil
	}
	return paymentrpc.NewPaymentClient(zrpc.MustNewClient(c).Conn())
}

func newCoinClient(c zrpc.RpcClientConf) coinrpc.CoinClient {
	if !rpcConfigured(c) {
		return nil
	}
	return coinrpc.NewCoinClient(zrpc.MustNewClient(c).Conn())
}

// DownstreamNotes 输出装配期诊断信息，便于运维确认「哪个能力在这个环境必然失败」。
func (s *ServiceContext) DownstreamNotes() []string {
	entries := []struct {
		name   string
		built  bool
		impact string
	}{
		{"MembershipRPC", s.Membership != nil, "CreateOrder 取价与会员单履约/回收"},
		{"PaymentRPC", s.Payment != nil, "支付受理与退款到余额"},
		{"CoinRPC", s.Coin != nil, "硬币包履约与回收"},
	}
	notes := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.built {
			notes = append(notes, e.name+" 已接入（供 "+e.impact+" 使用）")
			continue
		}
		notes = append(notes, e.name+" 未配置：依赖它的能力运行时显式失败（不建假订单、不伪造发放）")
	}
	return notes
}

// Transact 在 go_video_trade_order 库上执行一个事务。
//
// logic 用它把「CAS 推进订单状态 + 写 to_order_event 台账」绑成一次原子操作
// （AGENTS.md §5：状态迁移必须有可回溯台账，且与主表更新同事务）；
// 传出的 session 只能交给 model 的 Tx 变体使用，logic 自身不得拼 SQL。
func (s *ServiceContext) Transact(ctx context.Context, fn func(ctx context.Context, tx sqlx.Session) error) error {
	if s.Conn == nil {
		return errors.New("trade-order: database connection is not configured")
	}
	return s.Conn.TransactCtx(ctx, fn)
}
