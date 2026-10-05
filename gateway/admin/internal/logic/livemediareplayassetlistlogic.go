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

type LiveMediaReplayAssetListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 回放资产引用分页（含 video 侧审核/发布投影与回收标记）
func NewLiveMediaReplayAssetListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LiveMediaReplayAssetListLogic {
	return &LiveMediaReplayAssetListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// LiveMediaReplayAssetList 聚合 live-media ListReplayAssetRefs。
//
// review_state / review_state_at / published_at 是 video 侧的**只读投影**（事实源不是 live-media），
// retention_state 是引用行的生命周期标记（0 正常、1 待回收、2 已回收）：四列都按值透传，
// 网关不在这里推断「回放到底能不能对外播放」——那要看 video 的结论（AGENTS.md §5/§8）。
func (l *LiveMediaReplayAssetListLogic) LiveMediaReplayAssetList(req *types.ParamLiveMediaReplayAssetList) (resp *types.LiveMediaReplayAssetListResponse, err error) {
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
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"room_id", req.RoomId},
		{"live_session_id", req.SessionId},
		{"anchor_mid", req.AnchorMid},
	} {
		if err := liveNonNeg(f.name, f.v); err != nil {
			return nil, err
		}
	}
	if err := liveNonNeg32("review_state", req.ReviewState); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.LiveMedia.ListReplayAssetRefs(l.ctx, &livemediarpc.ListReplayAssetRefsReq{
		RoomId:        req.RoomId,
		LiveSessionId: req.SessionId,
		ReviewState:   livemediarpc.ReviewState(req.ReviewState),
		AnchorMid:     req.AnchorMid,
		Page:          page,
	})
	if err != nil {
		l.Errorf("gateway/admin/liveMediaReplayAssetList: room_id=%d live_session_id=%d review_state=%d err=%v",
			req.RoomId, req.SessionId, req.ReviewState, err)
		return nil, err
	}
	return &types.LiveMediaReplayAssetListResponse{
		Code:    0,
		Message: "ok",
		Data: types.LiveMediaReplayAssetListData{
			Total: liveMediaPageTotal(reply.GetPage()),
			List:  liveMediaReplayAssetRefsToAPI(reply.GetRefs()),
		},
		TTL: 0,
	}, nil
}
