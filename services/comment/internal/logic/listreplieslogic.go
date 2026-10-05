package logic

import (
	"context"
	"errors"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListRepliesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListRepliesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListRepliesLogic {
	return &ListRepliesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListReplies 分页查询某根评论下的楼中楼回复。
func (l *ListRepliesLogic) ListReplies(in *rpc.ListRepliesReq) (*rpc.ListRepliesReply, error) {
	if in.Root <= 0 {
		return nil, errors.New("comment: invalid root")
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 || in.Ps > 49 {
		return nil, model.ErrPsTooLarge
	}
	replies, total, err := l.svcCtx.Repository.ListReplies(l.ctx, in.Root, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("comment/ListReplies: root=%d err=%v", in.Root, err)
		return nil, err
	}
	out := make([]*rpc.CommentInfo, 0, len(replies))
	for _, r := range replies {
		out = append(out, toRPCComment(r))
	}
	return &rpc.ListRepliesReply{Replies: out, Total: total}, nil
}
