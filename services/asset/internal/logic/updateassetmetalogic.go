package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateAssetMetaLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateAssetMetaLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateAssetMetaLogic {
	return &UpdateAssetMetaLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// UpdateAssetMeta 更新媒资元数据（由 transcode 完成回调写入 duration/width/height/codec）。
func (l *UpdateAssetMetaLogic) UpdateAssetMeta(in *rpc.UpdateAssetReq) (*rpc.AssetReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	m, err := l.svcCtx.Repository.UpdateAssetMeta(l.ctx, in.AssetId, in.Duration, in.Width, in.Height, in.Codec)
	if err != nil {
		if err == model.ErrAssetNotFound {
			return nil, err
		}
		l.Errorf("asset/UpdateAssetMeta: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	return assetMetaToReply(m), nil
}
