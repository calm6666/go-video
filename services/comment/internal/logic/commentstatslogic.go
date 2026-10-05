package logic

import (
	"context"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CommentStatsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCommentStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CommentStatsLogic {
	return &CommentStatsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CommentStats 查询目标下的评论计数快照。
func (l *CommentStatsLogic) CommentStats(in *rpc.CommentStatsReq) (*rpc.CommentStatsReply, error) {
	if in.Oid <= 0 {
		return nil, model.ErrInvalidTarget
	}
	total, rootTotal, err := l.svcCtx.Repository.CommentStats(l.ctx, in.Oid, in.Tp)
	if err != nil {
		l.Errorf("comment/CommentStats: oid=%d tp=%d err=%v", in.Oid, in.Tp, err)
		return nil, err
	}
	return &rpc.CommentStatsReply{Total: total, RootTotal: rootTotal}, nil
}
