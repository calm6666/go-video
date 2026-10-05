package logic

import (
	"context"

	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	"go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CompleteUploadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCompleteUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CompleteUploadLogic {
	return &CompleteUploadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 客户端上传完全部分片后调用，服务端校验分片清单并触发 OSS 完成分片上传；
func (l *CompleteUploadLogic) CompleteUpload(in *rpc.CompleteUploadReq) (*rpc.CompleteUploadReply, error) {
	if in.UploadId == "" {
		return nil, model.ErrInvalidUploadID
	}
	if len(in.Parts) == 0 {
		return nil, model.ErrChunkMismatch
	}
	parts := make([]model.ChunkPart, 0, len(in.Parts))
	for _, p := range in.Parts {
		parts = append(parts, model.ChunkPart{ChunkNo: p.ChunkNo, Etag: p.Etag})
	}
	session, err := l.svcCtx.Repository.CompleteUpload(l.ctx, in.UploadId, parts, in.Md5)
	if err != nil {
		l.Errorf("upload/CompleteUpload: upload_id=%s err=%v", in.UploadId, err)
		return nil, err
	}
	return &rpc.CompleteUploadReply{
		UploadId:  session.UploadId,
		AssetId:   session.AssetId,
		ObjectKey: session.ObjectKey,
		Size:      session.Size,
		Md5:       session.Md5,
		State:     rpc.UploadState(session.State),
	}, nil
}
