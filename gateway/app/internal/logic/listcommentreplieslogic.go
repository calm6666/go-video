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

type ListCommentRepliesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询某根评论下的楼中楼
func NewListCommentRepliesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCommentRepliesLogic {
	return &ListCommentRepliesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListCommentReplies 聚合 comment ListReplies RPC：某根评论下的楼中楼分页。
// 与根评论共用 CommentListResponse；root 之外的可见性规则由 comment 服务判定。
func (l *ListCommentRepliesLogic) ListCommentReplies(req *types.ParamListCommentReplies) (resp *types.CommentListResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	reply, err := l.svcCtx.Comment.ListReplies(l.ctx, &commentrpc.ListRepliesReq{
		Root:      req.Root,
		ViewerMid: req.Mid,
		Pn:        req.Pn,
		Ps:        req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/app/listCommentReplies: root=%d viewer_mid=%d pn=%d ps=%d err=%v",
			req.Root, req.Mid, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.CommentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CommentListData{
			List:  commentListToAPI(reply.GetReplies()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
