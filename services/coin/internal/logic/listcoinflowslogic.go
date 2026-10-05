package logic

import (
	"context"
	"fmt"

	"go-video/services/coin/internal/svc"
	"go-video/services/coin/model"
	"go-video/services/coin/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCoinFlowsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListCoinFlowsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCoinFlowsLogic {
	return &ListCoinFlowsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 硬币流水台账分页（用户面 mid>0，运营面 mid=0）。
//
// 无界扫描拒绝：mid=0 且既没给时间窗也没给 biz_no 时，查询会退化成对 append-only
// 大表的全表扫描 + COUNT(*)，一次运营误查就能把主库拖住。这类请求直接报错，
// 不返回「空台账」——空列表在调用方眼里等于「这个人没有过任何资金变动」，
// 把故障伪装成一个业务事实是对账里最坏的错误。
// 同理：List/Count 任一失败都必须上抛，绝不折叠成空结果冒充成功。
func (l *ListCoinFlowsLogic) ListCoinFlows(in *rpc.ListCoinFlowsReq) (*rpc.ListCoinFlowsReply, error) {
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	if in.FlowType != rpc.CoinFlowType_COIN_FLOW_TYPE_UNSPECIFIED &&
		!validFlowType(int32(in.FlowType)) {
		return nil, model.ErrInvalidFlowType
	}
	if in.FromTs < 0 || in.ToTs < 0 {
		return nil, fmt.Errorf("%w: from_ts=%d to_ts=%d", model.ErrInvalidTimeRange, in.FromTs, in.ToTs)
	}
	if in.FromTs > 0 && in.ToTs > 0 && in.FromTs > in.ToTs {
		return nil, fmt.Errorf("%w: from_ts=%d > to_ts=%d", model.ErrInvalidTimeRange, in.FromTs, in.ToTs)
	}
	size, err := l.svcCtx.PageSize(in.Size)
	if err != nil {
		return nil, err
	}
	page := in.Page
	if page <= 0 {
		page = 1
	}
	offset := l.svcCtx.Offset(page, size)

	filter := model.FlowFilter{
		Mid:      in.Mid,
		FlowType: int32(in.FlowType),
		BizNo:    in.BizNo,
		FromTs:   in.FromTs,
		ToTs:     in.ToTs,
	}
	rows, err := l.svcCtx.Flows.List(l.ctx, filter, offset, size)
	if err != nil {
		return nil, err
	}
	total, err := l.svcCtx.Flows.Count(l.ctx, filter)
	if err != nil {
		return nil, err
	}
	return &rpc.ListCoinFlowsReply{
		Flows: flowInfos(rows),
		Total: total,
		Page:  page,
		Size:  int64(size),
	}, nil
}
