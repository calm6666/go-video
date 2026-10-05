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

type LiveMediaReplayContentStateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 手工刷新 video 侧审核/发布投影（source 固定 manual；方向单一，不反向推进稿件）
func NewLiveMediaReplayContentStateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplayContentStateLogic {
	return &LiveMediaReplayContentStateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplayContentState 聚合 live-media ApplyReplayContentState（排障入口）。
//
// 方向单一：video → live-media。本路由**不能**推进稿件状态（服务侧也不会有任何反向调用），
// 它只把「从 video 看到的既成事实」写进引用行的投影列，所以权限点叫 live:replay:state，
// 与 bind（写引用）与 submit（登记拼接）分列。
//
// 三条网关口径：
//  1. review_state 必须是具体投影值（1 审核中 … 5 已删除），0=REVIEW_STATE_UNSPECIFIED 在这里
//     没有语义（「投影成未同步」等于把已有结论擦掉），直接拒；published_at 只做非负门槛，
//     它是不是真的发布时间由调用方从 video 读到，网关不复算；
//  2. source 由网关固定为 manual：后台点出来的刷新不可能来自 content.published.v1 消费者，
//     也不可能来自 video.rpc 回调，放开这个字段等于让表单伪造投影留痕；
//  3. event_id 可选（契约里它是「驱动本次同步的事件 ID」，人工刷新没有事件可引，网关不伪造随机值
//     ——那会让服务侧按 event_id 的去重永远命不中，形同放开重复写），因此这条也是 12 条写路由里
//     唯一没有 request_id 的；同时它没有 operator 位，谁刷的只落在网关日志（缺口见 admin.api/README）。
func (l *LiveMediaReplayContentStateLogic) LiveMediaReplayContentState(req *types.ParamLiveMediaReplayContentState) (resp *types.LiveMediaReplayContentStateResponse, err error) {
	if l.svcCtx.LiveMedia == nil {
		return nil, errLiveMediaNotConfigured
	}
	if req == nil {
		return nil, errLiveRequestMissing
	}
	if err := liveMediaSessionGate(l.ctx, "liveMediaReplayContentState"); err != nil {
		return nil, err
	}
	if err := liveMediaReplaySubjectGate(req.ReplayId, req.AssetId); err != nil {
		return nil, err
	}
	if err := liveMediaEnum("review_state", req.ReviewState); err != nil {
		return nil, err
	}
	if err := liveNonNeg("published_at", req.PublishedAt); err != nil {
		return nil, err
	}
	info, err := l.svcCtx.LiveMedia.ApplyReplayContentState(l.ctx, &livemediarpc.ApplyReplayContentStateReq{
		ReplayId:    req.ReplayId,
		AssetId:     req.AssetId,
		ReviewState: livemediarpc.ReviewState(req.ReviewState),
		PublishedAt: req.PublishedAt,
		EventId:     req.EventId,
		Source:      liveMediaContentStateSource,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplayContentState: replay_id=%d asset_id=%d review_state=%d err=%v",
			req.ReplayId, req.AssetId, req.ReviewState, err)
		return nil, err
	}
	return &types.LiveMediaReplayContentStateResponse{
		Code:    0,
		Message: "ok",
		Data:    liveMediaReplayAssetRefToAPI(info),
		TTL:     0,
	}, nil
}
