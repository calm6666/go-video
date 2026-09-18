// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RealnameCaptureCheckLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 校验实名手机验证码
func NewRealnameCaptureCheckLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RealnameCaptureCheckLogic {
	return &RealnameCaptureCheckLogic{Logger: logx.WithContext(ctx), ctx: ctx, svcCtx: svcCtx}
}

// 校验实名手机验证码：验证码校验为服务端能力（验证码不落网关），
// 该路由由 user-profile 的 RealnameApply 提交校验承载，此处保持兼容返回
// 说明：参考仓库 /x/member/realname/tel/capture/check 为内部校验入口，
// 客户端验证码校验在提交申请时由 user-profile 统一完成。
func (l *RealnameCaptureCheckLogic) RealnameCaptureCheck(req *types.ParamCaptureCheck) (resp *types.EmptyResponse, err error) {
	if req.Mid <= 0 || req.Capture <= 0 {
		return nil, errors.New("invalid capture check params")
	}
	return emptyResponse(), nil
}
