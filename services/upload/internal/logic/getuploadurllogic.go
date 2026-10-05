package logic

import (
	"context"

	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	"go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUploadUrlLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUploadUrlLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUploadUrlLogic {
	return &GetUploadUrlLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 为某分片获取 OSS 预签名 PUT URL（短期，默认 15 分钟有效）
func (l *GetUploadUrlLogic) GetUploadUrl(in *rpc.GetUrlReq) (*rpc.GetUrlReply, error) {
	if in.UploadId == "" {
		return nil, model.ErrInvalidUploadID
	}
	if in.ChunkNo <= 0 {
		return nil, model.ErrInvalidChunkNo
	}
	url, expiration, err := l.svcCtx.Repository.GetUploadUrl(l.ctx, in.UploadId, in.ChunkNo, in.ChunkSize)
	if err != nil {
		l.Errorf("upload/GetUploadUrl: upload_id=%s chunk_no=%d err=%v", in.UploadId, in.ChunkNo, err)
		return nil, err
	}
	return &rpc.GetUrlReply{
		UploadId:   in.UploadId,
		ChunkNo:    in.ChunkNo,
		Url:        url,
		Method:     "PUT",
		Expiration: expiration,
	}, nil
}
