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

type CreateRightsContractLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营创建版权合同
func NewCreateRightsContractLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateRightsContractLogic {
	return &CreateRightsContractLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 运营创建版权合同：聚合 rights CreateContract RPC。
// 日期区间、版权方存在性等规则由 rights 服务校验（AGENTS.md §5），网关不重复实现。
// Operator 透传用于 rights 侧审计。
// 契约缺口（见交付报告）：proto 的 ip 字段在 types.ParamCreateContract 中不存在，
// 网关无法携带调用方 IP，此处留空由 rights 侧按缺省处理。
func (l *CreateRightsContractLogic) CreateRightsContract(req *types.ParamCreateContract) (resp *types.RightsContractResponse, err error) {
	if l.svcCtx.Rights == nil {
		return nil, errors.New("rights service not configured")
	}
	if err := adminActorGate(l.ctx, "createRightsContract", "operator", req.Operator); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Rights.CreateContract(l.ctx, &rightsrpc.CreateContractReq{
		OwnerId:   req.OwnerId,
		Title:     req.Title,
		SignDate:  req.SignDate,
		StartDate: req.StartDate,
		EndDate:   req.EndDate,
		Regions:   req.Regions,
		Operator:  req.Operator,
	})
	if err != nil {
		l.Errorf("gateway/admin/createRightsContract: owner_id=%d title=%q operator=%q err=%v",
			req.OwnerId, req.Title, req.Operator, err)
		return nil, err
	}
	return &types.RightsContractResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RightsContractData{Contract: toAdminRightsContract(reply.GetContract())},
		TTL:     0,
	}, nil
}

// toAdminRightsContract 把 rights Contract 实体映射为管理后台载荷。
// 供本域 create/list logic 复用；state 由 protobuf 枚举降为 int32 对外暴露。
func toAdminRightsContract(p *rightsrpc.Contract) types.RightsContractItem {
	if p == nil {
		return types.RightsContractItem{}
	}
	return types.RightsContractItem{
		ContractId: p.GetContractId(),
		OwnerId:    p.GetOwnerId(),
		Title:      p.GetTitle(),
		SignDate:   p.GetSignDate(),
		StartDate:  p.GetStartDate(),
		EndDate:    p.GetEndDate(),
		Regions:    p.GetRegions(),
		State:      int32(p.GetState()),
		Ctime:      p.GetCtime(),
		Mtime:      p.GetMtime(),
	}
}
