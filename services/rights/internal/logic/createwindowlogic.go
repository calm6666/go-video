package logic

import (
	"context"

	"go-video/services/rights/internal/svc"
	"go-video/services/rights/model"
	"go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateWindowLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateWindowLogic {
	return &CreateWindowLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateWindow 为内容创建时间窗口（关联合同）。
// 校验：合同存在且 active、content_id > 0、region 非空、时间范围合法。
func (l *CreateWindowLogic) CreateWindow(in *rpc.CreateWindowReq) (*rpc.WindowReply, error) {
	if in.ContractId <= 0 {
		return nil, model.ErrInvalidContractID
	}
	if in.ContentId <= 0 {
		return nil, model.ErrInvalidContentID
	}
	if in.Region == "" {
		return nil, model.ErrInvalidRegion
	}
	if in.EndTime <= in.StartTime {
		return nil, model.ErrInvalidTimeRange
	}
	// 校验关联合同生效中
	if _, err := l.svcCtx.Repository.GetContractActive(l.ctx, in.ContractId); err != nil {
		l.Errorf("rights/CreateWindow: contract_id=%d active-check err=%v", in.ContractId, err)
		return nil, err
	}
	now := model.NowUnix()
	w := &model.RightsWindow{
		ContractID:  in.ContractId,
		ContentID:   in.ContentId,
		ContentType: int32(in.ContentType),
		Region:      in.Region,
		StartTime:   in.StartTime,
		EndTime:     in.EndTime,
		State:       model.WindowStateActive,
		Ctime:       now,
		Mtime:       now,
	}
	windowID, err := l.svcCtx.Repository.CreateWindow(l.ctx, w)
	if err != nil {
		l.Errorf("rights/CreateWindow: contract_id=%d content_id=%d region=%s err=%v",
			in.ContractId, in.ContentId, in.Region, err)
		return nil, err
	}
	w.WindowID = windowID
	return &rpc.WindowReply{Window: windowToRPC(w)}, nil
}
