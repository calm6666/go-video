// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	privatemessagerpc "go-video/services/private-message/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PmUnreadSummaryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 未读汇总（角标用，投影可重算）
func NewPmUnreadSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PmUnreadSummaryLogic {
	return &PmUnreadSummaryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PmUnreadSummary 未读角标是服务侧投影（可由成员游标重算），与 feed 的动态未读、
// inbox 的站内信未读是三个不同域的计数，网关不合并（AGENTS.md §5）。
// ttl 固定 0：角标是实时状态，不建议客户端缓存。
func (l *PmUnreadSummaryLogic) PmUnreadSummary(req *types.ParamPmUnread) (resp *types.PmUnreadResponse, err error) {
	if l.svcCtx.PrivateMessage == nil {
		return nil, errors.New("private-message service not configured")
	}
	reply, err := l.svcCtx.PrivateMessage.GetUnreadSummary(l.ctx, &privatemessagerpc.GetUnreadSummaryReq{
		Mid:     req.Mid,
		Force:   req.Force,
		TraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/pmUnreadSummary: mid=%d force=%v err=%v", req.Mid, req.Force, err)
		return nil, err
	}
	return &types.PmUnreadResponse{
		Code:    0,
		Message: "ok",
		Data: types.PmUnreadData{
			UnreadTotal:         reply.GetUnreadTotal(),
			UnreadConversations: reply.GetUnreadConversations(),
			ComputedAt:          reply.GetComputedAt(),
		},
		TTL: 0,
	}, nil
}
