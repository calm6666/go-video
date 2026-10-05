package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListSubtitlesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSubtitlesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSubtitlesLogic {
	return &ListSubtitlesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListSubtitles 查询某媒资的字幕列表。
func (l *ListSubtitlesLogic) ListSubtitles(in *rpc.AssetReq) (*rpc.SubtitlesReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	subs, err := l.svcCtx.Repository.ListSubtitles(l.ctx, in.AssetId)
	if err != nil {
		l.Errorf("asset/ListSubtitles: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	items := make([]*rpc.SubtitleReply, 0, len(subs))
	for _, s := range subs {
		items = append(items, &rpc.SubtitleReply{
			SubId:     s.SubID,
			AssetId:   s.AssetID,
			Lang:      s.Lang,
			Bucket:    s.Bucket,
			ObjectKey: s.ObjectKey,
			Ctime:     s.Ctime,
		})
	}
	return &rpc.SubtitlesReply{Items: items}, nil
}
