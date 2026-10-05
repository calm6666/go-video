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

type GetUploadUrlLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 为某分片获取 OSS 预签名 PUT URL（短期）
func NewGetUploadUrlLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetUploadUrlLogic {
	return &GetUploadUrlLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 获取分片预签名 URL：聚合 upload GetUploadUrl RPC。
// 依据 AGENTS.md §6，只向客户端下发短期预签名 PUT URL，不下发 OSS 长期密钥。
func (l *GetUploadUrlLogic) GetUploadUrl(req *types.ParamGetUploadUrl) (resp *types.UploadUrlResponse, err error) {
	if l.svcCtx.Upload == nil {
		return nil, errors.New("upload service not configured")
	}
	reply, err := l.svcCtx.Upload.GetUploadUrl(l.ctx, &uploadrpc.GetUrlReq{
		UploadId:  req.UploadId,
		ChunkNo:   req.ChunkNo,
		ChunkSize: req.ChunkSize,
		Ip:        req.IP,
	})
	if err != nil {
		l.Errorf("gateway/app/getUploadUrl: upload_id=%s chunk_no=%d err=%v", req.UploadId, req.ChunkNo, err)
		return nil, err
	}
	return &types.UploadUrlResponse{
		Code:    0,
		Message: "ok",
		Data:    toUploadUrlData(reply),
		TTL:     0,
	}, nil
}
