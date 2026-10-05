// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	collectorrpc "go-video/services/event-collector/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CollectorPolicyListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 策略版本游标翻页（含 ARCHIVED：历史批次的归因依据）
func NewCollectorPolicyListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CollectorPolicyListLogic {
	return &CollectorPolicyListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CollectorPolicyList 转发 event-collector ListDispatchPolicies。
// 默认「state 不给 = 全部」，包含 ARCHIVED：它们是历史批次的归因依据，
// 后台要能回答「这批当时用的是哪版采样规则」，只列 DRAFT/ACTIVE 就等于把答案删掉。
// 状态取值合法性与 page_size 上限由服务判定，网关只挡负数（0 是合法的「不过滤」哨兵）。
func (l *CollectorPolicyListLogic) CollectorPolicyList(req *types.ParamCollectorPolicyList) (resp *types.CollectorPolicyListResponse, err error) {
	if l.svcCtx.EventCollector == nil {
		return nil, errCollectorServiceNotConfigured
	}
	if req == nil {
		return nil, errCollectorRequestMissing
	}
	if err := collectorNonNeg32("state", req.State); err != nil {
		return nil, err
	}
	if err := collectorNonNeg32("page_size", req.PageSize); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.EventCollector.ListDispatchPolicies(l.ctx, &collectorrpc.ListDispatchPoliciesReq{
		State:    collectorrpc.PolicyState(req.State),
		Cursor:   req.Cursor,
		PageSize: req.PageSize,
	})
	if err != nil {
		l.Errorf("gateway/admin/collectorPolicyList: state=%d cursor=%q page_size=%d err=%v",
			req.State, req.Cursor, req.PageSize, err)
		return nil, err
	}
	return &types.CollectorPolicyListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CollectorPolicyListData{
			List:       collectorPolicyListToAPI(reply.GetList()),
			NextCursor: reply.GetNextCursor(),
			HasMore:    reply.GetHasMore(),
			Total:      reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
