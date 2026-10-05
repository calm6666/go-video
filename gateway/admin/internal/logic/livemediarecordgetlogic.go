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

type LiveMediaRecordGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个录制任务（last_seq/gap_count 是断点续录与时间轴空洞的读数）
func NewLiveMediaRecordGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaRecordGetLogic {
	return &LiveMediaRecordGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaRecordGet 聚合 live-media GetLiveRecordTask。
//
// 这一行是「录制到底跑到哪」的唯一事实源：last_seq 决定断点续录从哪一片继续，
// gap_count 决定拼出来的回放有没有洞，version 是回传 expected_version 的令牌，全部按值投影。
func (l *LiveMediaRecordGetLogic) LiveMediaRecordGet(req *types.ParamLiveMediaRecordGet) (resp *types.LiveMediaRecordResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("record_id", req.RecordId); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.GetLiveRecordTask(l.ctx, &livemediarpc.LiveRecordTaskReq{
		RecordId: req.RecordId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaRecordGet: record_id=%d err=%v", req.RecordId, err)
		return nil, err
	}
	return &types.LiveMediaRecordResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaRecordTaskToAPI(info),
		TTL:     0,
	}, nil
}
