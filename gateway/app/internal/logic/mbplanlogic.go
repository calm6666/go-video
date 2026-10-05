// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	membershiprpc "go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type MbPlanLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个套餐读取（下单前置展示）
func NewMbPlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MbPlanLogic {
	return &MbPlanLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// MbPlan 按 plan_id 或 plan_code 定位套餐；两者至少给一个（否则下游只能收到全零请求）。
// 传入值原样透传，不 TrimSpace 后回填——plan_code 是对外稳定编码，改一个字符就是另一个档。
// found=false（档不存在或仍是草稿）是结论不是错误：返回 Code:0 与零值 plan，
// 端上按 plan_id==0 判定「无此档」；下单金额仍以 trade-order 服务端重算为准。
func (l *MbPlanLogic) MbPlan(req *types.ParamMbPlan) (resp *types.MbPlanResponse, err error) {
	if l.svcCtx.Membership == nil {
		return nil, errors.New("membership service not configured")
	}
	if err := requirePlanTarget(req.PlanId, req.PlanCode); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Membership.GetPlan(l.ctx, &membershiprpc.GetPlanReq{
		PlanId:   req.PlanId,
		PlanCode: req.PlanCode,
	})
	if err != nil {
		l.Errorf("gateway/app/mbPlan: plan_id=%d plan_code=%q err=%v", req.PlanId, req.PlanCode, err)
		return nil, err
	}
	return &types.MbPlanResponse{
		Code:    0,
		Message: "ok",
		Data:    mbPlanToAPI(reply.GetPlan()),
		TTL:     60,
	}, nil
}
