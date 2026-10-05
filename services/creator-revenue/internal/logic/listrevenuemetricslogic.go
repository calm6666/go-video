package logic

import (
	"context"
	"fmt"
	"strings"

	"go-video/services/creator-revenue/internal/svc"
	"go-video/services/creator-revenue/model"
	"go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRevenueMetricsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRevenueMetricsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRevenueMetricsLogic {
	return &ListRevenueMetricsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 计量台账分页
//
// 判定口径：
//   - 必须带 period 或 mid 之一：cr_metric 是唯一会随内容数线性膨胀的表，
//     无界分页会把一次全表扫的代价挂在读接口上（索引也只有 (period,mid,…) 前缀可用）。
//     这是「查询范围守卫」，不是伪成功：守卫不通过回 ErrQueryScopeRequired；
//   - period 给就必须是合法 YYYYMM，不允许「格式不对就当没传」地放宽成全表；
//   - 查询错误上抛，空结果投影成非 nil 空数组。
func (l *ListRevenueMetricsLogic) ListRevenueMetrics(
	in *rpc.ListRevenueMetricsReq,
) (*rpc.ListRevenueMetricsReply, error) {
	if in == nil {
		in = &rpc.ListRevenueMetricsReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	period, err := model.ValidatePeriod(in.Period)
	if err != nil && strings.TrimSpace(in.Period) != "" {
		return nil, err
	}
	if period == "" && in.Mid == 0 {
		return nil, fmt.Errorf("%w: 需要至少一个 period 或 mid", model.ErrQueryScopeRequired)
	}
	if in.Mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d", model.ErrInvalidMid, in.Mid)
	}
	aid, err := normalizeAid(in.Aid)
	if err != nil {
		return nil, err
	}
	sourceType := int32(in.SourceType)
	if sourceType != 0 {
		if err := validSourceType(sourceType); err != nil {
			return nil, err
		}
	}
	page := l.svcCtx.PageSize(in.Page, in.Size)

	rows, err := l.svcCtx.Metrics.List(l.ctx, period, in.Mid, aid, sourceType, page.Offset, page.Limit)
	if err != nil {
		l.Errorf("ListRevenueMetrics failed period=%s mid=%d aid=%d source_type=%d: %v",
			period, in.Mid, aid, sourceType, err)
		return nil, err
	}
	total, err := l.svcCtx.Metrics.Count(l.ctx, period, in.Mid, aid, sourceType)
	if err != nil {
		l.Errorf("ListRevenueMetrics count failed period=%s mid=%d: %v", period, in.Mid, err)
		return nil, err
	}
	return &rpc.ListRevenueMetricsReply{
		Metrics: metricInfos(rows),
		Total:   total,
		Page:    page.RequestedPage,
		Size:    page.RequestedSize,
	}, nil
}
