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

type OpenAuthorizationRevokeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 撤销授权（TOKEN/GRANT/USER_ALL 三种范围；必填组合由服务判，明文 hint 不进日志）
func NewOpenAuthorizationRevokeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenAuthorizationRevokeLogic {
	return &OpenAuthorizationRevokeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenAuthorizationRevoke 转发 open-platform RevokeAuthorization（RFC 7009 语义）。
//
// 撤销范围这张矩阵整个留在服务侧：GRANT 要 app_id+mid、USER_ALL 只要 mid、
// TOKEN 要 app_id 加 (token_id 或 token_hint)。网关既不复算它，也不「帮忙猜」——
// mid 没填时推成 USER_ALL 会一次打死该用户对所有第三方的授权，猜错的方向是扩大伤害。
// target=0（UNSPECIFIED）在本域是写侧必填位，proto:77 写明服务端拒绝，所以挡在这里。
// is_operator 恒为 true，因此服务的 requireReason（代他人撤销必须有原因）在本入口一律生效。
// **token_hint 是明文凭证**：只随本次请求下传，绝不进日志；日志里的 target 与两个 ID 足够定位。
func (l *OpenAuthorizationRevokeLogic) OpenAuthorizationRevoke(req *types.ParamOpenAuthorizationRevoke) (resp *types.OpenAuthorizationRevokeResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openPositive("target", req.Target); err != nil {
		return nil, err
	}
	if err := openNonNeg("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := openNonNeg("mid", req.Mid); err != nil {
		return nil, err
	}
	if err := openNonNeg("token_id", req.TokenId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openAuthorizationRevoke", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.RevokeAuthorization(l.ctx, &openplatformrpc.RevokeAuthorizationReq{
		Target:      openplatformrpc.RevokeTarget(req.Target),
		AppId:       req.AppId,
		Mid:         req.Mid,
		TokenId:     req.TokenId,
		TokenHint:   req.TokenHint,
		OperatorMid: req.OperatorMid,
		IsOperator:  true,
		Reason:      req.Reason,
		TraceId:     req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/admin/openAuthorizationRevoke: target=%d app_id=%d mid=%d operator_mid=%d err=%v",
			req.Target, req.AppId, req.Mid, req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenAuthorizationRevokeResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenAuthorizationRevokeData{
			GrantsRevoked: reply.GetGrantsRevoked(),
			TokensRevoked: reply.GetTokensRevoked(),
			EffectiveAt:   reply.GetEffectiveAt(),
		},
		TTL: 0,
	}, nil
}
