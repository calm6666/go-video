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

type RevLeaveLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 退出分成计划
func NewRevLeaveLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevLeaveLogic {
	return &RevLeaveLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevLeave 自助退出分成计划，响应与 /creator/revenue/enroll 同形（都是参与关系 + duplicated）。
// 闸门只有 mid 与 request_id：reason 在 LeavePlanReq 里标注的是「运营代操作必填」，
// 自助退出没有可填的理由位（app.api 的 ParamRevLeave 也没这一位），因此网关留空由服务侧
// 按 operator=user 处理，不代客户端编一个理由文本。
// operator 由网关渲染成自助身份 "user"；request_id 原样透传保证重放不重复退出。
// 退出只是停止后续计量，历史已确认结算单不受影响——这条口径由 creator-revenue 保证，
// 网关不解释金额也不回滚记录。duplicated=true 是结论不是错误；TTL 0。
func (l *RevLeaveLogic) RevLeave(req *types.ParamRevLeave) (resp *types.RevEnrollResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	if err := requireText("request_id", req.RequestId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.LeavePlan(l.ctx, &creatorrevenuerpc.LeavePlanReq{
		Mid:       req.Mid,
		Operator:  commerceSelfOperator,
		RequestId: req.RequestId,
	})
	if err != nil {
		l.Errorf("gateway/app/revLeave: mid=%d err=%v", req.Mid, err)
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
