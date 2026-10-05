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

type PublishCatalogEpisodeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 上架集（状态流转到 PUBLISHED，需 rights 校验）
func NewPublishCatalogEpisodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PublishCatalogEpisodeLogic {
	return &PublishCatalogEpisodeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 上架集：聚合 catalog PublishEpisode RPC。
//
// 版权校验归属：按 AGENTS.md §5/§8，上架必须通过合法状态机并由 rights 窗口约束，
// 因此校验落在拥有稿件状态的 catalog 服务侧（catalog 内部调用 rights CheckPlayable）。
// 网关不复制该领域规则：若在网关先调 rights.CheckPlayable 再调 catalog，
// 会引入 TOCTOU 竞态并绕过 catalog 的状态机。
//
// 契约缺口（见交付报告）：catalog.PublishEpisode 入参仅 EpisodeReq{epid}，
// 既无 region 也无 operator，catalog 侧无法据此完成带地区维度的窗口校验并记录操作人。
func (l *PublishCatalogEpisodeLogic) PublishCatalogEpisode(req *types.ParamCatalogEpid) (resp *types.CatalogEpisodeResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	if err := adminSessionGate(l.ctx, "publishCatalogEpisode"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Catalog.PublishEpisode(l.ctx, &catalogrpc.EpisodeReq{
		Epid: req.Epid,
	})
	if err != nil {
		l.Errorf("gateway/admin/publishCatalogEpisode: epid=%d err=%v", req.Epid, err)
		return nil, err
	}
	return &types.CatalogEpisodeResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogEpisodeData{Episode: toAdminCatalogEpisode(reply)},
		TTL:     0,
	}, nil
}
