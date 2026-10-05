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

type OpenScopeListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// scope 目录读（可带 app_id 回该应用对每条的获批状态；契约无操作者位）
func NewOpenScopeListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OpenScopeListLogic {
	return &OpenScopeListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// OpenScopeList 转发 open-platform ListScopes——本域唯一不进权限表的路由。
//
// 理由是形状而不是重要性：ListScopesReq 里没有任何操作者位（app_id 只是「顺带回这一列
// 获批状态」的上下文，不是主体），服务侧也无从按身份收敛行集，所以「挂一个权限点」
// 只会变成一个谁都需要的开关。scope 目录本身是对外公示口径（授权页文案就来自这里）。
// app_id=0 = 只要目录本身，此时 granted_state 整列恒 0——那是「没问」不是「都没获批」，
// 服务原样回，网关也不把它填成别的值。
func (l *OpenScopeListLogic) OpenScopeList(req *types.ParamOpenScopeList) (resp *types.OpenScopeListResponse, err error) {
	if l.svcCtx.OpenPlatform == nil {
		return nil, errOpenPlatformNotConfigured
	}
	if req == nil {
		return nil, errOpenPlatformRequestMissing
	}
	if err := openNonNeg("app_id", req.AppId); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.OpenPlatform.ListScopes(l.ctx, &openplatformrpc.ListScopesReq{
		AppId:       req.AppId,
		OnlyEnabled: req.OnlyEnabled,
		TraceId:     req.TraceId,
	})
	if err != nil {
		// 只记目录查询条件：scope 目录无个人数据，但获批状态关联具体应用，同样不打明细。
		l.Errorf("gateway/admin/openScopeList: app_id=%d only_enabled=%t err=%v",
			req.AppId, req.OnlyEnabled, err)
		return nil, err
	}
	return &types.OpenScopeListResponse{
		Code:    0,
		Message: "ok",
		Data: types.OpenScopeListData{
			List: openScopesToAPI(reply.GetList()),
		},
		TTL: 0,
	}, nil
}
