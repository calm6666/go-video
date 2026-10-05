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

type ListCommentsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询目标下的根评论
func NewListCommentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCommentsLogic {
	return &ListCommentsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// ListComments 聚合 comment ListComments RPC：目标（oid+tp）下的根评论分页。
// 查看者 mid 原样透传为 viewer_mid，黑名单/屏蔽过滤与 ps 上限（49）均由 comment 服务判定。
func (l *ListCommentsLogic) ListComments(req *types.ParamListComments) (resp *types.CommentListResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	reply, err := l.svcCtx.Comment.ListComments(l.ctx, &commentrpc.ListCommentsReq{
		Oid:       req.Oid,
		Tp:        req.Tp,
		ViewerMid: req.Mid,
		Sort:      commentrpc.SortMode(req.Sort),
		Pn:        req.Pn,
		Ps:        req.Ps,
	})
	if err != nil {
		l.Errorf("gateway/app/listComments: oid=%d tp=%d viewer_mid=%d sort=%d pn=%d ps=%d err=%v",
			req.Oid, req.Tp, req.Mid, req.Sort, req.Pn, req.Ps, err)
		return nil, err
	}
	return &types.CommentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.CommentListData{
			List:  commentListToAPI(reply.GetComments()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
