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

type CreateCatalogEpisodeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建集
func NewCreateCatalogEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCatalogEpisodeLogic {
	return &CreateCatalogEpisodeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建集：聚合 catalog CreateEpisode RPC。
// req.SeasonId 是具体季 ID；asset_id 存在性与就绪状态分别由 catalog/asset 服务校验，
// 网关不判断媒资是否可播放（AGENTS.md §5 数据所有权）。
func (l *CreateCatalogEpisodeLogic) CreateCatalogEpisode(req *types.ParamCreateEpisode) (resp *types.CatalogEpisodeResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	if err := adminActorGate(l.ctx, "createCatalogEpisode", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Catalog.CreateEpisode(l.ctx, &catalogrpc.CreateEpisodeReq{
		SeasonId: req.SeasonId,
		EpNo:     req.EpNo,
		Title:    req.Title,
		AssetId:  req.AssetId,
		Duration: req.Duration,
		Operator: req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/createCatalogEpisode: season_id=%d ep_no=%d asset_id=%d operator=%q err=%v",
			req.SeasonId, req.EpNo, req.AssetId, req.Operator, err)
		return nil, err
	}
	return &types.CatalogEpisodeResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogEpisodeData{Episode: toAdminCatalogEpisode(reply)},
		TTL:     0,
	}, nil
}

// toAdminCatalogEpisode 把 catalog EpisodeReply 映射为管理后台载荷。
// 供本域 create/publish/offline logic 复用。
func toAdminCatalogEpisode(p *catalogrpc.EpisodeReply) types.CatalogEpisodeItem {
	if p == nil {
		return types.CatalogEpisodeItem{}
	}
	return types.CatalogEpisodeItem{
		Epid:     p.GetEpid(),
		SeasonId: p.GetSeasonId(),
		EpNo:     p.GetEpNo(),
		Title:    p.GetTitle(),
		AssetId:  p.GetAssetId(),
		Duration: p.GetDuration(),
		State:    p.GetState(),
	}
}
