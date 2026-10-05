// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	assetrpc "go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAssetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询单个媒资元数据
func NewGetAssetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAssetLogic {
	return &GetAssetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询单个媒资：聚合 asset GetAsset RPC。
// 网关只做透传与字段映射，媒资状态机与数据所有权仍属于 asset 服务（AGENTS.md §5）。
func (l *GetAssetLogic) GetAsset(req *types.ParamAssetId) (resp *types.AssetResponse, err error) {
	if l.svcCtx.Asset == nil {
		return nil, errors.New("asset service not configured")
	}
	reply, err := l.svcCtx.Asset.GetAsset(l.ctx, &assetrpc.AssetReq{
		AssetId: req.AssetId,
	})
	if err != nil {
		l.Errorf("gateway/admin/getAsset: asset_id=%d err=%v", req.AssetId, err)
		return nil, err
	}
	// pb getter 对 nil reply 安全，返回零值条目而不是报错。
	asset := types.AssetItem{
		AssetId:   reply.GetAssetId(),
		UploadId:  reply.GetUploadId(),
		Mid:       reply.GetMid(),
		Bucket:    reply.GetBucket(),
		ObjectKey: reply.GetObjectKey(),
		Size:      reply.GetSize(),
		Md5:       reply.GetMd5(),
		Duration:  reply.GetDuration(),
		Width:     reply.GetWidth(),
		Height:    reply.GetHeight(),
		Codec:     reply.GetCodec(),
		State:     int32(reply.GetState()),
		Ctime:     reply.GetCtime(),
		Mtime:     reply.GetMtime(),
	}
	return &types.AssetResponse{
		Code:    0,
		Message: "ok",
		Data:    types.AssetData{Asset: asset},
		TTL:     0,
	}, nil
}
