package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/live-gateway/internal/svc"
	"go-video/services/live-gateway/model"
	"go-video/services/live-gateway/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRoomRoutesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRoomRoutesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRoomRoutesLogic {
	return &ListRoomRoutesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询房间路由（运营/发布排障）
func (l *ListRoomRoutesLogic) ListRoomRoutes(in *rpc.ListRoomRoutesReq) (*rpc.ListRoomRoutesReply, error) {
	// 已实现行为：
	// 1. 参数：node_id 空表示不过滤，非空经 repository.CleanNodeID 归一（超长/含空白直接 ErrEmptyNodeID，
	//    它同时是 Redis key 与 MySQL 列的取值形状）；state 经 model.ValidRouteState 判定，
	//    UNSPECIFIED 不过滤，非法取值直接 ErrInvalidTransition —— 静默当「无过滤」会让运营
	//    看着全量列表以为过滤生效了。
	// 2. 分页一律经 gate.pageParams：ps 越界**报错而不是截断**（截断会让调用方写出错误的翻页循环）。
	//    model 侧 clampPage 仍会兜一次，两处口径一致。
	// 3. 授权：跨房间枚举路由暴露节点拓扑，属运营面能力 → gate.requireOperatorRead。
	//    RequireAttestedOperator=false（默认）时放行未归因主体但记告警，接入 metadata 后翻开关即收紧。
	// 4. 读 live_gw_room_route.List：node_id 同时命中主节点与副本节点
	//    （primary_node = ? OR JSON_CONTAINS(replica_nodes, JSON_QUOTE(?))）——只看主节点会让
	//    大房间的分片副本在排障视图里凭空消失。排序 room_id ASC，保证翻页可重放。
	// 5. 本方法**不走路由读缓存**（区别于 GetRoomRoute）：发布排障要看此刻真实归属，
	//    10 秒缓存会让人误判「节点已经排空完了」。
	// 6. serving_connections 逐行取 Redis 读数，Redis 故障时整页只记一次降级日志并按 0 回显。
	// 7. 空结果返回 total=0 + 空切片（不报错）；page 恒非 nil，调用方不必判空。
	// 8. 只读：不写 MySQL、不写 Redis 业务键、不写审计。
	if in == nil {
		in = &rpc.ListRoomRoutesReq{}
	}
	g := newGate(l.ctx, l.svcCtx, l.Logger)
	if _, err := g.requireOperatorRead("ListRoomRoutes"); err != nil {
		return nil, err
	}
	nodeID := strings.TrimSpace(in.GetNodeId())
	if nodeID != "" {
		clean, cerr := g.requireNodeID(nodeID)
		if cerr != nil {
			return nil, cerr
		}
		nodeID = clean
	}
	state := int32(in.GetState())
	if state != model.RouteStateUnspecified && !model.ValidRouteState(state) {
		return nil, fmt.Errorf("%w: unknown route state %d, refusing to list unfiltered",
			model.ErrInvalidTransition, state)
	}
	pn, ps, err := g.pageParams(in.GetPage().GetPn(), in.GetPage().GetPs())
	if err != nil {
		return nil, err
	}
	store, err := g.svcCtx.RequireStore()
	if err != nil {
		return nil, err
	}
	rows, total, err := store.RoomRoutes.List(l.ctx, model.RoomRouteFilter{
		NodeId:      nodeID,
		State:       state,
		Pn:          pn,
		Ps:          ps,
		MaxPageSize: g.cfg().MaxPageSize,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*rpc.RoomRouteInfo, 0, len(rows))
	var degradeLogged bool
	for _, row := range rows {
		if row == nil {
			continue
		}
		replicas, derr := row.ReplicaNodesList()
		if derr != nil {
			// 脏数据不回 500：路由主节点仍然有效，但必须把「副本视图不可信」说清楚（与 GetRoomRoute 同口径）。
			g.errorf("live-gateway: live_gw_room_route replica_nodes 解析失败 room_id=%d，本行副本按空回显: %v",
				row.RoomId, derr)
			replicas = nil
		}
		out = append(out, routeInfo(row, replicas, lgwServingConnections(l.ctx, g, row.RoomId, &degradeLogged)))
	}
	return &rpc.ListRoomRoutesReply{Page: pageResult(total), Routes: out}, nil
}
