// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/app/internal/svc"
	"go-video/gateway/app/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 删除本人评论（管理员删除走 gateway/admin）
func NewDeleteCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteCommentLogic {
	return &DeleteCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// DeleteComment 聚合 comment DeleteComment RPC：终端只能删除本人评论。
// admin 固定 false（管理员删除走 gateway/admin）；软删留审计与归属校验由 comment 服务落地。
func (l *DeleteCommentLogic) DeleteComment(req *types.ParamDeleteComment) (resp *types.EmptyResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	if _, err = l.svcCtx.Comment.DeleteComment(l.ctx, &commentrpc.DeleteCommentReq{
		Rpid:  req.Rpid,
		Mid:   req.Mid,
		Admin: false,
	}); err != nil {
		l.Errorf("gateway/app/deleteComment: rpid=%d mid=%d err=%v", req.Rpid, req.Mid, err)
		return nil, err
	}
	return emptyResponse(), nil
}
