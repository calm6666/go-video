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

type GetEpisodeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询集详情
func NewGetEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetEpisodeLogic {
	return &GetEpisodeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 查询集详情：聚合 catalog GetEpisode RPC。
func (l *GetEpisodeLogic) GetEpisode(req *types.ParamCatalogEpid) (resp *types.CatalogEpisodeResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	reply, err := l.svcCtx.Catalog.GetEpisode(l.ctx, &catalogrpc.EpisodeReq{
		Epid: req.Epid,
	})
	if err != nil {
		l.Errorf("gateway/app/getEpisode: epid=%d err=%v", req.Epid, err)
		return nil, err
	}
	return &types.CatalogEpisodeResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogEpisodeData{Episode: toCatalogEpisode(reply)},
		TTL:     0,
	}, nil
}
