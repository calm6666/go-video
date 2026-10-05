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

type AbortUploadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 取消上传（删除 OSS 分片）
func NewAbortUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AbortUploadLogic {
	return &AbortUploadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 取消上传：聚合 upload AbortUpload RPC，成功时返回统一空数据信封。
func (l *AbortUploadLogic) AbortUpload(req *types.ParamAbortUpload) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Upload == nil {
		return nil, errors.New("upload service not configured")
	}
	if _, err := l.svcCtx.Upload.AbortUpload(l.ctx, &uploadrpc.AbortUploadReq{
		UploadId: req.UploadId,
		Ip:       req.IP,
	}); err != nil {
		l.Errorf("gateway/app/abortUpload: upload_id=%s err=%v", req.UploadId, err)
		return nil, err
	}
	return emptyResponse(), nil
}
