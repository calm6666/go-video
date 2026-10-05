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

type LiveAnchorListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 房间主播绑定分页（含已解绑历史行）
func NewLiveAnchorListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveAnchorListLogic {
	return &LiveAnchorListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveAnchorList 聚合 live-room ListAnchors。
// room_id 必填；绑定关系谁能建、单主播房间数上限、房主唯一性都由 live-room 判定，
// 后台在这里只是查台账（主播绑定的变更走 gateway/app 的主播面，后台不代做）。
// only_enabled=false 时把 state=0 的历史行一并回传，这正是排查「房管怎么突然没了」需要的证据。
func (l *LiveAnchorListLogic) LiveAnchorList(req *types.ParamLiveAnchorList) (resp *types.LiveAnchorListResponse, err error) {
	if l.svcCtx.LiveRoom == nil {
		return nil, errLiveServiceNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("role", req.Role); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page", req.Page); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveRoom.ListAnchors(l.ctx, &liveroomrpc.ListAnchorsReq{
		RoomId:      req.RoomId,
		Role:        liveroomrpc.AnchorRole(req.Role),
		OnlyEnabled: req.OnlyEnabled,
		Page:        req.Page,
		PageSize:    req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveAnchorList: room_id=%d role=%d page=%d err=%v",
			req.RoomId, req.Role, req.Page, err)
		return nil, err
	}
	return &types.LiveAnchorListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveAnchorListData{
			List:  liveAnchorsToAPI(reply.GetAnchors()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
