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

type ListSettlementsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSettlementsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSettlementsLogic {
	return &ListSettlementsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 结算单分页
//
// 判定口径（与 ListRevenueMetrics 同一套收口）：
//   - 必须带 period 或 mid 之一（ErrQueryScopeRequired）：结算单表按账期线性增长，
//     无界分页只是把「查全部」伪装成一次普通请求；真要全量数据请走离线导出，不是在线接口；
//   - period 给了就必须是合法 YYYYMM，不允许「格式不对就当没传」地放宽成全表扫描；
//   - state 过滤值越界直接报错（validSettlementStateFilter）：把「筛选条件写错」渲染成
//     「这个状态没单」，运营会据此去催错方向的确认；state=0 表示不过滤，
//     此时**含 VOIDED**——复核必须看得到作废历史；
//   - 分页由 svcCtx.PageSize 折成有界值（上限 CreatorRevenue.MaxPageSize）；
//   - 查询错误上抛；空列表投影成非 nil 空数组（网关要能区分「没有」与「没查」）。
func (l *ListSettlementsLogic) ListSettlements(
	in *rpc.ListSettlementsReq,
) (*rpc.ListSettlementsReply, error) {
	if in == nil {
		in = &rpc.ListSettlementsReq{}
	}
	if err := l.svcCtx.Ready(); err != nil {
		return nil, err
	}
	period, err := model.ValidatePeriod(in.Period)
	if err != nil && strings.TrimSpace(in.Period) != "" {
		return nil, err
	}
	if period == "" && in.Mid == 0 {
		return nil, fmt.Errorf("%w: 需要至少一个 period 或 mid 来界定结算单扫描范围",
			model.ErrQueryScopeRequired)
	}
	if in.Mid < 0 {
		return nil, fmt.Errorf("%w: mid=%d", model.ErrInvalidMid, in.Mid)
	}
	state := int32(in.State)
	if err := validSettlementStateFilter(state); err != nil {
		return nil, err
	}
	page := l.svcCtx.PageSize(in.Page, in.Size)

	rows, err := l.svcCtx.Settlements.List(l.ctx, period, in.Mid, state, page.Offset, page.Limit)
	if err != nil {
		l.Errorf("ListSettlements failed period=%s mid=%d state=%d offset=%d limit=%d: %v",
			period, in.Mid, state, page.Offset, page.Limit, err)
		return nil, err
	}
	total, err := l.svcCtx.Settlements.Count(l.ctx, period, in.Mid, state)
	if err != nil {
		l.Errorf("ListSettlements count failed period=%s mid=%d state=%d: %v", period, in.Mid, state, err)
		return nil, err
	}
	return &rpc.ListSettlementsReply{
		Settlements: settlementInfos(rows),
		Total:       total,
		Page:        page.RequestedPage,
		Size:        page.RequestedSize,
	}, nil
}
