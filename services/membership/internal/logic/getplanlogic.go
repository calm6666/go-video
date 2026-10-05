package logic

import (
	"context"
	"strings"

	"go-video/services/membership/internal/svc"
	"go-video/services/membership/model"
	"go-video/services/membership/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetPlanLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetPlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetPlanLogic {
	return &GetPlanLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GetPlan 单个套餐读取（下单前置校验用）。
//
// 口径：plan_id 与 plan_code 二选一，plan_id 优先。本接口按主键/唯一码读，
// 不隐藏 DRAFT/OFF_SALE —— 调用方（trade-order 建单）要自己校验 state 是否为 ON_SALE，
// 所以这里必须把真实 state 带回去，而不是替它过滤成「不存在」。
// found=false 只在确实没有这行时给出；DB 错误一律上抛，不能折叠成「套餐不存在」。
func (l *GetPlanLogic) GetPlan(in *rpc.GetPlanReq) (*rpc.GetPlanReply, error) {
	code := strings.TrimSpace(in.PlanCode)
	if in.PlanId <= 0 && code == "" {
		return nil, model.ErrPlanIdentifierRequired
	}

	var (
		p   *model.Plan
		err error
	)
	if in.PlanId > 0 {
		p, err = l.svcCtx.Plan.FindOne(l.ctx, in.PlanId)
	} else {
		p, err = l.svcCtx.Plan.FindByCode(l.ctx, code)
	}
	if err != nil {
		l.Errorf("membership/GetPlan: read failed plan_id=%d plan_code=%s err=%v", in.PlanId, code, err)
		return nil, err
	}
	if p == nil {
		return &rpc.GetPlanReply{Found: false}, nil
	}
	return &rpc.GetPlanReply{Found: true, Plan: planToRPC(p)}, nil
}
