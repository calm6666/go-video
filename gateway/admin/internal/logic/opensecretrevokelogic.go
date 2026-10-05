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

type OpenSecretRevokeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 吊销应用密钥（secret_id=0 = 全部生效密钥；token 不受影响，要撤授权走 /authorization/revoke）
func NewOpenSecretRevokeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenSecretRevokeLogic {
	return &OpenSecretRevokeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenSecretRevoke 转发 open-platform RevokeApplicationSecret（泄露应急处置）。
//
// secret_id=0 在契约里是「吊销该应用全部生效密钥」，不是「没选」，因此绝不能在这里当缺参拒掉，
// 也不能反向替调用方填一个具体 ID——吊销几把是数据面后果，由表单显式决定、由服务回读（revoked）。
// proto 明确「已签发的 token 不受影响」：本方法不声称打死授权，要一并撤授权是另一条路由。
func (l *OpenSecretRevokeLogic) OpenSecretRevoke(req *types.ParamOpenSecretRevoke) (resp *types.OpenSecretRevokeResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("secret_id", req.SecretId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openSecretRevoke", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.RevokeApplicationSecret(l.ctx, &openplatformrpc.RevokeApplicationSecretReq{
		AppId:       req.AppId,
		SecretId:    req.SecretId,
		OperatorMid: req.OperatorMid,
		IsOperator:  true,
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openSecretRevoke: app_id=%d secret_id=%d operator_mid=%d err=%v",
			req.AppId, req.SecretId, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenSecretRevokeResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenSecretRevokeData{
			Revoked:     reply.GetRevoked(),
			EffectiveAt: reply.GetEffectiveAt(),
		},
		TTL: 0,
	}, nil
}
