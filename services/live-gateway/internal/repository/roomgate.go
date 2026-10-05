package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"

	"go-video/services/live-gateway/model"
	liveroomrpc "go-video/services/live-room/rpc"
)

// RoomGate 是「房间事实」的读取口：房间归属主播、房间是否可广播都归 live-room 判定，
// 本服务一律不复判（AGENTS.md §5 单一事实源）。
//
// fail-closed 约定：未接线时每个方法都返回 ErrLiveRoomNotConfigured，
// 调用方据此**收紧**（不把连接降为 ANCHOR、不允许主播态广播），
// 绝不把「问不到」当成「检查通过」——方向性降级只在服务 README 里对内部可信事件放行一侧允许。
type RoomGate interface {
	// RoomOwner 返回房间归属主播 mid；房间不存在返回 model.ErrInvalidRoomID 包装的错误。
	RoomOwner(ctx context.Context, roomID int64) (int64, error)
	// IsOwner 判定 mid 是否是该房间的归属主播（ANCHOR 角色与主播运营动作的唯一依据）。
	IsOwner(ctx context.Context, roomID, mid int64) (bool, error)
	// Broadcastable 判定房间当前是否允许下发（已关闭/禁播/停用的房间不允许）。
	// 返回 false 时 reason 是可回显给调用方的脱敏说明，drop 恒为 model.DropRoomClosed。
	Broadcastable(ctx context.Context, roomID int64) (bool, string, error)
	// Available 报告是否真的接了 live-room 客户端；false 时上面三个方法必然失败。
	Available() bool
}

// NewLiveRoomGate 构造跨服务房间门禁。conn 为 nil 时返回 UnwiredRoomGate，
// 让服务在灰度环境仍可启动（缺依赖的后果推迟到调用点显式报错）。
func NewLiveRoomGate(conn grpc.ClientConnInterface) RoomGate {
	if conn == nil {
		return UnwiredRoomGate{}
	}
	return &liveRoomGate{client: liveroomrpc.NewLiveRoomClient(conn)}
}

// NewLiveRoomGateFromConf 用 zrpc 配置构造客户端；配置为空时返回 UnwiredRoomGate。
func NewLiveRoomGateFromConf(c zrpc.RpcClientConf) (RoomGate, error) {
	if c.Target == "" && len(c.Etcd.Hosts) == 0 && len(c.Endpoints) == 0 {
		return UnwiredRoomGate{}, nil
	}
	client, err := zrpc.NewClient(c)
	if err != nil {
		return nil, fmt.Errorf("%w: live-room rpc client: %v", ErrLiveRoomNotConfigured, err)
	}
	return NewLiveRoomGate(client.Conn()), nil
}

type liveRoomGate struct {
	client liveroomrpc.LiveRoomClient
}

func (g *liveRoomGate) Available() bool { return g != nil && g.client != nil }

// RoomOwner 读房间归属主播。这里刻意不复用任何缓存：
// 房间换绑主播是低频但高权限影响的事件，缓存 10 秒就意味着换绑后 10 秒内旧主播仍能踢人。
func (g *liveRoomGate) RoomOwner(ctx context.Context, roomID int64) (int64, error) {
	if !g.Available() {
		return 0, ErrLiveRoomNotConfigured
	}
	if roomID <= 0 {
		return 0, model.ErrInvalidRoomID
	}
	reply, err := g.client.GetRoom(ctx, &liveroomrpc.GetRoomReq{RoomId: roomID})
	if err != nil {
		return 0, fmt.Errorf("%w: live-room GetRoom room_id=%d: %v", ErrLiveRoomNotConfigured, roomID, err)
	}
	if reply == nil || reply.GetRoom() == nil || reply.GetRoom().GetRoomId() <= 0 {
		return 0, fmt.Errorf("live-gateway: room %d not found in live-room: %w", roomID, model.ErrInvalidRoomID)
	}
	return reply.GetRoom().GetOwnerMid(), nil
}

