// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	liveroomrpc "go-video/services/live-room/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLiveAreasLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 观众面：直播分区列表（客户端与运营共用读接口）
func NewListLiveAreasLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLiveAreasLogic {
	return &ListLiveAreasLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListLiveAreas parent_area_id 与 state 的 -1 是「不过滤」哨兵（proto 语义），
// 已在 .api 用 default=-1 表达，因此客户端省略参数时不会误取「只有一级分区」。
// 分区维护（UpsertArea）属运营面，不进终端入口。
func (l *ListLiveAreasLogic) ListLiveAreas(req *types.ParamLiveAreas) (resp *types.LiveAreasResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errors.New("live-room service not configured")
	}
	reply, err := l.svcCtx.LiveRoom.ListAreas(l.ctx, &liveroomrpc.ListAreasReq{
		ParentAreaId: req.ParentAreaId,
		State:        req.State,
		Page:         req.Page,
		PageSize:     req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/app/listLiveAreas: parent_area_id=%d state=%d err=%v", req.ParentAreaId, req.State, err)
		return nil, err
	}
	return &types.LiveAreasResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAreasData{
			Areas: liveAreasToAPI(reply.GetAreas()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
