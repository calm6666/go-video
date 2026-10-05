// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	creatorrevenuerpc "go-video/services/creator-revenue/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RevenueMetricListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 计量台账分页（某周期某内容某来源的折算结果；应计金额，未支付）
func NewRevenueMetricListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueMetricListLogic {
	return &RevenueMetricListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueMetricList 转发 creator-revenue ListRevenueMetrics（计量台账检索，只读）。
//
// 只读路由，无 operator、不看会话。**写入方是 spm / coin / cron**，RecordRevenueMetric
// 不开后台路由（见 admin.api 的「刻意不开的路由」段）：后台代塞一条 quantity 就等于
// 污染计量源，这份台账再也无法用来复核规则有没有被如实执行（§5/§7）。
//
// 网关只挡形状（mid/aid/source_type/page/size 非负），有界性与格式一条都不接管：
//   - period 的 YYYYMM 合法性是服务的 ValidatePeriod（ErrInvalidPeriod），网关不预先拒、
//     也不替调用方补「上个月」——补了就是在替运营挑周期；
//   - 「period 与 mid 至少给一个」是服务的有界性判定（ErrQueryScopeRequired）：
//     它才知道哪个点位能界定全表扫描，网关自己复算只会漂移；两个都不给时照常下传，
//     服务的拒绝逐字透出，绝不渲染成「这一期没有计量」；
//   - aid=0 是「不挂具体内容」（活动激励类台账）而不是「查 aid 为 0 的行」，原样下传；
//   - source_type=0 是「不按来源过滤」，非 0 是否已定义由服务判；
//   - page/size 越上限由服务拒绝（ErrInvalidPageParam），网关不夹。
//
// 应计两位（amount_minor 封顶前 / capped_amount_minor 门槛封顶后）都转达，
// 合并成一位就看不出这一期被封顶砍掉了多少。
func (l *RevenueMetricListLogic) RevenueMetricList(req *types.ParamRevenueMetricList) (resp *types.RevenueMetricListResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := revenueNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("aid", req.Aid); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("source_type", int64(req.SourceType)); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("page", req.Page); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("size", req.Size); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.ListRevenueMetrics(l.ctx, &creatorrevenuerpc.ListRevenueMetricsReq{
		Period:     req.Period,
		Mid:        req.Mid,
		Aid:        req.Aid,
		SourceType: creatorrevenuerpc.RevenueSourceType(req.SourceType),
		Page:       req.Page,
		Size:       req.Size,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueMetricList: period=%q mid=%d aid=%d source_type=%d page=%d size=%d err=%v",
			req.Period, req.Mid, req.Aid, req.SourceType, req.Page, req.Size, err)
		return nil, err
	}
	return &types.RevenueMetricListResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueMetricListData{
			List:  revenueMetricsToAPI(reply.GetMetrics()),
			Total: reply.GetTotal(),
			Page:  reply.GetPage(),
			Size:  reply.GetSize(),
		},
		TTL: 0,
	}, nil
}
