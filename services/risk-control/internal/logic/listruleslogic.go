package logic

import (
	"context"

	"go-video/common/validation"
	"go-video/services/risk-control/internal/svc"
	"go-video/services/risk-control/model"
	"go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRulesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRulesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRulesLogic {
	return &ListRulesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 分页查询风控规则。
// action_type=ACTION_UNSPECIFIED 与 metric=METRIC_UNSPECIFIED 表示该维度不过滤；
// state=-1 不过滤，0/1 精确匹配启停状态。
func (l *ListRulesLogic) ListRules(in *rpc.ListRulesReq) (*rpc.ListRulesReply, error) {
	action := int32(in.GetActionType())
	if !model.ValidRuleAction(action) {
		return nil, model.ErrInvalidTarget
	}
	metric := metricFromProto(in.GetMetric())
	if in.GetMetric() != rpc.Metric_METRIC_UNSPECIFIED && metric == "" {
		return nil, model.ErrInvalidTarget
	}
	state := in.GetState()
	if state != -1 && state != int32(model.StateDisabled) && state != int32(model.StateEnabled) {
		return nil, model.ErrInvalidTarget
	}
	page := validation.NormalizePage(int(in.GetPn()), int(in.GetPs()), maxPageSize)

	rows, total, err := l.svcCtx.Repository.ListRules(l.ctx, action, metric, state, page.Page, page.PageSize)
	if err != nil {
		l.Errorf("risk-control/ListRules: failed action=%d metric=%s state=%d err=%v", action, metric, state, err)
		return nil, err
	}
	return &rpc.ListRulesReply{
		Rules: rulesToProto(rows),
		Total: total,
		Pn:    int32(page.Page),
		Ps:    int32(page.PageSize),
	}, nil
}
