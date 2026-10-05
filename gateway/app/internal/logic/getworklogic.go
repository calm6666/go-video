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

type GetWorkLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询作品详情
func NewGetWorkLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetWorkLogic {
	return &GetWorkLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询作品详情：聚合 catalog GetWork RPC。
// catalog 以主季 ID 作为作品唯一标识，故路径参数 work_id 直接映射 WorkReq.SeasonId。
func (l *GetWorkLogic) GetWork(req *types.ParamCatalogWorkId) (resp *types.CatalogWorkResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	reply, err := l.svcCtx.Catalog.GetWork(l.ctx, &catalogrpc.WorkReq{
		SeasonId: req.WorkId,
	})
	if err != nil {
		l.Errorf("gateway/app/getWork: work_id=%d err=%v", req.WorkId, err)
		return nil, err
	}
	return &types.CatalogWorkResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogWorkData{Work: toCatalogWork(reply)},
		TTL:     0,
	}, nil
}
