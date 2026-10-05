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

type LiveMediaRecordListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 录制任务分页（房间/场次/状态过滤）
func NewLiveMediaRecordListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRecordListLogic {
	return &LiveMediaRecordListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRecordList 聚合 live-media ListLiveRecordTasks。
//
// 与转码列表同一口径：三个过滤位都是服务的「不过滤」哨兵（0/UNSPECIFIED），网关只拒负数，
// 分页夹取与状态取值合法性都留给 live-media（AGENTS.md §5）。
func (l *LiveMediaRecordListLogic) LiveMediaRecordList(req *types.ParamLiveMediaRecordList) (resp *types.LiveMediaRecordListResponse, err error) {
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
	reply, err := l.svcCtx.LiveMedia.ListLiveRecordTasks(l.ctx, &livemediarpc.ListLiveRecordTasksReq{
		RoomId:        req.RoomId,
		LiveSessionId: req.SessionId,
		State:         livemediarpc.LiveRecordState(req.State),
		Page:          page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRecordList: room_id=%d live_session_id=%d state=%d err=%v",
			req.RoomId, req.SessionId, req.State, err)
		return nil, err
	}
	return &types.LiveMediaRecordListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaRecordListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaRecordTasksToAPI(reply.GetTasks()),
		},
		TTL: 0,
	}, nil
}
