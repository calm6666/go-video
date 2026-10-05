package logic

import (
	"context"
	"errors"
	"time"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type PostCommentLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewPostCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PostCommentLogic {
	return &PostCommentLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// PostComment 发布评论或回复。
// state 通常为 STATE_PENDING（4）待审核；UP 主直发可设为 STATE_NORMAL（0）。
func (l *PostCommentLogic) PostComment(in *rpc.PostCommentReq) (*rpc.PostCommentReply, error) {
	if in.Oid <= 0 {
		return nil, model.ErrInvalidTarget
	}
	if in.Mid <= 0 {
		return nil, model.ErrInvalidMid
	}
	if in.Content == "" {
		return nil, model.ErrContentEmpty
	}
	// 校验 root/parent 一致性：有 root 必有 parent，且 parent != 0
	if in.Root > 0 && in.Parent == 0 {
		return nil, errors.New("comment: parent required when root specified")
	}

	now := time.Now().Unix()
	c := &model.Comment{
		Oid:     in.Oid,
		Tp:      in.Tp,
		Root:    in.Root,
		Parent:  in.Parent,
		Mid:     in.Mid,
		Content: in.Content,
		State:   in.State,
		Ctime:   now,
		Mtime:   now,
	}
	rpid, ctime, err := l.svcCtx.Repository.PostComment(l.ctx, c)
	if err != nil {
		l.Errorf("comment/PostComment: oid=%d mid=%d err=%v", in.Oid, in.Mid, err)
		return nil, err
	}
	return &rpc.PostCommentReply{Rpid: rpid, Ctime: ctime}, nil
}
