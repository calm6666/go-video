// 本文件是 gateway/admin 的手写投影扩展（非 goctl 生成产物）。
//
// live-gateway RPC ↔ 管理后台投影 + 写入口门槛。
//
// 职责边界（AGENTS.md §4/§5/§8），与 conv_live.go / conv_live_ingest.go 同一套口径：
//  1. 网关只做三件事：入参形态门槛（会话身份、幂等键非空、数值非负）、调用下游、逐字段投影。
//     租约三元组是否匹配、角色权限矩阵、配额与限流、票据有效期、路由版本冲突、广播是否该丢弃
//     全部由 services/live-gateway 判定，网关不复算，也不把下游错误改写成看起来成功的空结果；
//  2. 本域的操作者字段是 `operator string`（审计字符串，不是用户 mid 空间），因此网关**直接用
//     会话 admin_id 生成 admin:<admin_id>**，表单不得声明 operator——后台的每一次处置都能追到
//     具体账号，不存在 live-room / live-ingest 那种「operator_mid 该填谁的 mid」的歧义；
//  3. 凭据不回显：LeaseInfo.reconnect_ticket 是一次性重连凭据（proto 注明「返回体即客户端
//     重连凭据」），本文件不为它留任何投影出口，与不接 RotateStreamKey 同一条理由；
//     BroadcastToRoomReq.sender_lease_id / sender_ticket 同样是客户端凭据，后台发的是运营消息，
//     网关固定 sender_role=OPERATOR + sender_mid=0，不暴露这两个字段；
//  4. 列表一律返回非 nil 切片：把 null 与 [] 区分给前端是多余的契约负担。

package logic

