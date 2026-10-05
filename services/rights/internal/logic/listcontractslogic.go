package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListContractsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListContractsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListContractsLogic {
	return &ListContractsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListContracts 分页查询合同。owner_id=0/state=UNSPECIFIED 表示不筛选。
func (l *ListContractsLogic) ListContracts(in *rpc.ListReq) (*rpc.ContractsReply, error) {
	if in.Ps <= 0 || in.Ps > 50 {
		return nil, model.ErrPsTooLarge
	}
	rows, total, err := l.svcCtx.Repository.ListContracts(l.ctx, in.OwnerId, int32(in.State), in.Pn, in.Ps)
	if err != nil {
		l.Errorf("rights/ListContracts: owner=%d state=%v err=%v", in.OwnerId, in.State, err)
		return nil, err
	}
	out := make([]*rpc.Contract, 0, len(rows))
	for _, c := range rows {
		out = append(out, contractToRPC(c))
	}
	return &rpc.ContractsReply{Total: total, Contracts: out}, nil
}
