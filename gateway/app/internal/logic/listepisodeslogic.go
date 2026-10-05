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

type ListEpisodesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询某季的集列表
func NewListEpisodesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListEpisodesLogic {
	return &ListEpisodesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询某季的集列表：聚合 catalog ListEpisodes RPC。
// 此处 SeasonReq.SeasonId 语义为具体季 ID（见 catalog.proto SeasonReq 注释）。
func (l *ListEpisodesLogic) ListEpisodes(req *types.ParamCatalogSeasonId) (resp *types.CatalogEpisodesResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	reply, err := l.svcCtx.Catalog.ListEpisodes(l.ctx, &catalogrpc.SeasonReq{
		SeasonId: req.SeasonId,
	})
	if err != nil {
		l.Errorf("gateway/app/listEpisodes: season_id=%d err=%v", req.SeasonId, err)
		return nil, err
	}
	return &types.CatalogEpisodesResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogEpisodesData{Episodes: toCatalogEpisodes(reply.GetEpisodes())},
		TTL:     0,
	}, nil
}
