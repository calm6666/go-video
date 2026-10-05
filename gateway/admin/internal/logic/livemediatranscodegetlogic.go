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

type LiveMediaTranscodeGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个直播转码任务（state/attempt/heartbeat/version 全可见）
func NewLiveMediaTranscodeGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaTranscodeGetLogic {
	return &LiveMediaTranscodeGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaTranscodeGet 聚合 live-media GetLiveTranscodeTask。
//
// 只读面不挂 AdminPermission（与 audit / cron / live-* 各域同一口径，见 admin.api 头部说明），
// 但 task_id 是服务的查询主键：传 0 只会落到一个不存在的主键上，回给后台的是零值行而不是
// 「你少传了参数」，因此网关先拒。状态与尝试次数的含义全部由服务给出，网关只逐字段投影。
func (l *LiveMediaTranscodeGetLogic) LiveMediaTranscodeGet(req *types.ParamLiveMediaTranscodeGet) (resp *types.LiveMediaTranscodeResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("task_id", req.TaskId); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.GetLiveTranscodeTask(l.ctx, &livemediarpc.LiveTranscodeTaskReq{
		TaskId: req.TaskId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaTranscodeGet: task_id=%d err=%v", req.TaskId, err)
		return nil, err
	}
	return &types.LiveMediaTranscodeResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaTranscodeTaskToAPI(info),
		TTL:     0,
	}, nil
}
