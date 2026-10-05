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

type AdminListCommentsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 分页查询目标下的根评论（含被折叠/待审状态，运营可见全量）
func NewAdminListCommentsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListCommentsLogic {
	return &AdminListCommentsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// AdminListComments 聚合 comment ListComments RPC：运营侧按 oid/tp 翻页看根评论全量。
// viewer_mid 固定 0（后台不是某个普通用户，不做黑名单/个性化过滤）；sort 是 comment.v1.SortMode，
// 需要显式升维，未知值直接拒绝而不是让服务侧静默退回热度序。
func (l *AdminListCommentsLogic) AdminListComments(req *types.ParamAdminListComments) (resp *types.AdminCommentListResponse, err error) {
	if l.svcCtx.Comment == nil {
		return nil, errors.New("comment service not configured")
	}
	sortMode, err := commentSortMode(req.Sort)
	if err != nil {
		return nil, err
	}
	pn, ps := normalizeCommentPage(req.Pn, req.Ps)

	reply, err := l.svcCtx.Comment.ListComments(l.ctx, &commentrpc.ListCommentsReq{
		Oid:  req.Oid,
		Tp:   req.Tp,
		Sort: sortMode,
		Pn:   pn,
		Ps:   ps,
	})
	if err != nil {
		l.Errorf("gateway/admin/adminListComments: oid=%d tp=%d sort=%d pn=%d ps=%d err=%v",
			req.Oid, req.Tp, req.Sort, pn, ps, err)
		return nil, err
	}
	return &types.AdminCommentListResponse{
		Code:    0,
		Message: "ok",
		Data: types.AdminCommentListData{
			List:  commentListToAPI(reply.GetComments()),
			Total: reply.GetTotal(),
		},
		TTL: 0,
	}, nil
}
