// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	uploadrpc "go-video/services/upload/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type InitUploadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 初始化上传会话，返回 upload_id 和 OSS bucket/object_key 占位
func NewInitUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *InitUploadLogic {
	return &InitUploadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *InitUploadLogic) InitUpload(req *types.ParamInitUpload) (resp *types.UploadInitResponse, err error) {
	if l.svcCtx.Upload == nil {
		return nil, errors.New("upload service not configured")
	}
	reply, err := l.svcCtx.Upload.InitUpload(l.ctx, &uploadrpc.InitUploadReq{
		Mid:         req.Mid,
		Filename:    req.Filename,
		Size:        req.Size,
		Typeid:      req.Typeid,
		Md5:         req.Md5,
		ChunkSize:   req.ChunkSize,
		TotalChunks: req.TotalChunks,
		Ip:          req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/initUpload: mid=%d filename=%s err=%v", req.Mid, req.Filename, err)
		return nil, err
	}
	return &types.UploadInitResponse{
		Code:    0,
		Message: "ok",
		Data:    toUploadInitData(reply),
		TTL:     0,
	}, nil
}
