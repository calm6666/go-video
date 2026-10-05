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

type LiveAreaListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分区字典（含停用项，终端面裁掉的运营字段在此可见）
func NewLiveAreaListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveAreaListLogic {
	return &LiveAreaListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveAreaList 聚合 live-room ListAreas。
// parent_area_id 与 state 的 -1 是「不过滤」哨兵，已在 .api 用 default=-1 表达，
// 所以后台省略参数时不会误取「只有一级分区」或「只有停用分区」——0 在这两个字段上都是合法值。
// 分区列表的缓存 TTL 与页大小上限都由 live-room 决定（AreaListCacheTTLSeconds / AreaPageSize），
// 网关不缓存：字典改了之后后台必须立刻看见，多一层网关缓存就多一段「谁在看旧分区」的争议。
func (l *LiveAreaListLogic) LiveAreaList(req *types.ParamLiveAreaList) (resp *types.LiveAreaListResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveSentinelGE("parent_area_id", req.ParentAreaId); err != nil {
		return nil, err
	}
	if err := liveSentinelGE("state", int64(req.State)); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page", req.Page); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.ListAreas(l.ctx, &liveroomrpc.ListAreasReq{
		ParentAreaId: req.ParentAreaId,
		State:        req.State,
		Page:         req.Page,
		PageSize:     req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveAreaList: parent_area_id=%d state=%d page=%d err=%v",
			req.ParentAreaId, req.State, req.Page, err)
		return nil, err
	}
	return &types.LiveAreaListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAreaListData{
			List:  liveAreasToAPI(reply.GetAreas()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
