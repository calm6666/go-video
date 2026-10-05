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

type ListZonesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分区树（扁平列表）
func NewListZonesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListZonesLogic {
	return &ListZonesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分区树（扁平列表）：聚合 catalog ListZones RPC。
// catalog 返回扁平分区列表（ZoneReply.parent 指向父分区），组装成树由客户端负责。
func (l *ListZonesLogic) ListZones() (resp *types.CatalogZonesResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	reply, err := l.svcCtx.Catalog.ListZones(l.ctx, &catalogrpc.EmptyReq{})
	if err != nil {
		l.Errorf("gateway/app/listZones: err=%v", err)
		return nil, err
	}
	return &types.CatalogZonesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogZonesData{Zones: toCatalogZones(reply.GetZones())},
		TTL:     0,
	}, nil
}
