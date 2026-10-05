package logic

import (
	"context"
	"time"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddSubtitleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddSubtitleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddSubtitleLogic {
	return &AddSubtitleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AddSubtitle 添加字幕；返回 sub_id。
func (l *AddSubtitleLogic) AddSubtitle(in *rpc.AddSubtitleReq) (*rpc.SubtitleReply, error) {
	if in.AssetId <= 0 {
		return nil, model.ErrInvalidAssetID
	}
	if in.Lang == "" {
		return nil, model.ErrInvalidLang
	}
	if in.Bucket == "" {
		return nil, model.ErrInvalidBucket
	}
	if in.ObjectKey == "" {
		return nil, model.ErrInvalidObjectKey
	}
	s := &model.AssetSubtitle{
		AssetID:   in.AssetId,
		Lang:      in.Lang,
		Bucket:    in.Bucket,
		ObjectKey: in.ObjectKey,
		Ctime:     time.Now().Unix(),
	}
	subID, err := l.svcCtx.Repository.AddSubtitle(l.ctx, s)
	if err != nil {
		l.Errorf("asset/AddSubtitle: asset_id=%d lang=%s err=%v", in.AssetId, in.Lang, err)
		return nil, err
	}
	return &rpc.SubtitleReply{
		SubId:     subID,
		AssetId:   s.AssetID,
		Lang:      s.Lang,
		Bucket:    s.Bucket,
		ObjectKey: s.ObjectKey,
		Ctime:     s.Ctime,
	}, nil
}
