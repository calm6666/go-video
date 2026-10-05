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

type GetUploadStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询上传状态和已完成分片列表
func NewGetUploadStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUploadStatusLogic {
	return &GetUploadStatusLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询上传状态：聚合 upload GetUploadStatus RPC。
func (l *GetUploadStatusLogic) GetUploadStatus(req *types.ParamUploadStatus) (resp *types.UploadStatusResponse, err error) {
	if l.svcCtx.Upload == nil {
		return nil, errors.New("upload service not configured")
	}
	reply, err := l.svcCtx.Upload.GetUploadStatus(l.ctx, &uploadrpc.UploadStatusReq{
		UploadId: req.UploadId,
		Ip:       req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/getUploadStatus: upload_id=%s err=%v", req.UploadId, err)
		return nil, err
	}
	return &types.UploadStatusResponse{
		Code:    0,
		Message: "ok",
		Data:    toUploadStatusData(reply),
		TTL:     0,
	}, nil
}
