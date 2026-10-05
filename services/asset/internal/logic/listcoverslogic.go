package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCoversLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListCoversLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCoversLogic {
	return &ListCoversLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListCovers 查询某媒资的封面列表。
func (l *ListCoversLogic) ListCovers(in *rpc.AssetReq) (*rpc.CoversReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	covers, err := l.svcCtx.Repository.ListCovers(l.ctx, in.AssetId)
	if err != nil {
		l.Errorf("asset/ListCovers: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	items := make([]*rpc.CoverReply, 0, len(covers))
	for _, c := range covers {
		items = append(items, &rpc.CoverReply{
			CoverId:   c.CoverID,
			AssetId:   c.AssetID,
			Bucket:    c.Bucket,
			ObjectKey: c.ObjectKey,
			Width:     c.Width,
			Height:    c.Height,
			Ctime:     c.Ctime,
		})
	}
	return &rpc.CoversReply{Items: items}, nil
}
