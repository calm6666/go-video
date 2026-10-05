package logic

import (
	"context"
	"fmt"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件同时承载路由读路径的包内共用构件 lgwServingConnections（GetRoomRoute / ListRoomRoutes /
// DrainRoomRoute 三处都要回显 serving_connections，口径必须同一条）：helpers.go 是 22 个方法的
// 公共件、本轮不改动，所以这几个只被路由族用到的构件落在这里，命名统一带 lgw 前缀避免同包撞名。

// lgwServingConnections 取房间实时连接数（RoomRouteInfo.serving_connections 的唯一来源）。
//
// 该字段只能来自 Redis：live_gw_room_route 刻意不存连接数（README 数据分层——每房间连接数是易失数据，
// 落 MySQL 只会拖垮主库）。Redis 故障时回 0 并记降级日志，且**本服务不做任何基于这个 0 的判定**
// （把读数当权限依据会出现「Redis 抖动 → 房间看起来没人 → 广播被跳过」这类假结论）。
// degradeLogged 非空时只记一次日志：ListRoomRoutes 整页 50 行都取不到读数时，
// 50 条同样的告警会把真正的故障信号淹掉。
func lgwServingConnections(ctx context.Context, g gate, roomID int64, degradeLogged *bool) int32 {
	n, err := g.svcCtx.Leases.RoomConnectionCount(ctx, roomID, lgwScanLimit(g))
	if err != nil {
		if degradeLogged == nil || !*degradeLogged {
			if degradeLogged != nil {
				*degradeLogged = true
			}
			g.errorf("live-gateway: 房间 %d 连接数读数不可用，serving_connections 按 0 回显（仅观测，不参与判定）: %v",
				roomID, err)
		}
		return 0
	}
	return n
}

type GetRoomRouteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetRoomRouteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetRoomRouteLogic {
	return &GetRoomRouteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询房间路由（广播第一跳与副本节点）
func (l *GetRoomRouteLogic) GetRoomRoute(in *rpc.RoomRouteReq) (*rpc.RoomRouteInfo, error) {
	// 已实现行为：
	// 1. 参数：room_id>0（ErrInvalidRoomID）。
	// 2. 读路径唯一入口 gate.routeForFanout：先按 RoomRouteCacheTTLSeconds 读缓存，miss 回源
	//    live_gw_room_route.FindOne 并回填缓存。它与 BroadcastToRoom 用的是**同一个键、同一份 TTL**，
	//    于是不会出现「运营查到的路由和广播实际打到的节点不一致」这种最难解释的观感。
	// 3. 行不存在：ErrRouteNotFound，绝不返回 state=UNSPECIFIED 的空路由伪装成有效路由
	//    （空路由被调用方读成「有路由但没节点」，广播就会静默丢消息）。
	// 4. replica_nodes 由 LiveGwRoomRoute.ReplicaNodesList() 解 JSON；脏数据在 routeForFanout 里
	//    已按「回源 DB + 记告警」处理，不静默当无副本。
	// 5. serving_connections 是 Redis 实时读数（见 lgwServingConnections），不来自本表。
	// 6. version 原样回传：DrainRoomRoute 要求带 expected_version，投影层丢掉版本号会让运营无法排空。
	// 7. 只读无副作用：不登记路由、不推进状态、不写审计；缓存回填不算业务副作用。
	// 8. 本方法不加调用方门禁（与 ListRoomRoutes 的运营面读门禁不同）：单房间路由是广播链路的
	//    内部依赖，gateway/app 每次下发前都要读；跨房间枚举才暴露节点拓扑。
	if in == nil {
		return nil, model.ErrInvalidRoomID
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	if err := g.requireRoomID(in.GetRoomId()); err != nil {
		return nil, err
	}
	roomID := in.GetRoomId()
	row, replicas, err := g.routeForFanout(l.ctx, roomID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("%w: room_id=%d", model.ErrRouteNotFound, roomID)
	}
	return routeInfo(row, replicas, lgwServingConnections(l.ctx, g, roomID, nil)), nil
}
