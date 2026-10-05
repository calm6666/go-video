// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	danmakurpc "go-video/services/danmaku/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListDanmakuLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 按时间轴分段拉取弹幕
func NewListDanmakuLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListDanmakuLogic {
	return &ListDanmakuLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListDanmaku 透传分段窗口与观看者 mid：用户级屏蔽过滤在 danmaku 服务内完成，
// segment_seconds 回给客户端用于自行计算下一段（不在网关写死分段）。
func (l *ListDanmakuLogic) ListDanmaku(req *types.ParamDanmakuList) (resp *types.DanmakuListResponse, err error) {
	if l.svcCtx.Danmaku == nil {
		return nil, errors.New("danmaku service not configured")
	}
	reply, err := l.svcCtx.Danmaku.ListDanmaku(l.ctx, &danmakurpc.ListDanmakuReq{
		Oid:             req.Oid,
		ViewerMid:       req.ViewerMid,
		StartSeg:        req.StartSeg,
		EndSeg:          req.EndSeg,
		StartProgressMs: req.StartProgressMs,
		EndProgressMs:   req.EndProgressMs,
		Limit:           req.Limit,
		WithSelfPending: req.WithSelfPending,
	})
	if err != nil {
		l.Errorf("gateway/app/listDanmaku: oid=%d seg=[%d,%d] err=%v", req.Oid, req.StartSeg, req.EndSeg, err)
		return nil, err
	}
	return &types.DanmakuListResponse{
		Code:    0,
		Message: "ok",
		Data: types.DanmakuListData{
			Danmaku:        danmakuInfoListToAPI(reply.GetDanmaku()),
			SegmentCounts:  danmakuSegmentCountsToAPI(reply.GetSegmentCounts()),
			SegmentSeconds: reply.GetSegmentSeconds(),
			NextSeg:        reply.GetNextSeg(),
		},
		TTL: 0,
	}, nil
}
