package repository

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/model"
)

// FanoutRequest 一次扇出/单播的下发请求（值语义，Transport 实现不得回写）。
//
// 注意：这里不带 payload 的副本指针，也不带任何凭据原文——扇出目标由 lease_id 列表表达，
// 由接入层把它映射回真实连接，本服务不把 conn_id/IP 交给跨节点通道。
type FanoutRequest struct {
	RoomID    int64
	MessageID string
	Kind      int32
	// Nodes 是本服务判定的扇出节点（已排除 DRAINING，除非调用方明确要求含副本）。
	Nodes []string
	// LeaseIDs 非空时是精确投递（单播/定向）；为空表示节点侧按订阅集合自行匹配。
	LeaseIDs []string
	Topics   []string
	Roles    []string
	Payload  []byte
	ExpireAt int64
	TraceID  string
}

// FanoutResult 下发结果。TargetedConnections 是接入层回报的实际命中连接数，
// 本服务不猜数字：猜出来的「已下发给 N 人」在排障时比 0 更有害。
type FanoutResult struct {
	FanoutNodes         int32
	TargetedConnections int32
}

// Transport 是跨节点/到 WS 接入层的下发通道。
//
// 本期未接线（仓库无 websocket 依赖、禁止新增；internal/connection 与 internal/consumer 尚未创建），
// 由 UnwiredTransport 显式返回 model.ErrTransportUnavailable：
// 静默「成功下发 0 人」会把审核处置变成事实上的未执行（README 安全约束）。
type Transport interface {
	// Fanout 向房间扇出（BroadcastToRoom / ForwardDanmaku / ForwardSystemEvent 共用）。
	Fanout(ctx context.Context, req *FanoutRequest) (*FanoutResult, error)
	// Deliver 定向投递到若干租约（SendToUser）。
	Deliver(ctx context.Context, req *FanoutRequest, leaseIDs []string) (*FanoutResult, error)
	// CloseConnections 请求接入层关闭若干连接（KickConnection）。
	// 返回关闭条数；未接线时返回 model.ErrTransportUnavailable，且**不回滚**已完成的凭据撤销。
	CloseConnections(ctx context.Context, nodeID string, leaseIDs []string, reason string) (int32, error)
	// Available 通道是否已接线（false 时上面三个方法必然失败）。
	Available() bool
}

// UnwiredTransport 未接线实现：所有下发都显式失败。
type UnwiredTransport struct{}

func (UnwiredTransport) Available() bool { return false }

func (UnwiredTransport) Fanout(_ context.Context, req *FanoutRequest) (*FanoutResult, error) {
	return nil, transportErr("Fanout", req)
}

func (UnwiredTransport) Deliver(_ context.Context, req *FanoutRequest, _ []string) (*FanoutResult, error) {
	return nil, transportErr("Deliver", req)
}

func (UnwiredTransport) CloseConnections(_ context.Context, nodeID string, leaseIDs []string, _ string) (int32, error) {
	return 0, fmt.Errorf("%w: CloseConnections node=%q leases=%d (internal/connection Manager not wired in this build)",
		model.ErrTransportUnavailable, sanitizeNode(nodeID), len(leaseIDs))
}

func transportErr(op string, req *FanoutRequest) error {
	roomID := int64(0)
	nodes := 0
	if req != nil {
		roomID = req.RoomID
		nodes = len(req.Nodes)
	}
	return fmt.Errorf("%w: %s room_id=%d nodes=%d (internal/connection Manager not wired in this build)",
		model.ErrTransportUnavailable, op, roomID, nodes)
}

// sanitizeNode 错误文本里的节点标识兜底：换行会污染日志行，超长会撑爆错误消息。
func sanitizeNode(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "-"
	}
	if len(v) > 48 {
		return v[:48] + "…"
	}
	return strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(v)
}
