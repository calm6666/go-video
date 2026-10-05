// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminCommentStatsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 目标下的评论计数快照
func NewAdminCommentStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminCommentStatsLogic {
	return &AdminCommentStatsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminCommentStats 聚合 comment CommentStats RPC：total 含楼中楼、root_total 只数根评论。
// 计数口径（是否含已删除、是否实时）由 comment 服务决定，网关不重算也不做缓存，
// 因此 ttl 固定 0，建议后台每次回源。
func (l *AdminCommentStatsLogic) AdminCommentStats(req *types.ParamAdminCommentStats) (resp *types.AdminCommentStatsResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}

	reply, err := l.svcCtx.Comment.CommentStats(l.ctx, &commentrpc.CommentStatsReq{
		Oid: req.Oid,
		Tp:  req.Tp,
	})
	if err != nil {
		l.Errorf("gateway/admin/adminCommentStats: oid=%d tp=%d err=%v", req.Oid, req.Tp, err)
		return nil, err
	}
	return &types.AdminCommentStatsResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminCommentStatsData{
			Total:     reply.GetTotal(),
			RootTotal: reply.GetRootTotal(),
		},
		TTL: 0,
	}, nil
}
