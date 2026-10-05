// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	userprofilerc "go-video/services/user-profile/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameStrippedLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 查询脱敏实名信息
func NewRealnameStrippedLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameStrippedLogic {
	return &RealnameStrippedLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 查询脱敏实名信息：调用 user-profile RealnameStrippedInfo RPC。
func (l *RealnameStrippedLogic) RealnameStripped(req *types.ParamModify) (resp *types.RealnameStrippedResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if err := adminSessionGate(l.ctx, "realnameStripped"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.UserProfile.RealnameStrippedInfo(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/admin/realnameStripped: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return &types.RealnameStrippedResponse{
		Code:    0,
		Message: "ok",
		Data: types.RealnameStrippedData{
			Mid:       reply.GetMid(),
			Status:    int8(reply.GetStatus()),
			Channel:   int8(reply.GetChannel()),
			Country:   int16(reply.GetCountry()),
			CardType:  int8(reply.GetCardType()),
			AdultType: int8(reply.GetAdultType()),
		},
		TTL: 0,
	}, nil
}
