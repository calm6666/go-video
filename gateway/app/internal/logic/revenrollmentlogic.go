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

type RevEnrollmentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 我的分成参与状态
func NewRevEnrollmentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RevEnrollmentLogic {
	return &RevEnrollmentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// RevEnrollment 读本人参与关系：mid 必填且为正（GetEnrollmentReq 只有 mid 一个位，
// 跨用户的 ListEnrollments 属运营面，不在终端路由出现）。
// found=false 是「没参加过」这一正常结论，不是错误：信封仍回 Code:0 + found=false +
// 零值 enrollment，端上据此显示参加入口。
// 参与状态会被运营暂停/恢复，TTL 0。
func (l *RevEnrollmentLogic) RevEnrollment(req *types.ParamRevEnrollment) (resp *types.RevEnrollmentResponse, err error) {
	if l.svcCtx.CreatorRevenue == nil {
		return nil, errors.New("creator-revenue service not configured")
	}
	if err := requireMid(req.Mid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.CreatorRevenue.GetEnrollment(l.ctx, &creatorrevenuerpc.GetEnrollmentReq{
		Mid: req.Mid,
	})
	if err != nil {
		l.Errorf("gateway/app/revEnrollment: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.RevEnrollmentResponse{
		Code:    0,
		Message: "ok",
		Found:   reply.GetFound(),
		Data:    revEnrollmentToAPI(reply.GetEnrollment()),
		TTL:     0,
	}, nil
}
