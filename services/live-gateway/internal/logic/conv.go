package logic

import (
	"go-video/services/live-gateway/internal/repository"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"
)

// 本文件是 model/Redis 记录 → rpc 投影的**唯一**落点。
// 放在一处有两个原因：
//  1. 脱敏字段（票据原文、设备摘要、载荷正文）只需在一处把关，不会出现「某个方法忘了脱敏」；
//  2. 枚举编号在 model 与 rpc 两侧是手工对齐的（proto 注释即约定），集中映射才能被一个测试覆盖。

// leaseInfo 投影租约。ticket 非空表示**只在这一条路径**回显一次性票据：
// 列表/批量场景一律传空（见 ListRoomConnections 第 6 步：列表里发票据等于批量发放重连凭据）。
func leaseInfo(rec *repository.LeaseRecord, ticket string) *rpc.LeaseInfo {
	if rec == nil {
		return nil
	}
	now := model.NowUnix()
	return &rpc.LeaseInfo{
		LeaseId:         rec.LeaseID,
		ConnId:          rec.ConnID,
		RoomId:          rec.RoomID,
		Mid:             rec.Mid,
		Role:            rpc.ConnRole(model.NormalizeRole(rec.Role)),
		NodeId:          rec.NodeID,
		State:           leaseState(rec.EffectiveState(now)),
		IssuedAt:        rec.IssuedAt,
		ExpireAt:        rec.ExpireAt,
		TtlSeconds:      rec.RemainingTTL(now),
		RenewCount:      rec.RenewCount,
		LastHeartbeatAt: rec.LastHeartbeatAt,
		ReconnectTicket: ticket,
		TraceId:         rec.TraceID,
	}
}

func leaseState(s int32) rpc.LeaseState   { return rpc.LeaseState(s) }
func ticketState(s int32) rpc.TicketState { return rpc.TicketState(s) }
func routeState(s int32) rpc.RouteState   { return rpc.RouteState(s) }
func dropReason(r int32) rpc.DropReason   { return rpc.DropReason(r) }
func broadcastKind(k int32) rpc.BroadcastKind {
	return rpc.BroadcastKind(k)
}
func quotaScope(s int32) rpc.QuotaScope { return rpc.QuotaScope(s) }
func deliveryResult(r int32) rpc.DeliveryResult {
	return rpc.DeliveryResult(r)
}
func platform(p int32) rpc.Platform { return rpc.Platform(p) }

// routeInfo 投影房间路由；servingConnections 必须来自 Redis（本表不存连接数）。
func routeInfo(row *model.LiveGwRoomRoute, replicas []string, serving int32) *rpc.RoomRouteInfo {
	if row == nil {
		return nil
	}
	return &rpc.RoomRouteInfo{
		RoomId:             row.RoomId,
		NodeId:             row.PrimaryNode,
		ReplicaNodes:       replicas,
		State:              routeState(row.State),
		ShardCount:         row.ShardCount,
		ServingConnections: serving,
		Version:            row.Version,
		UpdatedAt:          row.Mtime,
		Ctime:              row.Ctime,
	}
}

// ticketInfo 投影票据。ticket 只允许在「刚签发」时回显（调用方是票据的合法持有者），
// 任何按 ticket_id/hash 反查的路径都必须传空串——回显他人票据等于把凭据发给第三方。
func ticketInfo(row *model.LiveGwReconnectTicket, ticket string) *rpc.ReconnectTicketInfo {
	if row == nil {
		return nil
	}
	return &rpc.ReconnectTicketInfo{
		Ticket:      ticket,
		TicketId:    row.TicketId,
		RoomId:      row.RoomId,
		Mid:         row.Mid,
		ConnId:      row.ConnId,
		NodeId:      row.NodeId,
		State:       ticketState(row.State),
		Role:        rpc.ConnRole(model.NormalizeRole(row.Role)),
		IssuedAt:    row.IssuedAt,
		ExpireAt:    row.ExpireAt,
		UsedAt:      row.UsedAt,
		IssueReason: row.IssueReason,
		TraceId:     row.TraceId,
	}
}

// quotaInfo 投影**解析后的**生效配额（0 值继承语义在返回体里必须已被展开成具体数字，
// 否则客户端还要自己再解一次继承链，两条路径迟早漂移）。
func quotaInfo(scope int32, scopeID int64, scopeKey string, eff *model.EffectiveQuota,
	row *model.LiveGwAccessQuota) *rpc.AccessQuotaInfo {
	if eff == nil {
		return nil
	}
	out := &rpc.AccessQuotaInfo{
		Scope:            quotaScope(scope),
		ScopeId:          scopeID,
		ScopeKey:         scopeKey,
		MaxConnections:   eff.MaxConnections,
		BroadcastQps:     eff.BroadcastQps,
		DanmakuQps:       eff.DanmakuQps,
		LeaseTtlSeconds:  eff.LeaseTtlSeconds,
		TicketTtlSeconds: eff.TicketTtlSeconds,
		MaxPayloadBytes:  eff.MaxPayloadBytes,
		AllowGuest:       eff.AllowGuest,
	}
	if row != nil {
		out.Version = row.Version
		out.UpdatedBy = row.UpdatedBy
		out.Ctime = row.Ctime
		out.Mtime = row.Mtime
		if out.ScopeKey == "" {
			out.ScopeKey = row.ScopeKey
		}
	}
	return out
}

// broadcastLogInfo 投影审计流水。这里能回显的只有 payload_digest 与字节数：
// 表里就没有正文，投影层不可能凭空造出弹幕内容（README 数据分层）。
func broadcastLogInfo(l *model.LiveGwBroadcastLog) *rpc.BroadcastLogInfo {
	if l == nil {
		return nil
	}
	return &rpc.BroadcastLogInfo{
		Id:                  l.Id,
		MessageId:           l.MessageId,
		RoomId:              l.RoomId,
		Kind:                broadcastKind(l.Kind),
		SenderMid:           l.SenderMid,
		SenderRole:          rpc.ConnRole(model.NormalizeRole(l.SenderRole)),
		EventId:             l.EventId,
		PayloadDigest:       l.PayloadDigest,
		PayloadBytes:        l.PayloadBytes,
		FanoutNodes:         l.FanoutNodes,
		TargetedConnections: l.TargetedConnections,
		State:               l.State,
		DropReason:          dropReason(l.DropReason),
		SourceService:       l.SourceService,
		TraceId:             l.TraceId,
		Ctime:               l.Ctime,
	}
}

func pageResult(total int32) *rpc.PageResult { return &rpc.PageResult{Total: total} }
