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

type LiveMediaRetentionListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回收任务分页（回收对象/状态/房间过滤）
func NewLiveMediaRetentionListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRetentionListLogic {
	return &LiveMediaRetentionListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRetentionList 聚合 live-media ListRetentionTasks。
//
// target_kind / state 作为**过滤位**时 UNSPECIFIED 是合法的「不过滤」（与写入口的
// liveMediaEnum 门槛正好相反：那里 0 表示「没决定回收什么/刷新成什么」，必须拒），
// 网关只拒负数并原样透传。
func (l *LiveMediaRetentionListLogic) LiveMediaRetentionList(req *types.ParamLiveMediaRetentionList) (resp *types.LiveMediaRetentionListResponse, err error) {
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
	if err := liveNonNeg32("target_kind", req.TargetKind); err != nil {
		return nil, err
	}
	if err := liveNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListRetentionTasks(l.ctx, &livemediarpc.ListRetentionTasksReq{
		TargetKind: livemediarpc.RetentionTargetKind(req.TargetKind),
		State:      livemediarpc.RetentionState(req.State),
		RoomId:     req.RoomId,
		Page:       page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRetentionList: room_id=%d target_kind=%d state=%d err=%v",
			req.RoomId, req.TargetKind, req.State, err)
		return nil, err
	}
	return &types.LiveMediaRetentionListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaRetentionListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaRetentionTasksToAPI(reply.GetTasks()),
		},
		TTL: 0,
	}, nil
}
