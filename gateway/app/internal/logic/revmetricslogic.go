// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevMetricsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的收益计量明细
func NewRevMetricsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevMetricsLogic {
	return &RevMetricsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevMetrics 只读本人计量台账：mid 必填且为正，空 mid 的跨用户查询属运营面，不在终端出现。
// period / aid 为可选过滤位，空串与 0 表示不过滤，网关不补当前月也不改写；
// source_type 枚举位原样透传（0 不过滤），page/page_size 原样透传给服务裁剪。
// amount_minor 与 capped_amount_minor（封顶前/后）都原样投影，网关不重算折算、不核对封顶，
// 让作者看得到「量怎么变成钱」正是这张表的作用。
// 台账会被同周期更正覆盖，TTL 0。
func (l *RevMetricsLogic) RevMetrics(req *types.ParamRevMetrics) (resp *types.RevMetricsResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ListRevenueMetrics(l.ctx, &creatorrevenuerpc.ListRevenueMetricsReq{
		Period:     req.Period,
		Mid:        req.Mid,
		Aid:        req.Aid,
		SourceType: creatorrevenuerpc.RevenueSourceType(req.SourceType),
		Page:       int64(req.Page),
		Size:       int64(req.PageSize),
	})
	if err != nil {
		l.Errorf("gateway/app/revMetrics: mid=%d period=%s aid=%d page=%d err=%v", req.Mid, req.Period, req.Aid, req.Page, err)
		return nil, err
	}
	return &types.RevMetricsResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevMetricsData{
			Metrics:  revMetricsToAPI(reply.GetMetrics()),
			Total:    reply.GetTotal(),
			Page:     reply.GetPage(),
			PageSize: reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
