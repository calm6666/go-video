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

type CreateCatalogWorkLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建作品
func NewCreateCatalogWorkLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateCatalogWorkLogic {
	return &CreateCatalogWorkLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建作品：聚合 catalog CreateWork RPC。
// 领域校验（标题/类型/状态机）由 catalog 服务负责，网关只透传参数与错误；
// Operator 作为运营操作人透传，用于 catalog 侧审计。
func (l *CreateCatalogWorkLogic) CreateCatalogWork(req *types.ParamCreateWork) (resp *types.CatalogWorkResponse, err error) {
	if l.svcCtx.Catalog == nil {
		return nil, errors.New("catalog service not configured")
	}
	if err := adminActorGate(l.ctx, "createCatalogWork", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Catalog.CreateWork(l.ctx, &catalogrpc.CreateWorkReq{
		Title:    req.Title,
		Cover:    req.Cover,
		Typeid:   req.Typeid,
		Intro:    req.Intro,
		Operator: req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/createCatalogWork: title=%q typeid=%d operator=%q err=%v",
			req.Title, req.Typeid, req.Operator, err)
		return nil, err
	}
	return &types.CatalogWorkResponse{
		Code:    0,
		Message: "ok",
		Data:    types.CatalogWorkData{Work: toAdminCatalogWork(reply)},
		TTL:     0,
	}, nil
}

// toAdminCatalogWork 把 catalog WorkReply 映射为管理后台载荷。
// season_id 既是作品 ID 也是其主季 ID（见 catalog.proto WorkReply）。
// 供本域 create/list logic 复用。
func toAdminCatalogWork(p *catalogrpc.WorkReply) types.CatalogWorkItem {
	if p == nil {
		return types.CatalogWorkItem{}
	}
	return types.CatalogWorkItem{
		SeasonId: p.GetSeasonId(),
		Title:    p.GetTitle(),
		Cover:    p.GetCover(),
		Typeid:   p.GetTypeid(),
		Intro:    p.GetIntro(),
		State:    p.GetState(),
	}
}