import (
	"context"
	"errors"
	"fmt"

	"go-video/gateway/admin/internal/middleware"
	"go-video/gateway/admin/internal/types"
	livegatewayrpc "go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// errLiveGatewayNotConfigured：未配置 LiveGatewayRPC 时 live-gateway 域路由一律返回它。
// 不退化成伪造空列表——那会让后台把「下游没接」读成「房间没有连接」。
var errLiveGatewayNotConfigured = errors.New("live-gateway service not configured")

// errLiveGatewaySubjectRequired：连接视图与广播审计都必须落在具体房间上（room_id 是服务的
// 查询主键与配额作用域），没有房间就没有可解释的处置对象。
var errLiveGatewaySubjectRequired = errors.New("gateway/admin: room_id required")

// liveGatewayOperator 是写入口的统一门槛：会话身份必须存在（AdminPermission 判定通过后才会挂上），
// 并由它生成服务侧审计用的 operator 字符串。
//
// 只打路由与 admin_id：reason 正文、payload 内容、lease/ticket 凭据都不进日志（AGENTS.md §4）。
func liveGatewayOperator(ctx context.Context, route string) (string, error) {
	id, ok := middleware.AdminFromContext(ctx)
	if !ok {
		return "", errLiveSessionRequired
	}
	operator := fmt.Sprintf("admin:%d", id.AdminID)
	logx.WithContext(ctx).Infof("gateway/admin/%s: operator=%s", route, operator)
	return operator, nil
}

// liveGatewayIdempotencyGate 拦住空幂等键。与 live-room 同一口径：只判空、不改值，
// TrimSpace 后回写会让服务侧的 request_id 去重失去语义。
func liveGatewayIdempotencyGate(requestID string) error {
	return requireNonEmpty("request_id", requestID)
}

// liveGatewayPage 组装 PageParam。pn/ps 的取值合法性（ps 上限、pn 下界）由 live-gateway 夹取，
// 网关只做非负门槛，不复算分页。
func liveGatewayPage(pn, ps int32) (*livegatewayrpc.PageParam, error) {
	if err := liveNonNeg32("pn", pn); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("ps", ps); err != nil {
		return nil, err
	}
	return &livegatewayrpc.PageParam{Pn: pn, Ps: ps}, nil
}

// liveGatewayKickSubjectGate：KickConnectionReq 的 mid / lease_id 至少给一个。
// 都不给会让服务无从判定要断哪条连接（服务不回「你少传了参数」，而是按三元组查不到）。
func liveGatewayKickSubjectGate(mid int64, leaseID string) error {
	if mid <= 0 && leaseID == "" {
		return errors.New("gateway/admin: mid or lease_id required")
	}
	return liveNonNeg("mid", mid)
}

// liveQuotaNonNeg 逐位挡负数：配额里的 0 普遍是「继承上一层/不限制」的合法哨兵，
// 负值没有任何下游语义。上界（TTL 上下限、MaxPayloadBytes 上限、QPS 合理性）
// 是 live-gateway 的判定域，网关不复算。
func liveQuotaNonNeg(q types.AccessQuotaInput) error {
	for _, f := range []struct {
		name string
		v    int32
	}{
		{"quota.max_connections", q.MaxConnections},
		{"quota.broadcast_qps", q.BroadcastQps},
		{"quota.danmaku_qps", q.DanmakuQps},
		{"quota.lease_ttl_seconds", q.LeaseTtlSeconds},
		{"quota.ticket_ttl_seconds", q.TicketTtlSeconds},
		{"quota.max_payload_bytes", q.MaxPayloadBytes},
	} {
		if err := liveNonNeg32(f.name, f.v); err != nil {
			return err
		}
	}
	return nil
}

// --- rpc → 后台 types 投影 ---

// liveConnectionLeaseToAPI 租约投影。**刻意丢掉 ReconnectTicket**：
// 它能一次性换取该 (mid, room) 的新租约，回显到后台页面等于让控制台成为凭据通道。
// lease_id / conn_id 保留：运营定位单条连接要用它，而 KickConnection 仍需服务端校验三元组，
// 泄露一个 ID 不构成会话接管。
func liveConnectionLeaseToAPI(p *livegatewayrpc.LeaseInfo) types.LiveConnectionLease {
	if p == nil {
		return types.LiveConnectionLease{}
	}
	return types.LiveConnectionLease{
		LeaseId:         p.GetLeaseId(),
		ConnId:          p.GetConnId(),
		RoomId:          p.GetRoomId(),
		Mid:             p.GetMid(),
		Role:            int32(p.GetRole()),
		NodeId:          p.GetNodeId(),
		State:           int32(p.GetState()),
		IssuedAt:        p.GetIssuedAt(),
		ExpireAt:        p.GetExpireAt(),
		TtlSeconds:      p.GetTtlSeconds(),
		RenewCount:      p.GetRenewCount(),
		LastHeartbeatAt: p.GetLastHeartbeatAt(),
		TraceId:         p.GetTraceId(),
	}
}

func liveConnectionLeasesToAPI(list []*livegatewayrpc.LeaseInfo) []types.LiveConnectionLease {
	out := make([]types.LiveConnectionLease, 0, len(list))
	for _, item := range list {
		out = append(out, liveConnectionLeaseToAPI(item))
	}
	return out
}

func liveRoomRoutesToAPI(list []*livegatewayrpc.RoomRouteInfo) []types.RoomRouteInfo {
	out := make([]types.RoomRouteInfo, 0, len(list))
	for _, r := range list {
		if r == nil {
			out = append(out, types.RoomRouteInfo{})
			continue
		}
		replicas := make([]string, 0, len(r.GetReplicaNodes()))
		replicas = append(replicas, r.GetReplicaNodes()...)
		out = append(out, types.RoomRouteInfo{
			RoomId:             r.GetRoomId(),
			NodeId:             r.GetNodeId(),
			ReplicaNodes:       replicas,
			State:              int32(r.GetState()),
			ShardCount:         r.GetShardCount(),
			ServingConnections: r.GetServingConnections(),
			Version:            r.GetVersion(),
			UpdatedAt:          r.GetUpdatedAt(),
			Ctime:              r.GetCtime(),
		})
	}
	return out
}

func liveRoomRouteToAPI(p *livegatewayrpc.RoomRouteInfo) types.RoomRouteInfo {
	if p == nil {
		return types.RoomRouteInfo{}
	}
	replicas := make([]string, 0, len(p.GetReplicaNodes()))
	replicas = append(replicas, p.GetReplicaNodes()...)
	return types.RoomRouteInfo{
		RoomId:             p.GetRoomId(),
		NodeId:             p.GetNodeId(),
		ReplicaNodes:       replicas,
		State:              int32(p.GetState()),
		ShardCount:         p.GetShardCount(),
		ServingConnections: p.GetServingConnections(),
		Version:            p.GetVersion(),
		UpdatedAt:          p.GetUpdatedAt(),
		Ctime:              p.GetCtime(),
	}
}

// liveBroadcastLogsToAPI 广播审计流水投影。服务只存 payload_digest 与字节数，
// 没有正文字段（弹幕/私信正文不落 live-gateway），因此后台在这里天然看不到消息内容。
func liveBroadcastLogsToAPI(list []*livegatewayrpc.BroadcastLogInfo) []types.BroadcastLogInfo {
	out := make([]types.BroadcastLogInfo, 0, len(list))
	for _, b := range list {
		if b == nil {
			out = append(out, types.BroadcastLogInfo{})
			continue
		}
		out = append(out, types.BroadcastLogInfo{
			Id:                  b.GetId(),
			MessageId:           b.GetMessageId(),
			RoomId:              b.GetRoomId(),
			Kind:                int32(b.GetKind()),
			SenderMid:           b.GetSenderMid(),
			SenderRole:          int32(b.GetSenderRole()),
			EventId:             b.GetEventId(),
			PayloadDigest:       b.GetPayloadDigest(),
			PayloadBytes:        b.GetPayloadBytes(),
			FanoutNodes:         b.GetFanoutNodes(),
			TargetedConnections: b.GetTargetedConnections(),
			State:               b.GetState(),
			DropReason:          int32(b.GetDropReason()),
			SourceService:       b.GetSourceService(),
			TraceId:             b.GetTraceId(),
			Ctime:               b.GetCtime(),
		})
	}
	return out
}

func liveAccessQuotaToAPI(p *livegatewayrpc.AccessQuotaInfo) types.AccessQuotaInfo {
	if p == nil {
		return types.AccessQuotaInfo{}
	}
	return types.AccessQuotaInfo{
		Scope:            int32(p.GetScope()),
		ScopeId:          p.GetScopeId(),
		ScopeKey:         p.GetScopeKey(),
		MaxConnections:   p.GetMaxConnections(),
		BroadcastQps:     p.GetBroadcastQps(),
		DanmakuQps:       p.GetDanmakuQps(),
		LeaseTtlSeconds:  p.GetLeaseTtlSeconds(),
		TicketTtlSeconds: p.GetTicketTtlSeconds(),
		MaxPayloadBytes:  p.GetMaxPayloadBytes(),
		AllowGuest:       p.GetAllowGuest(),
		Version:          p.GetVersion(),
		UpdatedBy:        p.GetUpdatedBy(),
		Ctime:            p.GetCtime(),
		Mtime:            p.GetMtime(),
	}
}

// liveAccessQuotaForRPC 把后台可写字段组装成 protobuf AccessQuotaInfo。
// version / updated_by / ctime / mtime 保持零值：并发版本走请求顶层 expected_version，
// 修改者由服务按 operator 落账，后台声明「谁改的」只会污染审计。
func liveAccessQuotaForRPC(in types.AccessQuotaInput) *livegatewayrpc.AccessQuotaInfo {
	return &livegatewayrpc.AccessQuotaInfo{
		Scope:            livegatewayrpc.QuotaScope(in.Scope),
		ScopeId:          in.ScopeId,
		ScopeKey:         in.ScopeKey,
		MaxConnections:   in.MaxConnections,
		BroadcastQps:     in.BroadcastQps,
		DanmakuQps:       in.DanmakuQps,
		LeaseTtlSeconds:  in.LeaseTtlSeconds,
		TicketTtlSeconds: in.TicketTtlSeconds,
		MaxPayloadBytes:  in.MaxPayloadBytes,
		AllowGuest:       in.AllowGuest,
	}
}

// livePageTotal 回读 PageResult.total。服务未回 page 时返回 0——
// 这与「total 真的是 0」在 HTTP 层无法区分，因此投影里 total 只做展示，
// 计数缺失由 live-gateway 侧决定，网关不猜。
func livePageTotal(page *livegatewayrpc.PageResult) int32 {
	return page.GetTotal()
}
