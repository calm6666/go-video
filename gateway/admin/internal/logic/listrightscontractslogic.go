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

type ListRightsContractsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询版权合同
func NewListRightsContractsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRightsContractsLogic {
	return &ListRightsContractsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 分页查询版权合同：聚合 rights ListContracts RPC。
// owner_id 为 0 表示不筛选版权方；state 直接映射到 ContractState 枚举，
// 0（CONTRACT_STATE_UNSPECIFIED）表示不筛选状态。
func (l *ListRightsContractsLogic) ListRightsContracts(req *types.ParamListContracts) (resp *types.RightsContractsResponse, err error) {
	if l.svcCtx.Rights == nil {
		return nil, errors.New("rights service not configured")
	}
	reply, err := l.svcCtx.Rights.ListContracts(l.ctx, &rightsrpc.ListReq{
		OwnerId: req.OwnerId,
		State:   rightsrpc.ContractState(req.State),
		Pn:      req.Pn,
		Ps:      req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRightsContracts: owner_id=%d state=%d pn=%d ps=%d err=%v",
			req.OwnerId, req.State, req.Pn, req.Ps, err)
		return nil, err
	}
	contracts := make([]types.RightsContractItem, 0, len(reply.GetContracts()))
	for _, item := range reply.GetContracts() {
		contracts = append(contracts, toAdminRightsContract(item))
	}
	return &types.RightsContractsResponse{
		Code:    0,
		Message: "ok",
		Data: types.RightsContractsData{
			// rights proto 的 total 是 int32，HTTP 载荷按 int64 暴露。
			Total:     int64(reply.GetTotal()),
			Contracts: contracts,
		},
		TTL: 0,
	}, nil
}
