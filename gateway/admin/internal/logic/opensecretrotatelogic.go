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

type OpenSecretRotateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 轮换应用密钥（新明文仅此一次返回且不进日志；旧密钥宽限期由服务落地）
func NewOpenSecretRotateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenSecretRotateLogic {
	return &OpenSecretRotateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenSecretRotate 转发 open-platform RotateApplicationSecret（运营通道）。
//
// grace_seconds=0 是「旧密钥立即失效」的真实语义而不是「没填」，因此原样下传，
// 宽限期上界与「这个状态的应用能不能轮换」由服务判（applyRotationGrace 一侧）。
// **响应里的 client_secret 是一次性明文**：本方法不写日志、不进缓存（ttl 固定 0），
// 也不在任何错误路径上回显；日志只记 app_id 与主体两个 ID。
func (l *OpenSecretRotateLogic) OpenSecretRotate(req *types.ParamOpenSecretRotate) (resp *types.OpenSecretRotateResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("grace_seconds", req.GraceSeconds); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openSecretRotate", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.RotateApplicationSecret(l.ctx, &openplatformrpc.RotateApplicationSecretReq{
		AppId:        req.AppId,
		OperatorMid:  req.OperatorMid,
		IsOperator:   true,
		GraceSeconds: req.GraceSeconds,
		Reason:       req.Reason,
		TraceId:      req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openSecretRotate: app_id=%d grace_seconds=%d operator_mid=%d err=%v",
			req.AppId, req.GraceSeconds, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenSecretRotateResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenSecretRotateData{
			ClientSecret:       reply.GetClientSecret(),
			SecretId:           reply.GetSecretId(),
			OldSecretId:        reply.GetOldSecretId(),
			OldSecretExpiresAt: reply.GetOldSecretExpiresAt(),
			RotatedAt:          reply.GetRotatedAt(),
		},
		TTL: 0,
	}, nil
}
