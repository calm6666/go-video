package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateContractLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateContractLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateContractLogic {
	return &CreateContractLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateContract 运营创建合同。
// 校验：版权方 ID、标题非空、date 范围合法；新建后默认状态为 active。
func (l *CreateContractLogic) CreateContract(in *rpc.CreateContractReq) (*rpc.ContractReply, error) {
	if in.OwnerId <= 0 {
		return nil, model.ErrInvalidOwnerID
	}
	if in.Title == "" {
		return nil, model.ErrInvalidTitle
	}
	if in.EndDate <= in.StartDate {
		return nil, model.ErrInvalidDateRange
	}
	now := model.NowUnix()
	c := &model.RightsContract{
		OwnerID:    in.OwnerId,
		Title:      in.Title,
		SignDate:   in.SignDate,
		StartDate:  in.StartDate,
		EndDate:    in.EndDate,
		RegionsCSV: model.RegionsToCSV(in.Regions),
		State:      model.ContractStateActive,
		Ctime:      now,
		Mtime:      now,
	}
	contractID, err := l.svcCtx.Repository.CreateContract(l.ctx, c)
	if err != nil {
		l.Errorf("rights/CreateContract: owner=%d title=%s err=%v", in.OwnerId, in.Title, err)
		return nil, err
	}
	c.ContractID = contractID
	return &rpc.ContractReply{Contract: contractToRPC(c)}, nil
}
