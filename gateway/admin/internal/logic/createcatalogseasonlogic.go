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

type CreateCatalogSeasonLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建季
func NewCreateCatalogSeasonLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCatalogSeasonLogic {
	return &CreateCatalogSeasonLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建季：聚合 catalog CreateSeason RPC。
// req.SeasonId 是所属作品的主季 ID（父作品），季编号唯一性等规则由 catalog 校验。
func (l *CreateCatalogSeasonLogic) CreateCatalogSeason(req *types.ParamCreateSeason) (resp *types.CatalogSeasonResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	if err := adminActorGate(l.ctx, "createCatalogSeason", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Catalog.CreateSeason(l.ctx, &catalogrpc.CreateSeasonReq{
		SeasonId: req.SeasonId,
		SeasonNo: req.SeasonNo,
		Title:    req.Title,
		Cover:    req.Cover,
		Operator: req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/createCatalogSeason: season_id=%d season_no=%d operator=%q err=%v",
			req.SeasonId, req.SeasonNo, req.Operator, err)
		return nil, err
	}
	return &types.CatalogSeasonResponse{
		Code:    0,
		Message: "ok",
		Data: types.CatalogSeasonData{
			Season: types.CatalogSeasonItem{
				SeasonId: reply.GetSeasonId(),
				SeasonNo: reply.GetSeasonNo(),
				Title:    reply.GetTitle(),
				Cover:    reply.GetCover(),
				State:    reply.GetState(),
			},
		},
		TTL: 0,
	}, nil
}
