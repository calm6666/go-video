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

type CompleteUploadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 完成上传：校验分片清单并触发 OSS 完成分片上传
func NewCompleteUploadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CompleteUploadLogic {
	return &CompleteUploadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 完成上传：聚合 upload CompleteUpload RPC。
// 依据 AGENTS.md §8，上传完成只代表媒资就绪，稿件状态由 upload 服务发布
// media.task.v1 事件后交给 asset/transcode 推进，网关不在此写入发布态。
func (l *CompleteUploadLogic) CompleteUpload(req *types.ParamCompleteUpload) (resp *types.UploadCompleteResponse, err error) {
	if l.svcCtx.Upload == nil {
		return nil, errors.New("upload service not configured")
	}
	reply, err := l.svcCtx.Upload.CompleteUpload(l.ctx, &uploadrpc.CompleteUploadReq{
		UploadId: req.UploadId,
		Parts:    toUploadChunkParts(req.Parts),
		Md5:      req.Md5,
		Ip:       req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/completeUpload: upload_id=%s parts=%d err=%v", req.UploadId, len(req.Parts), err)
		return nil, err
	}
	return &types.UploadCompleteResponse{
		Code:    0,
		Message: "ok",
		Data:    toUploadCompleteData(reply),
		TTL:     0,
	}, nil
}
