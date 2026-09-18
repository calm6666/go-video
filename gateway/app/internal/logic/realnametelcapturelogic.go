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

type RealnameTelCaptureLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发送实名手机验证码
func NewRealnameTelCaptureLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameTelCaptureLogic {
	return &RealnameTelCaptureLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 发送实名手机验证码：聚合 user-profile RealnameTelCapture RPC。
func (l *RealnameTelCaptureLogic) RealnameTelCapture(req *types.ParamMid) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.UserProfile == nil {
		return nil, errors.New("user-profile service not configured")
	}
	if _, err = l.svcCtx.UserProfile.RealnameTelCapture(l.ctx, &userprofilerc.MemberMidReq{Mid: req.Mid}); err != nil {
		l.Errorf("gateway/app/realnameTelCapture: mid=%d err=%v", req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
