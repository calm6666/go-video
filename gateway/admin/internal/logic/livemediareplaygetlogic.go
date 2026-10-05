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

type LiveMediaReplayGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个回放任务（asset_id/aid/bvid 只是引用，发布状态事实源在 video）
func NewLiveMediaReplayGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplayGetLogic {
	return &LiveMediaReplayGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplayGet 聚合 live-media GetReplayTask。
//
// 这里刻意**不**跨服务去 video 查稿件标题/审核状态：asset_id/aid/bvid 只是引用，
// state=6 COMPLETED 的含义是「由 video 投影得知回放已可用」（见 LiveMediaReplayAssetList
// 的只读投影与本域的 ApplyReplayContentState），网关把引用当事实源会误导排障（AGENTS.md §5）。
func (l *LiveMediaReplayGetLogic) LiveMediaReplayGet(req *types.ParamLiveMediaReplayGet) (resp *types.LiveMediaReplayResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveRequiredID("replay_id", req.ReplayId); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.GetReplayTask(l.ctx, &livemediarpc.ReplayTaskReq{
		ReplayId: req.ReplayId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplayGet: replay_id=%d err=%v", req.ReplayId, err)
		return nil, err
	}
	return &types.LiveMediaReplayResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaReplayTaskToAPI(info),
		TTL:     0,
	}, nil
}
