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

type LiveMediaRetentionGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个回收任务（scanned/deleted/skipped 是先登记后执行的凭证）
func NewLiveMediaRetentionGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRetentionGetLogic {
	return &LiveMediaRetentionGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRetentionGet 聚合 live-media GetRetentionTask。
//
// scanned/deleted/skipped 与 purge 是核对「有没有误删」的唯一依据（purge=false 时 deleted 恒为 0），
// 一列都不裁；operator 直接回读任务行而不是网关日志，因为登记时已按 admin:<admin_id> 落账。
func (l *LiveMediaRetentionGetLogic) LiveMediaRetentionGet(req *types.ParamLiveMediaRetentionGet) (resp *types.LiveMediaRetentionResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("retention_id", req.RetentionId); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.GetRetentionTask(l.ctx, &livemediarpc.RetentionTaskReq{
		RetentionId: req.RetentionId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRetentionGet: retention_id=%d err=%v", req.RetentionId, err)
		return nil, err
	}
	return &types.LiveMediaRetentionResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaRetentionTaskToAPI(info),
		TTL:     0,
	}, nil
}
