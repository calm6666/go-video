// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	catalogrpc "go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCatalogWorksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询作品
func NewListCatalogWorksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCatalogWorksLogic {
	return &ListCatalogWorksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询作品：聚合 catalog ListWorks RPC。
// typeid 为 0 表示不过滤类型，state 为 -1 表示不过滤状态（由 .api 默认值保证）。
// 分页上限（ps 最大 50）由 catalog 服务裁剪，网关不改写调用方意图。
func (l *ListCatalogWorksLogic) ListCatalogWorks(req *types.ParamListWorks) (resp *types.CatalogWorksResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	reply, err := l.svcCtx.Catalog.ListWorks(l.ctx, &catalogrpc.ListReq{
		Typeid: req.Typeid,
		State:  req.State,
		Pn:     req.Pn,
		Ps:     req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listCatalogWorks: typeid=%d state=%d pn=%d ps=%d err=%v",
			req.Typeid, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	works := make([]types.CatalogWorkItem, 0, len(reply.GetWorks()))
	for _, item := range reply.GetWorks() {
		works = append(works, toAdminCatalogWork(item))
	}
	return &types.CatalogWorksResponse{
		Code:    0,
		Message: "ok",
		Data: types.CatalogWorksData{
			// catalog proto 的 total 是 int32，HTTP 载荷按 int64 暴露。
			Total: int64(reply.GetTotal()),
			Works: works,
		},
		TTL: 0,
	}, nil
}
