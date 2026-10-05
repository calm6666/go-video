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

type AdminDeleteCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 运营删除评论（admin=true，由 comment 服务写审计）
func NewAdminDeleteCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminDeleteCommentLogic {
	return &AdminDeleteCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminDeleteComment 聚合 comment DeleteComment RPC：operator_mid 作为 admin_mid 传给下游，
// 并固定 admin=true，表示「以运营身份删除」，可绕过本人校验；评论归属、状态推进与审计证据
// 全部由 comment 服务落库，网关不预判某条评论能不能删（AGENTS.md §5/§8）。
func (l *AdminDeleteCommentLogic) AdminDeleteComment(req *types.ParamAdminDeleteComment) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	if err := adminSubjectGate(l.ctx, "adminDeleteComment", "operator_mid", req.OperatorMid); err != nil {
		return nil, err
	}

	if _, err := l.svcCtx.Comment.DeleteComment(l.ctx, &commentrpc.DeleteCommentReq{
		Rpid:  req.Rpid,
		Mid:   req.OperatorMid,
		Admin: true,
	}); err != nil {
		l.Errorf("gateway/admin/adminDeleteComment: operator=%d rpid=%d err=%v", req.OperatorMid, req.Rpid, err)
		return nil, err
	}
	return &types.EmptyResponse{Code: 0, Message: "ok", Data: types.EmptyData{}, TTL: 0}, nil
}
