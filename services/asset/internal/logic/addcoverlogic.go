package logic

import (
	"context"
	"time"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddCoverLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddCoverLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddCoverLogic {
	return &AddCoverLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddCover 添加封面；返回 cover_id。
// 不校验 asset 是否存在（由 DB 外键或上层调用顺序保证），仅校验入参。
func (l *AddCoverLogic) AddCover(in *rpc.AddCoverReq) (*rpc.CoverReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.Bucket == "" {
		return nil, model.ErrInvalidBucket
	}
	if in.ObjectKey == "" {
		return nil, model.ErrInvalidObjectKey
	}
	c := &model.AssetCover{
		AssetID:   in.AssetId,
		Bucket:    in.Bucket,
		ObjectKey: in.ObjectKey,
		Width:     in.Width,
		Height:    in.Height,
		Ctime:     time.Now().Unix(),
	}
	coverID, err := l.svcCtx.Repository.AddCover(l.ctx, c)
	if err != nil {
		l.Errorf("asset/AddCover: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	return &rpc.CoverReply{
		CoverId:   coverID,
		AssetId:   c.AssetID,
		Bucket:    c.Bucket,
		ObjectKey: c.ObjectKey,
		Width:     c.Width,
		Height:    c.Height,
		Ctime:     c.Ctime,
	}, nil
}
