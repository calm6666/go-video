package logic

import (
	"context"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteCommentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteCommentLogic {
	return &DeleteCommentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// DeleteComment 删除评论。
// admin=true 时管理员可删任意评论；否则仅本人可删。
func (l *DeleteCommentLogic) DeleteComment(in *rpc.DeleteCommentReq) (*rpc.EmptyReply, error) {
	if in.Rpid <= 0 {
		return nil, model.ErrCommentNotFoundOrForbidden
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if err := l.svcCtx.Repository.DeleteComment(l.ctx, in.Rpid, in.Mid, in.Admin); err != nil {
		if err == model.ErrCommentNotFoundOrForbidden {
			return nil, err
		}
		l.Errorf("comment/DeleteComment: rpid=%d mid=%d admin=%v err=%v", in.Rpid, in.Mid, in.Admin, err)
		return nil, err
	}
	return &rpc.EmptyReply{}, nil
}
