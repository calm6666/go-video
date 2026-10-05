package logic

import (
	"context"

	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	"go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type InitUploadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewInitUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InitUploadLogic {
	return &InitUploadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位
func (l *InitUploadLogic) InitUpload(in *rpc.InitUploadReq) (*rpc.InitUploadReply, error) {
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Filename == "" {
		return nil, model.ErrInvalidFilename
	}
	if in.Size <= 0 {
		return nil, model.ErrInvalidSize
	}
	if in.TotalChunks <= 0 {
		return nil, model.ErrInvalidTotalChunks
	}
	session, err := l.svcCtx.Repository.InitUpload(l.ctx, &model.UploadSession{
		Mid:         in.Mid,
		Filename:    in.Filename,
		Size:        in.Size,
		Typeid:      in.Typeid,
		Md5:         in.Md5,
		ChunkSize:   in.ChunkSize,
		TotalChunks: in.TotalChunks,
	})
	if err != nil {
		l.Errorf("upload/InitUpload: mid=%d filename=%s err=%v", in.Mid, in.Filename, err)
		return nil, err
	}
	return &rpc.InitUploadReply{
		UploadId:       session.UploadId,
		Bucket:         session.Bucket,
		ObjectKey:      session.ObjectKey,
		UploadProtocol: "multipart",
		ChunkSize:      session.ChunkSize,
		TotalChunks:    session.TotalChunks,
		// 秒传本期不实现，TODO: 接入 asset 指纹查询后回填。
		Instant: false,
	}, nil
}
