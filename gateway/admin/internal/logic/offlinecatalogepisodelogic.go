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

type OfflineCatalogEpisodeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 下架集
func NewOfflineCatalogEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OfflineCatalogEpisodeLogic {
	return &OfflineCatalogEpisodeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 下架集：聚合 catalog OfflineEpisode RPC。
// 下架是收敛操作，不需要 rights 窗口校验；CDN/搜索/推荐投影的下架处理与审计证据
// 由 catalog 服务及其事件投递负责（AGENTS.md §8），网关只透传参数与错误。
func (l *OfflineCatalogEpisodeLogic) OfflineCatalogEpisode(req *types.ParamCatalogEpid) (resp *types.CatalogEpisodeResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	if err := adminSessionGate(l.ctx, "offlineCatalogEpisode"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Catalog.OfflineEpisode(l.ctx, &catalogrpc.EpisodeReq{
		Epid: req.Epid,
	})
	if err != nil {
		l.Errorf("gateway/admin/offlineCatalogEpisode: epid=%d err=%v", req.Epid, err)
		return nil, err
	}
	return &types.CatalogEpisodeResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogEpisodeData{Episode: toAdminCatalogEpisode(reply)},
		TTL:     0,
	}, nil
}
