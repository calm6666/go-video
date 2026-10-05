package logic

import (
	"context"
	"strings"

	"go-video/services/payment/internal/svc"
	"go-video/services/payment/model"
	"go-video/services/payment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListFlowsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListFlowsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListFlowsLogic {
	return &ListFlowsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListFlows 资金流水分页（append-only 台账的唯一读出口）。
//
// 判定口径：
//   - mid=0 是运营面跨用户查询，必须给完整时间窗且不超 Payment.MaxListWindowSeconds；
//     给了 biz_no 也不能免掉时间窗——时间窗是「有界」的判定依据，biz_no 只是附加过滤；
//   - biz_type 不在 1..4 内直接报错，不把「查错类型」伪装成「没有资金变动」；
//   - size/offset 上限同上；空结果是合法结果（空列表 + total=0），
//     但 Count/List 任一失败都上抛错误，禁止折叠成空台账冒充成功。
func (l *ListFlowsLogic) ListFlows(in *rpc.ListFlowsReq) (*rpc.ListFlowsReply, error) {
	cfg := l.svcCtx.Config.Payment
	if in.Mid < 0 {
		return nil, model.ErrInvalidMid
	}
	bizType := int32(in.BizType)
	if bizType < 0 || bizType > model.FlowBizAdminAdjust {
		return nil, model.ErrDetail(model.ErrInvalidBizType, "biz_type="+in.BizType.String())
	}
	bizNo := strings.TrimSpace(in.BizNo)
	if err := requireMaxLength("biz_no", bizNo, 64); err != nil {
		return nil, err
	}
	if err := requireListBounds(cfg, in.Mid, in.FromTs, in.ToTs); err != nil {
		return nil, err
	}
	page, size, err := normalizePage(cfg, in.Page, in.Size)
	if err != nil {
		return nil, err
	}

	q := model.FlowListQuery{
		Mid:     in.Mid,
		BizType: bizType,
		BizNo:   bizNo,
		FromTs:  in.FromTs,
		ToTs:    in.ToTs,
		Page:    listPage(page, size),
	}
	total, err := l.svcCtx.Models.Flow.Count(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListFlows: count mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	rows, err := l.svcCtx.Models.Flow.List(l.ctx, q)
	if err != nil {
		l.Errorf("payment/ListFlows: list mid=%d err=%v", in.Mid, err)
		return nil, err
	}
	return &rpc.ListFlowsReply{
		Flows: flowInfos(rows),
		Total: total,
		Page:  page,
		Size:  size,
	}, nil
}
