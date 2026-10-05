// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	inboxrpc "go-video/services/inbox/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminRecomputeInboxUnreadLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重算某用户未读快照并回填缓存（计数漂移修复工具，幂等）
func NewAdminRecomputeInboxUnreadLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminRecomputeInboxUnreadLogic {
	return &AdminRecomputeInboxUnreadLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminRecomputeInboxUnread 聚合 inbox RecomputeUnread RPC：以明细表为准重算未读，
// 覆盖快照并回填 Redis，用于「未读数对不上」时的运维修复。下游只接受 mid>0 的单人重算，
// 全量校准属于 services/cron（尚未实现），不在此处循环调用刷库。
func (l *AdminRecomputeInboxUnreadLogic) AdminRecomputeInboxUnread(req *types.ParamAdminRecomputeInboxUnread) (resp *types.AdminRecomputeInboxUnreadResponse, err error) {
	if l.svcCtx.Inbox == nil {
		return nil, errors.New("inbox service not configured")
	}
	if err := adminSubjectGate(l.ctx, "adminRecomputeInboxUnread", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}

	reply, err := l.svcCtx.Inbox.RecomputeUnread(l.ctx, &inboxrpc.RecomputeUnreadReq{Mid: req.Mid})
	if err != nil {
		l.Errorf("gateway/admin/adminRecomputeInboxUnread: operator=%d mid=%d err=%v",
			req.OperatorMid, req.Mid, err)
		return nil, err
	}
	return &types.AdminRecomputeInboxUnreadResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminRecomputeInboxUnreadData{
			Mid:        req.Mid,
			Total:      reply.GetTotal(),
			ByCategory: inboxUnreadByCategoryToAPI(reply.GetByCategory()),
		},
		TTL: 0,
	}, nil
}
