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

type ListAssetsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询媒资列表（按 mid/state 过滤）
func NewListAssetsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAssetsLogic {
	return &ListAssetsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询媒资：聚合 asset ListAssets RPC。
// state 取值与 asset.v1.AssetState 枚举一致（0 表示不按状态过滤）。
func (l *ListAssetsLogic) ListAssets(req *types.ParamListAssets) (resp *types.AssetsResponse, err error) {
	if l.svcCtx.Asset == nil {
		return nil, errors.New("asset service not configured")
	}
	reply, err := l.svcCtx.Asset.ListAssets(l.ctx, &assetrpc.ListReq{
		Mid:   req.Mid,
		State: assetrpc.AssetState(req.State),
		Pn:    req.Pn,
		Ps:    req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listAssets: mid=%d state=%d pn=%d ps=%d err=%v", req.Mid, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	items := make([]types.AssetItem, 0, len(reply.GetItems()))
	for _, a := range reply.GetItems() {
		items = append(items, types.AssetItem{
			AssetId:   a.GetAssetId(),
			UploadId:  a.GetUploadId(),
			Mid:       a.GetMid(),
			Bucket:    a.GetBucket(),
			ObjectKey: a.GetObjectKey(),
			Size:      a.GetSize(),
			Md5:       a.GetMd5(),
			Duration:  a.GetDuration(),
			Width:     a.GetWidth(),
			Height:    a.GetHeight(),
			Codec:     a.GetCodec(),
			State:     int32(a.GetState()),
			Ctime:     a.GetCtime(),
			Mtime:     a.GetMtime(),
		})
	}
	return &types.AssetsResponse{
		Code:    0,
		Message: "ok",
		Data: types.AssetsData{
			Total: int64(reply.GetTotal()),
			Items: items,
		},
		TTL: 0,
	}, nil
}
