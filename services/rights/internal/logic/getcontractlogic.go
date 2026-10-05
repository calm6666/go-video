package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetContractLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetContractLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetContractLogic {
	return &GetContractLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetContract 查询单个合同；不存在返回 ErrContractNotFound。
func (l *GetContractLogic) GetContract(in *rpc.ContractReq) (*rpc.ContractReply, error) {
	if in.ContractId <= 0 {
		return nil, model.ErrInvalidContractID
	}
	c, err := l.svcCtx.Repository.GetContract(l.ctx, in.ContractId)
	if err != nil {
		l.Errorf("rights/GetContract: contract_id=%d err=%v", in.ContractId, err)
		return nil, err
	}
	if c == nil {
		return nil, model.ErrContractNotFound
	}
	return &rpc.ContractReply{Contract: contractToRPC(c)}, nil
}
