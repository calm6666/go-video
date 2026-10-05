// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	catalogrpc "go-video/services/catalog/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListWorksLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询作品
func NewListWorksLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListWorksLogic {
	return &ListWorksLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询作品：聚合 catalog ListWorks RPC。
// typeid 为 0 表示不过滤类型，state 为 -1 表示不过滤状态（由 .api 默认值保证）。
func (l *ListWorksLogic) ListWorks(req *types.ParamListWorks) (resp *types.CatalogWorksResponse, err error) {
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
		l.Errorf("gateway/app/listWorks: typeid=%d state=%d pn=%d ps=%d err=%v", req.Typeid, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.CatalogWorksResponse{
		Code:    0,
		Message: "ok",
		Data: types.CatalogWorksData{
			Total: reply.GetTotal(),
			Works: toCatalogWorks(reply.GetWorks()),
		},
		TTL: 0,
	}, nil
}
