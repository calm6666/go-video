package logic

import (
	"context"

	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	"go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetUploadStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetUploadStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUploadStatusLogic {
	return &GetUploadStatusLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 查询上传状态和已完成分片列表
func (l *GetUploadStatusLogic) GetUploadStatus(in *rpc.UploadStatusReq) (*rpc.UploadStatusReply, error) {
	if in.UploadId == "" {
		return nil, model.ErrInvalidUploadID
	}
	session, chunks, err := l.svcCtx.Repository.GetUploadStatus(l.ctx, in.UploadId)
	if err != nil {
		l.Errorf("upload/GetUploadStatus: upload_id=%s err=%v", in.UploadId, err)
		return nil, err
	}
	var uploadedSize int64
	completed := int32(0)
	rpcChunks := make([]*rpc.Chunk, 0, len(chunks))
	for _, c := range chunks {
		if c.State >= model.ChunkStateUploaded {
			completed++
			uploadedSize += c.Size
		}
		rpcChunks = append(rpcChunks, &rpc.Chunk{
			ChunkNo: c.ChunkNo,
			Size:    c.Size,
			Etag:    c.Etag,
			State:   rpc.ChunkState(c.State),
			Ctime:   c.Ctime,
		})
	}
	return &rpc.UploadStatusReply{
		UploadId:        session.UploadId,
		State:           rpc.UploadState(session.State),
		Size:            session.Size,
		UploadedSize:    uploadedSize,
		TotalChunks:     session.TotalChunks,
		CompletedChunks: completed,
		Chunks:          rpcChunks,
		AssetId:         session.AssetId,
	}, nil
}
