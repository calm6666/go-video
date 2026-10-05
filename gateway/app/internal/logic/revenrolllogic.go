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

type RevEnrollLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 参加分成计划（必须带已确认的规则版本）
func NewRevEnrollLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevEnrollLogic {
	return &RevEnrollLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevEnroll 自助参加分成计划。agreed_rule_version 是「用户当时看到并确认的规则版本」，
// 契约标注必填（可回溯承诺口径），因此网关先挡掉 0/负数——但**这个版本号是否真的存在、
// 是否已过期**由 creator-revenue 判定，网关不查规则表也不替客户端补最新版本号
// （补一个就等于网关代用户确认了他没看过的条款）。
// operator 由网关渲染成自助身份 "user"，绝不取客户端自报；request_id 原样透传，
// 改一个字符等于换幂等键，会造成重复参加记录。
// duplicated=true 命中重放、仍是 Code:0 结论；暂停/恢复等处置只能由运营面 SetEnrollmentState 做，
// 终端没有对应路由。参与状态会变，TTL 0。
func (l *RevEnrollLogic) RevEnroll(req *types.ParamRevEnroll) (resp *types.RevEnrollResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requirePositive("agreed_rule_version", req.AgreedRuleVersion); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.EnrollCreator(l.ctx, &creatorrevenuerpc.EnrollCreatorReq{
		Mid:               req.Mid,
		AgreedRuleVersion: req.AgreedRuleVersion,
		Operator:          commerceSelfOperator,
		RequestId:         req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/revEnroll: mid=%d agreed_rule_version=%d err=%v", req.Mid, req.AgreedRuleVersion, err)
		return nil, err
	}
	return &types.RevEnrollResponse{
		Code:    0,
		Message: "ok",
		Data: types.RevEnrollData{
			Duplicated: reply.GetDuplicated(),
			Enrollment: revEnrollmentToAPI(reply.GetEnrollment()),
		},
		TTL: 0,
	}, nil
}
