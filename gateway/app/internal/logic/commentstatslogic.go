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

type CommentStatsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 目标下的评论计数快照
func NewCommentStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CommentStatsLogic {
	return &CommentStatsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// CommentStats 聚合 comment CommentStats RPC：目标下的评论计数快照（含回复）。
// 计数由 comment 服务维护，点赞计数不在本域（AGENTS.md §5 归 engagement）。
func (l *CommentStatsLogic) CommentStats(req *types.ParamCommentStats) (resp *types.CommentStatsResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	reply, err := l.svcCtx.Comment.CommentStats(l.ctx, &commentrpc.CommentStatsReq{
		Oid: req.Oid,
		Tp:  req.Tp,
	})
	if err != nil {
		l.Errorf("gateway/app/commentStats: oid=%d tp=%d err=%v", req.Oid, req.Tp, err)
		return nil, err
	}
	return &types.CommentStatsResponse{
		Code:    0,
		Message: "ok",
		Data: types.CommentStatsData{
			Total:     reply.GetTotal(),
			RootTotal: reply.GetRootTotal(),
		},
		TTL: 0,
	}, nil
}
