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

type OpenScopeGrantLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// scope 授予/回收（部分授予是正常结果，rejected 列表原样回；幂等键必填）
func NewOpenScopeGrantLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenScopeGrantLogic {
	return &OpenScopeGrantLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenScopeGrant 转发 open-platform GrantApplicationScopes（运营审批，只有运营通道）。
//
// 网关不重做这张判定表：授予与回收能否同时给、同一 scope 是否出现在两侧（服务判成
// 未定义行为并拒）、目录里有没有这一条、风险级别能否批量授予、商业化红线类目，
// 全在 grantapplicationscopeslogic 里。这里只挡三个「下游没有对应语义」的形状：
// 没有应用、没有问责原因、没有幂等键——重试会把回收再执行一遍的那种后果就来自漏填幂等键。
// rejected 是这条 RPC 的正常结果而不是失败：网关原样回三列，不折叠成整体成功或整体失败。
func (l *OpenScopeGrantLogic) OpenScopeGrant(req *types.ParamOpenScopeGrant) (resp *types.OpenScopeGrantResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openIDGate("app_id", req.AppId); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("reason", req.Reason); err != nil {
		return nil, err
	}
	if err := requireNonEmpty("idempotency_key", req.IdempotencyKey); err != nil {
		return nil, err
	}
	if err := openOperatorGate(l.ctx, "openScopeGrant", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.GrantApplicationScopes(l.ctx, &openplatformrpc.GrantApplicationScopesReq{
		AppId:          req.AppId,
		Grant:          req.Grant,
		Revoke:         req.Revoke,
		OperatorMid:    req.OperatorMid,
		Reason:         req.Reason,
		IdempotencyKey: req.IdempotencyKey,
		TraceId:        req.TraceId,
	})
	if err != nil {
		// 只记条数不记 scope 名列表：名称组合本身能看出这次批了什么范围。
		l.Errorf("gateway/admin/openScopeGrant: app_id=%d grant=%d revoke=%d operator_mid=%d err=%v",
			req.AppId, len(req.Grant), len(req.Revoke), req.OperatorMid, err)
		return nil, err
	}
	return &types.OpenScopeGrantResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenScopeGrantData{
			Granted:    openStrings(reply.GetGranted()),
			Revoked:    openStrings(reply.GetRevoked()),
			Rejected:   openStrings(reply.GetRejected()),
			AppVersion: reply.GetAppVersion(),
			Replayed:   reply.GetReplayed(),
		},
		TTL: 0,
	}, nil
}
