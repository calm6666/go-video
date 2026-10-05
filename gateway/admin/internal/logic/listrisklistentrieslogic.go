// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	riskcontrolrpc "go-video/services/risk-control/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRiskListEntriesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询黑白名单条目
func NewListRiskListEntriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRiskListEntriesLogic {
	return &ListRiskListEntriesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 名单列表：聚合 risk-control GetListEntries RPC。
// 过滤条件一律「0/-1 = 不过滤」；服务端按 target_type 决定 target_value 的规范化口径，
// 所以带 value 却不带 type 的查询在网关就被拒掉，避免落到一条看不懂的 ErrInvalidTarget。
func (l *ListRiskListEntriesLogic) ListRiskListEntries(req *types.ParamListRiskListEntries) (resp *types.RiskListEntriesResponse, err error) {
	if l.svcCtx.RiskControl == nil {
		return nil, errors.New("risk-control service not configured")
	}
	if err := requireOperatorID(req.OperatorId); err != nil {
		return nil, err
	}
	listType, err := riskListType(req.ListType, true) // 0 表示不按黑/白名单过滤
	if err != nil {
		return nil, err
	}
	targetType, err := riskTargetType(req.TargetType, true) // 0 表示不按目标类型过滤
	if err != nil {
		return nil, err
	}
	if err := riskStateFilter("state", req.State); err != nil {
		return nil, err
	}
	if req.TargetValue != "" && req.TargetType == 0 {
		return nil, errors.New("gateway/admin: target_type required when target_value is set")
	}
	pn, ps := normalizeRiskPage(req.Pn, req.Ps)
	// 契约缺口：riskcontrol.v1.GetListEntriesReq 没有 operator 字段，
	// 谁在查名单只能留在网关日志，风控侧审计看不到读取人。
	reply, err := l.svcCtx.RiskControl.GetListEntries(l.ctx, &riskcontrolrpc.GetListEntriesReq{
		ListType:    listType,
		TargetType:  targetType,
		TargetValue: req.TargetValue,
		State:       req.State,
		Pn:          pn,
		Ps:          ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/listRiskListEntries: list_type=%d target_type=%d has_target_value=%v state=%d pn=%d ps=%d operator_id=%d err=%v",
			req.ListType, req.TargetType, req.TargetValue != "", req.State, pn, ps, req.OperatorId, err)
		return nil, err
	}
	return &types.RiskListEntriesResponse{
		Code:    0,
		Message: "ok",
		Data: types.RiskListEntriesData{
			Total:   reply.GetTotal(),
			Pn:      reply.GetPn(),
			Ps:      reply.GetPs(),
			Entries: riskListEntriesToAPI(reply.GetEntries()),
		},
		TTL: 0,
	}, nil
}
