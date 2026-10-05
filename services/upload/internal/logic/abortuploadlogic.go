package logic

import (
	"context"

	"go-video/services/upload/internal/svc"
	"go-video/services/upload/model"
	"go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AbortUploadLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAbortUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AbortUploadLogic {
	return &AbortUploadLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 取消上传（删除 OSS 分片）
func (l *AbortUploadLogic) AbortUpload(in *rpc.AbortUploadReq) (*rpc.EmptyReply, error) {
	if in.UploadId == "" {
		return nil, model.ErrInvalidUploadID
	}
	if err := l.svcCtx.Repository.AbortUpload(l.ctx, in.UploadId); err != nil {
		l.Errorf("upload/AbortUpload: upload_id=%s err=%v", in.UploadId, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
