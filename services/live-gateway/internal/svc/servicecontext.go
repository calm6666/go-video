// Code scaffolded by goctl. Safe to edit.

package svc

import (
	"errors"
	"fmt"
	"os"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/redis"

	"go-video/services/live-gateway/internal/config"
	"go-video/services/live-gateway/internal/repository"
)

// ServiceContext 是 live-gateway 的运行时上下文。
//
// 装配原则（AGENTS.md §3/§5 + 服务 README「数据分层」）：
//   - Leases（Redis）是在线态的**唯一事实源**：租约、房间订阅集合、心跳簿记、广播去重窗口、
//     限流计数、票据有效位。丢失只造成「客户端重连」，不造成业务事实丢失，因此不镜像进 MySQL；
//   - Store（MySQL go_video_live_gateway）只有四张需要审计/重建的表：
//     live_gw_room_route / live_gw_access_quota / live_gw_broadcast_log / live_gw_reconnect_ticket；
//   - Rooms 跨服务读 live-room 的房间归属与可广播状态，本服务不复判房间事实；
//   - Fanout 是到 WS 接入层的下发通道，本期未接线（UnwiredTransport），
//     下发一律显式失败而不是「成功下发 0 人」；
//   - Signer 用 HMAC-SHA256 签发/验签重连票据，密钥只从 Security.TicketSignKeyRef 命名的环境变量注入，
//     字面量不进仓库；未注入时签发与兑换都失败（MissingSigner），绝不退化成固定 key。
//
// 依赖缺失一律不 panic：Redis 未配置时用 unavailableLeaseStore（每个方法显式报错），
// MySQL 未配置时 Store 为 nil 并由 RequireStore 在调用点报错。
// 这样服务能在灰度环境启动，同时**任何**请求都不会拿到假成功。
type ServiceContext struct {
	Config config.Config

	// Cache 原始 Redis 句柄，仅用于启动期探活与诊断；业务读写一律走 Leases。
	Cache *redis.Redis

	// Leases 在线态存储（Redis 实现 / 未配置时是显式失败的占位实现，永不为 nil）。
	Leases repository.LeaseStore

	// Store MySQL 四表；nil 表示 DataSource 未配置，DB 相关方法必须以错误失败。
	Store *repository.Store

	// Rooms live-room 房间门禁；未配置时是 UnwiredRoomGate（查询显式失败）。
	Rooms repository.RoomGate

	// Fanout 跨节点下发通道；本期是 UnwiredTransport。
	Fanout repository.Transport

	// Signer 重连票据签名器；密钥未注入时是 MissingSigner。
	Signer repository.Signer
}

// NewServiceContext 构造上下文。
//
// 刻意不做「下游连不上就退出」的强校验：领域服务要能在灰度环境独立启动，
// 缺依赖的后果推迟到调用点显式报错，并由 Notes() 在启动日志里点名「哪些能力必然失败」。
func NewServiceContext(c config.Config) *ServiceContext {
	ctx := &ServiceContext{Config: c}

	if c.CacheRedis.Host != "" {
		rds := redis.MustNewRedis(c.CacheRedis)
		ctx.Cache = rds
		store, err := repository.NewRedisLeaseStore(rds, int(c.LiveGateway.ReconnectGraceSeconds),
			int(c.LiveGateway.DedupWindowSeconds), int(c.LiveGateway.RateWindowSeconds))
		if err != nil {
			logx.Errorf("live-gateway/svc: lease store: %v", err)
			store = repository.NewUnavailableLeaseStore(err.Error())
		}
		ctx.Leases = store
	} else {
		ctx.Leases = repository.NewUnavailableLeaseStore("CacheRedis.Host is empty")
	}

	if store, err := repository.NewStore(c.DataSource); err == nil {
		ctx.Store = store
	} else {
		logx.Errorf("live-gateway/svc: mysql store: %v", err)
	}

	rooms, err := repository.NewLiveRoomGateFromConf(c.LiveRoomRPC)
	if err != nil {
		logx.Errorf("live-gateway/svc: live-room gate: %v", err)
		rooms = repository.UnwiredRoomGate{}
	}
	ctx.Rooms = rooms

	ctx.Fanout = repository.UnwiredTransport{}
	ctx.Signer = newSigner(c)

	for _, note := range ctx.Notes() {
		logx.Infof("live-gateway/svc: %s", note)
	}
	return ctx
}

// newSigner 从配置命名的环境变量读票据签名密钥。
// 读不到就用 MissingSigner：日志里只出现**变量名**，不出现任何密钥字面量（AGENTS.md §6）。
func newSigner(c config.Config) repository.Signer {
	ref := c.Security.TicketSignKeyRef
	if ref == "" {
		ref = "LIVEGW_TICKET_SIGN_KEY"
	}
	s, err := repository.NewSigner(os.Getenv(ref), ref)
	if err != nil {
		if !errors.Is(err, repository.ErrSignerMissing) {
			logx.Errorf("live-gateway/svc: ticket signer: %v", err)
		}
		return repository.MissingSigner{Ref: ref}
	}
	return s
}

// RequireStore 返回 MySQL Store；未配置时给出可归因错误而不是 nil 解引用 panic。
// 所有需要 DB 的 logic 都必须经它取用。
func (s *ServiceContext) RequireStore() (*repository.Store, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("%w: DataSource is empty in this environment", repository.ErrStoreUnavailable)
	}
	return s.Store, nil
}

// Notes 输出装配期诊断，便于运维确认「哪些能力在这个环境必然失败」。
func (s *ServiceContext) Notes() []string {
	notes := make([]string, 0, 6)
	if s.Cache == nil {
		notes = append(notes, "CacheRedis 未配置：租约/订阅/心跳/去重/限流/票据有效位全部以 store-unavailable 显式失败，绝不返回假成功")
	}
	if s.Store == nil {
		notes = append(notes, "DataSource 未配置：路由登记、配额解析、广播审计与票据审计都以 store-unavailable 显式失败")
	}
	if !s.Rooms.Available() {
		notes = append(notes, "LiveRoomRPC 未配置：ANCHOR 归属与 ROOM_CLOSED 判定问不到真值，主播态接入一律降级为 VIEWER（fail-closed）")
	}
	if !s.Fanout.Available() {
		notes = append(notes, "下发通道未接线（internal/connection Manager 不存在）：BroadcastToRoom/SendToUser/ForwardSystemEvent 以 DROP_REASON_TRANSPORT_UNAVAILABLE 显式失败")
	}
	if !s.Signer.Available() {
		notes = append(notes, "票据签名密钥未注入（环境变量名见 Security.TicketSignKeyRef）：IssueReconnectTicket/RedeemReconnectTicket 显式失败，绝不使用固定 key 或不签名")
	}
	return notes
}
