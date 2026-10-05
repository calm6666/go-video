package logic

import (
	"context"

	"go-video/services/comment/internal/svc"
	"go-video/services/comment/model"
	"go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCommentsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListCommentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCommentsLogic {
	return &ListCommentsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ListComments 分页查询目标下的根评论。
func (l *ListCommentsLogic) ListComments(in *rpc.ListCommentsReq) (*rpc.ListCommentsReply, error) {
	if in.Oid <= 0 {
		return nil, model.ErrInvalidTarget
	}
	if in.Pn <= 0 {
		in.Pn = 1
	}
	if in.Ps <= 0 || in.Ps > 49 {
		return nil, model.ErrPsTooLarge
	}
	sort := "hot"
	if in.Sort == rpc.SortMode_SORT_TIME {
		sort = "time"
	}

	comments, total, err := l.svcCtx.Repository.ListComments(l.ctx, in.Oid, in.Tp, sort, in.Pn, in.Ps)
	if err != nil {
		l.Errorf("comment/ListComments: oid=%d tp=%d sort=%s err=%v", in.Oid, in.Tp, sort, err)
		return nil, err
	}
	out := make([]*rpc.CommentInfo, 0, len(comments))
	for _, c := range comments {
		out = append(out, toRPCComment(c))
	}
	return &rpc.ListCommentsReply{Comments: out, Total: total}, nil
}

// toRPCComment 将 model.Comment 转换为 rpc.CommentInfo。
func toRPCComment(c *model.Comment) *rpc.CommentInfo {
	if c == nil {
		return nil
	}
	return &rpc.CommentInfo{
		Rpid:       c.Rpid,
		Oid:        c.Oid,
		Tp:         c.Tp,
		Root:       c.Root,
		Parent:     c.Parent,
		Mid:        c.Mid,
		Content:    c.Content,
		State:      c.State,
		Ctime:      c.Ctime,
		Mtime:      c.Mtime,
		LikeCount:  c.LikeCount,
		ReplyCount: c.ReplyCount,
	}
}
