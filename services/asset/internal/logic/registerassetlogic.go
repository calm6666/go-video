package logic

import (
	"context"
	"time"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegisterAssetLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegisterAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegisterAssetLogic {
	return &RegisterAssetLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// RegisterAsset 上传完成后登记媒资。
// 创建 asset_meta 记录，关联 upload_id 与 OSS 路径；初始状态为 UPLOADED。
// 本期不立即触发转码，转码由 video 服务直接调用 transcode 触发（占位）。
func (l *RegisterAssetLogic) RegisterAsset(in *rpc.RegisterAssetReq) (*rpc.AssetReply, error) {
	if in.UploadId <= 0 {
		return nil, model.ErrInvalidUploadID
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Bucket == "" {
		return nil, model.ErrInvalidBucket
	}
	if in.ObjectKey == "" {
		return nil, model.ErrInvalidObjectKey
	}
	now := time.Now().Unix()
	m := &model.AssetMeta{
		UploadID:  in.UploadId,
		Mid:       in.Mid,
		Bucket:    in.Bucket,
		ObjectKey: in.ObjectKey,
		Size:      in.Size,
		Md5:       in.Md5,
		State:     model.StateUploaded,
		Ctime:     now,
		Mtime:     now,
	}
	assetID, err := l.svcCtx.Repository.RegisterAsset(l.ctx, m)
	if err != nil {
		l.Errorf("asset/RegisterAsset: upload_id=%d mid=%d err=%v", in.UploadId, in.Mid, err)
		return nil, err
	}
	return &rpc.AssetReply{
		AssetId:   assetID,
		UploadId:  m.UploadID,
		Mid:       m.Mid,
		Bucket:    m.Bucket,
		ObjectKey: m.ObjectKey,
		Size:      m.Size,
		Md5:       m.Md5,
		State:     rpc.AssetState_STATE_UPLOADED,
		Ctime:     m.Ctime,
		Mtime:     m.Mtime,
	}, nil
}
