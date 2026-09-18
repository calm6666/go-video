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

type RealnameInfoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询实名简要信息
func NewRealnameInfoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameInfoLogic {
	return &RealnameInfoLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询实名简要信息：聚合 user-profile RealnameDetail + RealnameStatus RPC。
func (l *RealnameInfoLogic) RealnameInfo(req *types.ParamMid) (resp *types.RealnameBriefResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	detail, err := l.svcCtx.UserProfile.RealnameDetail(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/app/realnameInfo: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	brief := &realnameBriefData{
		Realname: detail.GetRealname(),
		Card:     detail.GetCard(),
		CardType: int(detail.GetCardType()),
	}
	if status, err := l.svcCtx.UserProfile.RealnameStatus(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid}); err == nil {
		brief.Status = int8(status.GetRealnameStatus())
	}
	return &types.RealnameBriefResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RealnameBriefData{RealnameBrief: toRealnameBrief(brief)},
		TTL:     0,
	}, nil
}
