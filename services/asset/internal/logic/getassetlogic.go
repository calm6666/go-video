package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAssetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAssetLogic {
	return &GetAssetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetAsset 查询单个媒资元数据。
func (l *GetAssetLogic) GetAsset(in *rpc.AssetReq) (*rpc.AssetReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	m, err := l.svcCtx.Repository.GetAsset(l.ctx, in.AssetId)
	if err != nil {
		if err == model.ErrAssetNotFound {
			return nil, err
		}
		l.Errorf("asset/GetAsset: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	return assetMetaToReply(m), nil
}
