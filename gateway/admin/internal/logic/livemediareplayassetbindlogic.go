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

type LiveMediaReplayAssetBindLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回填回放产物与 asset/稿件的引用（只存引用，不推进稿件状态）
func NewLiveMediaReplayAssetBindLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplayAssetBindLogic {
	return &LiveMediaReplayAssetBindLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplayAssetBind 聚合 live-media BindReplayAsset。
//
// 这条路由**只写引用行**：不动 asset_meta、不动 video_submission、不调用任何推进稿件状态的接口
// （AGENTS.md §5/§8 —— 回放的审核与发布归 video / moderation-orchestrator）。因此它的权限点是
// live:replay:bind，与 submit（登记拼接）和 state（刷新投影）分列。
//
// asset_id 与 aid 至少给一个（两阶段回填：先登记媒资、后建稿），都为空时这一行没有任何外部引用可存，
// 纯属噪声；bvid 是冗余展示字段，事实源仍是 video，网关不去反查补齐。bucket 与 object_key 成对。
// replay_id 对应的回放处于哪个阶段才允许绑定，由 live-media 判定，网关不查台账。
// 本方法没有 operator 位（缺口见 admin.api 与 README）。
func (l *LiveMediaReplayAssetBindLogic) LiveMediaReplayAssetBind(req *types.ParamLiveMediaReplayAssetBind) (resp *types.LiveMediaReplayAssetBindResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaReplayAssetBind"); err != nil {
		return nil, err
	}
	if err := liveMediaIdempotencyGate(req.RequestId); err != nil {
		return nil, err
	}
	if err := liveRequiredID("replay_id", req.ReplayId); err != nil {
		return nil, err
	}
	if err := liveMediaAssetRefGate(req.AssetId, req.Aid); err != nil {
		return nil, err
	}
	if err := liveMediaRefPair("bucket", "object_key", req.Bucket, req.ObjectKey); err != nil {
		return nil, err
	}
	if err := liveNonNeg("duration_ms", req.DurationMs); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.BindReplayAsset(l.ctx, &livemediarpc.BindReplayAssetReq{
		ReplayId:   req.ReplayId,
		AssetId:    req.AssetId,
		Aid:        req.Aid,
		Bvid:       req.Bvid,
		Bucket:     req.Bucket,
		ObjectKey:  req.ObjectKey,
		DurationMs: req.DurationMs,
		RequestId:  req.RequestId,
		TraceId:    req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplayAssetBind: replay_id=%d asset_id=%d aid=%d request_id=%s err=%v",
			req.ReplayId, req.AssetId, req.Aid, req.RequestId, err)
		return nil, err
	}
	return &types.LiveMediaReplayAssetBindResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaReplayAssetRefToAPI(info),
		TTL:     0,
	}, nil
}
