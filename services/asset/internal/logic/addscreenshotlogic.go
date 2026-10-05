package logic

import (
	"context"
	"time"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddScreenshotLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddScreenshotLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddScreenshotLogic {
	return &AddScreenshotLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddScreenshot 添加截图；返回 shot_id。
func (l *AddScreenshotLogic) AddScreenshot(in *rpc.AddScreenshotReq) (*rpc.ScreenshotReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.Bucket == "" {
		return nil, model.ErrInvalidBucket
	}
	if in.ObjectKey == "" {
		return nil, model.ErrInvalidObjectKey
	}
	if in.Timestamp < 0 {
		return nil, model.ErrInvalidTimestamp
	}
	s := &model.AssetScreenshot{
		AssetID:   in.AssetId,
		Bucket:    in.Bucket,
		ObjectKey: in.ObjectKey,
		Timestamp: in.Timestamp,
		Ctime:     time.Now().Unix(),
	}
	shotID, err := l.svcCtx.Repository.AddScreenshot(l.ctx, s)
	if err != nil {
		l.Errorf("asset/AddScreenshot: asset_id=%d err=%v", in.AssetId, err)
		return nil, err
	}
	return &rpc.ScreenshotReply{
		ShotId:    shotID,
		AssetId:   s.AssetID,
		Bucket:    s.Bucket,
		ObjectKey: s.ObjectKey,
		Timestamp: s.Timestamp,
		Ctime:     s.Ctime,
	}, nil
}
