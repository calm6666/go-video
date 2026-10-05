// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rightsrpc "go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateRightsWindowLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建播放窗口
func NewCreateRightsWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateRightsWindowLogic {
	return &CreateRightsWindowLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建播放窗口：聚合 rights CreateWindow RPC。
// 合同是否生效、时间区间合法性由 rights 服务校验（它拥有 rights_window 主数据），
// 网关只透传 content_type（1 PGC、2 UGC）等参数与错误。
// 契约缺口（见交付报告）：types.ParamCreateWindow 无 ip 字段，proto 的 ip 留空。
func (l *CreateRightsWindowLogic) CreateRightsWindow(req *types.ParamCreateWindow) (resp *types.RightsWindowResponse, err error) {
	if l.svcCtx.Rights == nil {
		return nil, errors.New("rights service not configured")
	}
	if err := adminActorGate(l.ctx, "createRightsWindow", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Rights.CreateWindow(l.ctx, &rightsrpc.CreateWindowReq{
		ContractId:  req.ContractId,
		ContentId:   req.ContentId,
		ContentType: rightsrpc.ContentType(req.ContentType),
		Region:      req.Region,
		StartTime:   req.StartTime,
		EndTime:     req.EndTime,
		Operator:    req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/createRightsWindow: contract_id=%d content_id=%d content_type=%d region=%q operator=%q err=%v",
			req.ContractId, req.ContentId, req.ContentType, req.Region, req.Operator, err)
		return nil, err
	}
	return &types.RightsWindowResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RightsWindowData{Window: toAdminRightsWindow(reply.GetWindow())},
		TTL:     0,
	}, nil
}

// toAdminRightsWindow 把 rights Window 实体映射为管理后台载荷。
// 供本域 create/list/expire logic 复用；state 由 protobuf 枚举降为 int32 对外暴露。
func toAdminRightsWindow(p *rightsrpc.Window) types.RightsWindowItem {
	if p == nil {
		return types.RightsWindowItem{}
	}
	return types.RightsWindowItem{
		WindowId:    p.GetWindowId(),
		ContractId:  p.GetContractId(),
		ContentId:   p.GetContentId(),
		ContentType: int32(p.GetContentType()),
		Region:      p.GetRegion(),
		StartTime:   p.GetStartTime(),
		EndTime:     p.GetEndTime(),
		State:       int32(p.GetState()),
		Ctime:       p.GetCtime(),
		Mtime:       p.GetMtime(),
	}
}
