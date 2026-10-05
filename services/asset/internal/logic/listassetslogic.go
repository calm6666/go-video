package logic

import (
	"context"

	"go-video/services/asset/internal/svc"
	"go-video/services/asset/model"
	"go-video/services/asset/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListAssetsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListAssetsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListAssetsLogic {
	return &ListAssetsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListAssets 分页查询媒资列表（可按 mid 或 state 过滤）。
func (l *ListAssetsLogic) ListAssets(in *rpc.ListReq) (*rpc.AssetsReply, error) {
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	metas, total, err := l.svcCtx.Repository.ListAssets(l.ctx, in.Mid, int32(in.State), in.Pn, in.Ps)
	if err != nil {
		l.Errorf("asset/ListAssets: mid=%d state=%v err=%v", in.Mid, in.State, err)
		return nil, err
	}
	items := make([]*rpc.AssetReply, 0, len(metas))
	for _, m := range metas {
		items = append(items, assetMetaToReply(m))
	}
	return &rpc.AssetsReply{Total: total, Items: items}, nil
}
