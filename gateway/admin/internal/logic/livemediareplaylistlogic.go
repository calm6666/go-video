// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	livemediarpc "go-video/services/live-media/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type LiveMediaReplayListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回放任务分页（房间/场次/状态过滤）
func NewLiveMediaReplayListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplayListLogic {
	return &LiveMediaReplayListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplayList 聚合 live-media ListReplayTasks。过滤位与分页口径同转码/录制列表。
func (l *LiveMediaReplayListLogic) LiveMediaReplayList(req *types.ParamLiveMediaReplayList) (resp *types.LiveMediaReplayListResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	page, err := liveMediaPage(req.Pn, req.Ps)
	if err != nil {
		return nil, err
	}
	if err := liveNonNeg("room_id", req.RoomId); err != nil {
		return nil, err
	}
	if err := liveNonNeg("live_session_id", req.SessionId); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListReplayTasks(l.ctx, &livemediarpc.ListReplayTasksReq{
		RoomId:        req.RoomId,
		LiveSessionId: req.SessionId,
		State:         livemediarpc.ReplayState(req.State),
		Page:          page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplayList: room_id=%d live_session_id=%d state=%d err=%v",
			req.RoomId, req.SessionId, req.State, err)
		return nil, err
	}
	return &types.LiveMediaReplayListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaReplayListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaReplayTasksToAPI(reply.GetTasks()),
		},
		TTL: 0,
	}, nil
}
