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

type RevenueRuleGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分成规则读取（version>0 按历史版本读，结算争议复核用）
func NewRevenueRuleGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevenueRuleGetLogic {
	return &RevenueRuleGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevenueRuleGet 转发 creator-revenue GetRevenueRule（单条规则读取，支持按历史版本复核）。
//
// 只读路由，无 operator、不看会话。
//
// 网关只挡三位形状：rule_id 非负、version 非负、rule_code 原样（长度归服务判）。
// 刻意**不接管**的（§5 规则与变更台账归 creator-revenue）：
//   - 「rule_id 与 rule_code 至少给一个」是服务的 ErrRuleTargetRequired：两个都为 0/空时
//     服务直接拒，网关不预先拒也不替调用方挑一个定位方式；
//   - version=0 是「读当前版本」，不是「读第 0 版」，原样下传；
//   - 「这一版还原得出来吗」是服务的结论：它拿现值与 cr_rule_change 比对，
//     版本还不存在、或变更台账缺失（迁移前的存量行）时**宁可回 found=false**，
//     也不把现单价当成历史单价返回。网关必须逐字转达这个 found=false，
//     绝不因为「rule_id 给对了」就渲染成 found=true —— 那等于在争议复核里伪造历史口径。
func (l *RevenueRuleGetLogic) RevenueRuleGet(req *types.ParamRevenueRuleGet) (resp *types.RevenueRuleGetResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errRevenueServiceNotConfigured
	}
	if req == nil {
		return nil, errRevenueRequestMissing
	}
	if err := revenueNonNeg("rule_id", req.RuleId); err != nil {
		return nil, err
	}
	if err := revenueNonNeg("version", req.Version); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GetRevenueRule(l.ctx, &creatorrevenuerpc.GetRevenueRuleReq{
		RuleId:   req.RuleId,
		RuleCode: req.RuleCode,
		Version:  req.Version,
	})
	if err != nil {
		l.Errorf("gateway/admin/revenueRuleGet: rule_id=%d rule_code=%q version=%d err=%v",
			req.RuleId, req.RuleCode, req.Version, err)
		return nil, err
	}
	return &types.RevenueRuleGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevenueRuleGetData{
			Found: reply.GetFound(),
			Rule:  revenueRuleToAPI(reply.GetRule()),
		},
		TTL: 0,
	}, nil
}
