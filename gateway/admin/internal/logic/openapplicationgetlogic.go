// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	openplatformrpc "go-video/services/open-platform/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type OpenApplicationGetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 单个应用详情（含回调白名单与密钥状态；不存在与无权看服务回同一错误，网关不折叠成 found=false）
func NewOpenApplicationGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenApplicationGetLogic {
	return &OpenApplicationGetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenApplicationGet 转发 open-platform GetApplication。
//
// caller_mid 即便在运营分支也必填 >0：服务的判定是 requireOwnerOrOperator(caller_mid, operator)，
// 匿名运营在本契约里不被接受（getapplicationlogic 第 3 步）。
// 非运营读不到非 ACTIVE 应用，运营能——这条差别由 operator=true 承载，网关不预判状态再决定给不给看。
// 应用不存在与无权看，服务刻意回同一个错误（不泄露「这个 app_key 存在」），网关不翻译也不折叠。
func (l *OpenApplicationGetLogic) OpenApplicationGet(req *types.ParamOpenApplicationGet) (resp *types.OpenApplicationGetResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openAppSubject(req.AppId, req.AppKey); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openApplicationGet", "caller_mid", req.CallerMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.GetApplication(l.ctx, &openplatformrpc.GetApplicationReq{
		AppId:     req.AppId,
		AppKey:    req.AppKey,
		CallerMid: req.CallerMid,
		Operator:  true,
		TraceId:   req.TraceId,
	})
	if err != nil {
		// app_key 是公开标识但仍可用于枚举探测，日志只打 app_id 与主体。
		l.Errorf("gateway/admin/openApplicationGet: app_id=%d caller_mid=%d err=%v",
			req.AppId, req.CallerMid, err)
		return nil, err
	}
	return &types.OpenApplicationGetResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenApplicationGetData{
			App: openAppToAPI(reply.GetApp()),
		},
		TTL: 0,
	}, nil
}
