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

type PostCommentLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 发布评论或楼中楼回复（一律以待审状态落库）
func NewPostCommentLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PostCommentLogic {
	return &PostCommentLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// PostComment 聚合 comment PostComment RPC。
// 一律以 STATE_PENDING 待审状态落库（AGENTS.md §8：不绕过审核）；内容判定与折叠由
// moderation 经服务侧状态推进，网关只注入 mid 与透传 trace_id 供审核链路关联。
func (l *PostCommentLogic) PostComment(req *types.ParamPostComment) (resp *types.CommentPostResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	reply, err := l.svcCtx.Comment.PostComment(l.ctx, &commentrpc.PostCommentReq{
		Oid:     req.Oid,
		Tp:      req.Tp,
		Root:    req.Root,
		Parent:  req.Parent,
		Mid:     req.Mid,
		Content: req.Content,
		State:   int32(commentrpc.CommentState_STATE_PENDING),
		TraceId: req.TraceId,
	})
	if err != nil {
		l.Errorf("gateway/app/postComment: oid=%d tp=%d mid=%d root=%d parent=%d err=%v",
			req.Oid, req.Tp, req.Mid, req.Root, req.Parent, err)
		return nil, err
	}
	return &types.CommentPostResponse{
		Code:    0,
		Message: "ok",
		Data: types.CommentPostData{
			Rpid:  reply.GetRpid(),
			Ctime: reply.GetCtime(),
		},
		TTL: 0,
	}, nil
}