func (g *liveRoomGate) IsOwner(ctx context.Context, roomID, mid int64) (bool, error) {
	if mid <= 0 {
		// 游客（mid=0）永远不可能是房主，也不必去问一次 RPC。
		return false, nil
	}
	owner, err := g.RoomOwner(ctx, roomID)
	if err != nil {
		return false, err
	}
	return owner == mid, nil
}

// Broadcastable 用 live-room 的 RoomState 判定可否下发：
// FINISHED（已关闭）、BANNED（违规禁播）、DISABLED（停用）三种状态一律拒发；
// PENDING/READY/LIVING 放行（开播前的暖场与系统公告仍需要能发到房间里）。
func (g *liveRoomGate) Broadcastable(ctx context.Context, roomID int64) (bool, string, error) {
	if !g.Available() {
		return false, "", ErrLiveRoomNotConfigured
	}
	if roomID <= 0 {
		return false, "", model.ErrInvalidRoomID
	}
	reply, err := g.client.GetRoom(ctx, &liveroomrpc.GetRoomReq{RoomId: roomID})
	if err != nil {
		return false, "", fmt.Errorf("%w: live-room GetRoom room_id=%d: %v", ErrLiveRoomNotConfigured, roomID, err)
	}
	info := reply.GetRoom()
	if info == nil || info.GetRoomId() <= 0 {
		return false, "", fmt.Errorf("live-gateway: room %d not found in live-room: %w", roomID, model.ErrInvalidRoomID)
	}
	switch info.GetState() {
	case liveroomrpc.RoomState_ROOM_STATE_FINISHED:
		return false, "room finished", nil
	case liveroomrpc.RoomState_ROOM_STATE_BANNED:
		return false, "room banned", nil
	case liveroomrpc.RoomState_ROOM_STATE_DISABLED:
		return false, "room disabled", nil
	default:
		return true, "", nil
	}
}

// UnwiredRoomGate 未接入 live-room 时的实现：任何查询都显式失败。
type UnwiredRoomGate struct{}

// ErrUnwired 统一包装，errors.Is(err, ErrLiveRoomNotConfigured) 恒真。
func (UnwiredRoomGate) RoomOwner(context.Context, int64) (int64, error) {
	return 0, fmt.Errorf("%w: LiveRoomRPC not configured in this environment", ErrLiveRoomNotConfigured)
}
func (UnwiredRoomGate) IsOwner(context.Context, int64, int64) (bool, error) {
	return false, fmt.Errorf("%w: LiveRoomRPC not configured in this environment", ErrLiveRoomNotConfigured)
}
func (UnwiredRoomGate) Broadcastable(context.Context, int64) (bool, string, error) {
	return false, "", fmt.Errorf("%w: LiveRoomRPC not configured in this environment", ErrLiveRoomNotConfigured)
}
func (UnwiredRoomGate) Available() bool { return false }

// ErrCallerUnattributed 无法从鉴权上下文归因调用方主体（详见 logic/caller.go 与 README「已知缺口」）。
var ErrCallerUnattributed = errors.New("live-gateway: caller identity not attested")

// CleanNodeID 归一节点标识：去空白、限长、拒绝空格/换行（Redis key 与 MySQL 列共用）。
// 放在这里而不是 logic，是因为 node_id 同时进入 Redis key、路由表列与 fanout 目标，三处口径必须一致。
func CleanNodeID(nodeID string) (string, error) {
	v := strings.TrimSpace(nodeID)
	if v == "" {
		return "", model.ErrEmptyNodeID
	}
	if len(v) > maxKeyPartBytes || strings.ContainsAny(v, " \t\r\n") {
		return "", fmt.Errorf("%w: node_id must be 1..%d bytes without whitespace", model.ErrEmptyNodeID, maxKeyPartBytes)
	}
	return v, nil
}
