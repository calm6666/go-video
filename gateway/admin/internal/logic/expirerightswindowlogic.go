// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	rightsrpc "go-video/services/rights/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ExpireRightsWindowLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 手动过期播放窗口
func NewExpireRightsWindowLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ExpireRightsWindowLogic {
	return &ExpireRightsWindowLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 手动过期播放窗口：聚合 rights ExpireWindow RPC。
// 窗口能否过期、过期后是否联动下架内容由 rights 服务决定（AGENTS.md §8 版权撤回需保留
// 审计证据），网关只按 window_id 透传。
// 契约缺口（见交付报告）：rights.WindowReq 只有 window_id/ip，无 operator，
// 且 types.ParamRightsWindowId 是纯 path 参数，因此本接口无法把运营操作人传给 rights 审计。
func (l *ExpireRightsWindowLogic) ExpireRightsWindow(req *types.ParamRightsWindowId) (resp *types.RightsWindowResponse, err error) {
	if l.svcCtx.Rights == nil {
		return nil, errors.New("rights service not configured")
	}
	if err := adminSessionGate(l.ctx, "expireRightsWindow"); err != nil {
		return nil, err
	}
	reply, err := l.svcCtx.Rights.ExpireWindow(l.ctx, &rightsrpc.WindowReq{
		WindowId: req.WindowId,
	})
	if err != nil {
		l.Errorf("gateway/admin/expireRightsWindow: window_id=%d err=%v", req.WindowId, err)
		return nil, err
	}
	return &types.RightsWindowResponse{
		Code:    0,
		Message: "ok",
		Data:    types.RightsWindowData{Window: toAdminRightsWindow(reply.GetWindow())},
		TTL:     0,
	}, nil
}
