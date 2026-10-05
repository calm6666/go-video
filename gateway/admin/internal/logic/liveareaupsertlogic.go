// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveAreaUpsertLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 新建/修改直播分区（area_id=0 新建；名称唯一与停用占用校验在服务侧）
func NewLiveAreaUpsertLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveAreaUpsertLogic {
	return &LiveAreaUpsertLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveAreaUpsert 聚合 live-room UpsertArea。
// area_id=0 新建、>0 修改，是同一个入口的两种意图，由 live-room 用 created 明确回传本次是否新建，
// 网关不改写也不「猜测」成创建/更新两个路由。
// 名称长度与全局唯一、父级存在与环检测、停用前是否仍有子分区或在线房间（checkAreaReusable）
// 全是 live-room 的领域判定，网关不复算；UpsertAreaReq 无 trace_id 字段，因此这里也不透传
// ——不为日志方便而发明下游无处安放的参数。
func (l *LiveAreaUpsertLogic) LiveAreaUpsert(req *types.ParamLiveAreaUpsert) (resp *types.LiveAreaUpsertResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveOperatorGate(l.ctx, "liveAreaUpsert", req.OperatorMid); err != nil {
		return nil, err
	}
	if err := liveIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("area_name", req.AreaName); err != nil {
		return nil, err
	}
	if err := liveNonNeg("area_id", req.AreaId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("parent_area_id", req.ParentAreaId); err != nil {
		return nil, err
	}
	// state 只允许 0/1（-1 是列表面的「不过滤」哨兵，写进来没有对应语义）；
	// 具体取值是否合法仍由 live-room 的 areaStateFilter 判定。
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.UpsertArea(l.ctx, &liveroomrpc.UpsertAreaReq{
		AreaId:       req.AreaId,
		AreaName:     req.AreaName,
		ParentAreaId: req.ParentAreaId,
		Sort:         req.Sort,
		State:        req.State,
		OperatorMid:  req.OperatorMid,
		RequestId:    req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveAreaUpsert: area_id=%d parent_area_id=%d operator_mid=%d request_id=%s err=%v",
			req.AreaId, req.ParentAreaId, req.OperatorMid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveAreaUpsertResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAreaUpsertData{
			AreaId:  reply.GetAreaId(),
			Created: reply.GetCreated(),
		},
		TTL: 0,
	}, nil
}
