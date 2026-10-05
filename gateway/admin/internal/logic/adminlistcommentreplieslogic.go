// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package logic

import (
	"context"
	"errors"

	"go-video/gateway/admin/internal/svc"
	"go-video/gateway/admin/internal/types"
	commentrpc "go-video/services/comment/rpc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListCommentRepliesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询某根评论下的楼中楼
func NewAdminListCommentRepliesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListCommentRepliesLogic {
	return &AdminListCommentRepliesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminListCommentReplies 聚合 comment ListReplies RPC：按 root 翻楼中楼。
// 回复没有排序维度（服务侧固定时间正序），分页口径与 ListComments 共用 ps<=49；
// 被折叠/已删除的回复同样返回，运营要看的是完整楼层。
func (l *AdminListCommentRepliesLogic) AdminListCommentReplies(req *types.ParamAdminListCommentReplies) (resp *types.AdminCommentListResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	pn, ps := normalizeCommentPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Comment.ListReplies(l.ctx, &commentrpc.ListRepliesReq{
		Root: req.Root,
		Pn:   pn,
		Ps:   ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/adminListCommentReplies: root=%d pn=%d ps=%d err=%v", req.Root, pn, ps, err)
		return nil, err
	}
	return &types.AdminCommentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminCommentListData{
			List:  commentListToAPI(reply.GetReplies()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
