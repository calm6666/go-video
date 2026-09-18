// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameApplyStatusLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询实名申请流程状态
func NewRealnameApplyStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameApplyStatusLogic {
	return &RealnameApplyStatusLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询实名申请流程状态：聚合 user-profile RealnameApplyStatus RPC，
// 冗余的 realname/card 字段经 RealnameDetail 补齐。
func (l *RealnameApplyStatusLogic) RealnameApplyStatus(req *types.ParamMid) (resp *types.RealnameApplyStatusResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	reply, err := l.svcCtx.UserProfile.RealnameApplyStatus(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/realnameApplyStatus: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	data := types.RealnameApplyStatus{
		Status: int8(reply.GetStatus()),
		Remark: reply.GetRemark(),
	}
	if detail, err := l.svcCtx.UserProfile.RealnameDetail(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid}); err == nil && detail != nil {
		data.Realname = detail.GetRealname()
		data.Card = detail.GetCard()
	}
	return &types.RealnameApplyStatusResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RealnameApplyStatusData{RealnameApplyStatus: data},
		TTL:     0,
	}, nil
}
